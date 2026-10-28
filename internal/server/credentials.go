package server

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/dundunHa/TokenPivot/internal/auth"
	"github.com/dundunHa/TokenPivot/internal/credentials"
	"github.com/dundunHa/TokenPivot/internal/workspaces"
	"github.com/jackc/pgx/v5/pgtype"
)

// Credentials bundles the owner-only credential management dependencies: the
// credential service and the membership role lookup that gates every request.
// The session and Origin checks are the shared Auth middleware, so this adapter
// never parses a cookie itself and never trusts the request Host.
type Credentials struct {
	Service    *credentials.Service
	Workspaces *workspaces.Service
}

// Credential management routes. They are browser owner endpoints under /api/v1;
// plugin tokens are not accepted here.
const (
	credentialsCollectionPath = "/api/v1/workspaces/{workspaceID}/sources/{sourceID}/credentials"
	credentialsRotatePath     = "/api/v1/workspaces/{workspaceID}/credentials/{credentialID}/rotate"
	credentialsRevokePath     = "/api/v1/workspaces/{workspaceID}/credentials/{credentialID}/revoke"
)

// credentialResponse is the list item and part of the issue/rotate body. It
// never carries a token, a digest or any digest-derived value.
type credentialResponse struct {
	CredentialID string  `json:"credential_id"`
	WorkspaceID  string  `json:"workspace_id"`
	SourceID     string  `json:"source_id"`
	RevokedAt    *string `json:"revoked_at"`
}

// credentialListResponse is the list body: every credential of the path source,
// revoked rows included.
type credentialListResponse struct {
	Credentials []credentialResponse `json:"credentials"`
}

// issuedCredentialResponse is the issue and rotate body. Token is the only
// place a plaintext token ever appears. A freshly issued credential is always
// active, so revocation state stays out of this body.
type issuedCredentialResponse struct {
	CredentialID string `json:"credential_id"`
	WorkspaceID  string `json:"workspace_id"`
	SourceID     string `json:"source_id"`
	Token        string `json:"token"`
}

// NewCredentials returns the management adapter for a credential service and a
// membership service. Both are required; routes are only registered when the
// handler also has an Auth adapter for the session and Origin gates.
func NewCredentials(service *credentials.Service, memberships *workspaces.Service) *Credentials {
	return &Credentials{Service: service, Workspaces: memberships}
}

// routes registers the list, issue, rotate and revoke endpoints on mux. Writes
// also require a matching Origin; the list is a read and needs only the
// session. A missing service, membership lookup or session adapter registers
// nothing, so the paths stay unreachable instead of failing open.
func (c *Credentials) routes(mux *http.ServeMux, sessions *Auth) {
	if c == nil || c.Service == nil || c.Workspaces == nil || sessions == nil || sessions.Service == nil {
		return
	}
	// Every response is uncacheable: the issue and rotate bodies carry a
	// one-time plaintext token, and a cached list would hide a revocation.
	// Writes check the Origin and then the session, exactly like the browser
	// authentication writes; the list is a read and skips only the Origin gate.
	write := func(handler http.Handler) http.Handler {
		return sessions.noStore(sessions.RequireOrigin(sessions.RequireSession(handler)))
	}
	read := func(handler http.Handler) http.Handler {
		return sessions.noStore(sessions.RequireSession(handler))
	}
	mux.Handle("GET "+credentialsCollectionPath, read(http.HandlerFunc(c.handleList)))
	mux.Handle("POST "+credentialsCollectionPath, write(http.HandlerFunc(c.handleIssue)))
	mux.Handle("POST "+credentialsRotatePath, write(http.HandlerFunc(c.handleRotate)))
	mux.Handle("POST "+credentialsRevokePath, write(http.HandlerFunc(c.handleRevoke)))
}

// handleList returns the metadata of every credential of the path workspace and
// source, revoked rows included. It never returns a token or a digest, and it
// creates nothing.
func (c *Credentials) handleList(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := c.requireOwner(w, r)
	if !ok {
		return
	}
	sourceID, ok := pathID(w, r, "sourceID")
	if !ok {
		return
	}
	list, err := c.Service.List(r.Context(), workspaceID, sourceID)
	if err != nil {
		writeCredentialError(r, w, err)
		return
	}
	response := credentialListResponse{Credentials: make([]credentialResponse, 0, len(list))}
	for _, credential := range list {
		response.Credentials = append(response.Credentials, newCredentialResponse(credential))
	}
	writeJSON(w, http.StatusOK, response)
}

// handleIssue creates one credential for the path source. The request body must
// be empty: neither the workspace, the source, nor any option may be supplied.
func (c *Credentials) handleIssue(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := c.requireOwner(w, r)
	if !ok {
		return
	}
	if !requireEmptyBody(w, r) {
		return
	}
	sourceID, ok := pathID(w, r, "sourceID")
	if !ok {
		return
	}
	issued, err := c.Service.Issue(r.Context(), workspaceID, sourceID)
	if err != nil {
		writeCredentialError(r, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, newIssuedCredentialResponse(issued))
}

// handleRotate creates a new credential for the fixed workspace and source of
// an existing credential. The old row keeps its revocation state, so both may
// be active until the owner revokes the old one.
func (c *Credentials) handleRotate(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := c.requireOwner(w, r)
	if !ok {
		return
	}
	if !requireEmptyBody(w, r) {
		return
	}
	credentialID, ok := pathID(w, r, "credentialID")
	if !ok {
		return
	}
	issued, err := c.Service.Rotate(r.Context(), workspaceID, credentialID)
	if err != nil {
		writeCredentialError(r, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, newIssuedCredentialResponse(issued))
}

// handleRevoke revokes one credential of the path workspace. Repeated
// revocation keeps the first revoked_at value and answers the same 204.
func (c *Credentials) handleRevoke(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := c.requireOwner(w, r)
	if !ok {
		return
	}
	if !requireEmptyBody(w, r) {
		return
	}
	credentialID, ok := pathID(w, r, "credentialID")
	if !ok {
		return
	}
	if err := c.Service.Revoke(r.Context(), workspaceID, credentialID); err != nil {
		writeCredentialError(r, w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// requireOwner resolves the path workspace and proves the session user is its
// owner. Every rejection happens before any credential read or write. A
// database failure is a real error, never a permission decision.
func (c *Credentials) requireOwner(w http.ResponseWriter, r *http.Request) (credentials.ID, bool) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
		return credentials.ID{}, false
	}
	workspaceID, ok := pathID(w, r, "workspaceID")
	if !ok {
		return credentials.ID{}, false
	}
	role, found, err := c.Workspaces.MembershipRole(r.Context(), membershipUserID(user), workspaceID)
	if err != nil {
		slog.ErrorContext(r.Context(), "credential request failed", "stage", "credentials owner", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return credentials.ID{}, false
	}
	if !found || role != workspaces.RoleOwner {
		writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "workspace owner required")
		return credentials.ID{}, false
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

// writeCredentialError maps a credential service error to its HTTP status
// without exposing a database message, a token or a digest.
func writeCredentialError(r *http.Request, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, credentials.ErrNotFound):
		// A source or credential outside the path workspace is indistinguishable
		// from a missing one, so no foreign resource is revealed.
		writeError(w, http.StatusNotFound, "NOT_FOUND", "credential resource not found")
	case errors.Is(err, credentials.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
	default:
		slog.ErrorContext(r.Context(), "credential request failed", "stage", "credentials", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
	}
}

// newCredentialResponse converts stored metadata into the list shape.
func newCredentialResponse(credential credentials.Credential) credentialResponse {
	return credentialResponse{
		CredentialID: credential.ID.String(),
		WorkspaceID:  credential.WorkspaceID.String(),
		SourceID:     credential.SourceID.String(),
		RevokedAt:    revokedAtString(credential.RevokedAt),
	}
}

// newIssuedCredentialResponse converts a one-time issuance into its response.
func newIssuedCredentialResponse(issued credentials.IssuedCredential) issuedCredentialResponse {
	return issuedCredentialResponse{
		CredentialID: issued.ID.String(),
		WorkspaceID:  issued.WorkspaceID.String(),
		SourceID:     issued.SourceID.String(),
		Token:        issued.Token,
	}
}

// revokedAtString formats the revocation stamp as RFC 3339 UTC, or nil while
// the credential is active.
func revokedAtString(revokedAt *time.Time) *string {
	if revokedAt == nil {
		return nil
	}
	formatted := revokedAt.UTC().Format(time.RFC3339)
	return &formatted
}
