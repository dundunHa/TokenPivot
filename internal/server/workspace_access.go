package server

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/dundunHa/TokenPivot/internal/auth"
	"github.com/dundunHa/TokenPivot/internal/workspaces"
	"github.com/jackc/pgx/v5/pgtype"
)

// writeRoute wraps a browser write handler: no-store, then the strict Origin
// check, then the session. Origin runs first, so a request with a bad Origin is
// refused before any session lookup.
func (a *Auth) writeRoute(handler http.Handler) http.Handler {
	return a.noStore(a.RequireOrigin(a.RequireSession(handler)))
}

// readRoute wraps a browser read handler: no-store and the session only. Reads
// do not require an Origin.
func (a *Auth) readRoute(handler http.Handler) http.Handler {
	return a.noStore(a.RequireSession(handler))
}

// requireMember proves the session user belongs to the path workspace in any
// role. It is the shared gate of every browser read of workspace resources.
func requireMember(w http.ResponseWriter, r *http.Request, memberships *workspaces.Service) (workspaces.ID, bool) {
	return requireRole(w, r, memberships, false)
}

// requireOwner proves the session user is the owner of the path workspace. It is
// the shared gate of every browser write of workspace resources.
func requireOwner(w http.ResponseWriter, r *http.Request, memberships *workspaces.Service) (workspaces.ID, bool) {
	return requireRole(w, r, memberships, true)
}

// requireRole resolves the path workspace and the session user's role through
// the one membership lookup, MembershipRole. Every rejection happens before any
// resource read or write. A lookup failure is a real error (500), never a
// permission decision; a non-member and an insufficient role are both 403.
func requireRole(w http.ResponseWriter, r *http.Request, memberships *workspaces.Service, ownerOnly bool) (workspaces.ID, bool) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
		return workspaces.ID{}, false
	}
	workspaceID, ok := pathID(w, r, "workspaceID")
	if !ok {
		return workspaces.ID{}, false
	}
	role, found, err := memberships.MembershipRole(r.Context(), membershipUserID(user), workspaceID)
	if err != nil {
		slog.ErrorContext(r.Context(), "workspace access failed", "stage", "workspace role", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return workspaces.ID{}, false
	}
	if !found {
		writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "workspace membership required")
		return workspaces.ID{}, false
	}
	if ownerOnly && role != workspaces.RoleOwner {
		writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "workspace owner required")
		return workspaces.ID{}, false
	}
	return workspaceID, true
}

// membershipUserID converts the authenticated browser identity into the UUID
// form the membership lookup uses. It takes the value from the session only.
func membershipUserID(user auth.User) workspaces.ID {
	return workspaces.ID{Bytes: user.ID, Valid: true}
}

// pathID parses one UUID path parameter. A malformed value is rejected as an
// invalid argument rather than being passed to the database.
func pathID(w http.ResponseWriter, r *http.Request, name string) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if err := id.Scan(r.PathValue(name)); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "path parameter must be a UUID")
		return pgtype.UUID{}, false
	}
	return id, true
}

// requireEmptyBody enforces the empty request body. Ownership and origin always
// come from the session, the request path and the stored row, so a body is
// never read as input: any content, including a workspace_id or source_id, is
// rejected before the database is touched.
func requireEmptyBody(w http.ResponseWriter, r *http.Request) bool {
	if r.Body == nil {
		return true
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "request body could not be read")
		return false
	}
	if len(body) != 0 {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "request body must be empty")
		return false
	}
	return true
}
