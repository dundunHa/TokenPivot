package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dundunHa/TokenPivot/internal/auth"
	"github.com/dundunHa/TokenPivot/internal/workspaces"
)

const (
	workspacesPath = "/api/v1/workspaces"
	// removedCurrentPath is the retired route. It now only matches the detail
	// path with an invalid UUID, so it can never act as a workspace selector.
	removedCurrentPath = "/api/v1/workspaces/current"
)

// workspaceCreatorStub records the creation call and returns canned results. The
// real creation transaction seam is internal to the workspaces package, so the
// HTTP layer holds a narrow function and the tests replace it here to exercise
// the status mapping without a database.
type workspaceCreatorStub struct {
	calls   int
	userID  workspaces.ID
	name    string
	created workspaces.Workspace
	err     error
}

func (f *workspaceCreatorStub) create(ctx context.Context, userID workspaces.ID, name string) (workspaces.Workspace, error) {
	f.calls++
	f.userID, f.name = userID, name
	if f.err != nil {
		return workspaces.Workspace{}, f.err
	}
	return f.created, nil
}

// workspaceFixture wires the workspace endpoints behind the real Auth adapter,
// so Origin, session and membership gates run end to end.
type workspaceFixture struct {
	*credentialFixture
	service   *workspaces.Service
	endpoints *Workspaces
	creator   *workspaceCreatorStub
}

func newWorkspaceFixture(t *testing.T) *workspaceFixture {
	t.Helper()
	adapter, authStore := newTestAuth(t)
	user := authStore.seedUser(t, "owner@example.com", "synthetic-password")
	roles := newRoleFakeStore()
	service := workspaces.New(roles, nil)
	creator := &workspaceCreatorStub{}
	endpoints := NewWorkspaces(service)
	endpoints.create = creator.create
	handler := NewHandler(Options{
		CheckReady: func(context.Context) error { return nil },
		Auth:       adapter,
		Workspaces: endpoints,
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
	base := &credentialFixture{
		handler:     handler,
		roles:       roles,
		authStore:   authStore,
		ownerCookie: sessionCookie(t, recorder),
		ownerID:     workspaces.ID{Bytes: user.ID, Valid: true},
		workspaceID: idWithByte(0x41),
		otherSpace:  idWithByte(0x42),
	}
	return &workspaceFixture{credentialFixture: base, service: service, endpoints: endpoints, creator: creator}
}

// request performs one request with the trusted Origin and a JSON content type.
// cookie=false leaves the session out, so 401 is exercised before any store call.
func (f *workspaceFixture) request(t *testing.T, method, path, body string, cookie bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "application/json")
	if cookie {
		req.AddCookie(f.ownerCookie)
	}
	return f.doRequest(t, req)
}

// readRequest performs one read without any Origin header, which the read routes
// must accept.
func (f *workspaceFixture) readRequest(t *testing.T, path string, cookie bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie {
		req.AddCookie(f.ownerCookie)
	}
	return f.doRequest(t, req)
}

// workspaceBody mirrors the documented workspace response.
type workspaceBody struct {
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
}

func decodeWorkspace(t *testing.T, w *httptest.ResponseRecorder) workspaceBody {
	t.Helper()
	var body workspaceBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("workspace body is not JSON: %v: %s", err, w.Body.String())
	}
	return body
}

func TestWorkspaceListRequiresSession(t *testing.T) {
	f := newWorkspaceFixture(t)
	f.roles.setList(workspaces.Workspace{ID: f.workspaceID, Name: "Alpha"})
	w := f.readRequest(t, workspacesPath, false)
	if code, _ := errorCode(t, w); w.Code != http.StatusUnauthorized || code != "UNAUTHENTICATED" {
		t.Fatalf("status %d code %s", w.Code, code)
	}
	assertNoStore(t, w)
	if f.roles.listCalls != 0 {
		t.Fatalf("list store reached %d times without a session", f.roles.listCalls)
	}
}

func TestWorkspaceListEmptyIsArray(t *testing.T) {
	f := newWorkspaceFixture(t)
	w := f.readRequest(t, workspacesPath, true)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	assertNoStore(t, w)
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Fatalf("body %s, want []", got)
	}
	if f.roles.listCalls != 1 {
		t.Fatalf("list store calls = %d", f.roles.listCalls)
	}
}

func TestWorkspaceListKeepsServerOrder(t *testing.T) {
	f := newWorkspaceFixture(t)
	beta := workspaces.Workspace{ID: idWithByte(0x51), Name: "Beta"}
	alpha := workspaces.Workspace{ID: idWithByte(0x52), Name: "Alpha"}
	f.roles.setList(beta, alpha)
	w := f.readRequest(t, workspacesPath, true)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body []workspaceBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("list body: %v: %s", err, w.Body.String())
	}
	if len(body) != 2 || body[0].WorkspaceID != beta.ID.String() || body[0].Name != "Beta" || body[1].WorkspaceID != alpha.ID.String() {
		t.Fatalf("handler reordered or rewrote the server order: %v", body)
	}
}

func TestWorkspaceListFailureIsInternal(t *testing.T) {
	f := newWorkspaceFixture(t)
	f.roles.setListError(errors.New("SELECT name FROM workspaces"))
	w := f.readRequest(t, workspacesPath, true)
	if code, _ := errorCode(t, w); w.Code != http.StatusInternalServerError || code != "INTERNAL" || strings.Contains(w.Body.String(), "SELECT") {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceListSessionUnavailableIs503(t *testing.T) {
	f := newWorkspaceFixture(t)
	f.authStore.failures.authenticate = errors.Join(auth.ErrUnavailable, errors.New("synthetic"))
	w := f.readRequest(t, workspacesPath, true)
	if code, _ := errorCode(t, w); w.Code != http.StatusServiceUnavailable || code != "UNAVAILABLE" {
		t.Fatalf("status %d code %s", w.Code, code)
	}
	assertNoStore(t, w)
}

func TestWorkspaceCreateAdmissionOrder(t *testing.T) {
	f := newWorkspaceFixture(t)

	// A write with a foreign Origin is refused before the session is even read.
	badOrigin := httptest.NewRequest(http.MethodPost, workspacesPath, strings.NewReader(`{"name":"Alpha"}`))
	badOrigin.Header.Set("Content-Type", "application/json")
	badOrigin.Header.Set("Origin", "http://evil.example")
	badOrigin.AddCookie(f.ownerCookie)
	w := f.doRequest(t, badOrigin)
	if code, _ := errorCode(t, w); w.Code != http.StatusForbidden || code != "PERMISSION_DENIED" {
		t.Fatalf("bad origin: status %d code %s", w.Code, code)
	}
	if calls := f.authStore.callCount("AuthenticateSession"); calls != 0 {
		t.Fatalf("bad origin reached the session store %d times", calls)
	}

	// A write without a session is 401 after Origin passes and before the body.
	w = f.request(t, http.MethodPost, workspacesPath, `{"name":"Alpha"}`, false)
	if code, _ := errorCode(t, w); w.Code != http.StatusUnauthorized || code != "UNAUTHENTICATED" {
		t.Fatalf("anonymous: status %d code %s", w.Code, code)
	}

	// A malformed body is refused before the creation use case runs.
	w = f.request(t, http.MethodPost, workspacesPath, `{"name":`, true)
	if code, _ := errorCode(t, w); w.Code != http.StatusBadRequest || code != "INVALID_ARGUMENT" {
		t.Fatalf("malformed body: status %d code %s", w.Code, code)
	}
	if f.creator.calls != 0 {
		t.Fatalf("creation reached %d times before validation", f.creator.calls)
	}
}

func TestWorkspaceCreateReturnsCreatedWorkspace(t *testing.T) {
	f := newWorkspaceFixture(t)
	f.creator.created = workspaces.Workspace{ID: f.workspaceID, Name: "Alpha"}
	w := f.request(t, http.MethodPost, workspacesPath, `{"name":"  Alpha  "}`, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	assertNoStore(t, w)
	if got := strings.TrimSpace(w.Body.String()); got != `{"workspace_id":"`+f.workspaceID.String()+`","name":"Alpha"}` {
		t.Fatalf("body %s", got)
	}
	// The handler forwards the raw value: trimming and every other name rule
	// belong to the service, which is exercised in the next test.
	if f.creator.name != "  Alpha  " || f.creator.userID != f.ownerID {
		t.Fatalf("creator got user %v name %q", f.creator.userID, f.creator.name)
	}
}

func TestWorkspaceCreateRejectsInvalidNamesBeforeDatabase(t *testing.T) {
	cases := map[string]string{
		"all whitespace":   `{"name":"   "}`,
		"empty":            `{"name":""}`,
		"81 code points":   `{"name":"` + strings.Repeat("a", 81) + `"}`,
		"257 utf8 bytes":   `{"name":"` + strings.Repeat("😀", 80) + `"}`,
		"control char":     `{"name":"bad\u001bname"}`,
		"unknown field":    `{"name":"Alpha","workspace_id":"018f2f2a-0000-7000-8000-000000000000"}`,
		"duplicate name":   `{"name":"Alpha","name":"Beta"}`,
		"non string value": `{"name":1}`,
		"missing name":     `{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := newWorkspaceFixture(t)
			// The real service validates before opening a transaction, so a nil
			// runner is enough and proves no database work happens.
			f.endpoints.create = f.service.Create
			w := f.request(t, http.MethodPost, workspacesPath, body, true)
			if code, _ := errorCode(t, w); w.Code != http.StatusBadRequest || code != "INVALID_ARGUMENT" {
				t.Fatalf("status %d body %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestWorkspaceCreateRequiresJSONContentType(t *testing.T) {
	f := newWorkspaceFixture(t)
	req := httptest.NewRequest(http.MethodPost, workspacesPath, strings.NewReader(`{"name":"Alpha"}`))
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "text/plain")
	req.AddCookie(f.ownerCookie)
	if code, _ := errorCode(t, f.doRequest(t, req)); code != "UNSUPPORTED_MEDIA_TYPE" {
		t.Fatalf("code %s", code)
	}
	if f.creator.calls != 0 {
		t.Fatal("creation ran for a non-JSON body")
	}
}

func TestWorkspaceCreateErrorMapping(t *testing.T) {
	cases := map[string]struct {
		err     error
		status  int
		code    string
		message string
	}{
		"name conflict": {
			err: workspaces.ErrNameConflict, status: http.StatusConflict,
			code: "WORKSPACE_NAME_CONFLICT", message: "workspace name already exists",
		},
		"limit reached": {
			err: workspaces.ErrLimitReached, status: http.StatusConflict,
			code: "WORKSPACE_LIMIT_REACHED", message: "workspace limit reached",
		},
		"user missing": {err: workspaces.ErrUserNotFound, status: http.StatusInternalServerError, code: "INTERNAL", message: "internal error"},
		"database failure": {
			err: errors.New("INSERT INTO workspaces failed"), status: http.StatusInternalServerError,
			code: "INTERNAL", message: "internal error",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newWorkspaceFixture(t)
			f.creator.err = tc.err
			w := f.request(t, http.MethodPost, workspacesPath, `{"name":"Alpha"}`, true)
			if code, _ := errorCode(t, w); w.Code != tc.status || code != tc.code {
				t.Fatalf("status %d code %s body %s", w.Code, code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"message":"`+tc.message+`"`) {
				t.Fatalf("message: %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), "INSERT") {
				t.Fatalf("database message leaked: %s", w.Body.String())
			}
			if f.creator.calls != 1 || f.creator.name != "Alpha" {
				t.Fatalf("creator calls %d name %q", f.creator.calls, f.creator.name)
			}
		})
	}
}

func TestWorkspaceCreateSessionUnavailableIs503(t *testing.T) {
	f := newWorkspaceFixture(t)
	f.authStore.failures.authenticate = errors.Join(auth.ErrUnavailable, errors.New("synthetic"))
	w := f.request(t, http.MethodPost, workspacesPath, `{"name":"Alpha"}`, true)
	if code, _ := errorCode(t, w); w.Code != http.StatusServiceUnavailable || code != "UNAVAILABLE" {
		t.Fatalf("status %d code %s", w.Code, code)
	}
	if f.creator.calls != 0 {
		t.Fatal("creation ran while the session store was unavailable")
	}
}

func TestWorkspaceDetailAuthorisation(t *testing.T) {
	f := newWorkspaceFixture(t)
	path := workspacesPath + "/" + f.workspaceID.String()

	// No session: 401 before any store read.
	if w := f.readRequest(t, path, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status %d", w.Code)
	}
	if f.roles.callCount() != 0 || f.roles.lookupCalls != 0 {
		t.Fatal("store reached before the session")
	}

	// Malformed path UUID: 400 before the role lookup.
	if code, _ := errorCode(t, f.readRequest(t, workspacesPath+"/not-a-uuid", true)); code != "INVALID_ARGUMENT" {
		t.Fatalf("invalid uuid code %s", code)
	}
	if f.roles.callCount() != 0 {
		t.Fatal("malformed UUID reached the role lookup")
	}

	// Non-member: 403 and no workspace read.
	path = workspacesPath + "/" + f.workspaceID.String()
	w := f.readRequest(t, path, true)
	if code, _ := errorCode(t, w); w.Code != http.StatusForbidden || code != "PERMISSION_DENIED" {
		t.Fatalf("non-member status %d code %s", w.Code, code)
	}
	if f.roles.lookupCalls != 0 {
		t.Fatal("workspace read before the membership decision")
	}

	// A member (read-only role) gets the workspace without an Origin header.
	f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleMember)
	f.roles.addWorkspace(workspaces.Workspace{ID: f.workspaceID, Name: "Alpha"})
	w = f.readRequest(t, path, true)
	if w.Code != http.StatusOK {
		t.Fatalf("member status %d: %s", w.Code, w.Body.String())
	}
	assertNoStore(t, w)
	if got := decodeWorkspace(t, w); got.WorkspaceID != f.workspaceID.String() || got.Name != "Alpha" {
		t.Fatalf("body %v", got)
	}
}

func TestWorkspaceDetailFailuresAreInternal(t *testing.T) {
	t.Run("role lookup failure", func(t *testing.T) {
		f := newWorkspaceFixture(t)
		f.roles.setError(errors.New("SELECT role FROM workspace_memberships"))
		w := f.readRequest(t, workspacesPath+"/"+f.workspaceID.String(), true)
		if code, _ := errorCode(t, w); w.Code != http.StatusInternalServerError || code != "INTERNAL" || strings.Contains(w.Body.String(), "SELECT") {
			t.Fatalf("status %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("orphan membership", func(t *testing.T) {
		f := newWorkspaceFixture(t)
		f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleOwner)
		w := f.readRequest(t, workspacesPath+"/"+f.workspaceID.String(), true)
		if code, _ := errorCode(t, w); w.Code != http.StatusInternalServerError || code != "INTERNAL" {
			t.Fatalf("status %d code %s", w.Code, code)
		}
	})
	t.Run("workspace read failure", func(t *testing.T) {
		f := newWorkspaceFixture(t)
		f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleOwner)
		f.roles.setWorkspaceError(errors.New("SELECT name FROM workspaces"))
		w := f.readRequest(t, workspacesPath+"/"+f.workspaceID.String(), true)
		if code, _ := errorCode(t, w); w.Code != http.StatusInternalServerError || code != "INTERNAL" || strings.Contains(w.Body.String(), "SELECT") {
			t.Fatalf("status %d body %s", w.Code, w.Body.String())
		}
	})
}

func TestWorkspaceCurrentRouteIsGone(t *testing.T) {
	f := newWorkspaceFixture(t)
	w := f.readRequest(t, removedCurrentPath, true)
	if code, _ := errorCode(t, w); w.Code != http.StatusBadRequest || code != "INVALID_ARGUMENT" {
		t.Fatalf("status %d code %s body %s", w.Code, code, w.Body.String())
	}
	if f.roles.listCalls != 0 || f.roles.lookupCalls != 0 || f.roles.callCount() != 0 {
		t.Fatal("the retired route reached a workspace store")
	}
}

func TestWorkspaceRoutesMethodAndDependencyGates(t *testing.T) {
	f := newWorkspaceFixture(t)
	if w := f.request(t, http.MethodDelete, workspacesPath, "", true); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("delete collection status %d", w.Code)
	}
	if w := f.request(t, http.MethodPost, workspacesPath+"/"+f.workspaceID.String(), "", true); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post detail status %d", w.Code)
	}

	adapter, _ := newTestAuth(t)
	roles := newRoleFakeStore()
	path := workspacesPath + "/" + idWithByte(1).String()
	handlers := map[string]http.Handler{
		"no auth":    NewHandler(Options{Workspaces: NewWorkspaces(workspaces.New(roles, nil))}),
		"no service": NewHandler(Options{Auth: adapter, Workspaces: NewWorkspaces(nil)}),
		"nothing":    NewHandler(Options{Auth: adapter}),
	}
	for name, handler := range handlers {
		for _, target := range []string{workspacesPath, path} {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
			if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s GET %s = %d, want route absent", name, target, w.Code)
			}
		}
	}
}
