package server

import (
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

	"github.com/dundunHa/TokenPivot/internal/auth"
	"github.com/dundunHa/TokenPivot/internal/config"
)

const testOrigin = "http://127.0.0.1:5173"

// authFakeStore is an in-memory auth store used by the HTTP tests. It mirrors
// only the SQL semantics the service relies on.
type authFakeStore struct {
	mu       sync.Mutex
	users    map[string]auth.Credential
	sessions map[string]fakeSession
	failures authStoreFailures
	nextID   auth.ID
	// calls counts the methods reached, so a rejected request can be proven to
	// have stopped before any store access.
	calls map[string]int
	// now is the store clock; nil means time.Now. A test drives it separately
	// from the HTTP clock to model a response that is written after the database
	// already assigned the expiry.
	now func() time.Time
}

type fakeSession struct {
	userID    auth.ID
	expiresAt time.Time
	revoked   bool
}

// authStoreFailures forces a specific store method to fail.
type authStoreFailures struct {
	credential   error
	authenticate error
	revoke       error
	create       error
}

func newAuthFakeStore() *authFakeStore {
	return &authFakeStore{users: make(map[string]auth.Credential), sessions: make(map[string]fakeSession), calls: make(map[string]int)}
}

// clock reads the store clock, so a test can hold the database time still while
// the response time moves on.
func (f *authFakeStore) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

// record counts one store call.
func (f *authFakeStore) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
}

// callCount reads how often one store method was reached.
func (f *authFakeStore) callCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *authFakeStore) identifier() auth.ID {
	var id auth.ID
	f.nextID[15]++
	if f.nextID[15] == 0 {
		f.nextID[14]++
	}
	id = f.nextID
	return id
}

// seedUser stores one account with a real Argon2id encoding of password.
func (f *authFakeStore) seedUser(t *testing.T, email, password string) auth.User {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	user := auth.User{ID: f.identifier(), Email: email}
	f.users[email] = auth.Credential{User: user, PasswordHash: hash}
	return user
}

func (f *authFakeStore) UserCredential(ctx context.Context, email string) (auth.Credential, bool, error) {
	f.record("UserCredential")
	if f.failures.credential != nil {
		return auth.Credential{}, false, f.failures.credential
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	credential, ok := f.users[email]
	return credential, ok, nil
}

func (f *authFakeStore) LockRegistration(ctx context.Context) error {
	f.record("LockRegistration")
	return nil
}

func (f *authFakeStore) HasUsers(ctx context.Context) (bool, error) {
	f.record("HasUsers")
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.users) > 0, nil
}

func (f *authFakeStore) CreateUser(ctx context.Context, email, passwordHash string) (auth.User, error) {
	f.record("CreateUser")
	if f.failures.create != nil {
		return auth.User{}, f.failures.create
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.users[email]; exists {
		return auth.User{}, auth.ErrEmailUnavailable
	}
	user := auth.User{ID: f.identifier(), Email: email}
	f.users[email] = auth.Credential{User: user, PasswordHash: passwordHash}
	return user, nil
}

func (f *authFakeStore) CreatePersonal(ctx context.Context, userID auth.ID) error {
	f.record("CreatePersonal")
	return nil
}

func (f *authFakeStore) CreateSession(ctx context.Context, userID auth.ID, tokenHash []byte) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	expiresAt := f.clock().UTC().Add(auth.SessionLifetime).Truncate(time.Second)
	f.sessions[string(tokenHash)] = fakeSession{userID: userID, expiresAt: expiresAt}
	return expiresAt, nil
}

func (f *authFakeStore) AuthenticateSession(ctx context.Context, tokenHash []byte) (auth.IssuedSession, bool, error) {
	f.record("AuthenticateSession")
	if f.failures.authenticate != nil {
		return auth.IssuedSession{}, false, f.failures.authenticate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	session, ok := f.sessions[string(tokenHash)]
	if !ok || session.revoked || !session.expiresAt.After(f.clock()) {
		return auth.IssuedSession{}, false, nil
	}
	// Renewal restarts the window from the store clock, mirroring the single
	// UPDATE ... now() + 30 days statement. The fake keeps whole seconds because
	// the cookie date has second precision, and a renewal never reports an
	// unchanged expiry, so a test always observes a strictly later time.
	renewed := f.clock().UTC().Add(auth.SessionLifetime).Truncate(time.Second)
	if !renewed.After(session.expiresAt) {
		renewed = session.expiresAt.Add(time.Second)
	}
	session.expiresAt = renewed
	f.sessions[string(tokenHash)] = session
	for _, credential := range f.users {
		if credential.User.ID == session.userID {
			return auth.IssuedSession{User: credential.User, ExpiresAt: session.expiresAt}, true, nil
		}
	}
	return auth.IssuedSession{}, false, nil
}

func (f *authFakeStore) RevokeSession(ctx context.Context, tokenHash []byte) error {
	f.record("RevokeSession")
	if f.failures.revoke != nil {
		return f.failures.revoke
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	session, ok := f.sessions[string(tokenHash)]
	if !ok {
		return nil
	}
	session.revoked = true
	f.sessions[string(tokenHash)] = session
	return nil
}

// sessionExpiry returns the stored expiry of a token for cookie assertions.
func (f *authFakeStore) sessionExpiry(token string) (time.Time, bool) {
	sum := sha256.Sum256([]byte(token))
	f.mu.Lock()
	defer f.mu.Unlock()
	session, ok := f.sessions[string(sum[:])]
	return session.expiresAt, ok
}

// authInTx runs the registration function directly; the store is already shared.
func (f *authFakeStore) InTx(ctx context.Context, fn func(context.Context, auth.RegistrationStore) error) error {
	return fn(ctx, f)
}

// newTestAuth returns an adapter whose registration is open, which keeps every
// pre-existing HTTP test on the plain success path. The switch tests build their
// own closed adapter below.
func newTestAuth(t *testing.T) (*Auth, *authFakeStore) {
	t.Helper()
	return newTestAuthWithRegistration(t, true)
}

// newTestAuthWithRegistration returns an adapter with the given startup
// registration setting.
func newTestAuthWithRegistration(t *testing.T, registrationOpen bool) (*Auth, *authFakeStore) {
	t.Helper()
	store := newAuthFakeStore()
	origin, err := config.ParseOrigin(testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	service := auth.New(store, store, auth.NewLimiter(nil), registrationOpen)
	return NewAuth(service, origin), store
}

// authHandler builds the full handler with authentication enabled.
func authHandler(t *testing.T) (*Auth, *authFakeStore, http.Handler) {
	t.Helper()
	adapter, store := newTestAuth(t)
	return adapter, store, NewHandler(Options{CheckReady: func(context.Context) error { return nil }, Auth: adapter})
}

// closedAuthHandler builds the full handler for a deployment that keeps
// registration closed.
func closedAuthHandler(t *testing.T) (*Auth, *authFakeStore, http.Handler) {
	t.Helper()
	adapter, store := newTestAuthWithRegistration(t, false)
	return adapter, store, NewHandler(Options{CheckReady: func(context.Context) error { return nil }, Auth: adapter})
}

// doAuthRequest performs one request against the handler with the trusted
// Origin and JSON content type already set, then returns the recorder.
func doAuthRequest(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	req.RemoteAddr = "127.0.0.1:40000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// sessionCookie extracts the session cookie from a recorder.
func sessionCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			return cookie
		}
	}
	t.Fatal("response did not set the session cookie")
	return nil
}

func TestRegisterSucceedsAndSetsSession(t *testing.T) {
	_, store, h := authHandler(t)
	w := doAuthRequest(t, h, http.MethodPost, registerPath, `{"email":"Synthetic@Example.com","password":"synthetic-password"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	cookie := sessionCookie(t, w)
	if !strings.HasPrefix(cookie.Value, "tps_") {
		t.Fatalf("cookie value does not carry the session prefix")
	}
	if cookie.Domain != "" || cookie.Path != "/" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Secure {
		t.Fatalf("unexpected cookie attributes: %+v", cookie)
	}
	if cookie.MaxAge != int(auth.SessionLifetime.Seconds()) {
		t.Fatalf("Max-Age = %d", cookie.MaxAge)
	}
	expiresAt, ok := store.sessionExpiry(cookie.Value)
	if !ok || !cookie.Expires.Equal(expiresAt) {
		t.Fatalf("cookie expiry %s does not match the database expiry %s", cookie.Expires, expiresAt)
	}
	if !strings.Contains(w.Body.String(), "synthetic@example.com") || strings.Contains(w.Body.String(), "synthetic-password") {
		t.Fatalf("response body leaked or omitted the wrong fields: %s", w.Body.String())
	}
	if n := len(w.Result().Cookies()); n != 1 {
		t.Fatalf("registration set %d cookies, want exactly one", n)
	}
}

func TestRegisterRejectsDuplicateEmail(t *testing.T) {
	_, _, h := authHandler(t)
	body := `{"email":"synthetic@example.com","password":"synthetic-password"}`
	if w := doAuthRequest(t, h, http.MethodPost, registerPath, body); w.Code != http.StatusCreated {
		t.Fatalf("first registration status = %d", w.Code)
	}
	w := doAuthRequest(t, h, http.MethodPost, registerPath, body)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "EMAIL_UNAVAILABLE") {
		t.Fatalf("duplicate status/body = %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("failure response Cache-Control = %q", got)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("failed registration set a cookie")
	}
}

// TestRegisterClosedRejectsWithoutSessionOrWrites proves a closed deployment
// refuses a second account with the documented 403, no session cookie and no
// account write, while the very first account of the empty database still works.
func TestRegisterClosedRejectsWithoutSessionOrWrites(t *testing.T) {
	_, store, h := closedAuthHandler(t)
	first := doAuthRequest(t, h, http.MethodPost, registerPath, `{"email":"first@example.com","password":"synthetic-password"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first registration status = %d, want 201: %s", first.Code, first.Body.String())
	}
	if calls := store.callCount("CreateUser"); calls != 1 {
		t.Fatalf("account writes after the first registration = %d, want 1", calls)
	}
	rejected := doAuthRequest(t, h, http.MethodPost, registerPath, `{"email":"second@example.com","password":"synthetic-password"}`)
	if rejected.Code != http.StatusForbidden || !strings.Contains(rejected.Body.String(), "REGISTRATION_CLOSED") {
		t.Fatalf("closed status/body = %d %s", rejected.Code, rejected.Body.String())
	}
	if got := rejected.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if len(rejected.Result().Cookies()) != 0 {
		t.Fatal("a rejected registration set a session cookie")
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rejected.Body.Bytes(), &body); err != nil || body.Error.Code != "REGISTRATION_CLOSED" || body.Error.Message != "registration is closed" {
		t.Fatalf("closed body = %s", rejected.Body.String())
	}
	if calls := store.callCount("CreateUser"); calls != 1 {
		t.Fatalf("rejected registration wrote an account: %d", calls)
	}
	if !strings.Contains(first.Body.String(), `"expires_at":"`) {
		t.Fatalf("the first registration body is incomplete: %s", first.Body.String())
	}
}

// TestRegisterClosedDoesNotRevealExistence proves an occupied and a free email get
// exactly the same closed response, so the refusal never discloses whether an
// account exists, and EMAIL_UNAVAILABLE is not used in closed mode.
func TestRegisterClosedDoesNotRevealExistence(t *testing.T) {
	_, store, h := closedAuthHandler(t)
	store.seedUser(t, "known@example.com", "synthetic-password")
	var responses []*httptest.ResponseRecorder
	for _, email := range []string{"known@example.com", "unknown@example.com"} {
		w := doAuthRequest(t, h, http.MethodPost, registerPath, `{"email":"`+email+`","password":"synthetic-password"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d, want 403: %s", email, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "EMAIL_UNAVAILABLE") {
			t.Fatalf("%s leaked the email conflict: %s", email, w.Body.String())
		}
		responses = append(responses, w)
	}
	if responses[0].Body.String() != responses[1].Body.String() {
		t.Fatalf("bodies differ: %s vs %s", responses[0].Body.String(), responses[1].Body.String())
	}
	if calls := store.callCount("CreateUser"); calls != 0 {
		t.Fatalf("a closed registration wrote an account %d times", calls)
	}
}

// TestRegisterClosedKeepsOriginAndRateLimit proves the new mapping does not
// override the earlier checks: a bad Origin is still PERMISSION_DENIED and an
// exhausted window is still 429 with Retry-After.
func TestRegisterClosedKeepsOriginAndRateLimit(t *testing.T) {
	_, store, h := closedAuthHandler(t)
	store.seedUser(t, "known@example.com", "synthetic-password")

	req := httptest.NewRequest(http.MethodPost, registerPath, strings.NewReader(`{"email":"new@example.com","password":"synthetic-password"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "PERMISSION_DENIED") {
		t.Fatalf("missing Origin status/body = %d %s", w.Code, w.Body.String())
	}
	if calls := store.callCount("HasUsers"); calls != 0 {
		t.Fatal("an Origin rejection reached the registration decision")
	}

	var limited *httptest.ResponseRecorder
	for range 5 {
		limited = doAuthRequest(t, h, http.MethodPost, registerPath, `{"email":"new@example.com","password":"synthetic-password"}`)
		if limited.Code != http.StatusForbidden {
			t.Fatalf("attempt status = %d, want 403: %s", limited.Code, limited.Body.String())
		}
	}
	limited = doAuthRequest(t, h, http.MethodPost, registerPath, `{"email":"new@example.com","password":"synthetic-password"}`)
	if limited.Code != http.StatusTooManyRequests || !strings.Contains(limited.Body.String(), "RATE_LIMITED") {
		t.Fatalf("exhausted status/body = %d %s", limited.Code, limited.Body.String())
	}
	if retryAfter := limited.Header().Get("Retry-After"); retryAfter == "" || retryAfter == "0" {
		t.Fatalf("Retry-After = %q", retryAfter)
	}
}

func TestLoginSucceedsAndRotatesSession(t *testing.T) {
	_, store, h := authHandler(t)
	store.seedUser(t, "synthetic@example.com", "synthetic-password")
	w := doAuthRequest(t, h, http.MethodPost, loginPath, `{"email":"synthetic@example.com","password":"synthetic-password"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	cookie := sessionCookie(t, w)
	if !strings.HasPrefix(cookie.Value, "tps_") {
		t.Fatal("login did not issue a session token")
	}
	second := doAuthRequest(t, h, http.MethodPost, loginPath, `{"email":"synthetic@example.com","password":"synthetic-password"}`)
	if secondCookie := sessionCookie(t, second); secondCookie.Value == cookie.Value {
		t.Fatal("login reused the existing session token")
	}
}

func TestLoginFailuresShareOneResponse(t *testing.T) {
	_, store, h := authHandler(t)
	store.seedUser(t, "synthetic@example.com", "synthetic-password")
	for name, body := range map[string]string{
		"unknown email":  `{"email":"unknown@example.com","password":"synthetic-password"}`,
		"wrong password": `{"email":"synthetic@example.com","password":"synthetic-wrong-password"}`,
	} {
		t.Run(name, func(t *testing.T) {
			w := doAuthRequest(t, h, http.MethodPost, loginPath, body)
			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "INVALID_CREDENTIALS") {
				t.Fatalf("status/body = %d %s", w.Code, w.Body.String())
			}
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("failed login set a cookie")
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("failure response Cache-Control = %q", got)
			}
		})
	}
}

func TestSessionEndpointRenewsCookieAndExpiry(t *testing.T) {
	_, store, h := authHandler(t)
	register := doAuthRequest(t, h, http.MethodPost, registerPath, `{"email":"synthetic@example.com","password":"synthetic-password"}`)
	cookie := sessionCookie(t, register)
	before, ok := store.sessionExpiry(cookie.Value)
	if !ok {
		t.Fatal("session was not stored")
	}
	req := httptest.NewRequest(http.MethodGet, sessionPath, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie.Value})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	renewed := sessionCookie(t, w)
	after, ok := store.sessionExpiry(cookie.Value)
	if !ok || !after.After(before) {
		t.Fatalf("expiry was not renewed: before=%s after=%s", before, after)
	}
	if !renewed.Expires.Equal(after) {
		t.Fatalf("cookie expiry %s differs from the database expiry %s", renewed.Expires, after)
	}
	if !strings.Contains(w.Body.String(), `"expires_at":"`) || !strings.Contains(w.Body.String(), `"email":"synthetic@example.com"`) {
		t.Fatalf("body = %s", w.Body.String())
	}
	var body struct {
		User      map[string]string `json:"user"`
		ExpiresAt string            `json:"expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.User["email"] != "synthetic@example.com" || body.ExpiresAt == "" {
		t.Fatalf("body = %s", w.Body.String())
	}
	if _, err := time.Parse(time.RFC3339, body.ExpiresAt); err != nil {
		t.Fatalf("expires_at is not RFC 3339: %v", err)
	}
}

func TestSessionEndpointRejectsUnusableSessions(t *testing.T) {
	_, store, h := authHandler(t)
	store.seedUser(t, "synthetic@example.com", "synthetic-password")
	login := doAuthRequest(t, h, http.MethodPost, loginPath, `{"email":"synthetic@example.com","password":"synthetic-password"}`)
	revoked := sessionCookie(t, login)
	second := doAuthRequest(t, h, http.MethodPost, loginPath, `{"email":"synthetic@example.com","password":"synthetic-password"}`)
	valid := sessionCookie(t, second)
	if valid.Value == revoked.Value {
		t.Fatal("two logins reused one token")
	}
	if err := store.RevokeSession(context.Background(), digest(revoked.Value)); err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{
		"missing":     "",
		"malformed":   "synthetic-token",
		"plugin":      "tpk_synthetic",
		"unknown":     "tps_unknown",
		"revoked":     revoked.Value,
		"empty value": "",
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, sessionPath, nil)
			if token != "" {
				req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "UNAUTHENTICATED") {
				t.Fatalf("status/body = %d %s", w.Code, w.Body.String())
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q", got)
			}
		})
	}
	// A valid cookie still works after the rejected attempts above.
	req := httptest.NewRequest(http.MethodGet, sessionPath, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: valid.Value})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("valid session status = %d", w.Code)
	}
}

func TestSessionEndpointDoesNotRequireOrigin(t *testing.T) {
	_, _, h := authHandler(t)
	register := doAuthRequest(t, h, http.MethodPost, registerPath, `{"email":"synthetic@example.com","password":"synthetic-password"}`)
	cookie := sessionCookie(t, register)
	req := httptest.NewRequest(http.MethodGet, sessionPath, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie.Value})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("a read request was rejected without Origin: %d", w.Code)
	}
}

func TestLogoutRevokesAndClearsCookie(t *testing.T) {
	_, store, h := authHandler(t)
	register := doAuthRequest(t, h, http.MethodPost, registerPath, `{"email":"synthetic@example.com","password":"synthetic-password"}`)
	cookie := sessionCookie(t, register)
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, logoutPath, nil)
		req.Header.Set("Origin", testOrigin)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie.Value})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("logout status = %d: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q", got)
		}
		cleared := sessionCookie(t, w)
		if cleared.Value != "" || cleared.MaxAge != -1 || cleared.Path != "/" || !cleared.HttpOnly || cleared.Secure {
			t.Fatalf("unexpected clearing cookie: %+v", cleared)
		}
	}
	if _, ok := store.sessionExpiry(cookie.Value); !ok {
		t.Fatal("session row disappeared instead of being revoked")
	}
	session := httptest.NewRequest(http.MethodGet, sessionPath, nil)
	session.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie.Value})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, session)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session status = %d", w.Code)
	}
}

func TestLogoutWithoutSessionStillClearsCookie(t *testing.T) {
	_, _, h := authHandler(t)
	req := httptest.NewRequest(http.MethodPost, logoutPath, nil)
	req.Header.Set("Origin", testOrigin)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d", w.Code)
	}
	if cleared := sessionCookie(t, w); cleared.MaxAge != -1 {
		t.Fatalf("cookie was not cleared: %+v", cleared)
	}
}

func TestLogoutReportsRevocationFailure(t *testing.T) {
	_, store, h := authHandler(t)
	register := doAuthRequest(t, h, http.MethodPost, registerPath, `{"email":"synthetic@example.com","password":"synthetic-password"}`)
	cookie := sessionCookie(t, register)
	store.failures.revoke = errors.New("synthetic database failure")
	req := httptest.NewRequest(http.MethodPost, logoutPath, nil)
	req.Header.Set("Origin", testOrigin)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie.Value})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "INTERNAL") {
		t.Fatalf("status/body = %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "synthetic database failure") {
		t.Fatal("database message leaked into the response")
	}
}

func TestEnvironmentFailureIsNotAnAuthFailure(t *testing.T) {
	_, store, h := authHandler(t)
	store.failures.authenticate = errors.New("synthetic database failure")
	register := doAuthRequest(t, h, http.MethodPost, registerPath, `{"email":"synthetic@example.com","password":"synthetic-password"}`)
	cookie := sessionCookie(t, register)
	req := httptest.NewRequest(http.MethodGet, sessionPath, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie.Value})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("a database failure cleared the cookie")
	}
}

// TestUnavailableDependencyIsNotInternalOrAuth proves a dependency failure maps
// to the documented 503 UNAVAILABLE, keeps the response uncached, does not clear
// the cookie and is never reported as an authentication failure.
func TestUnavailableDependencyIsNotInternalOrAuth(t *testing.T) {
	_, store, h := authHandler(t)
	register := doAuthRequest(t, h, http.MethodPost, registerPath, `{"email":"synthetic@example.com","password":"synthetic-password"}`)
	cookie := sessionCookie(t, register)
	store.failures.authenticate = errors.Join(auth.ErrUnavailable, errors.New("auth session authenticate: database operation failed"))
	req := httptest.NewRequest(http.MethodGet, sessionPath, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie.Value})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "UNAVAILABLE") {
		t.Fatalf("status/body = %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("an unavailable dependency cleared the cookie")
	}
	if strings.Contains(w.Body.String(), "database operation failed") {
		t.Fatal("database message leaked into the response")
	}
	// A login that cannot reach the database is not a credential error either.
	store.failures.credential = errors.Join(auth.ErrUnavailable, errors.New("auth account lookup: database operation failed"))
	login := doAuthRequest(t, h, http.MethodPost, loginPath, `{"email":"synthetic@example.com","password":"synthetic-password"}`)
	if login.Code != http.StatusServiceUnavailable || strings.Contains(login.Body.String(), "INVALID_CREDENTIALS") {
		t.Fatalf("login status/body = %d %s", login.Code, login.Body.String())
	}
	if len(login.Result().Cookies()) != 0 {
		t.Fatal("an unavailable dependency set a cookie")
	}
}

func TestAuthBodyValidation(t *testing.T) {
	_, _, h := authHandler(t)
	for name, tt := range map[string]struct {
		contentType, body string
		status            int
	}{
		"wrong media type":   {"text/plain", `{"email":"synthetic@example.com","password":"synthetic-password"}`, http.StatusUnsupportedMediaType},
		"missing media type": {"", `{}`, http.StatusUnsupportedMediaType},
		"unknown field":      {"application/json", `{"email":"synthetic@example.com","password":"synthetic-password","role":"owner"}`, http.StatusBadRequest},
		"duplicate key":      {"application/json", `{"email":"synthetic@example.com","email":"other@example.com","password":"synthetic-password"}`, http.StatusBadRequest},
		"trailing content":   {"application/json", `{"email":"synthetic@example.com","password":"synthetic-password"}{}`, http.StatusBadRequest},
		"array body":         {"application/json", `[]`, http.StatusBadRequest},
		"invalid utf8":       {"application/json", "{\"email\":\"synthetic@example.com\",\"password\":\"\xff\xfe\"}", http.StatusBadRequest},
		"too large":          {"application/json", `{"email":"synthetic@example.com","password":"` + strings.Repeat("a", maxAuthBodyBytes) + `"}`, http.StatusRequestEntityTooLarge},
		"empty body":         {"application/json", "", http.StatusBadRequest},
		"invalid email":      {"application/json", `{"email":"synthetic.example.com","password":"synthetic-password"}`, http.StatusBadRequest},
		"short password":     {"application/json", `{"email":"synthetic@example.com","password":"short"}`, http.StatusBadRequest},
		// encoding/json matches struct keys case-insensitively, so these bodies
		// would otherwise bind to Email/Password. Only the decoded exact spellings
		// count, and a second spelling of the same decoded key is a duplicate.
		"uppercase email key":     {"application/json", `{"EMAIL":"synthetic@example.com","password":"synthetic-password"}`, http.StatusBadRequest},
		"uppercase password key":  {"application/json", `{"email":"synthetic@example.com","PASSWORD":"synthetic-password"}`, http.StatusBadRequest},
		"mixed case duplicate":    {"application/json", `{"email":"synthetic@example.com","EMAIL":"other@example.com","password":"synthetic-password"}`, http.StatusBadRequest},
		"escaped duplicate key":   {"application/json", `{"email":"synthetic@example.com","\u0065mail":"other@example.com","password":"synthetic-password"}`, http.StatusBadRequest},
		"both password spellings": {"application/json", `{"email":"synthetic@example.com","password":"synthetic-password","\u0070assword":"other-password"}`, http.StatusBadRequest},
		"missing password":        {"application/json", `{"email":"synthetic@example.com"}`, http.StatusBadRequest},
		"missing email":           {"application/json", `{"password":"synthetic-password"}`, http.StatusBadRequest},
		"both empty":              {"application/json", `{}`, http.StatusBadRequest},
		"non-string email":        {"application/json", `{"email":42,"password":"synthetic-password"}`, http.StatusBadRequest},
		"object password":         {"application/json", `{"email":"synthetic@example.com","password":{"value":"synthetic-password"}}`, http.StatusBadRequest},
		"nested object":           {"application/json", `{"email":{"value":"synthetic@example.com"},"password":"synthetic-password"}`, http.StatusBadRequest},
		"whitespace padded key":   {"application/json", `{" email":"synthetic@example.com","password":"synthetic-password"}`, http.StatusBadRequest},
		"single quotation marks":  {"application/json", `{'email':'synthetic@example.com','password':'synthetic-password'}`, http.StatusBadRequest},
		"two objects":             {"application/json", `{"email":"synthetic@example.com","password":"synthetic-password"}{"email":"other@example.com","password":"synthetic-password"}`, http.StatusBadRequest},
		"null body":               {"application/json", `null`, http.StatusBadRequest},
		"number body":             {"application/json", `1`, http.StatusBadRequest},
		"string body":             {"application/json", `"synthetic"`, http.StatusBadRequest},
		"unterminated object":     {"application/json", `{"email":"synthetic@example.com","password":"synthetic-password"`, http.StatusBadRequest},
		"key without value":       {"application/json", `{"email":"synthetic@example.com","password"}`, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, registerPath, strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			req.Header.Set("Origin", testOrigin)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.status, w.Body.String())
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q", got)
			}
			if strings.Contains(w.Body.String(), "synthetic-password") {
				t.Fatal("response echoed the password")
			}
		})
	}
}

// TestAuthBodyAcceptsOnlyExactKeySpelling pins the strict key rule from both
// sides. Acceptance compares the decoded key exactly: JSON formatting, field
// order and an escaped spelling of a known key are free, but a differently cased,
// padded or otherwise different key name is an unknown field, a second spelling of
// the same decoded key is a duplicate, and neither reaches storage.
func TestAuthBodyAcceptsOnlyExactKeySpelling(t *testing.T) {
	for name, body := range map[string]string{
		"canonical":            `{"email":"synthetic@example.com","password":"synthetic-password"}`,
		"field order":          `{"password":"synthetic-password","email":"synthetic@example.com"}`,
		"formatted":            "{\n  \"email\": \"synthetic@example.com\",\n  \"password\": \"synthetic-password\"\n}\n",
		"escaped value only":   `{"email":"synthetic@example.com","password":"synthetic-\u0070assword"}`,
		"escaped email key":    `{"\u0065mail":"synthetic@example.com","password":"synthetic-password"}`,
		"escaped password key": `{"email":"synthetic@example.com","\u0070assword":"synthetic-password"}`,
		"both keys escaped":    `{"\u0065mail":"synthetic@example.com","\u0070assword":"synthetic-password"}`,
	} {
		t.Run("accepted "+name, func(t *testing.T) {
			_, store, h := authHandler(t)
			req := httptest.NewRequest(http.MethodPost, registerPath, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", testOrigin)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
			}
			if calls := store.callCount("CreateUser"); calls != 1 {
				t.Fatalf("storage calls = %d, want 1", calls)
			}
		})
	}
	for name, body := range map[string]string{
		"uppercase email":            `{"EMAIL":"synthetic@example.com","password":"synthetic-password"}`,
		"uppercase password":         `{"email":"synthetic@example.com","PASSWORD":"synthetic-password"}`,
		"title case":                 `{"Email":"synthetic@example.com","Password":"synthetic-password"}`,
		"mixed case duplicate":       `{"email":"synthetic@example.com","EMAIL":"other@example.com","password":"synthetic-password"}`,
		"escaped duplicate":          `{"email":"synthetic@example.com","\u0065mail":"other@example.com","password":"synthetic-password"}`,
		"escaped password duplicate": `{"email":"synthetic@example.com","password":"synthetic-password","\u0070assword":"synthetic-other-password"}`,
		"padded key":                 `{" email":"synthetic@example.com","password":"synthetic-password"}`,
		"padded password key":        `{"email":"synthetic@example.com","password ":"synthetic-password"}`,
		"extra field alongside":      `{"email":"synthetic@example.com","password":"synthetic-password","EMAIL":"other@example.com"}`,
		"user id field":              `{"email":"synthetic@example.com","password":"synthetic-password","user_id":"synthetic-forged"}`,
		"only password supplied":     `{"password":"synthetic-password"}`,
	} {
		t.Run("rejected "+name, func(t *testing.T) {
			_, store, h := authHandler(t)
			req := httptest.NewRequest(http.MethodPost, registerPath, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", testOrigin)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q", got)
			}
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("a rejected body set a session cookie")
			}
			if calls := store.callCount("CreateUser"); calls != 0 {
				t.Fatalf("a rejected body reached storage %d times", calls)
			}
		})
	}
}

func TestRateLimitedResponsesCarryRetryAfter(t *testing.T) {
	_, store, h := authHandler(t)
	store.seedUser(t, "synthetic@example.com", "synthetic-password")
	for range 5 {
		if w := doAuthRequest(t, h, http.MethodPost, loginPath, `{"email":"synthetic@example.com","password":"synthetic-wrong-password"}`); w.Code != http.StatusUnauthorized {
			t.Fatalf("early attempt status = %d", w.Code)
		}
	}
	w := doAuthRequest(t, h, http.MethodPost, loginPath, `{"email":"synthetic@example.com","password":"synthetic-password"}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if retryAfter := w.Header().Get("Retry-After"); retryAfter == "" || retryAfter == "0" {
		t.Fatalf("Retry-After = %q", retryAfter)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatal("rate limited response is cacheable")
	}
}

// TestRegisterRetryAfterLostResponseDoesNotReplayToken proves registration is
// not idempotent: once the response is lost, the retry is a conflict and cannot
// restore the first plaintext session token, which the database never stored.
func TestRegisterRetryAfterLostResponseDoesNotReplayToken(t *testing.T) {
	_, _, h := authHandler(t)
	body := `{"email":"synthetic@example.com","password":"synthetic-password"}`
	first := doAuthRequest(t, h, http.MethodPost, registerPath, body)
	firstToken := sessionCookie(t, first).Value
	retry := doAuthRequest(t, h, http.MethodPost, registerPath, body)
	if retry.Code != http.StatusConflict {
		t.Fatalf("retry status = %d, want 409", retry.Code)
	}
	if len(retry.Result().Cookies()) != 0 {
		t.Fatal("the retry re-issued a session cookie")
	}
	if strings.Contains(retry.Body.String(), firstToken) || strings.Contains(retry.Body.String(), "tps_") {
		t.Fatal("the retry response echoed a session token")
	}
}

func TestAuthRoutesAbsentWithoutAuth(t *testing.T) {
	h := NewHandler(Options{CheckReady: func(context.Context) error { return nil }})
	for _, path := range []string{registerPath, loginPath, sessionPath, logoutPath} {
		method := http.MethodPost
		if path == sessionPath {
			method = http.MethodGet
		}
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.Header.Set("Origin", testOrigin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s %s status = %d, want 404 without auth", method, path, w.Code)
		}
	}
}

// digest mirrors the store digest of a token for direct test setup.
func digest(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
