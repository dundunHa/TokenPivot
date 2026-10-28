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
	// tokens records every token digest handed to the store, in call order.
	tokens []string
	// hashes records every password encoding handed to registration.
	hashes []string
	// nextID is the identity assigned to the next created account.
	nextID ID

	errCredential   error
	errCreate       error
	errCreateSess   error
	errAuthenticate error
	errRevoke       error
	createCalls     int
	revokeCalls     int
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

func (f *fakeStore) CreateUserWithSession(ctx context.Context, email, passwordHash string, tokenHash []byte) (IssuedSession, error) {
	f.createCalls++
	if f.errCreate != nil {
		return IssuedSession{}, f.errCreate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.users[email]; exists {
		return IssuedSession{}, ErrEmailUnavailable
	}
	user := User{ID: f.identifier(), Email: email}
	f.users[email] = Credential{User: user, PasswordHash: passwordHash}
	f.hashes = append(f.hashes, passwordHash)
	expiresAt := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	f.tokens = append(f.tokens, string(tokenHash))
	f.sessions[string(tokenHash)] = fakeSession{userID: user.ID, expiresAt: expiresAt}
	return IssuedSession{User: user, ExpiresAt: expiresAt}, nil
}

func (f *fakeStore) CreateSession(ctx context.Context, userID ID, tokenHash []byte) (time.Time, error) {
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

// fakeTxRunner records whether registration actually used a transaction.
type fakeTxRunner struct {
	store    *fakeStore
	inTxCall int
}

func (r *fakeTxRunner) InTx(ctx context.Context, fn func(context.Context, Store) error) error {
	r.inTxCall++
	return fn(ctx, r.store)
}

func newTestService(t *testing.T) (*Service, *fakeStore, *fakeTxRunner, *fakeClock) {
	t.Helper()
	store := newFakeStore()
	runner := &fakeTxRunner{store: store}
	clock := newFakeClock()
	return New(store, runner, NewLimiter(clock.Now)), store, runner, clock
}

func TestRegisterCreatesUserAndSessionInOneTransaction(t *testing.T) {
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
	sum := sha256.Sum256([]byte(session.Token))
	if _, exists := store.sessions[string(sum[:])]; !exists {
		t.Fatal("store did not receive the SHA-256 digest of the token")
	}
	if _, exists := store.sessions[session.Token]; exists {
		t.Fatal("store received the plaintext token")
	}
}

func TestRegisterRollsBackUserWhenSessionInsertFails(t *testing.T) {
	service, store, runner, _ := newTestService(t)
	store.errCreate = errors.New("synthetic store failure")
	if _, err := service.Register(context.Background(), "synthetic@example.com", "synthetic-password", "127.0.0.1"); err == nil {
		t.Fatal("registration succeeded despite a store failure")
	}
	if runner.inTxCall != 1 {
		t.Fatalf("InTx calls = %d, want 1", runner.inTxCall)
	}
	if len(store.users) != 0 {
		t.Fatal("failed registration left an account behind")
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
