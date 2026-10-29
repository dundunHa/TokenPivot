package server

import (
	"net/http"
	"time"

	"github.com/dundunHa/TokenPivot/internal/sources"
	"github.com/dundunHa/TokenPivot/internal/workspaces"
)

// Sources bundles the data source management dependencies: the source service
// and the membership role lookup that gates every request.
type Sources struct {
	Service    *sources.Service
	Workspaces *workspaces.Service
}

// Source management routes under /api/v1.
const (
	sourcesCollectionPath = "/api/v1/workspaces/{workspaceID}/sources"
	sourcesItemPath       = "/api/v1/workspaces/{workspaceID}/sources/{sourceID}"
)

// sourceResponse is the source JSON shape.
type sourceResponse struct {
	SourceID       string  `json:"source_id"`
	WorkspaceID    string  `json:"workspace_id"`
	Platform       string  `json:"platform"`
	InstanceKey    string  `json:"instance_key"`
	CreatedAt      string  `json:"created_at"`
	LastReceivedAt *string `json:"last_received_at"`
	ArchivedAt     *string `json:"archived_at"`
}

// NewSources returns the source management adapter. Both dependencies are
// required; routes are only registered when the handler also has an Auth adapter.
func NewSources(service *sources.Service, memberships *workspaces.Service) *Sources {
	return &Sources{Service: service, Workspaces: memberships}
}

// routes registers the list, create, update and archive endpoints. A missing
// dependency registers nothing, so the paths stay unreachable instead of failing
// open.
func (s *Sources) routes(mux *http.ServeMux, sessions *Auth) {
	if s == nil || s.Service == nil || s.Workspaces == nil || sessions == nil || sessions.Service == nil {
		return
	}
	mux.Handle("GET "+sourcesCollectionPath, sessions.readRoute(http.HandlerFunc(s.handleList)))
	mux.Handle("POST "+sourcesCollectionPath, sessions.writeRoute(http.HandlerFunc(s.handleCreate)))
	mux.Handle("PATCH "+sourcesItemPath, sessions.writeRoute(http.HandlerFunc(s.handleUpdate)))
	mux.Handle("DELETE "+sourcesItemPath, sessions.writeRoute(http.HandlerFunc(s.handleArchive)))
}

func (s *Sources) handleList(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := requireMember(w, r, s.Workspaces)
	if !ok {
		return
	}
	list, err := s.Service.List(r.Context(), workspaceID)
	if err != nil {
		writeManagementError(r, w, err)
		return
	}
	response := make([]sourceResponse, 0, len(list))
	for _, source := range list {
		response = append(response, newSourceResponse(source))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Sources) handleCreate(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := requireOwner(w, r, s.Workspaces)
	if !ok {
		return
	}
	fields, ok := readManagementBody(w, r, []string{"platform", "instance_key"}, []string{"platform", "instance_key"})
	if !ok {
		return
	}
	source, err := s.Service.Create(r.Context(), workspaceID, fields["platform"], fields["instance_key"])
	if err != nil {
		writeManagementError(r, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, newSourceResponse(source))
}

func (s *Sources) handleUpdate(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := requireOwner(w, r, s.Workspaces)
	if !ok {
		return
	}
	sourceID, ok := pathID(w, r, "sourceID")
	if !ok {
		return
	}
	fields, ok := readManagementBody(w, r, []string{"platform", "instance_key"}, nil)
	if !ok {
		return
	}
	var platform, instanceKey *string
	if value, present := fields["platform"]; present {
		platform = &value
	}
	if value, present := fields["instance_key"]; present {
		instanceKey = &value
	}
	source, err := s.Service.Update(r.Context(), workspaceID, sourceID, platform, instanceKey)
	if err != nil {
		writeManagementError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, newSourceResponse(source))
}

func (s *Sources) handleArchive(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := requireOwner(w, r, s.Workspaces)
	if !ok {
		return
	}
	sourceID, ok := pathID(w, r, "sourceID")
	if !ok {
		return
	}
	if !requireEmptyBody(w, r) {
		return
	}
	if err := s.Service.Archive(r.Context(), workspaceID, sourceID); err != nil {
		writeManagementError(r, w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func newSourceResponse(source sources.Source) sourceResponse {
	return sourceResponse{
		SourceID:       source.ID.String(),
		WorkspaceID:    source.WorkspaceID.String(),
		Platform:       source.Platform,
		InstanceKey:    source.InstanceKey,
		CreatedAt:      source.CreatedAt.UTC().Format(time.RFC3339),
		LastReceivedAt: revokedAtString(source.LastReceivedAt),
		ArchivedAt:     revokedAtString(source.ArchivedAt),
	}
}
