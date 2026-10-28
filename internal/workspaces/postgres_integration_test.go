//go:build integration

package workspaces

import (
	"context"
	"crypto/rand"
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
	schema := "t019_" + fmt.Sprintf("%x", rand.Text()[:12])
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
		if err != nil || actual != role || !strings.HasPrefix(schema, "t019_") {
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

// newWorkspaces returns the pool-bound service, the pool and the service-role
// URL of a fresh schema. The URL is needed because a race test opens independent
// connections instead of relying on how many pooled connections are free.
func newWorkspaces(t *testing.T) (*Service, *pgxpool.Pool, string) {
	t.Helper()
	_, appURL := integrationSchema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return New(NewPostgres(pool)), pool, appURL
}

// openAppConn opens one independent service-role connection pinned to the test
// schema, so a race test observes lock waits from a connection that never blocks.
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

// waitForLockedBackend waits until the named backend is blocked on a lock. It
// polls pg_stat_activity by pid with a deadline, so the race order is asserted
// from observable server state instead of from a sleep, and an unrelated backend
// that happens to wait on a lock can never be mistaken for the one under test.
func waitForLockedBackend(t *testing.T, ctx context.Context, observer *pgx.Conn, pid int32) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting bool
		err := observer.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock')", pid).Scan(&waiting)
		if err != nil {
			t.Fatal(database.SafeError("read lock wait", err))
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend %d never waited on a lock", pid)
		}
		select {
		case <-ctx.Done():
			t.Fatal(database.SafeError("wait for lock", ctx.Err()))
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// seedUser inserts one synthetic account and returns its id.
func seedUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, email string) ID {
	t.Helper()
	var id ID
	if err := pool.QueryRow(ctx, "INSERT INTO users (email, password_hash) VALUES ($1,$2) RETURNING id", email, "synthetic-encoding").Scan(&id); err != nil {
		t.Fatal(database.SafeError("seed user", err))
	}
	return id
}

// seedWorkspace inserts one synthetic workspace and returns its id.
func seedWorkspace(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) ID {
	t.Helper()
	var id ID
	if err := pool.QueryRow(ctx, "INSERT INTO workspaces (name) VALUES ($1) RETURNING id", name).Scan(&id); err != nil {
		t.Fatal(database.SafeError("seed workspace", err))
	}
	return id
}

// seedMembership inserts one membership row with the given role.
func seedMembership(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, userID ID, role string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "INSERT INTO workspace_memberships (workspace_id, user_id, role) VALUES ($1,$2,$3)", workspaceID, userID, role); err != nil {
		t.Fatal(database.SafeError("seed membership", err))
	}
}

// createInTx runs CreatePersonal inside one explicit READ COMMITTED transaction
// on an independent connection, which is the isolation level T-017 must use.
func createInTx(t *testing.T, ctx context.Context, conn *pgx.Conn, userID ID) (Workspace, error) {
	t.Helper()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Workspace{}, database.SafeError("begin", err)
	}
	workspace, err := NewTx(tx).CreatePersonal(ctx, userID)
	if err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return Workspace{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Workspace{}, database.SafeError("commit", err)
	}
	return workspace, nil
}

func rowCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (workspaces, memberships int) {
	t.Helper()
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM workspaces), (SELECT count(*) FROM workspace_memberships)").Scan(&workspaces, &memberships); err != nil {
		t.Fatal(database.SafeError("count rows", err))
	}
	return workspaces, memberships
}

// TestCreatePersonalAgainstPostgres proves the first call writes exactly one
// workspace named Personal with one owner membership, and that the id comes from
// the database as a UUID v7.
func TestCreatePersonalAgainstPostgres(t *testing.T) {
	_, pool, appURL := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	userID := seedUser(t, ctx, pool, "personal@example.com")

	conn := openAppConn(t, ctx, appURL)
	workspace, err := createInTx(t, ctx, conn, userID)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Name != "Personal" {
		t.Fatalf("name=%q want Personal", workspace.Name)
	}
	var name string
	var version int
	if err := pool.QueryRow(ctx, "SELECT name, uuid_extract_version(id) FROM workspaces WHERE id=$1", workspace.ID).Scan(&name, &version); err != nil {
		t.Fatal(database.SafeError("load workspace", err))
	}
	if name != "Personal" || version != 7 {
		t.Fatalf("stored name=%q uuid version=%d want Personal and 7", name, version)
	}
	var role string
	if err := pool.QueryRow(ctx, "SELECT role FROM workspace_memberships WHERE workspace_id=$1 AND user_id=$2", workspace.ID, userID).Scan(&role); err != nil {
		t.Fatal(database.SafeError("load membership", err))
	}
	var memberships int
	if err := pool.QueryRow(ctx, "SELECT count(*)::int FROM workspace_memberships WHERE user_id=$1", userID).Scan(&memberships); err != nil {
		t.Fatal(database.SafeError("count memberships", err))
	}
	if role != "owner" || memberships != 1 {
		t.Fatalf("role=%q memberships=%d want owner and 1", role, memberships)
	}
	// The role query T-010/T-021 will use returns the owner result directly.
	foundRole, found, err := New(NewPostgres(pool)).MembershipRole(ctx, userID, workspace.ID)
	if err != nil || !found || foundRole != RoleOwner {
		t.Fatalf("role=%q found=%t err=%v want owner and found", foundRole, found, err)
	}
}

// TestCreatePersonalRepeatedReturnsSameWorkspace covers the sequential repeat:
// the second call returns the same workspace and writes nothing.
func TestCreatePersonalRepeatedReturnsSameWorkspace(t *testing.T) {
	_, pool, appURL := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	userID := seedUser(t, ctx, pool, "repeat@example.com")
	conn := openAppConn(t, ctx, appURL)

	first, err := createInTx(t, ctx, conn, userID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := createInTx(t, ctx, conn, userID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.Name != second.Name {
		t.Fatal("the repeated call returned a different workspace")
	}
	workspaces, memberships := rowCounts(t, ctx, pool)
	if workspaces != 1 || memberships != 1 {
		t.Fatalf("workspaces=%d memberships=%d want 1 and 1", workspaces, memberships)
	}
}

// TestCreatePersonalConcurrencyCreatesOneWorkspace drives two explicit READ
// COMMITTED transactions for the same user and asserts the documented order from
// observable server state: the second waits on the users row lock, the first
// commits, and the second then returns the same workspace. No workspace or
// membership is duplicated.
func TestCreatePersonalConcurrencyCreatesOneWorkspace(t *testing.T) {
	_, pool, appURL := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	userID := seedUser(t, ctx, pool, "concurrent@example.com")

	firstConn := openAppConn(t, ctx, appURL)
	secondConn := openAppConn(t, ctx, appURL)
	observer := openAppConn(t, ctx, appURL)

	// The first transaction creates the workspace and holds the users row lock
	// until it commits.
	firstTx, err := firstConn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(database.SafeError("begin first", err))
	}
	first, err := NewTx(firstTx).CreatePersonal(ctx, userID)
	if err != nil {
		_ = firstTx.Rollback(context.WithoutCancel(ctx))
		t.Fatal(err)
	}

	// Pin the backend id of the second connection before it runs anything, so the
	// wait is observed on that exact backend rather than on any lock waiter.
	var secondPID int32
	if err := secondConn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&secondPID); err != nil {
		t.Fatal(database.SafeError("read second backend pid", err))
	}

	// The second transaction starts while the lock is held and must wait.
	type result struct {
		workspace Workspace
		err       error
	}
	second := make(chan result, 1)
	go func() {
		workspace, err := createInTx(t, ctx, secondConn, userID)
		second <- result{workspace: workspace, err: err}
	}()
	waitForLockedBackend(t, ctx, observer, secondPID)

	if err := firstTx.Commit(ctx); err != nil {
		t.Fatal(database.SafeError("commit first", err))
	}
	got := <-second
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.workspace.ID != first.ID || got.workspace.Name != first.Name {
		t.Fatal("the waiting transaction did not return the first committed workspace")
	}
	workspaces, memberships := rowCounts(t, ctx, pool)
	if workspaces != 1 || memberships != 1 {
		t.Fatalf("workspaces=%d memberships=%d want 1 and 1", workspaces, memberships)
	}
}

// TestCreatePersonalConcurrencyDistinctUsersCreateDistinctWorkspaces proves the
// users row lock is per user: two different users never share a workspace.
func TestCreatePersonalConcurrencyDistinctUsersCreateDistinctWorkspaces(t *testing.T) {
	_, pool, appURL := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	firstUser := seedUser(t, ctx, pool, "distinct-a@example.com")
	secondUser := seedUser(t, ctx, pool, "distinct-b@example.com")

	results := make([]Workspace, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, userID := range []ID{firstUser, secondUser} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := pgx.Connect(ctx, appURL)
			if err != nil {
				errs[i] = database.SafeError("connect", err)
				return
			}
			defer func() {
				_ = conn.Close(context.Background())
			}()
			results[i], errs[i] = createInTx(t, ctx, conn, userID)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if results[0].ID == results[1].ID {
		t.Fatal("two users share one personal workspace")
	}
	workspaces, memberships := rowCounts(t, ctx, pool)
	if workspaces != 2 || memberships != 2 {
		t.Fatalf("workspaces=%d memberships=%d want 2 and 2", workspaces, memberships)
	}
}

// injectedStore wraps the real transaction adapter and fails one step, so the
// rollback of a half-written pair can be observed without a second code path.
type injectedStore struct {
	createStore
	failWorkspace  bool
	failMembership bool
}

func (s injectedStore) insertWorkspace(ctx context.Context, name string) (Workspace, error) {
	if s.failWorkspace {
		return Workspace{}, database.SafeError("workspaces insert", errors.New("synthetic-workspace-failure"))
	}
	return s.createStore.insertWorkspace(ctx, name)
}

func (s injectedStore) insertOwnerMembership(ctx context.Context, workspaceID, userID ID) error {
	if s.failMembership {
		return database.SafeError("workspaces membership insert", errors.New("synthetic-membership-failure"))
	}
	return s.createStore.insertOwnerMembership(ctx, workspaceID, userID)
}

// TestCreatePersonalFailureRollsBackEverything injects a workspace insert
// failure and a membership insert failure in turn. Neither may leave a committed
// workspace or membership behind, and the error must stay safe text.
func TestCreatePersonalFailureRollsBackEverything(t *testing.T) {
	_, pool, appURL := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	userID := seedUser(t, ctx, pool, "rollback@example.com")
	conn := openAppConn(t, ctx, appURL)

	cases := []struct {
		name  string
		store injectedStore
	}{
		{"workspace insert", injectedStore{failWorkspace: true}},
		{"membership insert", injectedStore{failMembership: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				t.Fatal(database.SafeError("begin", err))
			}
			store := tc.store
			store.createStore = NewTx(tx)
			_, err = createPersonal(ctx, store, userID)
			if err == nil {
				t.Fatal("the injected failure was reported as success")
			}
			if !strings.Contains(err.Error(), "database operation failed") {
				t.Fatalf("error=%q does not carry the fixed safe text", err)
			}
			if strings.Contains(err.Error(), "synthetic-") {
				t.Fatalf("error=%q leaked the injected detail", err)
			}
			if err := tx.Rollback(context.WithoutCancel(ctx)); err != nil {
				t.Fatal(database.SafeError("rollback", err))
			}
			workspaces, memberships := rowCounts(t, ctx, pool)
			if workspaces != 0 || memberships != 0 {
				t.Fatalf("workspaces=%d memberships=%d want 0 and 0 after rollback", workspaces, memberships)
			}
		})
	}
}

// TestCreatePersonalUnknownUserWritesNothing proves the users row lock doubles as
// the existence check: a missing user fails before any insert.
func TestCreatePersonalUnknownUserWritesNothingAgainstPostgres(t *testing.T) {
	_, pool, appURL := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := openAppConn(t, ctx, appURL)

	_, err := createInTx(t, ctx, conn, ID{Bytes: [16]byte{0x01}, Valid: true})
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("error=%v want ErrUserNotFound", err)
	}
	workspaces, memberships := rowCounts(t, ctx, pool)
	if workspaces != 0 || memberships != 0 {
		t.Fatalf("workspaces=%d memberships=%d want 0 and 0", workspaces, memberships)
	}
}

// TestMembershipRoleAgainstPostgres covers the authorisation input: owner,
// a directly seeded member row, a non-member, another user's workspace and an
// unknown stored role.
func TestMembershipRoleAgainstPostgres(t *testing.T) {
	service, pool, _ := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner := seedUser(t, ctx, pool, "role-owner@example.com")
	member := seedUser(t, ctx, pool, "role-member@example.com")
	other := seedUser(t, ctx, pool, "role-other@example.com")
	workspace := seedWorkspace(t, ctx, pool, "synthetic-role-workspace")
	foreign := seedWorkspace(t, ctx, pool, "synthetic-foreign-workspace")
	seedMembership(t, ctx, pool, workspace, owner, "owner")
	seedMembership(t, ctx, pool, workspace, member, "member")

	if role, found, err := service.MembershipRole(ctx, owner, workspace); err != nil || !found || role != RoleOwner {
		t.Fatalf("owner role=%q found=%t err=%v", role, found, err)
	}
	if role, found, err := service.MembershipRole(ctx, member, workspace); err != nil || !found || role != RoleMember {
		t.Fatalf("member role=%q found=%t err=%v", role, found, err)
	}
	// A user who only belongs to another workspace is not a member of this one.
	if role, found, err := service.MembershipRole(ctx, owner, foreign); err != nil || found || role != "" {
		t.Fatalf("foreign role=%q found=%t err=%v want empty and false", role, found, err)
	}
	// A user asking about somebody else's personal workspace is a non-member.
	personal := seedWorkspace(t, ctx, pool, "synthetic-personal")
	seedMembership(t, ctx, pool, personal, other, "owner")
	if role, found, err := service.MembershipRole(ctx, owner, personal); err != nil || found || role != "" {
		t.Fatalf("cross-user role=%q found=%t err=%v want empty and false", role, found, err)
	}
	// An unknown stored role must fail, not degrade to member or owner.
	seedMembership(t, ctx, pool, foreign, other, "synthetic-role")
	if role, found, err := service.MembershipRole(ctx, owner, foreign); err != nil || found {
		t.Fatalf("foreign role=%q found=%t err=%v want found=false", role, found, err)
	}
	role, found, err := service.MembershipRole(ctx, other, foreign)
	if !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("unknown role error=%v want ErrUnknownRole", err)
	}
	if found || role != "" {
		t.Fatalf("unknown role=%q found=%t want empty and false", role, found)
	}
}

// TestMembershipRoleDatabaseFailureIsNotANonMember proves a broken database is a
// real error: mapping it to found=false would silently deny or allow the wrong
// thing instead of surfacing the outage.
func TestMembershipRoleDatabaseFailureIsNotANonMember(t *testing.T) {
	service, _, appURL := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	broken, err := database.Open(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	broken.Close()
	role, found, err := New(NewPostgres(broken)).MembershipRole(ctx, ID{Bytes: [16]byte{0x02}, Valid: true}, ID{Bytes: [16]byte{0x03}, Valid: true})
	if err == nil {
		t.Fatal("a closed pool reported a membership result")
	}
	if found || role != "" {
		t.Fatalf("role=%q found=%t want empty and false", role, found)
	}
	// The healthy service still answers, so the failure came from the database.
	if _, _, err := service.MembershipRole(ctx, ID{Bytes: [16]byte{0x02}, Valid: true}, ID{Bytes: [16]byte{0x03}, Valid: true}); err != nil {
		t.Fatal(err)
	}
}

// TestPersonalHistoryIsNotShared proves an existing personal workspace keeps its
// business rows when the user later joins another workspace: no row is copied or
// moved, and the role query never touches business attribution.
func TestPersonalHistoryIsNotShared(t *testing.T) {
	service, pool, appURL := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	userID := seedUser(t, ctx, pool, "history@example.com")
	conn := openAppConn(t, ctx, appURL)
	personal, err := createInTx(t, ctx, conn, userID)
	if err != nil {
		t.Fatal(err)
	}

	var projectID, sourceID ID
	if err := pool.QueryRow(ctx, "INSERT INTO projects (workspace_id, project_key, name) VALUES ($1,$2,$3) RETURNING id", personal.ID, "synthetic-project", "Synthetic Project").Scan(&projectID); err != nil {
		t.Fatal(database.SafeError("seed project", err))
	}
	if err := pool.QueryRow(ctx, "INSERT INTO sources (workspace_id, platform, instance_key) VALUES ($1,$2,$3) RETURNING id", personal.ID, "synthetic-platform", "synthetic-instance").Scan(&sourceID); err != nil {
		t.Fatal(database.SafeError("seed source", err))
	}
	// Each seeded row keeps its own identity, so the assertions below can prove
	// that specific row still exists and still belongs to the personal workspace
	// instead of only counting rows that could have been re-created.
	var credentialID, eventID ID
	if err := pool.QueryRow(ctx, "INSERT INTO ingest_credentials (workspace_id, source_id, secret_hash) VALUES ($1,$2,$3) RETURNING id", personal.ID, sourceID, []byte("synthetic-digest-32-bytes-........")).Scan(&credentialID); err != nil {
		t.Fatal(database.SafeError("seed credential", err))
	}
	if err := pool.QueryRow(ctx, `INSERT INTO usage_events (workspace_id, source_id, source_event_id, project_id, provider, model, occurred_at, received_at, normalization_version, payload_hash,
input_tokens, input_tokens_quality, output_tokens, output_tokens_quality, total_tokens, total_tokens_quality,
cache_read_tokens, cache_read_tokens_quality, cache_write_tokens, cache_write_tokens_quality, reasoning_tokens, reasoning_tokens_quality)
VALUES ($1,$2,$3,$4,'synthetic-provider','synthetic-model',now(),now(),'v1','\x00',10,'known',20,'known',30,'known',0,'known',0,'known',0,'known') RETURNING id`,
		personal.ID, sourceID, "synthetic-event", projectID).Scan(&eventID); err != nil {
		t.Fatal(database.SafeError("seed usage event", err))
	}

	// Simulate a future team membership: the same user joins another workspace.
	team := seedWorkspace(t, ctx, pool, "synthetic-team")
	seedMembership(t, ctx, pool, team, userID, "member")
	if role, found, err := service.MembershipRole(ctx, userID, team); err != nil || !found || role != RoleMember {
		t.Fatalf("team role=%q found=%t err=%v", role, found, err)
	}

	// Every seeded row must still exist under its own id and still point at the
	// personal workspace: joining another workspace neither moves nor re-creates
	// the history.
	for _, row := range []struct {
		name  string
		query string
		id    ID
	}{
		{"project", "SELECT workspace_id FROM projects WHERE id=$1", projectID},
		{"source", "SELECT workspace_id FROM sources WHERE id=$1", sourceID},
		{"ingest credential", "SELECT workspace_id FROM ingest_credentials WHERE id=$1", credentialID},
		{"usage event", "SELECT workspace_id FROM usage_events WHERE id=$1", eventID},
	} {
		var workspaceID ID
		if err := pool.QueryRow(ctx, row.query, row.id).Scan(&workspaceID); err != nil {
			t.Fatalf("%s row is gone after the team membership: %v", row.name, database.SafeError("load "+row.name, err))
		}
		if workspaceID != personal.ID {
			t.Fatalf("%s row workspace_id changed away from the personal workspace", row.name)
		}
	}
	var inTeam int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM projects WHERE workspace_id=$1)
+ (SELECT count(*) FROM sources WHERE workspace_id=$1)
+ (SELECT count(*) FROM ingest_credentials WHERE workspace_id=$1)
+ (SELECT count(*) FROM usage_events WHERE workspace_id=$1)`, team).Scan(&inTeam); err != nil {
		t.Fatal(database.SafeError("count team rows", err))
	}
	if inTeam != 0 {
		t.Fatalf("team workspace has %d copied business rows, want 0", inTeam)
	}

	// Creating a personal space for another user never reuses this one.
	otherUser := seedUser(t, ctx, pool, "history-other@example.com")
	otherPersonal, err := createInTx(t, ctx, conn, otherUser)
	if err != nil {
		t.Fatal(err)
	}
	if otherPersonal.ID == personal.ID {
		t.Fatal("another user reused an existing personal workspace")
	}
}
