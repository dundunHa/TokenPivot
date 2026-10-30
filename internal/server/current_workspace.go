package server

import (
	"log/slog"
	"net/http"

	"github.com/dundunHa/TokenPivot/internal/workspaces"
)

// CurrentWorkspace serves the signed-in user's current workspace. It is not a
// workspace list or switcher: it answers "the personal workspace" while every
// user has exactly one.
type CurrentWorkspace struct {
	Workspaces *workspaces.Service
}

// currentWorkspacePath is the browser GET route.
const currentWorkspacePath = "/api/v1/workspaces/current"

// currentWorkspaceResponse is the success body.
type currentWorkspaceResponse struct {
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
}

// NewCurrentWorkspace returns the adapter for a membership service.
func NewCurrentWorkspace(memberships *workspaces.Service) *CurrentWorkspace {
	return &CurrentWorkspace{Workspaces: memberships}
}

// routes registers the GET endpoint. A missing dependency registers nothing.
func (c *CurrentWorkspace) routes(mux *http.ServeMux, sessions *Auth) {
	if c == nil || c.Workspaces == nil || sessions == nil || sessions.Service == nil {
		return
	}
	mux.Handle("GET "+currentWorkspacePath, sessions.readRoute(http.HandlerFunc(c.handle)))
}

// handle identifies the user from the session only. A missing workspace is a
// data integrity fault (500); nothing is created and no placeholder is returned.
func (c *CurrentWorkspace) handle(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
		return
	}
	workspace, found, err := c.Workspaces.CurrentWorkspace(r.Context(), membershipUserID(user))
	if err != nil {
		slog.ErrorContext(r.Context(), "current workspace failed", "stage", "current workspace", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	if !found {
		slog.ErrorContext(r.Context(), "current workspace missing", "stage", "current workspace")
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, currentWorkspaceResponse{WorkspaceID: workspace.ID.String(), Name: workspace.Name})
}
