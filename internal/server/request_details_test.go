package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dundunHa/TokenPivot/internal/requestdetails"
	"github.com/dundunHa/TokenPivot/internal/workspaces"
	"github.com/jackc/pgx/v5/pgtype"
)

type detailsFakeStore struct {
	calls     int
	projects  int
	workspace string
}

func (f *detailsFakeStore) CountProjects(_ context.Context, _ workspaces.ID, ids []pgtype.UUID) (int, error) {
	f.calls++
	return len(ids) - f.projects, nil
}

func (f *detailsFakeStore) Events(_ context.Context, p requestdetails.EventsParams) ([]requestdetails.EventRow, error) {
	f.calls++
	f.workspace = p.WorkspaceID.String()
	return nil, nil
}

func (f *detailsFakeStore) Rate(_ context.Context, p requestdetails.RateParams) (requestdetails.RateRow, error) {
	f.calls++
	f.workspace = p.WorkspaceID.String()
	return requestdetails.RateRow{AsOf: time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)}, nil
}

func newDetailsFixture(t *testing.T) (*usageFixture, *detailsFakeStore) {
	t.Helper()
	adapter, authStore := newTestAuth(t)
	user := authStore.seedUser(t, "reader@example.com", "synthetic-password")
	roles := newRoleFakeStore()
	store := &detailsFakeStore{}
	handler := NewHandler(Options{
		CheckReady:     func(context.Context) error { return nil },
		Auth:           adapter,
		RequestDetails: NewRequestDetails(requestdetails.New(store), workspaces.New(roles)),
	})
	login := httptest.NewRequest(http.MethodPost, loginPath, strings.NewReader(`{"email":"reader@example.com","password":"synthetic-password"}`))
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", testOrigin)
	login.RemoteAddr = "127.0.0.1:40000"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, login)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d", recorder.Code)
	}
	f := &credentialFixture{
		handler: handler, roles: roles, authStore: authStore, ownerCookie: sessionCookie(t, recorder),
		ownerID: workspaces.ID{Bytes: user.ID, Valid: true}, workspaceID: idWithByte(0x41), otherSpace: idWithByte(0x42),
	}
	return &usageFixture{credentialFixture: f}, store
}

func TestRequestDetailsAccess(t *testing.T) {
	for _, kind := range []string{"events", "rate"} {
		t.Run(kind, func(t *testing.T) {
			f, store := newDetailsFixture(t)
			path := f.path(kind, "provider=p&project=unattributed")
			w := f.get(t, path, false)
			if code, _ := errorCode(t, w); w.Code != http.StatusUnauthorized || code != "UNAUTHENTICATED" {
				t.Fatalf("anon status %d code %s", w.Code, code)
			}
			assertNoStore(t, w)
			w = f.get(t, path, true)
			if code, _ := errorCode(t, w); w.Code != http.StatusForbidden || code != "PERMISSION_DENIED" || store.calls != 0 {
				t.Fatalf("non member status %d code %s calls %d", w.Code, code, store.calls)
			}
			assertNoStore(t, w)
			for _, role := range []workspaces.Role{workspaces.RoleOwner, workspaces.RoleMember} {
				f.roles.setRole(f.ownerID, f.workspaceID, role)
				store.calls = 0
				w = f.get(t, path, true)
				if w.Code != http.StatusOK || store.workspace != f.workspaceID.String() {
					t.Fatalf("%s status %d: %s", role, w.Code, w.Body.String())
				}
				assertNoStore(t, w)
			}
		})
	}
}

func TestRequestDetailsInvalidArguments(t *testing.T) {
	f, store := newDetailsFixture(t)
	f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleMember)
	bad := map[string][]string{
		"events": {"limit=0", "limit=101", "limit=1&limit=2", "start=2026-01-01T00:00:00Z", "cursor=x", "bad=1", "provider=", "model=%ff", "project=nope", "%zz"},
		"rate":   {"limit=5", "start=2026-01-01T00:00:00Z", "project=nope"},
	}
	for kind, queries := range bad {
		for _, q := range queries {
			store.calls = 0
			w := f.get(t, f.path(kind, q), true)
			if code, _ := errorCode(t, w); w.Code != http.StatusBadRequest || code != "INVALID_ARGUMENT" || store.calls != 0 {
				t.Fatalf("%s?%s: status %d code %s calls %d", kind, q, w.Code, code, store.calls)
			}
			assertNoStore(t, w)
		}
	}
	// A project outside the workspace is the same 400 after membership.
	store.projects = 1
	w := f.get(t, f.path("rate", "project=0199a0b0-0000-7000-8000-00000000000a"), true)
	if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "0199a0b0") {
		t.Fatalf("foreign project status %d body %s", w.Code, w.Body.String())
	}
}

func TestRequestDetailsMalformedWorkspaceAndRateBody(t *testing.T) {
	f, _ := newDetailsFixture(t)
	f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleOwner)
	w := f.get(t, "/api/v1/workspaces/nope/usage/rate", true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d", w.Code)
	}
	w = f.get(t, f.path("rate", ""), true)
	body := w.Body.String()
	for _, want := range []string{`"window_seconds":60`, `"unit":"tokens/min"`, `"value":null`, `"window_last_received_at":null`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
	w = f.get(t, f.path("events", ""), true)
	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("events body %s", w.Body.String())
	}
}
