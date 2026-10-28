package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dundunHa/TokenPivot/internal/auth"
	"github.com/dundunHa/TokenPivot/internal/config"
)

// TestRequireOriginRejectsWriteRequests covers every Origin shape the plan
// forbids, and proves a rejected request never reaches the service or the store.
func TestRequireOriginRejectsWriteRequests(t *testing.T) {
	_, store, h := authHandler(t)
	for name, values := range map[string][]string{
		"missing":          nil,
		"empty":            {""},
		"blank":            {"   "},
		"null":             {"null"},
		"multiple":         {testOrigin, testOrigin},
		"other scheme":     {"https://127.0.0.1:5173"},
		"other host":       {"http://localhost:5173"},
		"other port":       {"http://127.0.0.1:5174"},
		"with path":        {testOrigin + "/path"},
		"with trailing":    {testOrigin + "/"},
		"subdomain":        {"http://api.127.0.0.1:5173"},
		"unrelated origin": {"https://evil.example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, registerPath, strings.NewReader(`{"email":"synthetic@example.com","password":"synthetic-password"}`))
			req.Header.Set("Content-Type", "application/json")
			for _, value := range values {
				req.Header.Add("Origin", value)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "PERMISSION_DENIED") {
				t.Fatalf("status/body = %d %s", w.Code, w.Body.String())
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q on a rejected request", got)
			}
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("forbidden request set a cookie")
			}
		})
	}
	if calls := store.callCount("CreateUserWithSession"); calls != 0 {
		t.Fatalf("forbidden requests reached storage %d times", calls)
	}
	if calls := store.callCount("AuthenticateSession"); calls != 0 {
		t.Fatalf("forbidden requests reached storage %d times", calls)
	}
}

// TestRequireOriginAppliesToEveryWriteRoute covers register, login and logout.
func TestRequireOriginAppliesToEveryWriteRoute(t *testing.T) {
	_, _, h := authHandler(t)
	for _, path := range []string{registerPath, loginPath, logoutPath} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"email":"synthetic@example.com","password":"synthetic-password"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s without Origin status = %d", path, w.Code)
		}
	}
}

// TestRequireSessionAttachesUser proves a protected route can read the identity
// from the request context and cannot invent one from the request body.
func TestRequireSessionAttachesUser(t *testing.T) {
	adapter, _, h := authHandler(t)
	register := doAuthRequest(t, h, http.MethodPost, registerPath, `{"email":"synthetic@example.com","password":"synthetic-password"}`)
	cookie := sessionCookie(t, register)

	var got auth.User
	var found bool
	protected := adapter.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, found = UserFromContext(r.Context())
		// A body field claiming another user must have no effect.
		if _, ok := r.Context().Value(sessionContextKey{}).(sessionIdentity); !ok {
			t.Error("session identity missing from the context")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/protected", strings.NewReader(`{"user_id":"synthetic-forged"}`))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie.Value})
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !found || got.Email != "synthetic@example.com" || got.ID.String() == "" {
		t.Fatalf("context user = %+v found=%t", got, found)
	}
}

// TestUserFromContextWithoutSession proves an unauthenticated context reports
// no identity instead of a zero-value user.
func TestUserFromContextWithoutSession(t *testing.T) {
	if _, ok := UserFromContext(context.Background()); ok {
		t.Fatal("an empty context reported an identity")
	}
}

// TestCookieSecureFollowsOrigin pins the Secure attribute derived from the
// configured origin: https requires it, loopback http does not.
func TestCookieSecureFollowsOrigin(t *testing.T) {
	for _, tt := range []struct {
		origin string
		secure bool
	}{
		{"http://127.0.0.1:5173", false},
		{"http://localhost:5173", false},
		{"https://app.example.com", true},
	} {
		t.Run(tt.origin, func(t *testing.T) {
			origin, err := config.ParseOrigin(tt.origin)
			if err != nil {
				t.Fatal(err)
			}
			store := newAuthFakeStore()
			adapter := NewAuth(auth.New(store, store, auth.NewLimiter(nil)), origin)
			h := NewHandler(Options{CheckReady: func(context.Context) error { return nil }, Auth: adapter})
			req := httptest.NewRequest(http.MethodPost, registerPath, strings.NewReader(`{"email":"synthetic@example.com","password":"synthetic-password"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", tt.origin)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			cookie := sessionCookie(t, w)
			if cookie.Secure != tt.secure {
				t.Fatalf("Secure = %t, want %t", cookie.Secure, tt.secure)
			}
			if cookie.Domain != "" {
				t.Fatalf("cookie has a Domain attribute: %q", cookie.Domain)
			}
		})
	}
}

// TestSessionFailureDoesNotLeakCookie proves a database failure keeps the
// cookie, so a transient outage cannot silently sign a user out.
func TestSessionFailureDoesNotLeakCookie(t *testing.T) {
	_, store, h := authHandler(t)
	register := doAuthRequest(t, h, http.MethodPost, registerPath, `{"email":"synthetic@example.com","password":"synthetic-password"}`)
	cookie := sessionCookie(t, register)
	store.failures.authenticate = errors.New("synthetic database failure")
	req := httptest.NewRequest(http.MethodGet, sessionPath, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie.Value})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatal("a database failure was reported as an authentication failure")
	}
	for _, emitted := range w.Result().Cookies() {
		if emitted.Name == sessionCookieName && emitted.MaxAge < 0 {
			t.Fatal("a database failure cleared the session cookie")
		}
	}
}

// TestExpiredSessionIsRejected proves an expired row is indistinguishable from
// any other unusable session and still clears the cookie.
func TestExpiredSessionIsRejected(t *testing.T) {
	_, store, h := authHandler(t)
	register := doAuthRequest(t, h, http.MethodPost, registerPath, `{"email":"synthetic@example.com","password":"synthetic-password"}`)
	cookie := sessionCookie(t, register)
	store.mu.Lock()
	session := store.sessions[string(digest(cookie.Value))]
	session.expiresAt = time.Now().Add(-time.Minute)
	store.sessions[string(digest(cookie.Value))] = session
	store.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, sessionPath, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie.Value})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	cleared := sessionCookie(t, w)
	if cleared.MaxAge != -1 {
		t.Fatalf("expired session cookie was not cleared: %+v", cleared)
	}
}

// TestAuthRoutesKeepProbesAndFrontend proves adding authentication does not
// change probe responses, frontend dispatch or the /api no-fallback rule.
func TestAuthRoutesKeepProbesAndFrontend(t *testing.T) {
	frontend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frontend", "yes")
		w.WriteHeader(http.StatusNoContent)
	})
	adapter, _ := newTestAuth(t)
	h := NewHandler(Options{CheckReady: func(context.Context) error { return nil }, Frontend: frontend, Auth: adapter})
	for _, tt := range []struct {
		method, path string
		status       int
		frontend     bool
	}{
		{"GET", "/healthz", http.StatusOK, false},
		{"GET", "/readyz", http.StatusOK, false},
		{"GET", "/api/not-implemented", http.StatusNotFound, false},
		{"POST", "/api/v1/auth/unknown", http.StatusNotFound, false},
		{"GET", "/", http.StatusNoContent, true},
		{"GET", "/deep-link", http.StatusNoContent, true},
	} {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		req.Header.Set("Accept", "text/html")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tt.status {
			t.Fatalf("%s %s status = %d, want %d", tt.method, tt.path, w.Code, tt.status)
		}
		if reached := w.Header().Get("X-Frontend") == "yes"; reached != tt.frontend {
			t.Fatalf("%s %s frontend reach = %t, want %t", tt.method, tt.path, reached, tt.frontend)
		}
	}
}

// TestWrongMethodOnAuthRoutes proves the fixed method of each route is enforced.
func TestWrongMethodOnAuthRoutes(t *testing.T) {
	_, _, h := authHandler(t)
	for _, tt := range []struct{ method, path string }{
		{http.MethodGet, registerPath},
		{http.MethodGet, loginPath},
		{http.MethodPost, sessionPath},
		{http.MethodGet, logoutPath},
	} {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		req.Header.Set("Origin", testOrigin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s status = %d, want 405", tt.method, tt.path, w.Code)
		}
	}
}
