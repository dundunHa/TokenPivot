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
	"sync/atomic"
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

// authSchema is one isolated test schema with the service wired to it. It keeps
// both connection URLs because the service-role URL is needed for independent
// observation connections and the migration-owner URL for privilege changes.
type authSchema struct {
	service *Service
	store   *Postgres
	pool    *pgxpool.Pool
	appURL  string
	appRole string
	// label is the application_name every connection of this schema carries, so
	// an observed lock waiter can be attributed to this test instead of any
	// session that happens to wait on an advisory lock in the database.
	label    string
	ownerURL string
}

// newAuthSchema creates a fresh random schema, applies every released migration
// to it and returns a service pool pinned to it.
func newAuthSchema(t *testing.T, registrationOpen bool) authSchema {
	t.Helper()
	ownerURL, appURL := integrationSchema(t)
	parsed, err := url.Parse(appURL)
	if err != nil || parsed.User == nil {
		t.Fatal("invalid test service URL")
	}
	// Every connection of this test carries its own short application name derived
	// from the test name, so pg_stat_activity and pg_locks rows can be attributed to
	// this test's backends rather than to an unrelated session in the same database.
	sum := sha256.Sum256([]byte(t.Name()))
	label := fmt.Sprintf("tp_it_%x", sum[:4])
	query := parsed.Query()
	query.Set("application_name", label)
	parsed.RawQuery = query.Encode()
	appURL = parsed.String()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := NewPostgres(pool)
	return authSchema{
		service:  New(store, store, NewLimiter(time.Now), registrationOpen),
		store:    store,
		pool:     pool,
		appURL:   appURL,
		appRole:  parsed.User.Username(),
		label:    label,
		ownerURL: ownerURL,
	}
}

// newPostgresAuth returns a service whose registration switch is open, which is
// the mode the pre-existing session and login tests use.
func newPostgresAuth(t *testing.T) (*Service, *Postgres, *pgxpool.Pool) {
	t.Helper()
	schema := newAuthSchema(t, true)
	return schema.service, schema.store, schema.pool
}

// newPostgresAuthWithURL additionally returns the service-role connection URL, so
// a race test can open independent connections and hold or observe a lock
// without depending on how many pooled connections happen to be free.
func newPostgresAuthWithURL(t *testing.T) (*Service, *Postgres, *pgxpool.Pool, string) {
	t.Helper()
	schema := newAuthSchema(t, true)
	return schema.service, schema.store, schema.pool, schema.appURL
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

// registrationLockPids returns the pids of this test's backends that currently hold
// or wait for the registration advisory lock. The lookup is restricted to the
// current database, the fixed two-integer registration key (classid 17017, objid 1,
// objsubid 2 for the two-integer form) and the application name this test's
// connections carry, so an unrelated session that happens to take an advisory lock
// in the same database can never be counted.
func registrationLockPids(t *testing.T, ctx context.Context, observer *pgx.Conn, label string, granted bool) []int32 {
	t.Helper()
	rows, err := observer.Query(ctx, `SELECT l.pid
FROM pg_locks l
JOIN pg_stat_activity a ON a.pid = l.pid
WHERE l.locktype = 'advisory'
  AND l.objsubid = 2
  AND l.classid::bigint = $1
  AND l.objid::bigint = $2
  AND l.granted = $3
  AND a.datname = current_database()
  AND a.application_name = $4
ORDER BY l.pid`, int64(registrationLockClass), int64(registrationLockObject), granted, label)
	if err != nil {
		t.Fatal(database.SafeError("read registration lock state", err))
	}
	defer rows.Close()
	var pids []int32
	for rows.Next() {
		var pid int32
		if err := rows.Scan(&pid); err != nil {
			t.Fatal(database.SafeError("scan registration lock state", err))
		}
		pids = append(pids, pid)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(database.SafeError("read registration lock state", err))
	}
	return pids
}

// waitForRegistrationLockWaiters waits until at least want backends of this test are
// queued on the registration advisory lock and returns their pids. pg_locks joined
// to pg_stat_activity is the only source: the competition is read from server state
// instead of being assumed from a sleep.
func waitForRegistrationLockWaiters(t *testing.T, ctx context.Context, observer *pgx.Conn, label string, want int) []int32 {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if pids := registrationLockPids(t, ctx, observer, label, false); len(pids) >= want {
			return pids
		}
		if time.Now().After(deadline) {
			pids := registrationLockPids(t, ctx, observer, label, false)
			t.Fatalf("%d of %d backends of this test waited on the registration advisory lock", len(pids), want)
		}
		select {
		case <-ctx.Done():
			t.Fatal(database.SafeError("wait for registration lock", ctx.Err()))
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// assertWaitingOnLockStatement proves the observed waiting backend is blocked in the
// advisory lock statement itself rather than in a later statement of the same
// transaction, so a reported failure can be attributed to the lock stage.
func assertWaitingOnLockStatement(t *testing.T, ctx context.Context, observer *pgx.Conn, pid int32) {
	t.Helper()
	var query string
	if err := observer.QueryRow(ctx, "SELECT query FROM pg_stat_activity WHERE pid = $1", pid).Scan(&query); err != nil {
		t.Fatal(database.SafeError("read waiting statement", err))
	}
	if !strings.Contains(query, "pg_advisory_xact_lock") {
		t.Fatalf("waiting backend %d runs %q, want the registration lock statement", pid, query)
	}
}

// pausingRegistrationTx runs the real registration transaction through inner but
// stops it after the first session write, before the transaction commits or rolls
// back. A test releases it with nil to commit or with an error to roll back, so a
// competing registration observes the outcome of another real registration
// transaction, not of a transaction that only holds the lock.
type pausingRegistrationTx struct {
	inner TxRunner
	// written is closed once the paused transaction has written the account, its
	// personal workspace, the owner membership and the first session.
	written chan struct{}
	// outcome carries exactly one decision: nil commits, any error rolls back.
	outcome chan error
	// claimed records whether the pause was already taken, so the registrations a
	// test starts afterwards pass through instead of blocking forever.
	claimed atomic.Bool
}

// newPausingRegistrationTx wraps inner so the first registration it runs stops after
// the session write and waits for the test's decision.
func newPausingRegistrationTx(inner TxRunner) *pausingRegistrationTx {
	return &pausingRegistrationTx{inner: inner, written: make(chan struct{}), outcome: make(chan error, 1)}
}

// InTx implements TxRunner on the real transaction runner.
func (p *pausingRegistrationTx) InTx(ctx context.Context, fn func(context.Context, RegistrationStore) error) error {
	return p.inner.InTx(ctx, func(ctx context.Context, tx RegistrationStore) error {
		return fn(ctx, &pausingRegistrationStore{RegistrationStore: tx, gate: p})
	})
}

// pausingRegistrationStore is the transaction-bound store with the one extra pause.
type pausingRegistrationStore struct {
	RegistrationStore
	gate *pausingRegistrationTx
}

// CreateSession writes the real session row and then, for the first registration
// only, pauses until the test decides the transaction outcome.
func (s *pausingRegistrationStore) CreateSession(ctx context.Context, userID ID, tokenHash []byte) (time.Time, error) {
	expiresAt, err := s.RegistrationStore.CreateSession(ctx, userID, tokenHash)
	if err != nil {
		return expiresAt, err
	}
	if !s.gate.claimed.CompareAndSwap(false, true) {
		return expiresAt, nil
	}
	close(s.gate.written)
	select {
	case outcome := <-s.gate.outcome:
		return expiresAt, outcome
	case <-ctx.Done():
		return expiresAt, ctx.Err()
	}
}

// waitForPausedRegistration waits until the wrapped registration transaction has
// written all four rows and is stopped before its commit, so a test never guesses
// that timing from a sleep.
func waitForPausedRegistration(t *testing.T, ctx context.Context, gate *pausingRegistrationTx) {
	t.Helper()
	select {
	case <-gate.written:
	case <-time.After(15 * time.Second):
		t.Fatal("the first registration never reached its pre-commit pause")
	case <-ctx.Done():
		t.Fatal(database.SafeError("wait for paused registration", ctx.Err()))
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
			err := store.InTx(ctx, func(ctx context.Context, tx RegistrationStore) error {
				user, err := tx.CreateUser(ctx, "postgres@example.com", encoding)
				if err != nil {
					return err
				}
				if err := tx.CreatePersonal(ctx, user.ID); err != nil {
					return err
				}
				_, err = tx.CreateSession(ctx, user.ID, digest[:])
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
	err = store.InTx(ctx, func(ctx context.Context, tx RegistrationStore) error {
		user, err := tx.CreateUser(ctx, "rolledback@example.com", "synthetic-encoding")
		if err != nil {
			return err
		}
		if err := tx.CreatePersonal(ctx, user.ID); err != nil {
			return err
		}
		_, err = tx.CreateSession(ctx, user.ID, colliding[:])
		return err
	})
	if err == nil {
		t.Fatal("colliding session digest was accepted")
	}
	var users, workspaces, memberships int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM users WHERE email='rolledback@example.com'),
       (SELECT count(*) FROM workspaces), (SELECT count(*) FROM workspace_memberships)`).Scan(&users, &workspaces, &memberships); err != nil {
		t.Fatal(database.SafeError("count rollback", err))
	}
	if users != 0 {
		t.Fatal("failed session insert left the account committed")
	}
	if workspaces != 1 || memberships != 1 {
		t.Fatalf("workspaces=%d memberships=%d, want only the first account's set", workspaces, memberships)
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
	err = store.InTx(ctx, func(ctx context.Context, tx RegistrationStore) error {
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
	err = store.InTx(ctx, func(ctx context.Context, tx RegistrationStore) error {
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

// registrationCounts reads the four tables registration writes, so a test can
// assert a whole commit or a whole rollback instead of one column.
func registrationCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (users, workspaces, memberships, sessions int) {
	t.Helper()
	err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM users),
       (SELECT count(*) FROM workspaces),
       (SELECT count(*) FROM workspace_memberships),
       (SELECT count(*) FROM sessions)`).Scan(&users, &workspaces, &memberships, &sessions)
	if err != nil {
		t.Fatal(database.SafeError("count registration rows", err))
	}
	return users, workspaces, memberships, sessions
}

// assertCompleteAccount proves one registered email has exactly the four rows a
// successful registration commits: one account, one personal workspace named
// Personal, one owner membership in it and one session.
func assertCompleteAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, email string) {
	t.Helper()
	var users, owned, sessions int
	err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM users u WHERE u.email = $1),
       (SELECT count(*) FROM workspaces w
          JOIN workspace_memberships m ON m.workspace_id = w.id
          JOIN users u ON u.id = m.user_id
         WHERE u.email = $1 AND m.role = 'owner' AND w.name = 'Personal'),
       (SELECT count(*) FROM sessions s JOIN users u ON u.id = s.user_id WHERE u.email = $1)`, email).Scan(&users, &owned, &sessions)
	if err != nil {
		t.Fatal(database.SafeError("load account rows", err))
	}
	if users != 1 || owned != 1 || sessions != 1 {
		t.Fatalf("%s users=%d personal owner=%d sessions=%d, want 1/1/1", email, users, owned, sessions)
	}
}

// serviceGrantee is the fixed role every migration grants business tables to. The
// test service role inherits it, so the effective privilege of the service role
// comes from this grantee: revoking INSERT for the role that holds the grant is
// the only way to remove it, and has_table_privilege then reports the effective
// state of the service role.
const serviceGrantee = "tokenpivot_app"

// setInsertPrivilege grants or revokes INSERT on one table at the grantee that
// migrations grant to, executing the change through the migration-owner
// connection. It always checks the app role's effective privilege afterwards,
// which includes privileges inherited through tokenpivot_app, so a later failure
// can be attributed to this table rather than guessed from empty counts.
func setInsertPrivilege(t *testing.T, ctx context.Context, ownerURL, appRole, table string, grant bool) {
	t.Helper()
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		t.Fatal(database.SafeError("owner connect", err))
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Error(database.SafeError("owner close", err))
		}
	})
	tableID := pgx.Identifier{table}.Sanitize()
	roleID := pgx.Identifier{serviceGrantee}.Sanitize()
	statement := "REVOKE INSERT ON " + tableID + " FROM " + roleID
	if grant {
		statement = "GRANT INSERT ON " + tableID + " TO " + roleID
	}
	if _, err := conn.Exec(ctx, statement); err != nil {
		t.Fatal(database.SafeError("change insert privilege", err))
	}
	var effective bool
	if err := conn.QueryRow(ctx, `SELECT has_table_privilege($1, $2, 'INSERT')`, appRole, table).Scan(&effective); err != nil {
		t.Fatal(database.SafeError("read insert privilege", err))
	}
	if effective != grant {
		t.Fatalf("effective INSERT on %s for %s = %t, want %t", table, appRole, effective, grant)
	}
}

// TestRegisterOpenCreatesPersonalWorkspaceForEachAccount covers the open
// deployment: consecutive registrations of different emails all succeed, each with
// its own personal workspace, owner membership and first session, while a repeat
// of an owned email is a conflict.
func TestRegisterOpenCreatesPersonalWorkspaceForEachAccount(t *testing.T) {
	schema := newAuthSchema(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for i, email := range []string{"first@example.com", "second@example.com"} {
		if _, err := schema.service.Register(ctx, email, "synthetic-password", fmt.Sprintf("127.0.0.%d", i+1)); err != nil {
			t.Fatalf("%s registration failed: %v", email, err)
		}
	}
	for _, email := range []string{"first@example.com", "second@example.com"} {
		assertCompleteAccount(t, ctx, schema.pool, email)
	}
	users, workspaces, memberships, sessions := registrationCounts(t, ctx, schema.pool)
	if users != 2 || workspaces != 2 || memberships != 2 || sessions != 2 {
		t.Fatalf("users=%d workspaces=%d memberships=%d sessions=%d, want 2 each", users, workspaces, memberships, sessions)
	}
	if _, err := schema.service.Register(ctx, "FIRST@example.com", "synthetic-password", "127.0.0.9"); !errors.Is(err, ErrEmailUnavailable) {
		t.Fatalf("repeat registration error = %v, want ErrEmailUnavailable", err)
	}
	if users, workspaces, memberships, sessions := registrationCounts(t, ctx, schema.pool); users != 2 || workspaces != 2 || memberships != 2 || sessions != 2 {
		t.Fatalf("a rejected repeat changed the rows: %d/%d/%d/%d", users, workspaces, memberships, sessions)
	}
}

// TestRegisterClosedFirstUserThenRejected covers the closed deployment against a
// real database: the first account of the empty database is created completely,
// and every later registration of any email is refused without adding rows.
func TestRegisterClosedFirstUserThenRejected(t *testing.T) {
	schema := newAuthSchema(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := schema.service.Register(ctx, "first@example.com", "synthetic-password", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	assertCompleteAccount(t, ctx, schema.pool, "first@example.com")
	users, workspaces, memberships, sessions := registrationCounts(t, ctx, schema.pool)
	if users != 1 || workspaces != 1 || memberships != 1 || sessions != 1 {
		t.Fatalf("users=%d workspaces=%d memberships=%d sessions=%d, want 1 each", users, workspaces, memberships, sessions)
	}
	for _, email := range []string{"first@example.com", "second@example.com"} {
		_, err := schema.service.Register(ctx, email, "synthetic-password", "127.0.0.2")
		if !errors.Is(err, ErrRegistrationClosed) {
			t.Fatalf("%s error = %v, want ErrRegistrationClosed", email, err)
		}
		if errors.Is(err, ErrEmailUnavailable) {
			t.Fatalf("%s leaked the email unique conflict", email)
		}
	}
	if users, workspaces, memberships, sessions := registrationCounts(t, ctx, schema.pool); users != 1 || workspaces != 1 || memberships != 1 || sessions != 1 {
		t.Fatalf("a refused registration added rows: %d/%d/%d/%d", users, workspaces, memberships, sessions)
	}
}

// TestRegisterClosedConcurrentFirstRegistration proves the advisory lock decides
// the empty-database case from the outcome of a real registration transaction: the
// first registration writes its account, personal workspace, owner membership and
// first session and is then stopped before its commit, the competitors are observed
// queued on the registration advisory lock in pg_locks, and after the first one
// commits exactly one registration exists while the others fail with the closed
// error, not with an email conflict.
func TestRegisterClosedConcurrentFirstRegistration(t *testing.T) {
	schema := newAuthSchema(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	observer := openAppConn(t, ctx, schema.appURL)

	gate := newPausingRegistrationTx(schema.store)
	// A failing assertion must not leave the paused registration blocked until the
	// suite deadline: the cleanup releases it with a rollback.
	t.Cleanup(func() {
		select {
		case gate.outcome <- errors.New("test cleanup rollback"):
		default:
		}
	})
	gated := New(schema.store, gate, NewLimiter(time.Now), false)
	winner := make(chan error, 1)
	go func() {
		_, err := gated.Register(ctx, "race-winner@example.com", "synthetic-password", "127.0.1.1")
		winner <- err
	}()
	waitForPausedRegistration(t, ctx, gate)

	const competitors = 2
	type outcome struct {
		email string
		err   error
	}
	results := make(chan outcome, competitors)
	var wg sync.WaitGroup
	for i := range competitors {
		email := fmt.Sprintf("race%d@example.com", i)
		ip := fmt.Sprintf("127.0.1.%d", i+2)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, registerErr := schema.service.Register(ctx, email, "synthetic-password", ip)
			results <- outcome{email: email, err: registerErr}
		}()
	}
	// The first registration still holds the lock, so every competitor must be
	// queued on that same lock before the outcome is decided.
	waiting := waitForRegistrationLockWaiters(t, ctx, observer, schema.label, competitors)
	for _, pid := range waiting {
		assertWaitingOnLockStatement(t, ctx, observer, pid)
	}
	if held := registrationLockPids(t, ctx, observer, schema.label, true); len(held) == 0 {
		t.Fatal("no backend of this test holds the registration lock while the competitors wait")
	}
	gate.outcome <- nil
	if err := <-winner; err != nil {
		t.Fatalf("the first registration failed: %v", err)
	}
	wg.Wait()
	close(results)
	succeeded := 0
	var refused []error
	for result := range results {
		switch {
		case result.err == nil:
			succeeded++
		case errors.Is(result.err, ErrRegistrationClosed):
			refused = append(refused, result.err)
			if errors.Is(result.err, ErrEmailUnavailable) {
				t.Fatalf("%s was refused by the email index", result.email)
			}
		default:
			t.Fatalf("%s failed unexpectedly: %v", result.email, result.err)
		}
	}
	if succeeded != 0 || len(refused) != competitors {
		t.Fatalf("succeeded=%d refused=%d, want 0 and %d after a committed first user", succeeded, len(refused), competitors)
	}
	assertCompleteAccount(t, ctx, schema.pool, "race-winner@example.com")
	users, workspaces, memberships, sessions := registrationCounts(t, ctx, schema.pool)
	if users != 1 || workspaces != 1 || memberships != 1 || sessions != 1 {
		t.Fatalf("users=%d workspaces=%d memberships=%d sessions=%d, want 1 each", users, workspaces, memberships, sessions)
	}
}

// TestRegisterClosedWaiterSucceedsAfterRollback proves the waiter reads the lock
// holder's outcome instead of assuming it: the first real registration writes all
// four rows and is then rolled back, so the database is still empty when the waiter
// acquires the lock and that waiter becomes the first user.
func TestRegisterClosedWaiterSucceedsAfterRollback(t *testing.T) {
	schema := newAuthSchema(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	observer := openAppConn(t, ctx, schema.appURL)

	rollback := errors.New("synthetic rollback")
	gate := newPausingRegistrationTx(schema.store)
	t.Cleanup(func() {
		select {
		case gate.outcome <- rollback:
		default:
		}
	})
	gated := New(schema.store, gate, NewLimiter(time.Now), false)
	first := make(chan error, 1)
	go func() {
		_, err := gated.Register(ctx, "aborted@example.com", "synthetic-password", "127.0.2.1")
		first <- err
	}()
	waitForPausedRegistration(t, ctx, gate)

	waiter := make(chan error, 1)
	var session Session
	go func() {
		var registerErr error
		session, registerErr = schema.service.Register(ctx, "waiter@example.com", "synthetic-password", "127.0.2.2")
		waiter <- registerErr
	}()
	waiting := waitForRegistrationLockWaiters(t, ctx, observer, schema.label, 1)
	for _, pid := range waiting {
		assertWaitingOnLockStatement(t, ctx, observer, pid)
	}
	// The holder rolls back, so the waiter must still see an empty database.
	gate.outcome <- rollback
	if err := <-first; !errors.Is(err, rollback) {
		t.Fatalf("first registration error = %v, want the injected rollback", err)
	}
	if waiterErr := <-waiter; waiterErr != nil {
		t.Fatalf("the waiter failed although the first registration rolled back: %v", waiterErr)
	}
	if session.User.Email != "waiter@example.com" {
		t.Fatalf("waiter session = %+v", session)
	}
	assertCompleteAccount(t, ctx, schema.pool, "waiter@example.com")
	users, workspaces, memberships, sessions := registrationCounts(t, ctx, schema.pool)
	if users != 1 || workspaces != 1 || memberships != 1 || sessions != 1 {
		t.Fatalf("users=%d workspaces=%d memberships=%d sessions=%d, want 1 each", users, workspaces, memberships, sessions)
	}
}

// TestRegisterRollsBackAtEveryStage injects a real INSERT permission failure into
// each of the four write stages in its own isolated schema. The effective
// privilege is asserted to be gone before the attempt, the returned error must
// name the intended stage, and no row of any table may survive the rollback.
func TestRegisterRollsBackAtEveryStage(t *testing.T) {
	for _, stage := range []struct {
		name  string
		table string
		// wantStage is the fixed safe stage text the failure must carry, so a
		// failure is attributed to the table it was injected into.
		wantStage string
	}{
		{"users", "users", "auth registration"},
		{"workspaces", "workspaces", "workspaces insert"},
		{"workspace_memberships", "workspace_memberships", "workspaces membership insert"},
		{"sessions", "sessions", "auth session insert"},
	} {
		t.Run(stage.name, func(t *testing.T) {
			schema := newAuthSchema(t, true)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			setInsertPrivilege(t, ctx, schema.ownerURL, schema.appRole, stage.table, false)
			_, err := schema.service.Register(ctx, "blocked@example.com", "synthetic-password", "127.0.0.3")
			if err == nil {
				t.Fatalf("registration succeeded although INSERT on %s was revoked", stage.table)
			}
			if errors.Is(err, ErrRegistrationClosed) || errors.Is(err, ErrEmailUnavailable) {
				t.Fatalf("a dependency failure was reported as %v", err)
			}
			message := err.Error()
			if !strings.Contains(message, stage.wantStage) {
				t.Fatalf("error = %q, want the %q stage", message, stage.wantStage)
			}
			for _, secret := range []string{"synthetic-password", "INSERT", "token_hash", "password_hash", "$argon2id$", "postgres"} {
				if strings.Contains(message, secret) {
					t.Fatalf("error leaked %q: %v", secret, err)
				}
			}
			users, workspaces, memberships, sessions := registrationCounts(t, ctx, schema.pool)
			if users != 0 || workspaces != 0 || memberships != 0 || sessions != 0 {
				t.Fatalf("%s failure left rows: users=%d workspaces=%d memberships=%d sessions=%d", stage.name, users, workspaces, memberships, sessions)
			}
			// Restoring the privilege and retrying the same input must produce one
			// complete set of rows, proving the rollback left nothing behind.
			setInsertPrivilege(t, ctx, schema.ownerURL, schema.appRole, stage.table, true)
			if _, err := schema.service.Register(ctx, "blocked@example.com", "synthetic-password", "127.0.0.4"); err != nil {
				t.Fatalf("retry after restoring %s failed: %v", stage.table, err)
			}
			assertCompleteAccount(t, ctx, schema.pool, "blocked@example.com")
			if users, workspaces, memberships, sessions := registrationCounts(t, ctx, schema.pool); users != 1 || workspaces != 1 || memberships != 1 || sessions != 1 {
				t.Fatalf("retry rows: users=%d workspaces=%d memberships=%d sessions=%d, want 1 each", users, workspaces, memberships, sessions)
			}
		})
	}
}

// TestRegisterClosedLockFailureIsNotClosed proves a failed registration lock
// acquisition is a safe dependency failure: it must not be reported as a closed
// registration and must not leave rows behind. The failure is real server state, not
// a removed privilege: another connection holds the registration advisory lock in an
// open transaction, the registration is observed queued on that exact lock in
// pg_locks, and its own context is then cancelled so the wait fails at the lock
// statement.
func TestRegisterClosedLockFailureIsNotClosed(t *testing.T) {
	schema := newAuthSchema(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	observer := openAppConn(t, ctx, schema.appURL)

	// The blocking holder commits nothing: it only takes the registration lock in an
	// open transaction, so the registration below cannot acquire it. Closing the
	// connection at cleanup would abort it as well, so no rollback is deferred here.
	holder := openAppConn(t, ctx, schema.appURL)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(database.SafeError("holder begin", err))
	}
	if _, err := tx.Exec(ctx, lockRegistrationQuery, registrationLockClass, registrationLockObject); err != nil {
		t.Fatal(database.SafeError("holder lock", err))
	}
	held := registrationLockPids(t, ctx, observer, schema.label, true)
	if len(held) == 0 {
		t.Fatal("the holder does not appear as a granted registration lock owner")
	}

	// A dedicated context lets the test cancel exactly this registration attempt.
	registerCtx, cancelRegister := context.WithCancel(ctx)
	defer cancelRegister()
	results := make(chan error, 1)
	go func() {
		_, registerErr := schema.service.Register(registerCtx, "blocked@example.com", "synthetic-password", "127.0.0.5")
		results <- registerErr
	}()
	waiting := waitForRegistrationLockWaiters(t, ctx, observer, schema.label, 1)
	for _, pid := range waiting {
		assertWaitingOnLockStatement(t, ctx, observer, pid)
	}
	// Only now that the lock wait is observed does the attempt get cancelled, so the
	// failure provably comes from acquiring the lock, not from a later statement.
	cancelRegister()
	err = <-results
	if err == nil {
		t.Fatal("registration succeeded without the registration lock")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancelled lock wait", err)
	}
	if errors.Is(err, ErrRegistrationClosed) || errors.Is(err, ErrEmailUnavailable) {
		t.Fatalf("a lock failure was reported as %v", err)
	}
	if !strings.Contains(err.Error(), "auth registration lock") {
		t.Fatalf("error = %q, want the registration lock stage", err.Error())
	}
	if users, workspaces, memberships, sessions := registrationCounts(t, ctx, schema.pool); users != 0 || workspaces != 0 || memberships != 0 || sessions != 0 {
		t.Fatalf("a lock failure left rows: %d/%d/%d/%d", users, workspaces, memberships, sessions)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(database.SafeError("holder rollback", err))
	}
}
