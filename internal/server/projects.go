package server

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/dundunHa/TokenPivot/internal/projects"
	"github.com/dundunHa/TokenPivot/internal/sources"
	"github.com/dundunHa/TokenPivot/internal/workspaces"
)

// Projects bundles the project management dependencies: the project service and
// the membership role lookup that gates every request. Session and Origin are
// the shared Auth middleware.
type Projects struct {
	Service    *projects.Service
	Workspaces *workspaces.Service
}

// Project management routes under /api/v1.
const (
	projectsCollectionPath = "/api/v1/workspaces/{workspaceID}/projects"
	projectsItemPath       = "/api/v1/workspaces/{workspaceID}/projects/{projectID}"
)

// projectResponse is the project JSON shape.
type projectResponse struct {
	ProjectID   string  `json:"project_id"`
	WorkspaceID string  `json:"workspace_id"`
	ProjectKey  string  `json:"project_key"`
	Name        string  `json:"name"`
	CreatedAt   string  `json:"created_at"`
	ArchivedAt  *string `json:"archived_at"`
}

// NewProjects returns the project management adapter. Both dependencies are
// required; routes are only registered when the handler also has an Auth adapter.
func NewProjects(service *projects.Service, memberships *workspaces.Service) *Projects {
	return &Projects{Service: service, Workspaces: memberships}
}

// routes registers the list, create, update and archive endpoints. A missing
// dependency registers nothing, so the paths stay unreachable instead of failing
// open.
func (p *Projects) routes(mux *http.ServeMux, sessions *Auth) {
	if p == nil || p.Service == nil || p.Workspaces == nil || sessions == nil || sessions.Service == nil {
		return
	}
	mux.Handle("GET "+projectsCollectionPath, sessions.readRoute(http.HandlerFunc(p.handleList)))
	mux.Handle("POST "+projectsCollectionPath, sessions.writeRoute(http.HandlerFunc(p.handleCreate)))
	mux.Handle("PATCH "+projectsItemPath, sessions.writeRoute(http.HandlerFunc(p.handleUpdate)))
	mux.Handle("DELETE "+projectsItemPath, sessions.writeRoute(http.HandlerFunc(p.handleArchive)))
}

func (p *Projects) handleList(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := requireMember(w, r, p.Workspaces)
	if !ok {
		return
	}
	list, err := p.Service.List(r.Context(), workspaceID)
	if err != nil {
		writeManagementError(r, w, err)
		return
	}
	response := make([]projectResponse, 0, len(list))
	for _, project := range list {
		response = append(response, newProjectResponse(project))
	}
	writeJSON(w, http.StatusOK, response)
}

func (p *Projects) handleCreate(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := requireOwner(w, r, p.Workspaces)
	if !ok {
		return
	}
	fields, ok := readManagementBody(w, r, []string{"project_key", "name"}, []string{"project_key", "name"})
	if !ok {
		return
	}
	project, err := p.Service.Create(r.Context(), workspaceID, fields["project_key"], fields["name"])
	if err != nil {
		writeManagementError(r, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, newProjectResponse(project))
}

func (p *Projects) handleUpdate(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := requireOwner(w, r, p.Workspaces)
	if !ok {
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	fields, ok := readManagementBody(w, r, []string{"name"}, []string{"name"})
	if !ok {
		return
	}
	project, err := p.Service.Update(r.Context(), workspaceID, projectID, fields["name"])
	if err != nil {
		writeManagementError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, newProjectResponse(project))
}

func (p *Projects) handleArchive(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := requireOwner(w, r, p.Workspaces)
	if !ok {
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	if !requireEmptyBody(w, r) {
		return
	}
	if err := p.Service.Archive(r.Context(), workspaceID, projectID); err != nil {
		writeManagementError(r, w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func newProjectResponse(project projects.Project) projectResponse {
	return projectResponse{
		ProjectID:   project.ID.String(),
		WorkspaceID: project.WorkspaceID.String(),
		ProjectKey:  project.Key,
		Name:        project.Name,
		CreatedAt:   project.CreatedAt.UTC().Format(time.RFC3339),
		ArchivedAt:  revokedAtString(project.ArchivedAt),
	}
}

// writeManagementError maps a project or source error to its HTTP status without
// exposing a database message or a foreign resource.
func writeManagementError(r *http.Request, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, projects.ErrNotFound), errors.Is(err, sources.ErrNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, projects.ErrConflict), errors.Is(err, sources.ErrConflict):
		writeError(w, http.StatusConflict, "CONFLICT", "resource already exists")
	default:
		slog.ErrorContext(r.Context(), "management request failed", "stage", "management", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
	}
}
