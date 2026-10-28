package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// fakeStore is an in-memory store. It mirrors only the SQL semantics the service
// relies on and records what the service wrote, so the service tests can assert
// on transaction boundaries and hashed values without a database.
type fakeStore struct {
	mu sync.Mutex
	// users maps normalized email to its stored credential.
	users map[string]Credential
	// sessions maps token digest to the session owner and its expiry.
	sessions map[string]fakeSession
	// personal maps a created user to the display name of its personal
	// workspace, so the tests can prove each account got exactly one.
	personal map[ID]string
	// owners marks the users that received the owner membership of their
	// personal workspace.
	owners map[ID]bool
	// tokens records every token digest handed to the store, in call order.
	tokens []string
	// hashes records every password encoding handed to registration.
	hashes []string
	// registrations records the registration steps in call order.
	registrations []string
	// nextID is the identity assigned to the next created account.
	nextID ID

	errCredential     error
	errCreate         error
	errCreatePersonal error
	errHasUsers       error
	errLock           error
	errCreateSess     error
	errAuthenticate   error
	errRevoke         error
	createCalls       int
	personalCalls     int
	hasUsersCalls     int
	lockCalls         int
	revokeCalls       int
}

type fakeSession struct {
	userID    ID
	expiresAt time.Time
	revoked   bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:    make(map[string]Credential),
		sessions: make(map[string]fakeSession),
		personal: make(map[ID]string),
		owners:   make(map[ID]bool),
	}
}

// seedUser inserts an account directly, so login tests need no registration.
func (f *fakeStore) seedUser(email, password string) User {
	f.mu.Lock()
	defer f.mu.Unlock()
	hash, err := HashPassword(password)
	if err != nil {
		panic(err)
	}
	user := User{ID: f.identifier(), Email: email}
	f.users[email] = Credential{User: user, PasswordHash: hash}
	return user
}

// identifier produces a deterministic distinct uuid for each fake account.
func (f *fakeStore) identifier() ID {
	var id ID
	f.nextID[15]++
	if f.nextID[15] == 0 {
		f.nextID[14]++
	}
	id = f.nextID
	return id
}

func (f *fakeStore) UserCredential(ctx context.Context, email string) (Credential, bool, error) {
	if f.errCredential != nil {
		return Credential{}, false, f.errCredential
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	credential, ok := f.users[email]
	return credential, ok, nil
}

// recordRegistration appends one registration step to the call order.
func (f *fakeStore) recordRegistration(step string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registrations = append(f.registrations, step)
}

// LockRegistration records the lock step, which isolates the first-account
// decision of a closed deployment.
func (f *fakeStore) LockRegistration(ctx context.Context) error {
	f.lockCalls++
	f.recordRegistration("lock")
	return f.errLock
}

// HasUsers reports whether an account already exists.
func (f *fakeStore) HasUsers(ctx context.Context) (bool, error) {
	f.hasUsersCalls++
	f.recordRegistration("has_users")
	if f.errHasUsers != nil {
		return false, f.errHasUsers
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.users) > 0, nil
}

// CreateUser inserts one account and keeps the email unique.
func (f *fakeStore) CreateUser(ctx context.Context, email, passwordHash string) (User, error) {
	f.createCalls++
	f.recordRegistration("user")
	if f.errCreate != nil {
		return User{}, f.errCreate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.users[email]; exists {
		return User{}, ErrEmailUnavailable
	}
	user := User{ID: f.identifier(), Email: email}
	f.users[email] = Credential{User: user, PasswordHash: passwordHash}
	f.hashes = append(f.hashes, passwordHash)
	return user, nil
}

// CreatePersonal records the personal workspace and owner membership of a user.
func (f *fakeStore) CreatePersonal(ctx context.Context, userID ID) error {
	f.personalCalls++
	f.recordRegistration("personal")
	if f.errCreatePersonal != nil {
		return f.errCreatePersonal
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.personal[userID]; exists {
		return nil
	}
	f.personal[userID] = "Personal"
	f.owners[userID] = true
	return nil
}

func (f *fakeStore) CreateSession(ctx context.Context, userID ID, tokenHash []byte) (time.Time, error) {
	f.recordRegistration("session")
	if f.errCreateSess != nil {
		return time.Time{}, f.errCreateSess
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	expiresAt := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	f.tokens = append(f.tokens, string(tokenHash))
	f.sessions[string(tokenHash)] = fakeSession{userID: userID, expiresAt: expiresAt}
	return expiresAt, nil
}

func (f *fakeStore) AuthenticateSession(ctx context.Context, tokenHash []byte) (IssuedSession, bool, error) {
	if f.errAuthenticate != nil {
		return IssuedSession{}, false, f.errAuthenticate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	session, ok := f.sessions[string(tokenHash)]
	if !ok || session.revoked {
		return IssuedSession{}, false, nil
	}
	session.expiresAt = session.expiresAt.Add(SessionLifetime)
	f.sessions[string(tokenHash)] = session
	for _, credential := range f.users {
		if credential.User.ID == session.userID {
			return IssuedSession{User: credential.User, ExpiresAt: session.expiresAt}, true, nil
		}
	}
	return IssuedSession{}, false, nil
}

func (f *fakeStore) RevokeSession(ctx context.Context, tokenHash []byte) error {
	f.revokeCalls++
	if f.errRevoke != nil {
		return f.errRevoke
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

// fakeTxRunner records whether registration actually used a transaction. rollback,
// when set, discards the writes of the function, so a test can prove the service
// reports the failure instead of a session.
type fakeTxRunner struct {
	store    *fakeStore
	inTxCall int
}

func (r *fakeTxRunner) InTx(ctx context.Context, fn func(context.Context, RegistrationStore) error) error {
	r.inTxCall++
	return fn(ctx, r.store)
}

func newTestService(t *testing.T) (*Service, *fakeStore, *fakeTxRunner, *fakeClock) {
	t.Helper()
	return newTestServiceWithRegistration(t, true)
}

// newTestServiceWithRegistration returns a service with the given startup
// registration setting, so every test states the deployment mode it covers.
func newTestServiceWithRegistration(t *testing.T, registrationOpen bool) (*Service, *fakeStore, *fakeTxRunner, *fakeClock) {
	t.Helper()
	store := newFakeStore()
	runner := &fakeTxRunner{store: store}
	clock := newFakeClock()
	return New(store, runner, NewLimiter(clock.Now), registrationOpen), store, runner, clock
}

func TestRegisterCreatesUserPersonalWorkspaceAndSessionInOneTransaction(t *testing.T) {
	service, store, runner, _ := newTestService(t)
	session, err := service.Register(context.Background(), "  Synthetic@Example.COM  ", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if runner.inTxCall != 1 {
		t.Fatalf("InTx calls = %d, want 1", runner.inTxCall)
	}
	if session.User.Email != "synthetic@example.com" {
		t.Fatalf("stored email = %q, want the normalized form", session.User.Email)
	}
	if session.ExpiresAt.IsZero() {
		t.Fatal("session expiry missing")
	}
	if len(session.Token) <= len(sessionTokenPrefix) || session.Token[:len(sessionTokenPrefix)] != sessionTokenPrefix {
		t.Fatalf("token %q lacks the tps_ prefix", session.Token)
	}
	if _, exists := store.users["synthetic@example.com"]; !exists {
		t.Fatal("account was not stored under the normalized email")
	}
	if name, exists := store.personal[session.User.ID]; !exists || name != "Personal" {
		t.Fatalf("personal workspace = %q exists=%t, want one named Personal", name, exists)
	}
	if !store.owners[session.User.ID] {
		t.Fatal("the first user did not receive the owner membership")
	}
	sum := sha256.Sum256([]byte(session.Token))
	if _, exists := store.sessions[string(sum[:])]; !exists {
		t.Fatal("store did not receive the SHA-256 digest of the token")
	}
	if _, exists := store.sessions[session.Token]; exists {
		t.Fatal("store received the plaintext token")
	}
}

// TestRegisterOpenSkipsTheClosedPath pins the open deployment: no registration
// lock, no users existence query, and every account still gets its own personal
// workspace and owner membership.
func TestRegisterOpenSkipsTheClosedPath(t *testing.T) {
	service, store, _, _ := newTestServiceWithRegistration(t, true)
	first, err := service.Register(context.Background(), "first@example.com", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Register(context.Background(), "second@example.com", "synthetic-password", "127.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	if store.lockCalls != 0 || store.hasUsersCalls != 0 {
		t.Fatalf("open registration used the closed path: lock=%d hasUsers=%d", store.lockCalls, store.hasUsersCalls)
	}
	for _, session := range []Session{first, second} {
		if _, exists := store.personal[session.User.ID]; !exists {
			t.Fatalf("user %s has no personal workspace", session.User.Email)
		}
		if !store.owners[session.User.ID] {
			t.Fatalf("user %s has no owner membership", session.User.Email)
		}
	}
}

// TestRegisterClosedUsesTheDocumentedOrder proves the closed deployment runs the
// steps in the documented order: lock, empty-database check, user, personal
// workspace, session.
func TestRegisterClosedUsesTheDocumentedOrder(t *testing.T) {
	service, store, _, _ := newTestServiceWithRegistration(t, false)
	if _, err := service.Register(context.Background(), "first@example.com", "synthetic-password", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"lock", "has_users", "user", "personal", "session"}
	if len(store.registrations) != len(want) {
		t.Fatalf("registration steps = %v, want %v", store.registrations, want)
	}
	for i, step := range want {
		if store.registrations[i] != step {
			t.Fatalf("registration steps = %v, want %v", store.registrations, want)
		}
	}
}

// TestRegisterClosedRejectsAnyEmailOnceAUserExists proves a closed deployment with
// an existing user rejects an occupied and a free email identically, without
// reaching any write.
func TestRegisterClosedRejectsAnyEmailOnceAUserExists(t *testing.T) {
	service, store, _, _ := newTestServiceWithRegistration(t, false)
	if _, err := service.Register(context.Background(), "first@example.com", "synthetic-password", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	store.createCalls, store.personalCalls = 0, 0
	store.registrations = nil
	for _, email := range []string{"first@example.com", "second@example.com"} {
		_, err := service.Register(context.Background(), email, "synthetic-password", "127.0.0.2")
		if !errors.Is(err, ErrRegistrationClosed) {
			t.Fatalf("%s returned %v, want ErrRegistrationClosed", email, err)
		}
		if errors.Is(err, ErrEmailUnavailable) {
			t.Fatalf("%s leaked the email unique conflict", email)
		}
	}
	if store.createCalls != 0 || store.personalCalls != 0 {
		t.Fatalf("a rejected registration wrote: users=%d personal=%d", store.createCalls, store.personalCalls)
	}
	for _, step := range store.registrations {
		if step != "lock" && step != "has_users" {
			t.Fatalf("a rejected registration ran %q", step)
		}
	}
	if len(store.users) != 1 || len(store.sessions) != 1 {
		t.Fatalf("users=%d sessions=%d, want the single committed account", len(store.users), len(store.sessions))
	}
}

// TestRegisterPropagatesEveryStageFailure proves a database failure is reported
// as it happened and never rewritten as "closed" or "email taken".
func TestRegisterPropagatesEveryStageFailure(t *testing.T) {
	for name, tt := range map[string]struct {
		registrationOpen bool
		prepare          func(*fakeStore)
		want             string
	}{
		"registration lock": {false, func(s *fakeStore) { s.errLock = errors.New("synthetic lock failure") }, "lock"},
		"users check":       {false, func(s *fakeStore) { s.errHasUsers = errors.New("synthetic check failure") }, "has_users"},
		"user insert":       {true, func(s *fakeStore) { s.errCreate = errors.New("synthetic user failure") }, "user"},
		"personal workspace": {true, func(s *fakeStore) {
			s.errCreatePersonal = errors.New("synthetic workspace failure")
		}, "personal"},
		"session insert": {true, func(s *fakeStore) { s.errCreateSess = errors.New("synthetic session failure") }, "session"},
	} {
		t.Run(name, func(t *testing.T) {
			service, store, runner, _ := newTestServiceWithRegistration(t, tt.registrationOpen)
			tt.prepare(store)
			_, err := service.Register(context.Background(), "synthetic@example.com", "synthetic-password", "127.0.0.1")
			if err == nil {
				t.Fatal("registration succeeded despite the injected failure")
			}
			if errors.Is(err, ErrRegistrationClosed) || errors.Is(err, ErrEmailUnavailable) {
				t.Fatalf("a database failure was reported as %v", err)
			}
			if runner.inTxCall != 1 {
				t.Fatalf("InTx calls = %d, want 1", runner.inTxCall)
			}
			last := store.registrations[len(store.registrations)-1]
			if last != tt.want {
				t.Fatalf("last step = %q, want %q", last, tt.want)
			}
		})
	}
}

// TestRegisterRejectedAttemptStillConsumesTheRateLimit proves a legal attempt
// refused by the closed deployment still uses one slot, and an exhausted window
// returns the rate-limit error without reaching the registration transaction.
func TestRegisterRejectedAttemptStillConsumesTheRateLimit(t *testing.T) {
	service, store, runner, _ := newTestServiceWithRegistration(t, false)
	if _, err := service.Register(context.Background(), "first@example.com", "synthetic-password", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	// One slot was already used by the successful first registration.
	for i := range limiterEmailLimit {
		if _, err := service.Register(context.Background(), "second@example.com", "synthetic-password", "127.0.0.3"); !errors.Is(err, ErrRegistrationClosed) {
			t.Fatalf("attempt %d returned %v", i+1, err)
		}
	}
	before := store.hasUsersCalls
	_, err := service.Register(context.Background(), "second@example.com", "synthetic-password", "127.0.0.3")
	var limited RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want RateLimitedError", err)
	}
	if store.hasUsersCalls != before {
		t.Fatal("a rate-limited attempt reached the registration transaction")
	}
	if runner.inTxCall != limiterEmailLimit+1 {
		t.Fatalf("InTx calls = %d, want %d", runner.inTxCall, limiterEmailLimit+1)
	}
	// Invalid input is still refused before the limiter, so it never consumes a
	// slot and never opens a transaction.
	calls := runner.inTxCall
	if _, err := service.Register(context.Background(), "synthetic.example.com", "synthetic-password", "127.0.0.4"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid input returned %v", err)
	}
	if runner.inTxCall != calls {
		t.Fatal("invalid input reached the registration transaction")
	}
}

func TestRegisterRejectsInvalidInputBeforeHashing(t *testing.T) {
	service, store, _, _ := newTestService(t)
	for name, tt := range map[string]struct{ email, password string }{
		"missing at":       {"synthetic.example.com", "synthetic-password"},
		"two at":           {"a@@example.com", "synthetic-password"},
		"leading at":       {"@example.com", "synthetic-password"},
		"trailing at":      {"synthetic@", "synthetic-password"},
		"inner space":      {"syn thetic@example.com", "synthetic-password"},
		"control char":     {"synthetic\x00@example.com", "synthetic-password"},
		"short password":   {"synthetic@example.com", "short"},
		"long password":    {"synthetic@example.com", "01234567890123456789012345678901234567890123456789012345678901234"},
		"too many bytes":   {"synthetic@example.com", repeatRune('ä', 130)},
		"invalid utf8":     {"synthetic@example.com", string([]byte{0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa, 0xf9, 0xf8})},
		"too long address": {repeatRune('a', 250) + "@example.com", "synthetic-password"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := service.Register(context.Background(), tt.email, tt.password, "127.0.0.1")
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want ErrInvalidArgument", err)
			}
		})
	}
	if store.createCalls != 0 || len(store.hashes) != 0 {
		t.Fatal("invalid input reached hashing or storage")
	}
}

func TestPasswordIsNotTrimmed(t *testing.T) {
	service, _, _, _ := newTestService(t)
	const password = "  synthetic-password  "
	session, err := service.Register(context.Background(), "synthetic@example.com", password, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := VerifyPassword(service.store.(*fakeStore).users[session.User.Email].PasswordHash, password)
	if err != nil || !ok {
		t.Fatal("stored encoding does not match the untrimmed password")
	}
	if _, err := service.Login(context.Background(), "synthetic@example.com", "synthetic-password", "127.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("trimmed password accepted: %v", err)
	}
}

func TestRegisterCountsRateLimitEvenOnSuccess(t *testing.T) {
	service, _, _, _ := newTestService(t)
	for i := range limiterEmailLimit {
		if _, err := service.Register(context.Background(), "synthetic@example.com", "synthetic-password", "127.0.0.1"); err != nil && !errors.Is(err, ErrEmailUnavailable) {
			t.Fatalf("attempt %d failed: %v", i+1, err)
		}
	}
	_, err := service.Register(context.Background(), "synthetic@example.com", "synthetic-password", "127.0.0.1")
	var limited RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want RateLimitedError", err)
	}
	if limited.RetryAfter <= 0 {
		t.Fatal("RetryAfter was not reported")
	}
}

func TestLoginSucceedsAndIssuesFreshSession(t *testing.T) {
	service, store, _, _ := newTestService(t)
	store.seedUser("synthetic@example.com", "synthetic-password")
	first, err := service.Login(context.Background(), "Synthetic@Example.com", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Login(context.Background(), "synthetic@example.com", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token {
		t.Fatal("login reused a token instead of issuing a new one")
	}
	if first.User.Email != "synthetic@example.com" || first.ExpiresAt.IsZero() {
		t.Fatal("login returned an incomplete session")
	}
}

func TestLoginHidesWhetherAccountExists(t *testing.T) {
	service, store, _, _ := newTestService(t)
	store.seedUser("synthetic@example.com", "synthetic-password")
	if _, err := service.Login(context.Background(), "unknown@example.com", "synthetic-password", "127.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown email returned %v", err)
	}
	if _, err := service.Login(context.Background(), "synthetic@example.com", "synthetic-wrong-password", "127.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password returned %v", err)
	}
}

func TestLoginStoreFailureIsNotACredentialError(t *testing.T) {
	service, store, _, _ := newTestService(t)
	store.errCredential = errors.New("synthetic database failure")
	_, err := service.Login(context.Background(), "synthetic@example.com", "synthetic-password", "127.0.0.1")
	if err == nil || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("store failure was mapped to a credential error: %v", err)
	}
	if errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrEmailUnavailable) {
		t.Fatalf("store failure leaked as a business identity error: %v", err)
	}
}

func TestLoginRejectsMalformedStoredHash(t *testing.T) {
	service, store, _, _ := newTestService(t)
	user := store.seedUser("synthetic@example.com", "synthetic-password")
	store.users["synthetic@example.com"] = Credential{User: user, PasswordHash: "not-an-argon2-encoding"}
	_, err := service.Login(context.Background(), "synthetic@example.com", "synthetic-password", "127.0.0.1")
	if !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("err = %v, want ErrInvalidHash", err)
	}
}

func TestLoginRateLimitCountsFailures(t *testing.T) {
	service, store, _, clock := newTestService(t)
	store.seedUser("synthetic@example.com", "synthetic-password")
	for range limiterEmailLimit {
		if _, err := service.Login(context.Background(), "synthetic@example.com", "synthetic-wrong-password", "127.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("unexpected failure: %v", err)
		}
	}
	_, err := service.Login(context.Background(), "synthetic@example.com", "synthetic-password", "127.0.0.1")
	var limited RateLimitedError
	if !errors.As(err, &limited) || limited.RetryAfter != limiterWindow {
		t.Fatalf("err = %v, want a rate limit with the full window", err)
	}
	clock.Advance(limiterWindow)
	if _, err := service.Login(context.Background(), "synthetic@example.com", "synthetic-password", "127.0.0.1"); err != nil {
		t.Fatalf("window did not reset: %v", err)
	}
}

func TestSessionRenewsAndReportsUnusableTokens(t *testing.T) {
	service, store, _, _ := newTestService(t)
	store.seedUser("synthetic@example.com", "synthetic-password")
	session, err := service.Login(context.Background(), "synthetic@example.com", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := service.Session(context.Background(), session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !renewed.ExpiresAt.After(session.ExpiresAt) {
		t.Fatal("session was not renewed on a valid request")
	}
	if renewed.User.ID != session.User.ID || renewed.Token != session.Token {
		t.Fatal("renewal changed the identity or the token")
	}
	for name, token := range map[string]string{
		"empty":           "",
		"plugin token":    "tpk_" + "synthetic",
		"no prefix":       "synthetic-token",
		"unknown session": "tps_unknown",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.Session(context.Background(), token); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestSessionStoreFailureIsNotUnauthenticated(t *testing.T) {
	service, store, _, _ := newTestService(t)
	store.errAuthenticate = errors.New("synthetic database failure")
	_, err := service.Session(context.Background(), "tps_synthetic")
	if err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("database failure was reported as unauthenticated: %v", err)
	}
}

func TestLogoutRevokesAndStaysIdempotent(t *testing.T) {
	service, store, _, _ := newTestService(t)
	session, err := service.Register(context.Background(), "synthetic@example.com", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Logout(context.Background(), session.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Session(context.Background(), session.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked session still authenticated: %v", err)
	}
	if err := service.Logout(context.Background(), session.Token); err != nil {
		t.Fatalf("repeated logout failed: %v", err)
	}
	if err := service.Logout(context.Background(), "tps_unknown"); err != nil {
		t.Fatalf("logout of an unknown token failed: %v", err)
	}
	if err := service.Logout(context.Background(), "synthetic-malformed"); err != nil {
		t.Fatalf("logout of a malformed token failed: %v", err)
	}
	if store.revokeCalls != 3 {
		t.Fatalf("revoke calls = %d, want 3 (the malformed token must not reach SQL)", store.revokeCalls)
	}
}

func TestLogoutReportsStoreFailure(t *testing.T) {
	service, store, _, _ := newTestService(t)
	store.errRevoke = errors.New("synthetic database failure")
	if err := service.Logout(context.Background(), "tps_synthetic"); err == nil {
		t.Fatal("logout claimed success despite a store failure")
	}
}

// TestStoreErrorsDistinguishUnavailableFromInternal proves a dependency failure
// is reported as ErrUnavailable for the documented 503, while a statement fault
// or a cancelled request stays an internal error and never looks like a
// credential or authentication failure.
func TestStoreErrorsDistinguishUnavailableFromInternal(t *testing.T) {
	for name, tt := range map[string]struct {
		err         error
		unavailable bool
	}{
		"connection exception":  {&pgconn.PgError{Code: "08006"}, true},
		"closed connection":     {pgconn.ErrConnClosed, true},
		"deadline exceeded":     {context.DeadlineExceeded, true},
		"plain statement fault": {errors.New("synthetic statement failure"), false},
		"unique violation":      {&pgconn.PgError{Code: "23505"}, false},
		"cancelled request":     {context.Canceled, false},
	} {
		t.Run(name, func(t *testing.T) {
			safe := safeStoreError("auth account lookup", tt.err)
			if got := errors.Is(safe, ErrUnavailable); got != tt.unavailable {
				t.Fatalf("ErrUnavailable = %t, want %t", got, tt.unavailable)
			}
			if errors.Is(safe, ErrInvalidCredentials) || errors.Is(safe, ErrUnauthenticated) {
				t.Fatalf("store failure leaked as a business identity error: %v", safe)
			}
			if tt.unavailable && safe.Error() == tt.err.Error() {
				t.Fatalf("store error was not sanitized: %v", safe)
			}
		})
	}
}

func repeatRune(r rune, count int) string {
	out := make([]rune, count)
	for i := range out {
		out[i] = r
	}
	return string(out)
}
