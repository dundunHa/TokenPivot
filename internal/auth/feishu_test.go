package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// feishuStub is a local stand-in for the Feishu open platform. Every response is
// synthetic: no test contacts the real provider.
type feishuStub struct {
	server *httptest.Server
	// openID, email and name are the profile the stub reports.
	openID string
	email  string
	name   string
	// includeProfileInToken puts open_id and email in the token response, so the
	// profile call is unnecessary.
	includeProfileInToken bool
	// failToken makes both token endpoints report a provider error.
	failToken bool
	// omitEmail removes the email from the profile.
	omitEmail bool
	// lastCode records the authorization code the token call carried.
	lastCode atomic.Value

	appTokenCalls  atomic.Int32
	tokenCalls     atomic.Int32
	userInfoCalls  atomic.Int32
	secretInQuery  atomic.Bool
	secretInBearer atomic.Bool
}

// newFeishuStub starts the stub and stops it when the test ends.
func newFeishuStub(t *testing.T) *feishuStub {
	t.Helper()
	stub := &feishuStub{openID: "ou_synthetic_open_id", email: "Synthetic@Example.com", name: "Synthetic User"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /open-apis/auth/v3/app_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		stub.appTokenCalls.Add(1)
		stub.observeCredentials(r)
		writeStubJSON(w, http.StatusOK, map[string]any{"code": 0, "app_access_token": "synthetic-app-token"})
	})
	mux.HandleFunc("POST /open-apis/authen/v1/oidc/access_token", func(w http.ResponseWriter, r *http.Request) {
		stub.tokenCalls.Add(1)
		stub.observeCredentials(r)
		if stub.failToken {
			writeStubJSON(w, http.StatusOK, map[string]any{"code": 20001, "msg": "synthetic provider failure"})
			return
		}
		body := stub.tokenRequest(t, r)
		stub.lastCode.Store(body)
		data := map[string]any{"user_access_token": "synthetic-user-token"}
		if stub.includeProfileInToken {
			data["open_id"] = stub.openID
			data["email"] = stub.email
		}
		writeStubJSON(w, http.StatusOK, map[string]any{"code": 0, "data": data})
	})
	mux.HandleFunc("GET /open-apis/authen/v1/user_info", func(w http.ResponseWriter, r *http.Request) {
		stub.userInfoCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-user-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		data := map[string]any{
			"open_id":    stub.openID,
			"union_id":   "on_synthetic_union",
			"tenant_key": "tk_synthetic_tenant",
			"name":       stub.name,
		}
		if !stub.omitEmail {
			data["email"] = stub.email
		}
		writeStubJSON(w, http.StatusOK, map[string]any{"code": 0, "data": data})
	})
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

// tokenRequest decodes the token request and records whether a credential ever
// travelled outside the request body.
func (s *feishuStub) tokenRequest(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("token request is not JSON: %v", err)
		return nil
	}
	if r.URL.RawQuery != "" {
		s.secretInQuery.Store(true)
	}
	if bearer := r.Header.Get("Authorization"); strings.Contains(bearer, "synthetic-secret") {
		s.secretInBearer.Store(true)
	}
	return body
}

// observeCredentials records whether the application secret leaked into the URL
// or the bearer header instead of the JSON body.
func (s *feishuStub) observeCredentials(r *http.Request) {
	if strings.Contains(r.URL.RawQuery, "synthetic-secret") {
		s.secretInQuery.Store(true)
	}
	if strings.Contains(r.Header.Get("Authorization"), "synthetic-secret") {
		s.secretInBearer.Store(true)
	}
}

// writeStubJSON writes one stub response.
func writeStubJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// newFeishuService returns a service configured against the stub. The pending
// binding clock is the same fake clock the limiter uses, so expiry is driven
// without sleeping.
func newFeishuService(t *testing.T, stub *feishuStub, registrationOpen, autoProvision bool) (*Service, *fakeStore, *fakeTxRunner, *fakeClock) {
	t.Helper()
	store := newFakeStore()
	runner := &fakeTxRunner{store: store}
	clock := newFakeClock()
	service := New(store, runner, NewLimiter(clock.Now), registrationOpen)
	service.now = clock.Now
	service.EnableFeishu(FeishuConfig{
		AppID:         "cli_synthetic_app",
		AppSecret:     "synthetic-secret",
		RedirectURI:   "https://tokenpivot.example.com/api/v1/auth/feishu/callback",
		BaseURL:       stub.server.URL,
		AutoProvision: autoProvision,
		HTTPClient:    stub.server.Client(),
	})
	return service, store, runner, clock
}

func TestFeishuAuthorizeURLCarriesEscapedValues(t *testing.T) {
	service, _, _, _ := newFeishuService(t, newFeishuStub(t), true, true)
	authorizeURL := service.FeishuAuthorizeURL("state with spaces&and=equals")
	parsed, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "http" || parsed.Path != "/open-apis/authen/v1/authorize" {
		t.Fatalf("authorize URL = %s", authorizeURL)
	}
	query := parsed.Query()
	if query.Get("app_id") != "cli_synthetic_app" {
		t.Fatalf("app_id = %q", query.Get("app_id"))
	}
	if query.Get("redirect_uri") != "https://tokenpivot.example.com/api/v1/auth/feishu/callback" {
		t.Fatalf("redirect_uri = %q", query.Get("redirect_uri"))
	}
	if query.Get("state") != "state with spaces&and=equals" {
		t.Fatalf("state = %q", query.Get("state"))
	}
	if !strings.Contains(authorizeURL, "state=state+with+spaces%26and%3Dequals") {
		t.Fatalf("state is not escaped in %s", authorizeURL)
	}
	if strings.Contains(authorizeURL, "synthetic-secret") {
		t.Fatal("the application secret appears in the authorize URL")
	}
}

func TestFeishuAuthorizeURLDefaultsToOpenPlatform(t *testing.T) {
	service := New(newFakeStore(), &fakeTxRunner{store: newFakeStore()}, NewLimiter(nil), true)
	service.EnableFeishu(FeishuConfig{AppID: "cli_synthetic_app", AppSecret: "synthetic-secret", RedirectURI: "https://tokenpivot.example.com/cb"})
	authorizeURL := service.FeishuAuthorizeURL("synthetic-state")
	if !strings.HasPrefix(authorizeURL, "https://open.feishu.cn/open-apis/authen/v1/authorize?") {
		t.Fatalf("authorize URL = %s", authorizeURL)
	}
}

func TestFeishuLoginUsesExistingBinding(t *testing.T) {
	stub := newFeishuStub(t)
	service, store, runner, _ := newFeishuService(t, stub, true, true)
	user := store.seedUser("synthetic@example.com", "synthetic-password")
	store.seedFeishuBinding(stub.openID, user)

	result, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil || result.BindToken != "" {
		t.Fatalf("result = %+v, want a session", result)
	}
	if result.Session.User.ID != user.ID || result.Session.User.Email != user.Email {
		t.Fatalf("session user = %+v", result.Session.User)
	}
	if !strings.HasPrefix(result.Session.Token, sessionTokenPrefix) {
		t.Fatal("session token lacks the tps_ prefix")
	}
	if runner.inTxCall != 0 {
		t.Fatalf("an existing binding used the transaction path %d times", runner.inTxCall)
	}
	if _, ok := store.sessions[string(sessionTokenHash(result.Session.Token))]; !ok {
		t.Fatal("store did not receive the session digest")
	}
	if stub.userInfoCalls.Load() != 1 || stub.tokenCalls.Load() != 1 || stub.appTokenCalls.Load() != 1 {
		t.Fatalf("provider calls app=%d token=%d info=%d", stub.appTokenCalls.Load(), stub.tokenCalls.Load(), stub.userInfoCalls.Load())
	}
	if code, _ := stub.lastCode.Load().(map[string]any); code == nil || code["code"] != "synthetic-code" {
		t.Fatalf("token request carried %v, want the authorization code", code)
	}
	if stub.secretInQuery.Load() || stub.secretInBearer.Load() {
		t.Fatal("the application secret left the JSON request body")
	}
}

func TestFeishuLoginAcceptsProfileInTokenResponse(t *testing.T) {
	stub := newFeishuStub(t)
	stub.includeProfileInToken = true
	service, store, _, _ := newFeishuService(t, stub, true, true)
	user := store.seedUser("synthetic@example.com", "synthetic-password")
	store.seedFeishuBinding(stub.openID, user)

	if _, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if calls := stub.userInfoCalls.Load(); calls != 0 {
		t.Fatalf("profile call = %d, want none when the token response is complete", calls)
	}
}

func TestFeishuLoginSameEmailNeedsPasswordConfirmation(t *testing.T) {
	stub := newFeishuStub(t)
	service, store, runner, _ := newFeishuService(t, stub, true, true)
	local := store.seedUser("synthetic@example.com", "synthetic-password")

	result, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Session != nil || !strings.HasPrefix(result.BindToken, "tpb_") {
		t.Fatalf("result = %+v, want a pending bind token", result)
	}
	if result.Email != "synthetic@example.com" {
		t.Fatalf("email = %q, want the normalized form", result.Email)
	}
	if len(store.sessions) != 0 || len(store.identities) != 0 {
		t.Fatal("an unconfirmed binding wrote a session or an identity")
	}
	key := feishuBindKey(result.BindToken)
	if _, ok := service.bindings.entries[key]; !ok {
		t.Fatal("the pending binding was not stored under its digest")
	}
	if _, ok := service.bindings.entries[result.BindToken]; ok {
		t.Fatal("the pending binding was stored in plaintext")
	}

	if _, err := service.FeishuBind(context.Background(), result.BindToken, "synthetic-wrong-password", "127.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password error = %v, want ErrInvalidCredentials", err)
	}
	if len(store.sessions) != 0 || len(store.identities) != 0 {
		t.Fatal("a wrong password wrote a session or an identity")
	}
	if _, ok := service.bindings.entries[key]; !ok {
		t.Fatal("a wrong password consumed the pending binding")
	}

	session, err := service.FeishuBind(context.Background(), result.BindToken, "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if session.User.ID != local.ID || !strings.HasPrefix(session.Token, sessionTokenPrefix) {
		t.Fatalf("session = %+v", session)
	}
	identity, ok := store.identities[local.ID]
	if !ok || identity.OpenID != stub.openID || identity.Email != "synthetic@example.com" {
		t.Fatalf("stored identity = %+v", identity)
	}
	if runner.inTxCall != 1 {
		t.Fatalf("bind transactions = %d, want 1", runner.inTxCall)
	}
	if _, ok := store.sessions[string(sessionTokenHash(session.Token))]; !ok {
		t.Fatal("store did not receive the session digest")
	}
	if _, ok := service.bindings.entries[key]; ok {
		t.Fatal("a completed binding kept the pending token")
	}
	if _, err := service.FeishuBind(context.Background(), result.BindToken, "synthetic-password", "127.0.0.1"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("second bind error = %v, want ErrUnauthenticated", err)
	}
}

// TestFeishuBindRefusesPasswordlessAccount proves a Feishu-only account cannot be
// bound through a password it has never set: the check costs one derivation and
// fails like a wrong password.
func TestFeishuBindRefusesPasswordlessAccount(t *testing.T) {
	stub := newFeishuStub(t)
	service, store, _, _ := newFeishuService(t, stub, true, true)
	store.seedFeishuUser("synthetic@example.com")

	result, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if result.BindToken == "" {
		t.Fatal("a password-less account did not enter the bind branch")
	}
	if _, err := service.FeishuBind(context.Background(), result.BindToken, "synthetic-password", "127.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("bind error = %v, want ErrInvalidCredentials", err)
	}
	if len(store.sessions) != 0 || len(store.identities) != 0 {
		t.Fatal("a password-less account gained a binding")
	}
}

func TestFeishuBindTokenExpires(t *testing.T) {
	stub := newFeishuStub(t)
	service, store, _, clock := newFeishuService(t, stub, true, true)
	store.seedUser("synthetic@example.com", "synthetic-password")
	result, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1")
	if err != nil || result.BindToken == "" {
		t.Fatalf("login result = %+v err = %v", result, err)
	}
	clock.Advance(FeishuBindLifetime + time.Second)
	if _, err := service.FeishuBind(context.Background(), result.BindToken, "synthetic-password", "127.0.0.1"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired bind error = %v, want ErrUnauthenticated", err)
	}
	if len(service.bindings.entries) != 0 {
		t.Fatal("the expired pending binding was not pruned")
	}
}

// TestFeishuBindConcurrentSingleUse proves the pending token is spent once: ten
// requests carrying the same token and the correct password must produce exactly
// one session, and every other request fails as unauthenticated.
func TestFeishuBindConcurrentSingleUse(t *testing.T) {
	stub := newFeishuStub(t)
	service, store, _, _ := newFeishuService(t, stub, true, true)
	store.seedUser("synthetic@example.com", "synthetic-password")
	result, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1")
	if err != nil || result.BindToken == "" {
		t.Fatalf("login result = %+v err = %v, want a pending bind token", result, err)
	}

	const attempts = 10
	start := make(chan struct{})
	sessions := make([]Session, attempts)
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			sessions[index], errs[index] = service.FeishuBind(context.Background(), result.BindToken, "synthetic-password", "127.0.0.1")
		}(i)
	}
	close(start)
	wg.Wait()

	var succeeded int
	for index, err := range errs {
		if err == nil {
			succeeded++
			if sessions[index].Token == "" {
				t.Fatalf("attempt %d succeeded without a session token", index)
			}
			continue
		}
		if !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("attempt %d error = %v, want ErrUnauthenticated", index, err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("successful binds = %d, want exactly 1", succeeded)
	}
	if len(store.sessions) != 1 {
		t.Fatalf("stored sessions = %d, want exactly 1", len(store.sessions))
	}
	if len(store.identities) != 1 {
		t.Fatalf("stored identities = %d, want exactly 1", len(store.identities))
	}
	if _, ok := service.bindings.entries[feishuBindKey(result.BindToken)]; ok {
		t.Fatal("a completed bind kept the pending token")
	}
}

// gatedStore blocks the first credential read until the test allows it, so a test
// can hold one bind request inside its checks while another one arrives.
type gatedStore struct {
	*fakeStore
	gateMu  sync.Mutex
	calls   int
	entered chan struct{}
	allow   chan struct{}
}

func newGatedStore(store *fakeStore) *gatedStore {
	return &gatedStore{fakeStore: store, entered: make(chan struct{}), allow: make(chan struct{})}
}

// UserCredential reports the first read on entered and waits for allow, then reads
// through to the in-memory store.
func (g *gatedStore) UserCredential(ctx context.Context, email string) (Credential, bool, error) {
	g.gateMu.Lock()
	g.calls++
	first := g.calls == 1
	g.gateMu.Unlock()
	if first {
		close(g.entered)
		<-g.allow
	}
	return g.fakeStore.UserCredential(ctx, email)
}

// readCalls reports how many credential reads started.
func (g *gatedStore) readCalls() int {
	g.gateMu.Lock()
	defer g.gateMu.Unlock()
	return g.calls
}

// failAfterCommitRunner reports an error for a transaction that already ran, the
// shape a lost connection gives a commit that did land: the writes exist but the
// caller cannot tell them apart from a rollback.
type failAfterCommitRunner struct {
	*fakeTxRunner
	err error
}

func (r failAfterCommitRunner) InTx(ctx context.Context, fn func(context.Context, RegistrationStore) error) error {
	if err := r.fakeTxRunner.InTx(ctx, fn); err != nil {
		return err
	}
	return r.err
}

// TestFeishuBindWaitsForConcurrentRequest proves a request carrying a token that
// another request is already spending waits for it instead of being refused: the
// first attempt fails a wrong password and releases the claim, and the waiting
// attempt then completes the binding with the correct one.
func TestFeishuBindWaitsForConcurrentRequest(t *testing.T) {
	stub := newFeishuStub(t)
	service, store, runner, _ := newFeishuService(t, stub, true, true)
	store.seedUser("synthetic@example.com", "synthetic-password")
	result, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1")
	if err != nil || result.BindToken == "" {
		t.Fatalf("login result = %+v err = %v, want a pending bind token", result, err)
	}
	gate := newGatedStore(store)
	service.store = gate

	firstErr := make(chan error, 1)
	go func() {
		_, err := service.FeishuBind(context.Background(), result.BindToken, "synthetic-wrong-password", "127.0.0.1")
		firstErr <- err
	}()
	<-gate.entered // the first request holds the claim and is inside the checks

	type outcome struct {
		session Session
		err     error
	}
	second := make(chan outcome, 1)
	go func() {
		session, err := service.FeishuBind(context.Background(), result.BindToken, "synthetic-password", "127.0.0.1")
		second <- outcome{session: session, err: err}
	}()

	// The waiting request must not return while the holder is still running, and it
	// must not read the credential either: it is blocked on the claim.
	select {
	case got := <-second:
		t.Fatalf("a concurrent bind returned before the holder finished: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	if calls := gate.readCalls(); calls != 1 {
		t.Fatalf("credential reads = %d while the claim was held, want 1", calls)
	}

	close(gate.allow)
	if err := <-firstErr; !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("first bind error = %v, want ErrInvalidCredentials", err)
	}
	got := <-second
	if got.err != nil {
		t.Fatalf("waiting bind error = %v, want success once the claim was released", got.err)
	}
	if !strings.HasPrefix(got.session.Token, sessionTokenPrefix) {
		t.Fatalf("waiting bind session = %+v", got.session)
	}
	if runner.inTxCall != 1 {
		t.Fatalf("bind transactions = %d, want 1", runner.inTxCall)
	}
	if len(store.sessions) != 1 {
		t.Fatalf("stored sessions = %d, want exactly 1", len(store.sessions))
	}
	if _, ok := service.bindings.entries[feishuBindKey(result.BindToken)]; ok {
		t.Fatal("a completed bind kept the pending token")
	}
}

// TestFeishuBindSpendsTokenWhenCommitOutcomeIsUnknown proves the pending token is
// destroyed once the transaction ran, even when it reports an error: a commit that
// did land must not be retried into a second session.
func TestFeishuBindSpendsTokenWhenCommitOutcomeIsUnknown(t *testing.T) {
	stub := newFeishuStub(t)
	service, store, runner, _ := newFeishuService(t, stub, true, true)
	store.seedUser("synthetic@example.com", "synthetic-password")
	result, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1")
	if err != nil || result.BindToken == "" {
		t.Fatalf("login result = %+v err = %v, want a pending bind token", result, err)
	}
	service.tx = failAfterCommitRunner{fakeTxRunner: runner, err: ErrUnavailable}

	if _, err := service.FeishuBind(context.Background(), result.BindToken, "synthetic-password", "127.0.0.1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("bind error = %v, want the reported storage failure", err)
	}
	// The transaction ran, so its write exists even though the caller saw a failure.
	if len(store.sessions) != 1 {
		t.Fatalf("stored sessions = %d, want the committed session", len(store.sessions))
	}
	if _, ok := service.bindings.entries[feishuBindKey(result.BindToken)]; ok {
		t.Fatal("a transaction that reported a failure kept the pending token")
	}

	if _, err := service.FeishuBind(context.Background(), result.BindToken, "synthetic-password", "127.0.0.1"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("retry error = %v, want ErrUnauthenticated", err)
	}
	if runner.inTxCall != 1 {
		t.Fatalf("bind transactions = %d, want the token to be spent after one", runner.inTxCall)
	}
	if len(store.sessions) != 1 {
		t.Fatalf("stored sessions after the retry = %d, want exactly 1", len(store.sessions))
	}
}

// TestFeishuBindRefusesSecondOpenIDForBoundAccount proves an account that already
// holds a Feishu open_id cannot be rebound to a different one: the bind fails as
// unauthenticated, keeps the original binding, spends the pending token and issues
// no session.
func TestFeishuBindRefusesSecondOpenIDForBoundAccount(t *testing.T) {
	stub := newFeishuStub(t)
	stub.openID = "ou_other"
	service, store, _, _ := newFeishuService(t, stub, true, true)
	local := store.seedUser("synthetic@example.com", "synthetic-password")
	store.seedFeishuBinding("ou_existing", local)

	result, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1")
	if err != nil || result.BindToken == "" {
		t.Fatalf("login result = %+v err = %v, want a pending bind token", result, err)
	}
	session, err := service.FeishuBind(context.Background(), result.BindToken, "synthetic-password", "127.0.0.1")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("bind error = %v, want ErrUnauthenticated", err)
	}
	if session.Token != "" || len(store.sessions) != 0 {
		t.Fatal("a conflicting rebind issued a session")
	}
	if identity := store.identities[local.ID]; identity.OpenID != "ou_existing" {
		t.Fatalf("stored identity = %+v, want the original binding kept", identity)
	}
	if _, ok := service.bindings.entries[feishuBindKey(result.BindToken)]; ok {
		t.Fatal("a conflicting rebind kept the pending token")
	}
}

func TestFeishuLoginAutoProvisionsWhenRegistrationIsClosed(t *testing.T) {
	stub := newFeishuStub(t)
	service, store, runner, _ := newFeishuService(t, stub, false, true)
	store.seedUser("existing@example.com", "synthetic-password")

	result, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil || result.Session.User.Email != "synthetic@example.com" {
		t.Fatalf("result = %+v, want a new session", result)
	}
	credential, ok := store.users["synthetic@example.com"]
	if !ok || credential.PasswordHash != "" {
		t.Fatalf("created account = %+v, want no local password", credential)
	}
	if identity := store.identities[result.Session.User.ID]; identity.OpenID != stub.openID {
		t.Fatalf("stored identity = %+v", identity)
	}
	// The auto-provisioning path still takes the registration lock, so a
	// concurrent ordinary registration cannot also pass the empty-database check,
	// but it stores no empty-database query of its own.
	want := []string{"lock", "feishu_user", "identity", "session"}
	if len(store.registrations) != len(want) {
		t.Fatalf("registration steps = %v, want %v", store.registrations, want)
	}
	for i, step := range want {
		if store.registrations[i] != step {
			t.Fatalf("registration steps = %v, want %v", store.registrations, want)
		}
	}
	if runner.inTxCall != 1 {
		t.Fatalf("transactions = %d, want 1", runner.inTxCall)
	}
	if store.lockCalls != 1 || store.hasUsersCalls != 0 {
		t.Fatalf("auto-provisioning skipped the lock: lock=%d hasUsers=%d", store.lockCalls, store.hasUsersCalls)
	}
}

// TestFeishuLoginProvisioningHoldsTheRegistrationLock proves the auto-provisioning
// path joins the same registration lock as ordinary registration: once it commits
// the first account, an ordinary registration must see a non-empty database rather
// than being accepted as a second first user.
func TestFeishuLoginProvisioningHoldsTheRegistrationLock(t *testing.T) {
	stub := newFeishuStub(t)
	service, store, _, _ := newFeishuService(t, stub, false, true)

	if _, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if store.lockCalls != 1 || store.hasUsersCalls != 0 {
		t.Fatalf("lock=%d hasUsers=%d, want the lock without the empty-database query", store.lockCalls, store.hasUsersCalls)
	}

	_, err := service.Register(context.Background(), "other@example.com", "synthetic-password", "127.0.0.2")
	if !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("register error = %v, want ErrRegistrationClosed", err)
	}
	if _, ok := store.users["other@example.com"]; ok {
		t.Fatal("a registration refused after provisioning created an account")
	}
	if len(store.sessions) != 1 {
		t.Fatalf("sessions = %d, want only the Feishu session", len(store.sessions))
	}
}

func TestFeishuLoginClosedRefusesWhenProvisioningIsOff(t *testing.T) {
	stub := newFeishuStub(t)
	service, store, _, _ := newFeishuService(t, stub, false, false)
	store.seedUser("existing@example.com", "synthetic-password")

	_, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1")
	if !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("error = %v, want ErrRegistrationClosed", err)
	}
	if _, ok := store.users["synthetic@example.com"]; ok {
		t.Fatal("a refused callback created an account")
	}
	if len(store.identities) != 0 || len(store.sessions) != 0 {
		t.Fatal("a refused callback wrote an identity or a session")
	}
	if len(store.registrations) != 2 || store.registrations[0] != "lock" || store.registrations[1] != "has_users" {
		t.Fatalf("registration steps = %v", store.registrations)
	}
}

func TestFeishuLoginAllowsFirstUserOfEmptyDatabase(t *testing.T) {
	stub := newFeishuStub(t)
	service, store, _, _ := newFeishuService(t, stub, false, false)

	if _, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.users["synthetic@example.com"]; !ok {
		t.Fatal("the first Feishu user of an empty database was not created")
	}
}

func TestFeishuLoginRequiresEmail(t *testing.T) {
	stub := newFeishuStub(t)
	stub.omitEmail = true
	service, store, _, _ := newFeishuService(t, stub, true, true)

	if _, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1"); !errors.Is(err, ErrFeishuEmailRequired) {
		t.Fatalf("error = %v, want ErrFeishuEmailRequired", err)
	}
	if len(store.users) != 0 || len(store.identities) != 0 {
		t.Fatal("a profile without an email wrote an account")
	}
}

func TestFeishuLoginReportsProviderFailureWithoutLeaking(t *testing.T) {
	stub := newFeishuStub(t)
	stub.failToken = true
	service, store, _, _ := newFeishuService(t, stub, true, true)

	_, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1")
	if !errors.Is(err, ErrFeishuAuthFailed) {
		t.Fatalf("error = %v, want ErrFeishuAuthFailed", err)
	}
	if strings.Contains(err.Error(), "synthetic-code") || strings.Contains(err.Error(), "synthetic provider failure") {
		t.Fatalf("error %q repeats a request value or provider text", err)
	}
	if len(store.users) != 0 || len(store.sessions) != 0 {
		t.Fatal("a failed exchange wrote an account or a session")
	}
}

func TestFeishuLoginRefusedWhenNotConfigured(t *testing.T) {
	stub := newFeishuStub(t)
	service := New(newFakeStore(), &fakeTxRunner{store: newFakeStore()}, NewLimiter(nil), true)
	service.EnableFeishu(FeishuConfig{})

	if _, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1"); !errors.Is(err, ErrFeishuAuthFailed) {
		t.Fatalf("error = %v, want ErrFeishuAuthFailed", err)
	}
	if calls := stub.tokenCalls.Load() + stub.appTokenCalls.Load() + stub.userInfoCalls.Load(); calls != 0 {
		t.Fatalf("a disabled instance contacted the provider %d times", calls)
	}
}

func TestFeishuLoginRateLimitsByRemoteAddress(t *testing.T) {
	stub := newFeishuStub(t)
	service, store, _, _ := newFeishuService(t, stub, true, true)
	user := store.seedUser("synthetic@example.com", "synthetic-password")
	store.seedFeishuBinding(stub.openID, user)

	for range limiterEmailLimit {
		if _, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := service.FeishuLogin(context.Background(), "synthetic-code", "127.0.0.1")
	var limited RateLimitedError
	if !errors.As(err, &limited) || limited.RetryAfter <= 0 {
		t.Fatalf("error = %v, want RateLimitedError with a retry window", err)
	}
}
