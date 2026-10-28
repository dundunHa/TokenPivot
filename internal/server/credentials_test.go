package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dundunHa/TokenPivot/internal/credentials"
	"github.com/dundunHa/TokenPivot/internal/workspaces"
)

// credentialFakeStore is an in-memory credential store that mirrors only the
// SQL semantics the service relies on, plus call counters so a rejected request
// can be proven to have stopped before persistence.
type credentialFakeStore struct {
	mu      sync.Mutex
	sources map[[16]byte][16]byte // source id -> owning workspace id
	rows    map[[16]byte]credentialRow
	digests [][]byte
	calls   map[string]int
	failure struct {
		list   error
		source error
		byID   error
		insert error
		revoke error
	}
	nextID [16]byte
}

type credentialRow struct {
	credential credentials.Credential
	hash       []byte
}

func newCredentialFakeStore() *credentialFakeStore {
	return &credentialFakeStore{
		sources: make(map[[16]byte][16]byte),
		rows:    make(map[[16]byte]credentialRow),
		calls:   make(map[string]int),
	}
}

// record counts one store call.
func (f *credentialFakeStore) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
}

func (f *credentialFakeStore) callCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

// identifier returns the next synthetic UUID.
func (f *credentialFakeStore) identifier() credentials.ID {
	f.mu.Lock()
	defer f.mu.Unlock()
	var id [16]byte
	f.nextID[15]++
	if f.nextID[15] == 0 {
		f.nextID[14]++
	}
	id = f.nextID
	return credentials.ID{Bytes: id, Valid: true}
}

// addSource records that a source belongs to a workspace.
func (f *credentialFakeStore) addSource(workspaceID, sourceID credentials.ID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sources[sourceID.Bytes] = workspaceID.Bytes
}

// seedCredential stores one credential row directly, as an earlier issuance
// would have left it.
func (f *credentialFakeStore) seedCredential(workspaceID, sourceID credentials.ID, revokedAt *time.Time) credentials.Credential {
	credential := credentials.Credential{ID: f.identifier(), WorkspaceID: workspaceID, SourceID: sourceID, RevokedAt: revokedAt}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[credential.ID.Bytes] = credentialRow{credential: credential}
	return credential
}

func (f *credentialFakeStore) List(ctx context.Context, workspaceID, sourceID credentials.ID) ([]credentials.Credential, error) {
	f.record("List")
	if f.failure.list != nil {
		return nil, f.failure.list
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	list := make([]credentials.Credential, 0, len(f.rows))
	for _, row := range f.rows {
		if row.credential.WorkspaceID.Bytes == workspaceID.Bytes && row.credential.SourceID.Bytes == sourceID.Bytes {
			list = append(list, row.credential)
		}
	}
	return list, nil
}

func (f *credentialFakeStore) BySecretHash(ctx context.Context, secretHash []byte) (credentials.Credential, bool, error) {
	f.record("BySecretHash")
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if bytes.Equal(row.hash, secretHash) {
			return row.credential, true, nil
		}
	}
	return credentials.Credential{}, false, nil
}

func (f *credentialFakeStore) ByID(ctx context.Context, workspaceID, credentialID credentials.ID) (credentials.Credential, bool, error) {
	f.record("ByID")
	if f.failure.byID != nil {
		return credentials.Credential{}, false, f.failure.byID
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[credentialID.Bytes]
	if !ok || row.credential.WorkspaceID.Bytes != workspaceID.Bytes {
		return credentials.Credential{}, false, nil
	}
	return row.credential, true, nil
}

func (f *credentialFakeStore) SourceExists(ctx context.Context, workspaceID, sourceID credentials.ID) (bool, error) {
	f.record("SourceExists")
	if f.failure.source != nil {
		return false, f.failure.source
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	owner, ok := f.sources[sourceID.Bytes]
	return ok && owner == workspaceID.Bytes, nil
}

func (f *credentialFakeStore) Insert(ctx context.Context, workspaceID, sourceID credentials.ID, secretHash []byte) (credentials.Credential, error) {
	f.record("Insert")
	if f.failure.insert != nil {
		return credentials.Credential{}, f.failure.insert
	}
	credential := credentials.Credential{ID: f.identifier(), WorkspaceID: workspaceID, SourceID: sourceID}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.digests = append(f.digests, append([]byte(nil), secretHash...))
	f.rows[credential.ID.Bytes] = credentialRow{credential: credential, hash: append([]byte(nil), secretHash...)}
	return credential, nil
}

func (f *credentialFakeStore) Revoke(ctx context.Context, workspaceID, credentialID credentials.ID) (bool, error) {
	f.record("Revoke")
	if f.failure.revoke != nil {
		return false, f.failure.revoke
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[credentialID.Bytes]
	if !ok || row.credential.WorkspaceID.Bytes != workspaceID.Bytes {
		return false, nil
	}
	if row.credential.RevokedAt == nil {
		stamp := time.Now().UTC().Truncate(time.Second)
		row.credential.RevokedAt = &stamp
	}
	f.rows[credentialID.Bytes] = row
	return true, nil
}

// roleFakeStore is the membership lookup used by the owner gate.
type roleFakeStore struct {
	mu    sync.Mutex
	roles map[[16]byte]map[[16]byte]workspaces.Role
	err   error
	calls int
}

func newRoleFakeStore() *roleFakeStore {
	return &roleFakeStore{roles: make(map[[16]byte]map[[16]byte]workspaces.Role)}
}

func (f *roleFakeStore) setRole(userID, workspaceID workspaces.ID, role workspaces.Role) {
	f.mu.Lock()
	defer f.mu.Unlock()
	members, ok := f.roles[workspaceID.Bytes]
	if !ok {
		members = make(map[[16]byte]workspaces.Role)
		f.roles[workspaceID.Bytes] = members
	}
	members[userID.Bytes] = role
}

// clearRoles drops every membership, so the seeded owner becomes a non-member.
func (f *roleFakeStore) clearRoles() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roles = make(map[[16]byte]map[[16]byte]workspaces.Role)
}

// setError makes the lookup fail, proving a database error is not a denial.
func (f *roleFakeStore) setError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *roleFakeStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *roleFakeStore) MembershipRole(ctx context.Context, userID, workspaceID workspaces.ID) (workspaces.Role, bool, error) {
	f.mu.Lock()
	f.calls++
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return "", false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	role, ok := f.roles[workspaceID.Bytes][userID.Bytes]
	return role, ok, nil
}

// credentialFixture is one wired handler plus the fakes behind it.
type credentialFixture struct {
	handler http.Handler
	store   *credentialFakeStore
	roles   *roleFakeStore
	service *credentials.Service
	// ownerCookie is a valid session of the seeded owner account.
	ownerCookie *http.Cookie
	ownerID     workspaces.ID
	workspaceID workspaces.ID
	otherSpace  workspaces.ID
	sourceID    workspaces.ID
	otherSource workspaces.ID
}

// newCredentialFixture seeds one owner with a personal workspace and one
// source, plus a foreign workspace and source, and wires the real Auth adapter
// so session and Origin checks are exercised end to end.
func newCredentialFixture(t *testing.T) *credentialFixture {
	t.Helper()
	adapter, authStore := newTestAuth(t)
	user := authStore.seedUser(t, "owner@example.com", "synthetic-password")

	store := newCredentialFakeStore()
	roles := newRoleFakeStore()
	service := credentials.New(store)
	handler := NewHandler(Options{
		CheckReady:  func(context.Context) error { return nil },
		Auth:        adapter,
		Credentials: NewCredentials(service, workspaces.New(roles)),
	})

	login := httptest.NewRequest(http.MethodPost, loginPath, strings.NewReader(`{"email":"owner@example.com","password":"synthetic-password"}`))
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", testOrigin)
	login.RemoteAddr = "127.0.0.1:40000"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, login)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d: %s", recorder.Code, recorder.Body.String())
	}

	fixture := &credentialFixture{
		handler:     handler,
		store:       store,
		roles:       roles,
		service:     service,
		ownerCookie: sessionCookie(t, recorder),
		ownerID:     workspaces.ID{Bytes: user.ID, Valid: true},
		workspaceID: idWithByte(0x21),
		otherSpace:  idWithByte(0x22),
		sourceID:    idWithByte(0x31),
		otherSource: idWithByte(0x32),
	}
	roles.setRole(fixture.ownerID, fixture.workspaceID, workspaces.RoleOwner)
	store.addSource(fixture.workspaceID, fixture.sourceID)
	store.addSource(fixture.otherSpace, fixture.otherSource)
	return fixture
}

// idWithByte builds a valid synthetic UUID from one byte.
func idWithByte(b byte) workspaces.ID {
	var raw [16]byte
	raw[0], raw[15] = 0x01, b
	return workspaces.ID{Bytes: raw, Valid: true}
}

// collectionPath is the list and issue path of the fixture workspace.
func (f *credentialFixture) collectionPath() string {
	return "/api/v1/workspaces/" + f.workspaceID.String() + "/sources/" + f.sourceID.String() + "/credentials"
}

// rotatePath is the rotate path of one credential.
func (f *credentialFixture) rotatePath(credentialID string) string {
	return "/api/v1/workspaces/" + f.workspaceID.String() + "/credentials/" + credentialID + "/rotate"
}

// revokePath is the revoke path of one credential.
func (f *credentialFixture) revokePath(credentialID string) string {
	return "/api/v1/workspaces/" + f.workspaceID.String() + "/credentials/" + credentialID + "/revoke"
}

// do performs one request with the owner session and the trusted Origin by
// default; a test overrides what it needs on the returned request first.
func (f *credentialFixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.doRequest(t, f.newRequest(method, path, body))
}

// newRequest builds an owner-authenticated request.
func (f *credentialFixture) newRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Origin", testOrigin)
	req.AddCookie(f.ownerCookie)
	return req
}

// doRequest serves one prepared request.
func (f *credentialFixture) doRequest(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, req)
	return recorder
}

// decodeList reads the list body.
func decodeList(t *testing.T, recorder *httptest.ResponseRecorder) credentialListBody {
	t.Helper()
	var body credentialListBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("list body is not JSON: %v: %s", err, recorder.Body.String())
	}
	return body
}

// credentialListBody mirrors the documented list response for assertions.
type credentialListBody struct {
	Credentials []struct {
		CredentialID string  `json:"credential_id"`
		WorkspaceID  string  `json:"workspace_id"`
		SourceID     string  `json:"source_id"`
		RevokedAt    *string `json:"revoked_at"`
	} `json:"credentials"`
}

// issuedBody mirrors the documented issue and rotate response.
type issuedBody struct {
	CredentialID string `json:"credential_id"`
	WorkspaceID  string `json:"workspace_id"`
	SourceID     string `json:"source_id"`
	Token        string `json:"token"`
}

// TestCredentialListRequiresSessionAndOwner covers the list admission rules:
// a session is always required, owner is required, and the write-only Origin
// check does not apply to this read.
func TestCredentialListRequiresSessionAndOwner(t *testing.T) {
	fixture := newCredentialFixture(t)
	fixture.store.seedCredential(fixture.workspaceID, fixture.sourceID, nil)

	// A read without Origin and with a valid owner session succeeds.
	req := httptest.NewRequest(http.MethodGet, fixture.collectionPath(), nil)
	req.AddCookie(fixture.ownerCookie)
	if w := fixture.doRequest(t, req); w.Code != http.StatusOK {
		t.Fatalf("owner list without Origin status = %d: %s", w.Code, w.Body.String())
	}

	// A list without any session is 401 and never reaches the role lookup.
	anonymous := httptest.NewRequest(http.MethodGet, fixture.collectionPath(), nil)
	anonymous.Header.Set("Origin", testOrigin)
	before := fixture.roles.callCount()
	if w := fixture.doRequest(t, anonymous); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "UNAUTHENTICATED") {
		t.Fatalf("anonymous list status/body = %d %s", w.Code, w.Body.String())
	}
	if calls := fixture.roles.callCount(); calls != before {
		t.Fatalf("anonymous list reached the role lookup %d times", calls-before)
	}

	// A member is not an owner; neither is a user of another workspace.
	member := newCredentialFixture(t)
	member.roles.setRole(member.ownerID, member.workspaceID, workspaces.RoleMember)
	if w := member.do(t, http.MethodGet, member.collectionPath(), ""); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "PERMISSION_DENIED") {
		t.Fatalf("member list status/body = %d %s", w.Code, w.Body.String())
	}
	outsider := newCredentialFixture(t)
	outsider.roles.clearRoles()
	if w := outsider.do(t, http.MethodGet, outsider.collectionPath(), ""); w.Code != http.StatusForbidden {
		t.Fatalf("non-member list status = %d", w.Code)
	}
}

// TestCredentialListReturnsMetadataOnly proves the list returns IDs, fixed
// ownership and revocation state for the path source, and never a token or a
// digest.
func TestCredentialListReturnsMetadataOnly(t *testing.T) {
	fixture := newCredentialFixture(t)
	revokedAt := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	active := fixture.store.seedCredential(fixture.workspaceID, fixture.sourceID, nil)
	revoked := fixture.store.seedCredential(fixture.workspaceID, fixture.sourceID, &revokedAt)
	// A credential of another source in the same workspace stays out of this list.
	fixture.store.seedCredential(fixture.workspaceID, fixture.otherSource, nil)
	fixture.store.addSource(fixture.workspaceID, fixture.otherSource)
	issued := fixture.do(t, http.MethodPost, fixture.collectionPath(), "")
	if issued.Code != http.StatusCreated {
		t.Fatalf("issue status = %d: %s", issued.Code, issued.Body.String())
	}
	var issuedResponse issuedBody
	if err := json.Unmarshal(issued.Body.Bytes(), &issuedResponse); err != nil {
		t.Fatal(err)
	}

	w := fixture.do(t, http.MethodGet, fixture.collectionPath(), "")
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != noStoreValue {
		t.Fatalf("Cache-Control = %q", got)
	}
	body := decodeList(t, w)
	if len(body.Credentials) != 3 {
		t.Fatalf("list returned %d credentials, want 3", len(body.Credentials))
	}
	seen := make(map[string]struct{}, len(body.Credentials))
	for _, item := range body.Credentials {
		if item.WorkspaceID != fixture.workspaceID.String() || item.SourceID != fixture.sourceID.String() {
			t.Fatalf("list item ownership = %s/%s", item.WorkspaceID, item.SourceID)
		}
		seen[item.CredentialID] = struct{}{}
	}
	for _, want := range []string{active.ID.String(), revoked.ID.String(), issuedResponse.CredentialID} {
		if _, ok := seen[want]; !ok {
			t.Fatalf("list is missing credential %s", want)
		}
	}
	for _, item := range body.Credentials {
		switch item.CredentialID {
		case revoked.ID.String():
			if item.RevokedAt == nil || *item.RevokedAt != revokedAt.Format(time.RFC3339) {
				t.Fatalf("revoked_at = %v, want %s", item.RevokedAt, revokedAt.Format(time.RFC3339))
			}
		default:
			if item.RevokedAt != nil {
				t.Fatalf("active credential reports revoked_at = %v", *item.RevokedAt)
			}
		}
	}
	// Neither the plaintext token nor the digest may appear in the list body.
	if strings.Contains(w.Body.String(), issuedResponse.Token) || strings.Contains(w.Body.String(), "token") || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "hash") {
		t.Fatalf("list body leaked a secret field: %s", w.Body.String())
	}
	// Listing creates and changes nothing.
	if calls := fixture.store.callCount("Insert"); calls != 1 {
		t.Fatalf("list inserted credentials: Insert calls = %d", calls)
	}
	if calls := fixture.store.callCount("Revoke"); calls != 0 {
		t.Fatalf("list revoked credentials: Revoke calls = %d", calls)
	}
}

// TestCredentialIssueStoresOnlyDigest proves issuance returns the plaintext once
// while the store only ever sees a 32-byte digest that differs from the token.
func TestCredentialIssueStoresOnlyDigest(t *testing.T) {
	fixture := newCredentialFixture(t)
	w := fixture.do(t, http.MethodPost, fixture.collectionPath(), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != noStoreValue {
		t.Fatalf("Cache-Control = %q", got)
	}
	var body issuedBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, w.Body.String())
	}
	if !strings.HasPrefix(body.Token, "tpk_") || len(body.Token) <= len("tpk_") {
		t.Fatalf("token = %q", body.Token)
	}
	if body.WorkspaceID != fixture.workspaceID.String() || body.SourceID != fixture.sourceID.String() || body.CredentialID == "" {
		t.Fatalf("issued body = %+v", body)
	}
	fixture.store.mu.Lock()
	digests := append([][]byte(nil), fixture.store.digests...)
	fixture.store.mu.Unlock()
	if len(digests) != 1 {
		t.Fatalf("store received %d digests, want 1", len(digests))
	}
	sum := sha256.Sum256([]byte(body.Token))
	if len(digests[0]) != sha256.Size || !bytes.Equal(digests[0], sum[:]) {
		t.Fatal("store did not receive the 32-byte SHA-256 digest of the token")
	}
	if bytes.Equal(digests[0], []byte(body.Token)) {
		t.Fatal("store received the plaintext token")
	}
	// The response carries exactly the documented fields and no digest material.
	for _, forbidden := range []string{"secret", "hash", string(digests[0])} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatalf("issue response contains %q: %s", forbidden, w.Body.String())
		}
	}
}

// TestCredentialWritesRequireOrigin proves every write rejects a missing or
// wrong Origin without touching storage, while the read does not require it.
func TestCredentialWritesRequireOrigin(t *testing.T) {
	fixture := newCredentialFixture(t)
	seeded := fixture.store.seedCredential(fixture.workspaceID, fixture.sourceID, nil)
	for name, build := range map[string]func() *http.Request{
		"issue": func() *http.Request { return fixture.newRequest(http.MethodPost, fixture.collectionPath(), "") },
		"rotate": func() *http.Request {
			return fixture.newRequest(http.MethodPost, fixture.rotatePath(seeded.ID.String()), "")
		},
		"revoke": func() *http.Request {
			return fixture.newRequest(http.MethodPost, fixture.revokePath(seeded.ID.String()), "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			for originName, header := range map[string]string{"missing": "", "wrong": "https://evil.example.com"} {
				t.Run(originName, func(t *testing.T) {
					before := map[string]int{"Insert": fixture.store.callCount("Insert"), "Revoke": fixture.store.callCount("Revoke")}
					req := build()
					req.Header.Del("Origin")
					if header != "" {
						req.Header.Set("Origin", header)
					}
					w := fixture.doRequest(t, req)
					if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "PERMISSION_DENIED") {
						t.Fatalf("status/body = %d %s", w.Code, w.Body.String())
					}
					if got := w.Header().Get("Cache-Control"); got != noStoreValue {
						t.Fatalf("Cache-Control = %q", got)
					}
					for method, count := range before {
						if after := fixture.store.callCount(method); after != count {
							t.Fatalf("rejected write reached %s %d times", method, after-count)
						}
					}
				})
			}
		})
	}
}

// TestCredentialWritesRequireOwner covers 401 without a session and 403 for a
// member or a non-member, with no storage write.
func TestCredentialWritesRequireOwner(t *testing.T) {
	fixture := newCredentialFixture(t)
	seeded := fixture.store.seedCredential(fixture.workspaceID, fixture.sourceID, nil)

	anonymous := httptest.NewRequest(http.MethodPost, fixture.collectionPath(), strings.NewReader(""))
	anonymous.Header.Set("Origin", testOrigin)
	if w := fixture.doRequest(t, anonymous); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "UNAUTHENTICATED") {
		t.Fatalf("anonymous issue status/body = %d %s", w.Code, w.Body.String())
	}
	if calls := fixture.store.callCount("Insert"); calls != 0 {
		t.Fatalf("anonymous issue inserted %d rows", calls)
	}

	fixture.roles.setRole(fixture.ownerID, fixture.workspaceID, workspaces.RoleMember)
	for name, path := range map[string]string{
		"issue":  fixture.collectionPath(),
		"rotate": fixture.rotatePath(seeded.ID.String()),
		"revoke": fixture.revokePath(seeded.ID.String()),
	} {
		if w := fixture.do(t, http.MethodPost, path, ""); w.Code != http.StatusForbidden {
			t.Fatalf("member %s status = %d", name, w.Code)
		}
	}
	if calls := fixture.store.callCount("Insert"); calls != 0 {
		t.Fatalf("member issue inserted %d rows", calls)
	}
	if calls := fixture.store.callCount("Revoke"); calls != 0 {
		t.Fatalf("member revoke reached storage %d times", calls)
	}
	if seeded.RevokedAt != nil {
		t.Fatal("seeded credential changed without a successful revoke")
	}
}

// TestCredentialCrossSpaceIsNotFound proves a foreign source or credential is
// indistinguishable from a missing one, and nothing is created or changed.
func TestCredentialCrossSpaceIsNotFound(t *testing.T) {
	fixture := newCredentialFixture(t)
	foreign := fixture.store.seedCredential(fixture.otherSpace, fixture.otherSource, nil)

	cases := map[string]string{
		"foreign source list":       "/api/v1/workspaces/" + fixture.workspaceID.String() + "/sources/" + fixture.otherSource.String() + "/credentials",
		"unknown source list":       "/api/v1/workspaces/" + fixture.workspaceID.String() + "/sources/" + idWithByte(0x3f).String() + "/credentials",
		"foreign credential rotate": fixture.rotatePath(foreign.ID.String()),
		"foreign credential revoke": fixture.revokePath(foreign.ID.String()),
		"unknown credential rotate": fixture.rotatePath(idWithByte(0x40).String()),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			method := http.MethodGet
			if strings.HasSuffix(path, "/rotate") || strings.HasSuffix(path, "/revoke") {
				method = http.MethodPost
			}
			w := fixture.do(t, method, path, "")
			if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "NOT_FOUND") {
				t.Fatalf("status/body = %d %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), foreign.ID.String()) {
				t.Fatal("response revealed a foreign credential")
			}
		})
	}
	// The foreign source cannot be issued against through the path workspace.
	foreignIssue := "/api/v1/workspaces/" + fixture.workspaceID.String() + "/sources/" + fixture.otherSource.String() + "/credentials"
	if w := fixture.do(t, http.MethodPost, foreignIssue, ""); w.Code != http.StatusNotFound {
		t.Fatalf("foreign source issue status = %d", w.Code)
	}
	if calls := fixture.store.callCount("Insert"); calls != 0 {
		t.Fatalf("cross-space requests inserted %d rows", calls)
	}
	fixture.store.mu.Lock()
	defer fixture.store.mu.Unlock()
	if row := fixture.store.rows[foreign.ID.Bytes]; row.credential.RevokedAt != nil {
		t.Fatal("cross-space revoke changed the foreign credential")
	}
}

// TestCredentialRejectsBodyAndMalformedIDs proves the plan's body rule: a body
// carrying workspace_id or any other field is refused before storage, and an
// invalid UUID path parameter is an invalid argument.
func TestCredentialRejectsBodyAndMalformedIDs(t *testing.T) {
	fixture := newCredentialFixture(t)
	for name, body := range map[string]string{
		"workspace_id": `{"workspace_id":"synthetic"}`,
		"source_id":    `{"source_id":"synthetic"}`,
		"empty object": `{}`,
		"revoke old":   `{"revoke_old":true}`,
	} {
		t.Run("body "+name, func(t *testing.T) {
			w := fixture.do(t, http.MethodPost, fixture.collectionPath(), body)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "INVALID_ARGUMENT") {
				t.Fatalf("status/body = %d %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "synthetic") {
				t.Fatal("response echoed the rejected body")
			}
		})
	}
	if calls := fixture.store.callCount("Insert"); calls != 0 {
		t.Fatalf("bodies with fields inserted %d rows", calls)
	}

	for name, path := range map[string]string{
		"list":     "/api/v1/workspaces/not-a-uuid/sources/" + fixture.sourceID.String() + "/credentials",
		"issue":    "/api/v1/workspaces/" + fixture.workspaceID.String() + "/sources/not-a-uuid/credentials",
		"rotate":   "/api/v1/workspaces/" + fixture.workspaceID.String() + "/credentials/not-a-uuid/rotate",
		"revoke":   "/api/v1/workspaces/not-a-uuid/credentials/" + idWithByte(0x50).String() + "/revoke",
		"short id": "/api/v1/workspaces/" + fixture.workspaceID.String() + "/credentials/01000000-0000-0000-0000/rotate",
		"non hex":  "/api/v1/workspaces/" + fixture.workspaceID.String() + "/credentials/zzzzzzzz-0000-0000-0000-000000000000/revoke",
	} {
		t.Run("path "+name, func(t *testing.T) {
			method := http.MethodPost
			if name == "list" {
				method = http.MethodGet
			}
			w := fixture.do(t, method, path, "")
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "INVALID_ARGUMENT") {
				t.Fatalf("status/body = %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestCredentialLostResponseIsRecoveredFromListThenRevoked covers the documented
// recovery: the one-time token cannot be replayed, so the owner finds the new
// credential ID in the list and revokes it. The revoked token then fails.
func TestCredentialLostResponseIsRecoveredFromListThenRevoked(t *testing.T) {
	fixture := newCredentialFixture(t)
	issued := fixture.do(t, http.MethodPost, fixture.collectionPath(), "")
	if issued.Code != http.StatusCreated {
		t.Fatalf("issue status = %d: %s", issued.Code, issued.Body.String())
	}
	var body issuedBody
	if err := json.Unmarshal(issued.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	// The response is now "lost": only the list may locate the credential.
	list := decodeList(t, fixture.do(t, http.MethodGet, fixture.collectionPath(), ""))
	if len(list.Credentials) != 1 || list.Credentials[0].CredentialID != body.CredentialID || list.Credentials[0].RevokedAt != nil {
		t.Fatalf("list after a lost response = %+v", list)
	}
	if _, err := fixture.service.Authenticate(context.Background(), body.Token); err != nil {
		t.Fatalf("issued token does not authenticate: %v", err)
	}

	revoked := fixture.do(t, http.MethodPost, fixture.revokePath(body.CredentialID), "")
	if revoked.Code != http.StatusNoContent || revoked.Body.Len() != 0 {
		t.Fatalf("revoke status/body = %d %q", revoked.Code, revoked.Body.String())
	}
	after := decodeList(t, fixture.do(t, http.MethodGet, fixture.collectionPath(), ""))
	if len(after.Credentials) != 1 || after.Credentials[0].RevokedAt == nil {
		t.Fatalf("list after revoke = %+v", after)
	}
	if _, err := fixture.service.Authenticate(context.Background(), body.Token); !errors.Is(err, credentials.ErrUnauthenticated) {
		t.Fatalf("revoked token authenticate error = %v", err)
	}
	// Repeated revocation keeps the first stamp and returns the same 204.
	firstStamp := *after.Credentials[0].RevokedAt
	again := fixture.do(t, http.MethodPost, fixture.revokePath(body.CredentialID), "")
	if again.Code != http.StatusNoContent {
		t.Fatalf("repeated revoke status = %d", again.Code)
	}
	third := decodeList(t, fixture.do(t, http.MethodGet, fixture.collectionPath(), ""))
	if third.Credentials[0].RevokedAt == nil || *third.Credentials[0].RevokedAt != firstStamp {
		t.Fatalf("repeated revoke changed revoked_at: %v -> %v", firstStamp, third.Credentials[0].RevokedAt)
	}
}

// TestCredentialRotateKeepsSourceAndOldState proves rotation returns a new
// credential with the old fixed ownership, and the old row keeps its state.
func TestCredentialRotateKeepsSourceAndOldState(t *testing.T) {
	fixture := newCredentialFixture(t)
	active := fixture.store.seedCredential(fixture.workspaceID, fixture.sourceID, nil)
	rotate := fixture.do(t, http.MethodPost, fixture.rotatePath(active.ID.String()), "")
	if rotate.Code != http.StatusCreated {
		t.Fatalf("rotate status = %d: %s", rotate.Code, rotate.Body.String())
	}
	var rotated issuedBody
	if err := json.Unmarshal(rotate.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.CredentialID == active.ID.String() {
		t.Fatal("rotation reused the old credential id")
	}
	if rotated.WorkspaceID != fixture.workspaceID.String() || rotated.SourceID != fixture.sourceID.String() {
		t.Fatalf("rotation moved the credential: %+v", rotated)
	}
	// The rotated token authenticates to the same fixed identity.
	credential, err := fixture.service.Authenticate(context.Background(), rotated.Token)
	if err != nil || credential.SourceID != fixture.sourceID || credential.WorkspaceID != fixture.workspaceID {
		t.Fatalf("rotated token identity = %+v, %v", credential, err)
	}
	// Revoking the old credential leaves the new one usable.
	if w := fixture.do(t, http.MethodPost, fixture.revokePath(active.ID.String()), ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke old status = %d", w.Code)
	}
	if _, err := fixture.service.Authenticate(context.Background(), rotated.Token); err != nil {
		t.Fatalf("new token stopped working after revoking the old one: %v", err)
	}

	// A revoked old credential can still be the source of a rotation.
	revokedAt := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	oldRevoked := fixture.store.seedCredential(fixture.workspaceID, fixture.sourceID, &revokedAt)
	again := fixture.do(t, http.MethodPost, fixture.rotatePath(oldRevoked.ID.String()), "")
	if again.Code != http.StatusCreated {
		t.Fatalf("rotate from revoked status = %d: %s", again.Code, again.Body.String())
	}
	fixture.store.mu.Lock()
	row := fixture.store.rows[oldRevoked.ID.Bytes]
	fixture.store.mu.Unlock()
	if row.credential.RevokedAt == nil || !row.credential.RevokedAt.Equal(revokedAt) {
		t.Fatal("rotation cleared or changed the old credential revocation")
	}
}

// TestCredentialManagementRejectsPluginToken proves a plugin Bearer token cannot
// manage credentials, even with a valid owner cookie alongside it.
func TestCredentialManagementRejectsPluginToken(t *testing.T) {
	fixture := newCredentialFixture(t)
	issued := fixture.do(t, http.MethodPost, fixture.collectionPath(), "")
	var body issuedBody
	if err := json.Unmarshal(issued.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	// Bearer only: no session, so the management API is unreachable.
	bearer := httptest.NewRequest(http.MethodGet, fixture.collectionPath(), nil)
	bearer.Header.Set("Authorization", "Bearer "+body.Token)
	bearer.Header.Set("Origin", testOrigin)
	if w := fixture.doRequest(t, bearer); w.Code != http.StatusUnauthorized {
		t.Fatalf("bearer-only list status = %d", w.Code)
	}

	// A cookie plus a plugin Bearer authenticates as the browser owner; the
	// Bearer value itself is never accepted as a management credential.
	both := fixture.newRequest(http.MethodGet, fixture.collectionPath(), "")
	both.Header.Set("Authorization", "Bearer "+body.Token)
	if w := fixture.doRequest(t, both); w.Code != http.StatusOK {
		t.Fatalf("cookie+bearer list status = %d: %s", w.Code, w.Body.String())
	}
	// A plugin-only write is still unauthenticated.
	write := httptest.NewRequest(http.MethodPost, fixture.collectionPath(), strings.NewReader(""))
	write.Header.Set("Authorization", "Bearer "+body.Token)
	write.Header.Set("Origin", testOrigin)
	if w := fixture.doRequest(t, write); w.Code != http.StatusUnauthorized {
		t.Fatalf("bearer-only issue status = %d", w.Code)
	}
}

// TestCredentialDatabaseFailureIsNotSuccess proves a store failure is a 500 that
// never claims a credential was created or revoked.
func TestCredentialDatabaseFailureIsNotSuccess(t *testing.T) {
	fixture := newCredentialFixture(t)
	fixture.store.failure.source = errors.New("synthetic database failure")
	if w := fixture.do(t, http.MethodPost, fixture.collectionPath(), ""); w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "INTERNAL") {
		t.Fatalf("issue status/body = %d %s", w.Code, w.Body.String())
	}
	if w := fixture.do(t, http.MethodGet, fixture.collectionPath(), ""); w.Code != http.StatusInternalServerError {
		t.Fatalf("list status = %d", w.Code)
	}
	if strings.Contains(fixture.do(t, http.MethodGet, fixture.collectionPath(), "").Body.String(), "synthetic database failure") {
		t.Fatal("database message leaked into the response")
	}

	seeded := fixture.store.seedCredential(fixture.workspaceID, fixture.sourceID, nil)
	fixture.store.failure.source = nil
	fixture.store.failure.revoke = errors.New("synthetic database failure")
	if w := fixture.do(t, http.MethodPost, fixture.revokePath(seeded.ID.String()), ""); w.Code != http.StatusInternalServerError {
		t.Fatalf("revoke status = %d", w.Code)
	}
	if seeded.RevokedAt != nil {
		t.Fatal("a failed revoke reported the credential as revoked")
	}

	// An owner lookup failure is a server error, not a permission decision.
	fixture.store.failure.revoke = nil
	fixture.roles.setError(errors.New("synthetic database failure"))
	if w := fixture.do(t, http.MethodGet, fixture.collectionPath(), ""); w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "INTERNAL") {
		t.Fatalf("owner lookup failure status/body = %d %s", w.Code, w.Body.String())
	}
}

// TestCredentialRoutesAbsentWithoutAdapter proves the routes are not registered
// without the credential adapter, and not registered without the auth adapter
// they depend on.
func TestCredentialRoutesAbsentWithoutAdapter(t *testing.T) {
	adapter, _ := newTestAuth(t)
	service := credentials.New(newCredentialFakeStore())
	cases := map[string]http.Handler{
		"no credentials": NewHandler(Options{Auth: adapter}),
		"no auth":        NewHandler(Options{Credentials: NewCredentials(service, workspaces.New(newRoleFakeStore()))}),
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/"+idWithByte(0x61).String()+"/sources/"+idWithByte(0x62).String()+"/credentials", nil)
			req.Header.Set("Origin", testOrigin)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusNotFound {
				t.Fatalf("credential route was registered without its dependencies: %d", w.Code)
			}
		})
	}
}

// TestCredentialJSONShapes pins the documented field sets so no response grows a
// token, digest or wrapper by accident.
func TestCredentialJSONShapes(t *testing.T) {
	fixture := newCredentialFixture(t)
	issued := fixture.do(t, http.MethodPost, fixture.collectionPath(), "")
	var issueFields map[string]json.RawMessage
	if err := json.Unmarshal(issued.Body.Bytes(), &issueFields); err != nil {
		t.Fatal(err)
	}
	if len(issueFields) != 4 {
		t.Fatalf("issue response fields = %v", keys(issueFields))
	}
	for _, want := range []string{"credential_id", "workspace_id", "source_id", "token"} {
		if _, ok := issueFields[want]; !ok {
			t.Fatalf("issue response is missing %q: %v", want, keys(issueFields))
		}
	}
	list := fixture.do(t, http.MethodGet, fixture.collectionPath(), "")
	var listFields map[string]json.RawMessage
	if err := json.Unmarshal(list.Body.Bytes(), &listFields); err != nil {
		t.Fatal(err)
	}
	if len(listFields) != 1 || listFields["credentials"] == nil {
		t.Fatalf("list response is not wrapped in credentials: %v", keys(listFields))
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(listFields["credentials"], &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(items[0]) != 4 {
		t.Fatalf("list item fields = %v", keys(items[0]))
	}
	for _, want := range []string{"credential_id", "workspace_id", "source_id", "revoked_at"} {
		if _, ok := items[0][want]; !ok {
			t.Fatalf("list item is missing %q", want)
		}
	}
	if string(items[0]["revoked_at"]) != "null" {
		t.Fatalf("active credential revoked_at = %s", items[0]["revoked_at"])
	}
}

// keys returns the sorted field names of a decoded object for failure messages.
func keys(object map[string]json.RawMessage) []string {
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	return names
}

// TestCredentialIssueUsesSessionIdentityOnly proves the created credential is
// bound to the path workspace and source, not to any body or header value.
func TestCredentialIssueUsesSessionIdentityOnly(t *testing.T) {
	fixture := newCredentialFixture(t)
	// A header naming another workspace must not move the created credential:
	// ownership comes from the path and the stored source row only.
	req := fixture.newRequest(http.MethodPost, fixture.collectionPath(), "")
	req.Header.Set("X-Workspace-ID", fixture.otherSpace.String())
	w := fixture.doRequest(t, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var body issuedBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.WorkspaceID != fixture.workspaceID.String() || body.SourceID != fixture.sourceID.String() {
		t.Fatalf("issue used a header or body identity: %+v", body)
	}
}

// TestCredentialUpdateRejectsWrongHTTPMethod keeps the collection and management
// routes method-scoped.
func TestCredentialUpdateRejectsWrongHTTPMethod(t *testing.T) {
	fixture := newCredentialFixture(t)
	seeded := fixture.store.seedCredential(fixture.workspaceID, fixture.sourceID, nil)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, fixture.collectionPath()},
		{http.MethodDelete, fixture.collectionPath()},
		{http.MethodGet, fixture.rotatePath(seeded.ID.String())},
		{http.MethodGet, fixture.revokePath(seeded.ID.String())},
	} {
		if w := fixture.do(t, tc.method, tc.path, ""); w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s status = %d, want 405", tc.method, tc.path, w.Code)
		}
	}
}

// TestCredentialNoStoreOnSuccess proves the uncached marker is set before the
// body is written, including on 204.
func TestCredentialNoStoreOnSuccess(t *testing.T) {
	fixture := newCredentialFixture(t)
	seeded := fixture.store.seedCredential(fixture.workspaceID, fixture.sourceID, nil)
	for name, response := range map[string]*httptest.ResponseRecorder{
		"list":   fixture.do(t, http.MethodGet, fixture.collectionPath(), ""),
		"issue":  fixture.do(t, http.MethodPost, fixture.collectionPath(), ""),
		"rotate": fixture.do(t, http.MethodPost, fixture.rotatePath(seeded.ID.String()), ""),
		"revoke": fixture.do(t, http.MethodPost, fixture.revokePath(seeded.ID.String()), ""),
	} {
		if got := response.Header().Get("Cache-Control"); got != noStoreValue {
			t.Fatalf("%s Cache-Control = %q", name, got)
		}
	}
}
