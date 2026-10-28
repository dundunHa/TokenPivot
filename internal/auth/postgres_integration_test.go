//go:build integration

package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dundunHa/TokenPivot/db/migrations"
	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// integrationSchema creates a fresh random schema in the dedicated test
// database, applies every released migration to it and returns connection URLs
// for the migration owner and the service role, both pinned to that schema.
// Missing or unsafe integration variables fail the test; nothing is skipped.
func integrationSchema(t *testing.T) (string, string) {
	t.Helper()
	values := []string{os.Getenv("TOKENPIVOT_TEST_DATABASE_URL"), os.Getenv("TOKENPIVOT_TEST_APP_DATABASE_URL")}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PG") {
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatal("unset PG failed")
			}
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PGPASSFILE", "/dev/null")
	t.Setenv("PGSERVICEFILE", "/dev/null")
	urls := make([]*url.URL, 2)
	for i, value := range values {
		if err := database.ValidateURL(value); err != nil {
			t.Fatal("explicit integration database variables required")
		}
		u, err := url.Parse(value)
		if err != nil {
			t.Fatal("invalid test URL")
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() || !strings.HasSuffix(strings.TrimPrefix(u.Path, "/"), "_test") {
			t.Fatal("test database must use a loopback IP and _test database suffix")
		}
		for key := range u.Query() {
			if key != "sslmode" && key != "search_path" {
				t.Fatal("test URL has unsupported query option")
			}
		}
		urls[i] = u
	}
	if urls[0].Host != urls[1].Host || urls[0].Path != urls[1].Path || urls[0].User.Username() == urls[1].User.Username() {
		t.Fatal("test roles must be distinct in the same database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner, err := pgx.Connect(ctx, values[0])
	if err != nil {
		t.Fatal(database.SafeError("test owner connect", err))
	}
	t.Cleanup(func() {
		if err := owner.Close(context.Background()); err != nil {
			t.Error("test owner close failed")
		}
	})
	var db, role string
	var super, createRole bool
	if err := owner.QueryRow(ctx, "SELECT current_database(),current_user,rolsuper,rolcreaterole FROM pg_roles WHERE rolname=current_user").Scan(&db, &role, &super, &createRole); err != nil {
		t.Fatal(database.SafeError("identity", err))
	}
	if db != strings.TrimPrefix(urls[0].Path, "/") || role != urls[0].User.Username() || super || createRole {
		t.Fatal("unsafe test migration role")
	}
	app, err := pgx.Connect(ctx, values[1])
	if err != nil {
		t.Fatal(database.SafeError("app connect", err))
	}
	var appRole string
	var unsafe bool
	err = app.QueryRow(ctx, "SELECT current_user, rolsuper OR rolcreaterole OR rolcreatedb OR has_database_privilege(current_user,current_database(),'CREATE') FROM pg_roles WHERE rolname=current_user").Scan(&appRole, &unsafe)
	if err != nil {
		_ = app.Close(ctx)
		t.Fatal(database.SafeError("service role identity", err))
	}
	if unsafe || appRole != urls[1].User.Username() {
		_ = app.Close(ctx)
		t.Fatal("unsafe test service role")
	}
	var appPrivileges bool
	err = app.QueryRow(ctx, "SELECT pg_has_role(current_user, 'tokenpivot_app', 'USAGE')").Scan(&appPrivileges)
	if err != nil {
		_ = app.Close(ctx)
		t.Fatal(database.SafeError("service role privileges", err))
	}
	if !appPrivileges {
		_ = app.Close(ctx)
		t.Fatal("test service role must have tokenpivot_app privileges")
	}
	sum := sha256.Sum256([]byte(t.Name()))
	schema := "t016_" + fmt.Sprintf("%x", sum[:6])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := owner.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		_ = app.Close(ctx)
		t.Fatal(database.SafeError("create owned schema", err))
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var actual string
		err := owner.QueryRow(cleanupCtx, "SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname=$1", schema).Scan(&actual)
		if err != nil || actual != role || !strings.HasPrefix(schema, "t016_") {
			t.Errorf("preserving schema %s: ownership check failed", schema)
			return
		}
		if _, err := owner.Exec(cleanupCtx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("preserving schema %s: cleanup failed", schema)
		}
	})
	if _, err := owner.Exec(ctx, "GRANT USAGE ON SCHEMA "+quoted+" TO "+pgx.Identifier{appRole}.Sanitize()); err != nil {
		_ = app.Close(ctx)
		t.Fatal(database.SafeError("grant usage", err))
	}
	if err := app.Close(context.Background()); err != nil {
		t.Error(database.SafeError("app close", err))
	}
	for i, u := range urls {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		values[i] = u.String()
	}
	applyMigrations(t, values[0])
	return values[0], values[1]
}

// applyMigrations runs every released migration inside the pinned schema.
func applyMigrations(t *testing.T, migrationURL string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(migrationURL)
	if err != nil {
		t.Fatal(database.SafeError("parse migration URL", err))
	}
	cfg.RuntimeParams["lock_timeout"] = "5s"
	cfg.RuntimeParams["statement_timeout"] = "15s"
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(database.SafeError("close migration db", err))
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.Files,
		goose.WithDisableGlobalRegistry(true),
		goose.WithSlog(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(database.SafeError("migration provider", err))
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(database.SafeError("apply migrations", err))
	}
}

// newPostgresAuth returns a service backed by a real pool pinned to the random
// schema.
func newPostgresAuth(t *testing.T) (*Service, *Postgres, *pgxpool.Pool) {
	t.Helper()
	service, store, pool, _ := newPostgresAuthWithURL(t)
	return service, store, pool
}

// newPostgresAuthWithURL additionally returns the service-role connection URL, so
// a race test can open independent connections and hold or observe a row lock
// without depending on how many pooled connections happen to be free.
func newPostgresAuthWithURL(t *testing.T) (*Service, *Postgres, *pgxpool.Pool, string) {
	t.Helper()
	_, appURL := integrationSchema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := NewPostgres(pool)
	return New(store, store, NewLimiter(time.Now)), store, pool, appURL
}

// openAppConn opens one independent service-role connection pinned to the test
// schema. A race test observes lock waits from it, so the observation connection
// is never the one that blocks.
func openAppConn(t *testing.T, ctx context.Context, appURL string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, appURL)
	if err != nil {
		t.Fatal(database.SafeError("open test connection", err))
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Error(database.SafeError("close test connection", err))
		}
	})
	return conn
}

// waitForLockedStatement waits until a backend other than observer is blocked on
// a row lock while running a statement matching pattern. It polls pg_stat_activity
// with a deadline, so the race order is asserted from observable server state
// instead of being assumed from a sleep.
func waitForLockedStatement(t *testing.T, ctx context.Context, observer *pgx.Conn, pattern string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		err := observer.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE $1 AND pid <> pg_backend_pid()", pattern).Scan(&waiting)
		if err != nil {
			t.Fatal(database.SafeError("read lock wait", err))
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no statement matching %q waited on a row lock", pattern)
		}
		select {
		case <-ctx.Done():
			t.Fatal(database.SafeError("wait for lock", ctx.Err()))
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Statement patterns identify the statement that is expected to wait. The renewal
// and the logout both mention revoked_at, so each pattern matches only the clause
// that starts that statement.
const (
	renewStatementPattern  = `%SET expires_at = now() + interval%`
	logoutStatementPattern = `%SET revoked_at = COALESCE%`
)

// sqlDigest returns the raw digest the service stores for a token.
func sqlDigest(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// loadSession reads one session row by digest.
func loadSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool, digest []byte) (expiresAt time.Time, revokedAt *time.Time, found bool) {
	t.Helper()
	var revoked *time.Time
	err := pool.QueryRow(ctx, "SELECT expires_at, revoked_at FROM sessions WHERE token_hash=$1", digest).Scan(&expiresAt, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil, false
	}
	if err != nil {
		t.Fatal(database.SafeError("load session", err))
	}
	return expiresAt, revoked, true
}

func TestRegisterAgainstPostgres(t *testing.T) {
	service, _, pool := newPostgresAuth(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := service.Register(ctx, "Postgres@Example.com", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var email, passwordHash string
	if err := pool.QueryRow(ctx, "SELECT email, password_hash FROM users WHERE id=$1", session.User.ID).Scan(&email, &passwordHash); err != nil {
		t.Fatal(database.SafeError("load user", err))
	}
	if email != "postgres@example.com" {
		t.Fatalf("stored email = %q, want the normalized form", email)
	}
	if !strings.HasPrefix(passwordHash, "$argon2id$") || strings.Contains(passwordHash, "synthetic-password") {
		t.Fatal("stored password column is not a bare Argon2id encoding")
	}
	var storedLength int
	var storedDigest []byte
	if err := pool.QueryRow(ctx, "SELECT octet_length(token_hash), token_hash FROM sessions WHERE user_id=$1", session.User.ID).Scan(&storedLength, &storedDigest); err != nil {
		t.Fatal(database.SafeError("load session digest", err))
	}
	if storedLength != 32 {
		t.Fatalf("stored digest length = %d, want 32", storedLength)
	}
	if !bytes.Equal(storedDigest, sqlDigest(session.Token)) {
		t.Fatal("stored digest is not the SHA-256 of the full token")
	}
	expiresAt, revokedAt, found := loadSession(t, ctx, pool, storedDigest)
	if !found || revokedAt != nil {
		t.Fatal("new session is missing or already revoked")
	}
	if !session.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("service expiry %s differs from the stored %s", session.ExpiresAt, expiresAt)
	}
	remaining := time.Until(expiresAt)
	if remaining < SessionLifetime-time.Minute || remaining > SessionLifetime+time.Minute {
		t.Fatalf("expiry is %s away, want about %s", remaining, SessionLifetime)
	}
}

// TestConcurrentRegistrationCreatesOneAccount proves uk_users_email really
// arbitrates a race: eight transactions insert the same normalized email at the
// same time, exactly one commits and the other seven fail with
// ErrEmailUnavailable.
//
// The service limiter allows only five attempts per email in ten minutes, so
// driving Service.Register here could never observe the index under load. This
// drives the transaction store directly, which is the layer the unique index
// protects; the encoding is derived once and every token digest is distinct, so
// the email is the only possible conflict.
func TestConcurrentRegistrationCreatesOneAccount(t *testing.T) {
	_, store, pool := newPostgresAuth(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	encoding, err := HashPassword("synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	const callers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	var conflicts, unexpected []error
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			digest := sha256.Sum256([]byte(fmt.Sprintf("tps_concurrent_registration_%d", i)))
			err := store.InTx(ctx, func(ctx context.Context, tx Store) error {
				_, err := tx.CreateUserWithSession(ctx, "postgres@example.com", encoding, digest[:])
				return err
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrEmailUnavailable):
				conflicts = append(conflicts, err)
			default:
				unexpected = append(unexpected, err)
			}
		}()
	}
	wg.Wait()
	if len(unexpected) != 0 {
		t.Fatalf("unexpected registration errors: %v", unexpected)
	}
	if succeeded != 1 || len(conflicts) != callers-1 {
		t.Fatalf("succeeded=%d conflicts=%d, want 1 and %d", succeeded, len(conflicts), callers-1)
	}
	var users, sessions int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM sessions)").Scan(&users, &sessions); err != nil {
		t.Fatal(database.SafeError("count rows", err))
	}
	if users != 1 || sessions != 1 {
		t.Fatalf("users=%d sessions=%d, want 1 and 1", users, sessions)
	}
}

func TestRegisterTransactionRollsBackUser(t *testing.T) {
	service, store, pool := newPostgresAuth(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Seed one session row whose digest collides, so the session insert inside the
	// registration transaction fails after the user insert succeeded.
	if _, err := service.Register(ctx, "first@example.com", "synthetic-password", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	first, found, err := store.UserCredential(ctx, "first@example.com")
	if err != nil || !found {
		t.Fatalf("seed account missing: found=%t err=%v", found, err)
	}
	colliding := sha256.Sum256([]byte("synthetic-colliding-digest"))
	if _, err := pool.Exec(ctx, "INSERT INTO sessions (user_id, token_hash, expires_at) VALUES ($1,$2,now() + interval '30 days')", first.User.ID, colliding[:]); err != nil {
		t.Fatal(database.SafeError("seed session", err))
	}
	// The store-level call is what registration runs inside its transaction, so a
	// failure here must not leave the user row behind.
	err = store.InTx(ctx, func(ctx context.Context, tx Store) error {
		_, err := tx.CreateUserWithSession(ctx, "rolledback@example.com", "synthetic-encoding", colliding[:])
		return err
	})
	if err == nil {
		t.Fatal("colliding session digest was accepted")
	}
	var users int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM users WHERE email='rolledback@example.com'").Scan(&users); err != nil {
		t.Fatal(database.SafeError("count user", err))
	}
	if users != 0 {
		t.Fatal("failed session insert left the account committed")
	}
	// A successful registration still commits both rows together.
	if _, err := service.Register(ctx, "second@example.com", "synthetic-password", "127.0.0.2"); err != nil {
		t.Fatal(err)
	}
	var committedUsers, committedSessions int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM users WHERE email='second@example.com'), (SELECT count(*) FROM sessions s JOIN users u ON u.id=s.user_id WHERE u.email='second@example.com')").Scan(&committedUsers, &committedSessions); err != nil {
		t.Fatal(database.SafeError("count registration", err))
	}
	if committedUsers != 1 || committedSessions != 1 {
		t.Fatalf("users=%d sessions=%d, want 1 and 1", committedUsers, committedSessions)
	}
}

func TestLoginAndSessionAgainstPostgres(t *testing.T) {
	service, _, pool := newPostgresAuth(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	registered, err := service.Register(ctx, "postgres@example.com", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	login, err := service.Login(ctx, "POSTGRES@example.com", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if login.Token == registered.Token {
		t.Fatal("login reused the registration token")
	}
	if login.User.ID != registered.User.ID {
		t.Fatal("login resolved a different account")
	}
	if _, err := service.Login(ctx, "postgres@example.com", "synthetic-wrong-password", "127.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password error = %v", err)
	}
	if _, err := service.Login(ctx, "absent@example.com", "synthetic-password", "127.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown email error = %v", err)
	}

	before, revokedAt, found := loadSession(t, ctx, pool, sqlDigest(login.Token))
	if !found || revokedAt != nil {
		t.Fatal("login session is missing or revoked")
	}
	renewed, err := service.Session(ctx, login.Token)
	if err != nil {
		t.Fatal(err)
	}
	after, _, found := loadSession(t, ctx, pool, sqlDigest(login.Token))
	if !found {
		t.Fatal("session disappeared after renewal")
	}
	if !after.After(before) {
		t.Fatalf("expiry was not renewed: before=%s after=%s", before, after)
	}
	if !renewed.ExpiresAt.Equal(after) {
		t.Fatalf("returned expiry %s differs from the stored %s", renewed.ExpiresAt, after)
	}
	remaining := time.Until(after)
	if remaining < SessionLifetime-time.Minute || remaining > SessionLifetime+time.Minute {
		t.Fatalf("renewed expiry is %s away, want about %s", remaining, SessionLifetime)
	}
	if renewed.User.Email != "postgres@example.com" {
		t.Fatalf("renewal returned %q", renewed.User.Email)
	}
}

func TestExpiredAndRevokedSessionsAreNotRenewed(t *testing.T) {
	service, _, pool := newPostgresAuth(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := service.Register(ctx, "postgres@example.com", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET expires_at = now() - interval '1 second' WHERE token_hash=$1", sqlDigest(session.Token)); err != nil {
		t.Fatal(database.SafeError("expire session", err))
	}
	if _, err := service.Session(ctx, session.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired session error = %v", err)
	}
	expiredBefore, _, found := loadSession(t, ctx, pool, sqlDigest(session.Token))
	if !found {
		t.Fatal("expired session row disappeared")
	}
	// A failed authentication must not extend the expiry.
	if !expiredBefore.Before(time.Now()) {
		t.Fatal("an expired session was silently renewed")
	}

	live, err := service.Login(ctx, "postgres@example.com", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Logout(ctx, live.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Session(ctx, live.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked session error = %v", err)
	}
}

func TestLogoutKeepsFirstRevocationTime(t *testing.T) {
	service, _, pool := newPostgresAuth(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := service.Register(ctx, "postgres@example.com", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	digest := sqlDigest(session.Token)
	if err := service.Logout(ctx, session.Token); err != nil {
		t.Fatal(err)
	}
	first, firstRevoked, found := loadSession(t, ctx, pool, digest)
	if !found || firstRevoked == nil {
		t.Fatal("logout did not stamp revoked_at")
	}
	if err := service.Logout(ctx, session.Token); err != nil {
		t.Fatalf("repeated logout failed: %v", err)
	}
	second, secondRevoked, found := loadSession(t, ctx, pool, digest)
	if !found || secondRevoked == nil {
		t.Fatal("repeated logout lost the session row")
	}
	if !secondRevoked.Equal(*firstRevoked) {
		t.Fatalf("revoked_at changed on repeated logout: %s then %s", *firstRevoked, *secondRevoked)
	}
	if !second.Equal(first) {
		t.Fatal("repeated logout changed the expiry")
	}
	// A concurrent pair of logouts keeps exactly one stamp as well.
	concurrent, err := service.Login(ctx, "postgres@example.com", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := service.Logout(ctx, concurrent.Token); err != nil {
				t.Errorf("concurrent logout failed: %v", err)
			}
		}()
	}
	wg.Wait()
	_, concurrentRevoked, found := loadSession(t, ctx, pool, sqlDigest(concurrent.Token))
	if !found || concurrentRevoked == nil {
		t.Fatal("concurrent logout did not revoke the session")
	}
	if err := service.Logout(ctx, "tps_unknown"); err != nil {
		t.Fatalf("logout of an unknown token failed: %v", err)
	}
}

func TestConcurrentSessionsRenewIndependently(t *testing.T) {
	service, _, pool := newPostgresAuth(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	session, err := service.Register(ctx, "postgres@example.com", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := service.Session(ctx, session.Token); err != nil {
				t.Errorf("concurrent renewal failed: %v", err)
			}
		}()
	}
	wg.Wait()
	expiresAt, revokedAt, found := loadSession(t, ctx, pool, sqlDigest(session.Token))
	if !found || revokedAt != nil {
		t.Fatal("session is missing or revoked after concurrent renewal")
	}
	remaining := time.Until(expiresAt)
	if remaining < SessionLifetime-time.Minute || remaining > SessionLifetime+time.Minute {
		t.Fatalf("expiry is %s away, want about %s", remaining, SessionLifetime)
	}
}

// TestLogoutRaceAfterCommitRejectsRenewal covers the second race order: logout
// commits first, so a renewal that was already waiting on the row lock re-reads
// the row, sees revoked_at and reports unauthenticated.
func TestLogoutRaceAfterCommitRejectsRenewal(t *testing.T) {
	service, store, pool, appURL := newPostgresAuthWithURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := service.Register(ctx, "postgres@example.com", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	digest := sqlDigest(session.Token)
	observer := openAppConn(t, ctx, appURL)

	// The logout runs the real revoke inside a transaction and holds the row lock.
	// The renewal is driven on the pool, so it waits on that lock; the wait is
	// confirmed from pg_stat_activity before the logout commits.
	renewDone := make(chan error, 1)
	err = store.InTx(ctx, func(ctx context.Context, tx Store) error {
		if err := tx.RevokeSession(ctx, digest); err != nil {
			return err
		}
		go func() {
			_, err := service.Session(ctx, session.Token)
			renewDone <- err
		}()
		waitForLockedStatement(t, ctx, observer, renewStatementPattern)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Logout committed first, so the waiting renewal must find no live row.
	if renewErr := <-renewDone; !errors.Is(renewErr, ErrUnauthenticated) {
		t.Fatalf("a renewal that waited for the logout returned %v", renewErr)
	}

	expiresAt, revokedAt, found := loadSession(t, ctx, pool, digest)
	if !found || revokedAt == nil {
		t.Fatal("session row missing after logout")
	}
	if !expiresAt.After(time.Now()) {
		t.Fatal("logout unexpectedly expired the row instead of revoking it")
	}
	// No later request may renew the same session either.
	if _, err := service.Session(ctx, session.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("post-logout renewal error = %v", err)
	}
}

// TestLogoutRaceAfterCommittedRenewalAllowsInflightRequest covers the first race
// order: the renewal commits first, so that in-flight request succeeds with its
// new expiry. The logout that was waiting on the row lock then revokes the row,
// and every request after the logout commit fails.
func TestLogoutRaceAfterCommittedRenewalAllowsInflightRequest(t *testing.T) {
	service, store, pool, appURL := newPostgresAuthWithURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := service.Register(ctx, "postgres@example.com", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	digest := sqlDigest(session.Token)
	before, _, found := loadSession(t, ctx, pool, digest)
	if !found {
		t.Fatal("session row missing after registration")
	}
	observer := openAppConn(t, ctx, appURL)

	// The in-flight renewal runs the real conditional update inside a transaction
	// and holds the row lock until it commits. A logout arriving now waits.
	logoutDone := make(chan error, 1)
	var renewed IssuedSession
	err = store.InTx(ctx, func(ctx context.Context, tx Store) error {
		issued, found, err := tx.AuthenticateSession(ctx, digest)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("the in-flight renewal did not find the live session")
		}
		renewed = issued
		go func() { logoutDone <- service.Logout(ctx, session.Token) }()
		waitForLockedStatement(t, ctx, observer, logoutStatementPattern)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !renewed.ExpiresAt.After(before) {
		t.Fatalf("in-flight renewal expiry %s is not later than %s", renewed.ExpiresAt, before)
	}
	// The renewal committed first, so the waiting logout completes afterwards.
	if err := <-logoutDone; err != nil {
		t.Fatal(database.SafeError("logout after renewal", err))
	}

	stored, revokedAt, found := loadSession(t, ctx, pool, digest)
	if !found || revokedAt == nil {
		t.Fatal("session row missing after the logout")
	}
	if !stored.Equal(renewed.ExpiresAt) {
		t.Fatalf("stored expiry %s differs from the committed renewal %s", stored, renewed.ExpiresAt)
	}
	// Once logout has committed, a new request must fail.
	if _, err := service.Session(ctx, session.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("post-logout renewal error = %v", err)
	}
}

func TestRegisterSessionHasNoPlaintextToken(t *testing.T) {
	service, _, pool := newPostgresAuth(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := service.Register(ctx, "postgres@example.com", "synthetic-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var leaked bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM sessions WHERE encode(token_hash,'escape') LIKE '%'||$1||'%')", session.Token).Scan(&leaked); err != nil {
		t.Fatal(database.SafeError("scan plaintext", err))
	}
	if leaked {
		t.Fatal("a session row contains the plaintext token")
	}
}
