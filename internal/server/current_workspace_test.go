package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/dundunHa/TokenPivot/internal/workspaces"
)

const currentPath = "/api/v1/workspaces/current"

func TestCurrentWorkspaceRequiresSession(t *testing.T) {
	f := newUsageFixture(t)
	w := f.get(t, currentPath, false)
	if code, _ := errorCode(t, w); w.Code != http.StatusUnauthorized || code != "UNAUTHENTICATED" {
		t.Fatalf("status %d code %s", w.Code, code)
	}
	assertNoStore(t, w)
}

func TestCurrentWorkspaceReturnsServerWorkspace(t *testing.T) {
	f := newUsageFixture(t)
	f.roles.current[f.ownerID.Bytes] = workspaces.Workspace{ID: f.workspaceID, Name: "Personal"}
	w := f.get(t, currentPath, true)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	assertNoStore(t, w)
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 2 || body["workspace_id"] != f.workspaceID.String() || body["name"] != "Personal" {
		t.Fatalf("body %v", body)
	}
}

func TestCurrentWorkspaceMissingIsInternalAndCreatesNothing(t *testing.T) {
	f := newUsageFixture(t)
	w := f.get(t, currentPath, true)
	if code, _ := errorCode(t, w); w.Code != http.StatusInternalServerError || code != "INTERNAL" {
		t.Fatalf("status %d code %s", w.Code, code)
	}
	assertNoStore(t, w)
	if len(f.roles.current) != 0 {
		t.Fatal("a workspace was created")
	}
}

func TestCurrentWorkspaceStoreFailureDoesNotLeak(t *testing.T) {
	f := newUsageFixture(t)
	f.roles.setError(errors.New("SELECT secret"))
	w := f.get(t, currentPath, true)
	if code, _ := errorCode(t, w); w.Code != http.StatusInternalServerError || code != "INTERNAL" || strings.Contains(w.Body.String(), "SELECT") {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

func TestCurrentWorkspaceRejectsNonGet(t *testing.T) {
	f := newUsageFixture(t)
	req, _ := http.NewRequest(http.MethodPost, currentPath, nil)
	req.AddCookie(f.ownerCookie)
	if w := f.doRequest(t, req); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", w.Code)
	}
}

func TestUsageOptions(t *testing.T) {
	f := newUsageFixture(t)
	f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleMember)
	f.store.options.Providers = []string{"openai", "anthropic", "openai"}
	w := f.get(t, f.path("options", ""), true)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	assertNoStore(t, w)
	if got := strings.TrimSpace(w.Body.String()); got != `{"providers":["anthropic","openai"],"models":[]}` {
		t.Fatalf("body %s", got)
	}
	if f.store.workspace != f.workspaceID.String() {
		t.Fatal("store got another workspace")
	}
}

func TestUsageOptionsAuthorisation(t *testing.T) {
	f := newUsageFixture(t)
	if w := f.get(t, f.path("options", ""), false); w.Code != http.StatusUnauthorized {
		t.Fatalf("no session: %d", w.Code)
	}
	w := f.get(t, f.path("options", ""), true) // not a member
	if code, _ := errorCode(t, w); w.Code != http.StatusForbidden || code != "PERMISSION_DENIED" {
		t.Fatalf("non member: %d %s", w.Code, code)
	}
	if f.store.calls != 0 {
		t.Fatal("store queried before authorisation")
	}
	f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleOwner)
	if w := f.get(t, f.path("options", "provider=x"), true); w.Code != http.StatusBadRequest {
		t.Fatalf("query string: %d", w.Code)
	}
	f.store.err = errors.New("SELECT secret")
	w = f.get(t, f.path("options", ""), true)
	if code, _ := errorCode(t, w); w.Code != http.StatusInternalServerError || code != "INTERNAL" || strings.Contains(w.Body.String(), "SELECT") {
		t.Fatalf("store failure: %d %s", w.Code, w.Body.String())
	}
}
