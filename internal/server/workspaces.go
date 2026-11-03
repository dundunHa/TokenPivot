package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/dundunHa/TokenPivot/internal/workspaces"
)

// Workspaces serves the browser workspace endpoints: list the signed-in user's
// workspaces, create one, and read one to verify a switch target. Identity
// always comes from the session; no path or body value may choose another user.
// Membership itself is still decided per request by requireMember, so these
// endpoints grant no lasting access.
type Workspaces struct {
	Service *workspaces.Service
	// create is the creation use case. The transaction seam of workspace
	// creation is internal to the workspaces package, so the HTTP layer holds
	// this narrow function and its tests can exercise the error mapping without
	// a database; the production value is Service.Create.
	create createWorkspaceFunc
}

// createWorkspaceFunc creates one workspace for the signed-in account.
type createWorkspaceFunc func(ctx context.Context, userID workspaces.ID, name string) (workspaces.Workspace, error)

// Browser workspace routes. There is no /current route: the list is the only
// zero-workspace answer, and a target is verified through the detail route.
const (
	workspacesCollectionPath = "/api/v1/workspaces"
	workspaceItemPath        = "/api/v1/workspaces/{workspaceID}"
)

// workspaceResponse is the workspace JSON shape.
type workspaceResponse struct {
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
}

// NewWorkspaces returns the workspace endpoint adapter.
func NewWorkspaces(service *workspaces.Service) *Workspaces {
	handler := &Workspaces{Service: service}
	if service != nil {
		handler.create = service.Create
	}
	return handler
}

// routes registers the list, create and detail endpoints. A missing dependency
// registers nothing, so the paths stay unreachable instead of failing open.
func (w *Workspaces) routes(mux *http.ServeMux, sessions *Auth) {
	if w == nil || w.Service == nil || w.create == nil || sessions == nil || sessions.Service == nil {
		return
	}
	mux.Handle("GET "+workspacesCollectionPath, sessions.readRoute(http.HandlerFunc(w.handleList)))
	mux.Handle("POST "+workspacesCollectionPath, sessions.writeRoute(http.HandlerFunc(w.handleCreate)))
	mux.Handle("GET "+workspaceItemPath, sessions.readRoute(http.HandlerFunc(w.handleGet)))
}

// handleList returns every workspace the session user belongs to in the order
// the database decided. No membership is a normal empty array, never an error
// and never a placeholder workspace.
func (w *Workspaces) handleList(rw http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(rw, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
		return
	}
	list, err := w.Service.List(r.Context(), membershipUserID(user))
	if err != nil {
		slog.ErrorContext(r.Context(), "workspace list failed", "stage", "workspace list", "err", err)
		writeError(rw, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	response := make([]workspaceResponse, 0, len(list))
	for _, workspace := range list {
		response = append(response, newWorkspaceResponse(workspace))
	}
	writeJSON(rw, http.StatusOK, response)
}

// handleCreate creates a workspace for the session user and makes the account
// its owner. The admission order is the shared write route: Origin, then
// session, then this strict body, then the database.
func (w *Workspaces) handleCreate(rw http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(rw, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
		return
	}
	fields, ok := readStrictManagementFields(rw, r, []string{"name"}, []string{"name"})
	if !ok {
		return
	}
	workspace, err := w.create(r.Context(), membershipUserID(user), fields["name"])
	if err != nil {
		writeWorkspaceCreateError(r, rw, err)
		return
	}
	writeJSON(rw, http.StatusCreated, newWorkspaceResponse(workspace))
}

// handleGet verifies the session user's membership first and only then reads
// the row. A non-member and a missing workspace are both 403; a membership that
// outlives its workspace is an integrity fault (500) and is not turned into a
// permission decision.
func (w *Workspaces) handleGet(rw http.ResponseWriter, r *http.Request) {
	workspaceID, ok := requireMember(rw, r, w.Service)
	if !ok {
		return
	}
	workspace, found, err := w.Service.Workspace(r.Context(), workspaceID)
	if err != nil {
		slog.ErrorContext(r.Context(), "workspace detail failed", "stage", "workspace detail", "err", err)
		writeError(rw, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	if !found {
		slog.ErrorContext(r.Context(), "workspace missing after membership", "stage", "workspace detail")
		writeError(rw, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	writeJSON(rw, http.StatusOK, newWorkspaceResponse(workspace))
}

func newWorkspaceResponse(workspace workspaces.Workspace) workspaceResponse {
	return workspaceResponse{WorkspaceID: workspace.ID.String(), Name: workspace.Name}
}

// writeWorkspaceCreateError maps a creation error to the documented status and
// fixed message. Only the account-scoped name conflict is a business conflict;
// every other failure stays internal and never echoes a database message.
func writeWorkspaceCreateError(r *http.Request, rw http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workspaces.ErrInvalidName):
		writeError(rw, http.StatusBadRequest, "INVALID_ARGUMENT", "request is not acceptable")
	case errors.Is(err, workspaces.ErrNameConflict):
		writeError(rw, http.StatusConflict, "WORKSPACE_NAME_CONFLICT", "workspace name already exists")
	case errors.Is(err, workspaces.ErrLimitReached):
		writeError(rw, http.StatusConflict, "WORKSPACE_LIMIT_REACHED", "workspace limit reached")
	default:
		slog.ErrorContext(r.Context(), "workspace create failed", "stage", "workspace create", "err", err)
		writeError(rw, http.StatusInternalServerError, "INTERNAL", "internal error")
	}
}
