//go:build integration

package workspaces

import (
	"bytes"
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
	schema := "t064_" + fmt.Sprintf("%x", rand.Text()[:12])
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
		if err != nil || actual != role || !strings.HasPrefix(schema, "t064_") {
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

// newWorkspaces returns the service, the pool and the service-role URL of a
// fresh schema. One PostgreSQL adapter serves both the reads and the creation
// transaction runner. The URL is needed because a race test opens independent
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
	store := NewPostgres(pool)
	return New(store, store), pool, appURL
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

// seedUser inserts one synthetic account and returns its id.
func seedUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, email string) ID {
	t.Helper()
	var id ID
	if err := pool.QueryRow(ctx, "INSERT INTO users (email, password_hash) VALUES ($1,$2) RETURNING id", email, "synthetic-encoding").Scan(&id); err != nil {
		t.Fatal(database.SafeError("seed user", err))
	}
	return id
}

// seedWorkspace inserts one synthetic workspace with its required creator and
// returns its id.
func seedWorkspace(t *testing.T, ctx context.Context, pool *pgxpool.Pool, createdBy ID, name string) ID {
	t.Helper()
	var id ID
	if err := pool.QueryRow(ctx, "INSERT INTO workspaces (created_by_user_id, name) VALUES ($1,$2) RETURNING id", createdBy, name).Scan(&id); err != nil {
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

// createOnConn runs creation inside one explicit READ COMMITTED transaction on an
// independent connection, which is the isolation level the quota relies on.
func createOnConn(t *testing.T, ctx context.Context, conn *pgx.Conn, userID ID, name string) (Workspace, error) {
	t.Helper()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Workspace{}, database.SafeError("begin", err)
	}
	workspace, err := createWorkspace(ctx, &txStore{q: tx}, userID, name)
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

// createdBy returns the stored creator of one workspace.
func createdBy(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID ID) ID {
	t.Helper()
	var owner ID
	if err := pool.QueryRow(ctx, "SELECT created_by_user_id FROM workspaces WHERE id=$1", workspaceID).Scan(&owner); err != nil {
		t.Fatal(database.SafeError("load creator", err))
	}
	return owner
}

// TestLatestSchemaShape proves a fresh schema built only from the released
// migrations has the columns, collation and unique index the domain code relies
// on, and no new foreign key, CHECK, trigger or row level security.
func TestLatestSchemaShape(t *testing.T) {
	_, pool, _ := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var collation string
	if err := pool.QueryRow(ctx, `SELECT collation_name FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = 'workspaces' AND column_name = 'name'`).Scan(&collation); err != nil {
		t.Fatal(database.SafeError("workspaces name collation", err))
	}
	if collation != "C" {
		t.Fatalf("workspaces.name collation=%q want C", collation)
	}
	var nullable string
	if err := pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = 'workspaces' AND column_name = 'created_by_user_id'`).Scan(&nullable); err != nil {
		t.Fatal(database.SafeError("workspaces creator nullability", err))
	}
	if nullable != "NO" {
		t.Fatalf("created_by_user_id nullable=%q want NO", nullable)
	}
	var indexdef string
	if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes
WHERE schemaname = current_schema() AND indexname = 'uk_workspaces_created_by_user_id_name'`).Scan(&indexdef); err != nil {
		t.Fatal(database.SafeError("workspaces name index", err))
	}
	if !strings.Contains(indexdef, "UNIQUE") || !strings.Contains(indexdef, "(created_by_user_id, name)") {
		t.Fatalf("indexdef=%q want a unique index on (created_by_user_id, name)", indexdef)
	}
	var constraints, triggers, rls int
	if err := pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM pg_constraint c JOIN pg_class r ON r.oid = c.conrelid
   JOIN pg_namespace n ON n.oid = r.relnamespace
   WHERE n.nspname = current_schema() AND c.contype IN ('f', 'c')),
 (SELECT count(*) FROM pg_trigger g JOIN pg_class r ON r.oid = g.tgrelid
   JOIN pg_namespace n ON n.oid = r.relnamespace
   WHERE n.nspname = current_schema() AND NOT g.tgisinternal),
 (SELECT count(*) FROM pg_class r JOIN pg_namespace n ON n.oid = r.relnamespace
   WHERE n.nspname = current_schema() AND r.relrowsecurity)`).Scan(&constraints, &triggers, &rls); err != nil {
		t.Fatal(database.SafeError("schema shape", err))
	}
	if constraints != 0 || triggers != 0 || rls != 0 {
		t.Fatalf("foreign keys/checks=%d triggers=%d rls=%d want 0", constraints, triggers, rls)
	}
}

// TestCreateWorkspaceAgainstPostgres proves the first call stores the trimmed
// name with its creator, writes exactly one owner membership, and that the id
// comes from the database as a UUID v7. The new workspace is visible in the
// creator's list immediately and counts toward the quota.
func TestCreateWorkspaceAgainstPostgres(t *testing.T) {
	service, pool, _ := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	userID := seedUser(t, ctx, pool, "create@example.com")

	workspace, err := service.Create(ctx, userID, "  Alpha  ")
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Name != "Alpha" {
		t.Fatalf("name=%q want the trimmed name Alpha", workspace.Name)
	}
	var name string
	var version int
	if err := pool.QueryRow(ctx, "SELECT name, uuid_extract_version(id) FROM workspaces WHERE id=$1", workspace.ID).Scan(&name, &version); err != nil {
		t.Fatal(database.SafeError("load workspace", err))
	}
	if name != "Alpha" || version != 7 {
		t.Fatalf("stored name=%q uuid version=%d want Alpha and 7", name, version)
	}
	if owner := createdBy(t, ctx, pool, workspace.ID); owner != userID {
		t.Fatal("stored creator does not match the creating account")
	}
	var role string
	if err := pool.QueryRow(ctx, "SELECT role FROM workspace_memberships WHERE workspace_id=$1 AND user_id=$2", workspace.ID, userID).Scan(&role); err != nil {
		t.Fatal(database.SafeError("load membership", err))
	}
	if role != "owner" {
		t.Fatalf("role=%q want owner", role)
	}
	workspaces, memberships := rowCounts(t, ctx, pool)
	if workspaces != 1 || memberships != 1 {
		t.Fatalf("workspaces=%d memberships=%d want 1 and 1", workspaces, memberships)
	}
	// The role query the workspace-scoped routes use returns the owner directly.
	foundRole, found, err := service.MembershipRole(ctx, userID, workspace.ID)
	if err != nil || !found || foundRole != RoleOwner {
		t.Fatalf("role=%q found=%t err=%v want owner and found", foundRole, found, err)
	}
	list, err := service.List(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0] != workspace {
		t.Fatalf("list=%v want the created workspace", list)
	}
}

// TestCreateWorkspaceRejectsInvalidName proves the domain rule runs before any
// SQL: nothing is written and no error is reported as a name conflict.
func TestCreateWorkspaceRejectsInvalidName(t *testing.T) {
	service, pool, _ := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	userID := seedUser(t, ctx, pool, "invalid@example.com")

	for _, name := range []string{"", "   ", "Al\x00pha", strings.Repeat("a", nameMaxRunes+1), strings.Repeat("\U0001F600", 65)} {
		if _, err := service.Create(ctx, userID, name); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("name=%q error=%v want ErrInvalidName", name, err)
		}
	}
	workspaces, memberships := rowCounts(t, ctx, pool)
	if workspaces != 0 || memberships != 0 {
		t.Fatalf("workspaces=%d memberships=%d want 0 and 0", workspaces, memberships)
	}
}

// TestCreateWorkspaceUnknownUserWritesNothing proves the users row lock doubles
// as the existence check: a missing account fails before any insert.
func TestCreateWorkspaceUnknownUserWritesNothing(t *testing.T) {
	service, pool, _ := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := service.Create(ctx, ID{Bytes: [16]byte{0x01}, Valid: true}, "Alpha"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("error=%v want ErrUserNotFound", err)
	}
	workspaces, memberships := rowCounts(t, ctx, pool)
	if workspaces != 0 || memberships != 0 {
		t.Fatalf("workspaces=%d memberships=%d want 0 and 0", workspaces, memberships)
	}
}

// TestCreateWorkspaceAtLimit proves the third creation fails with the quota
// error and leaves the stored rows untouched. It also covers the sequential
// duplicate name: the second attempt with the same name is a name conflict from
// the unique index, not a second workspace.
func TestCreateWorkspaceAtLimit(t *testing.T) {
	service, pool, _ := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	userID := seedUser(t, ctx, pool, "limit@example.com")

	if _, err := service.Create(ctx, userID, "Alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, userID, "Alpha"); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("duplicate error=%v want ErrNameConflict", err)
	}
	if _, err := service.Create(ctx, userID, "Beta"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, userID, "Gamma"); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("error=%v want ErrLimitReached", err)
	}
	workspaces, memberships := rowCounts(t, ctx, pool)
	if workspaces != 2 || memberships != 2 {
		t.Fatalf("workspaces=%d memberships=%d want 2 and 2", workspaces, memberships)
	}
	var stored int
	if err := pool.QueryRow(ctx, "SELECT count(*)::int FROM workspaces WHERE name='Gamma'").Scan(&stored); err != nil {
		t.Fatal(database.SafeError("count rejected name", err))
	}
	if stored != 0 {
		t.Fatal("a rejected creation left a workspace row")
	}
}

// TestCreateWorkspaceConcurrencyDistinctNames drives eight independent READ
// COMMITTED transactions for the same account and distinct names: the users row
// lock serialises them, so exactly two creations succeed and the other six see
// the quota error. No partial workspace or membership is left behind.
func TestCreateWorkspaceConcurrencyDistinctNames(t *testing.T) {
	_, pool, appURL := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	userID := seedUser(t, ctx, pool, "race-limit@example.com")

	names := []string{"Alpha", "Beta", "Gamma", "Delta", "Epsilon", "Zeta", "Eta", "Theta"}
	errs := make([]error, len(names))
	results := make([]Workspace, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
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
			results[i], errs[i] = createOnConn(t, ctx, conn, userID, name)
		}()
	}
	wg.Wait()

	succeeded := 0
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
			if results[i].Name != names[i] {
				t.Fatalf("result %d name=%q want %q", i, results[i].Name, names[i])
			}
		case errors.Is(err, ErrLimitReached):
		default:
			t.Fatalf("name=%q error=%v want success or ErrLimitReached", names[i], err)
		}
	}
	if succeeded != freeWorkspaceLimit {
		t.Fatalf("succeeded=%d want exactly %d", succeeded, freeWorkspaceLimit)
	}
	workspaces, memberships := rowCounts(t, ctx, pool)
	if workspaces != freeWorkspaceLimit || memberships != freeWorkspaceLimit {
		t.Fatalf("workspaces=%d memberships=%d want %d and %d", workspaces, memberships, freeWorkspaceLimit, freeWorkspaceLimit)
	}
	var owners int
	if err := pool.QueryRow(ctx, "SELECT count(*)::int FROM workspace_memberships WHERE user_id=$1 AND role='owner'", userID).Scan(&owners); err != nil {
		t.Fatal(database.SafeError("count owners", err))
	}
	if owners != freeWorkspaceLimit {
		t.Fatalf("owner memberships=%d want %d", owners, freeWorkspaceLimit)
	}
}

// TestCreateWorkspaceConcurrencySameName drives four independent transactions
// with the same name: the unique index, not a prior lookup, decides, so exactly
// one succeeds and the rest are name conflicts.
func TestCreateWorkspaceConcurrencySameName(t *testing.T) {
	_, pool, appURL := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	userID := seedUser(t, ctx, pool, "race-name@example.com")

	const attempts = 4
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
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
			_, errs[i] = createOnConn(t, ctx, conn, userID, "Alpha")
		}()
	}
	wg.Wait()

	succeeded := 0
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrNameConflict):
		default:
			t.Fatalf("attempt %d error=%v want success or ErrNameConflict", i, err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("succeeded=%d want exactly 1", succeeded)
	}
	workspaces, memberships := rowCounts(t, ctx, pool)
	if workspaces != 1 || memberships != 1 {
		t.Fatalf("workspaces=%d memberships=%d want 1 and 1", workspaces, memberships)
	}
}

// TestCreateWorkspaceQuotaIsPerAccount proves different accounts never share the
// quota: each one may create its two workspaces, and each third call fails.
func TestCreateWorkspaceQuotaIsPerAccount(t *testing.T) {
	service, pool, _ := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	first := seedUser(t, ctx, pool, "quota-a@example.com")
	second := seedUser(t, ctx, pool, "quota-b@example.com")

	for _, userID := range []ID{first, second} {
		for _, name := range []string{"Alpha", "Beta"} {
			if _, err := service.Create(ctx, userID, name); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, userID := range []ID{first, second} {
		if _, err := service.Create(ctx, userID, "Gamma"); !errors.Is(err, ErrLimitReached) {
			t.Fatalf("error=%v want ErrLimitReached for a second account", err)
		}
	}
	workspaces, memberships := rowCounts(t, ctx, pool)
	if workspaces != 4 || memberships != 4 {
		t.Fatalf("workspaces=%d memberships=%d want 4 and 4", workspaces, memberships)
	}
}

// injectedStore wraps the real transaction adapter and fails one step, so the
// rollback of a half-written pair can be observed without a second code path.
type injectedStore struct {
	createStore
	failWorkspace  bool
	failMembership bool
}

func (s injectedStore) insertWorkspace(ctx context.Context, userID ID, name string) (Workspace, error) {
	if s.failWorkspace {
		return Workspace{}, database.SafeStoreError("workspaces insert", errors.New("synthetic-workspace-failure"))
	}
	return s.createStore.insertWorkspace(ctx, userID, name)
}

func (s injectedStore) insertOwnerMembership(ctx context.Context, workspaceID, userID ID) error {
	if s.failMembership {
		return database.SafeStoreError("workspaces membership insert", errors.New("synthetic-membership-failure"))
	}
	return s.createStore.insertOwnerMembership(ctx, workspaceID, userID)
}

// TestCreateWorkspaceFailureRollsBackEverything injects a workspace insert
// failure and a membership insert failure in turn. Neither may leave a committed
// workspace or membership behind, and the error must stay safe fixed text.
func TestCreateWorkspaceFailureRollsBackEverything(t *testing.T) {
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
			store.createStore = &txStore{q: tx}
			_, err = createWorkspace(ctx, store, userID, "Alpha")
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

// TestListWorkspacesOrderAndIsolation runs the real SQL: an account without
// memberships gets an empty non-nil slice, the C collation orders names
// bytewise, equal names fall back to the workspace id, and workspaces the
// account is not a member of are never returned.
func TestListWorkspacesOrderAndIsolation(t *testing.T) {
	service, pool, _ := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	userID := seedUser(t, ctx, pool, "list@example.com")
	creator := seedUser(t, ctx, pool, "list-creator@example.com")
	stranger := seedUser(t, ctx, pool, "list-stranger@example.com")

	empty, err := service.List(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty list=%v want a non-nil empty slice", empty)
	}

	// C collation is bytewise: upper case sorts before lower case regardless of
	// the database locale.
	seeded := []struct {
		name string
		id   ID
	}{
		{"Beta", seedWorkspace(t, ctx, pool, creator, "Beta")},
		{"alpha", seedWorkspace(t, ctx, pool, creator, "alpha")},
		{"Alpha", seedWorkspace(t, ctx, pool, creator, "Alpha")},
		{"Same", seedWorkspace(t, ctx, pool, creator, "Same")},
		{"Same", seedWorkspace(t, ctx, pool, stranger, "Same")},
	}
	for _, row := range seeded {
		seedMembership(t, ctx, pool, row.id, userID, "member")
	}
	foreign := seedWorkspace(t, ctx, pool, stranger, "Private")
	seedMembership(t, ctx, pool, foreign, stranger, "owner")

	list, err := service.List(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != len(seeded) {
		t.Fatalf("list=%v want %d rows", list, len(seeded))
	}
	for i := 1; i < len(list); i++ {
		previous, current := list[i-1], list[i]
		byName := bytes.Compare([]byte(previous.Name), []byte(current.Name))
		if byName > 0 || (byName == 0 && bytes.Compare(previous.ID.Bytes[:], current.ID.Bytes[:]) > 0) {
			t.Fatalf("list=%v is not ordered by name then id", list)
		}
	}
	if list[0].Name != "Alpha" || list[1].Name != "Beta" || list[2].Name != "Same" || list[3].Name != "Same" || list[4].Name != "alpha" {
		t.Fatalf("list names=%q want C collation order", names(list))
	}
	for _, workspace := range list {
		if workspace.ID == foreign {
			t.Fatal("the list returned a workspace the account is not a member of")
		}
	}
}

// names returns the display names of a list for failure messages.
func names(list []Workspace) []string {
	out := make([]string, len(list))
	for i, workspace := range list {
		out[i] = workspace.Name
	}
	return out
}

// TestWorkspaceLookupAgainstPostgres covers the detail read used after the
// membership gate: a stored id is found, an unknown id is not.
func TestWorkspaceLookupAgainstPostgres(t *testing.T) {
	service, pool, _ := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	userID := seedUser(t, ctx, pool, "lookup@example.com")
	created, err := service.Create(ctx, userID, "Alpha")
	if err != nil {
		t.Fatal(err)
	}

	got, found, err := service.Workspace(ctx, created.ID)
	if err != nil || !found || got != created {
		t.Fatalf("got=%+v found=%t err=%v want the created workspace", got, found, err)
	}
	if _, found, err := service.Workspace(ctx, ID{Bytes: [16]byte{0x0f}, Valid: true}); err != nil || found {
		t.Fatalf("unknown id found=%t err=%v want not found", found, err)
	}
}

// TestMembershipRoleAgainstPostgres covers the authorisation input: owner, a
// directly seeded member row, a non-member, another user's workspace and an
// unknown stored role.
func TestMembershipRoleAgainstPostgres(t *testing.T) {
	service, pool, _ := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner := seedUser(t, ctx, pool, "role-owner@example.com")
	member := seedUser(t, ctx, pool, "role-member@example.com")
	other := seedUser(t, ctx, pool, "role-other@example.com")
	workspace := seedWorkspace(t, ctx, pool, owner, "synthetic-role-workspace")
	foreign := seedWorkspace(t, ctx, pool, other, "synthetic-foreign-workspace")
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
	// A user asking about somebody else's workspace is a non-member.
	personal := seedWorkspace(t, ctx, pool, other, "synthetic-personal")
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
	brokenStore := NewPostgres(broken)
	role, found, err := New(brokenStore, brokenStore).MembershipRole(ctx, ID{Bytes: [16]byte{0x02}, Valid: true}, ID{Bytes: [16]byte{0x03}, Valid: true})
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

// TestWorkspaceHistoryStaysInItsWorkspace proves a second workspace never
// re-homes the business rows of the first one: creation writes only the
// workspace row and its owner membership.
func TestWorkspaceHistoryStaysInItsWorkspace(t *testing.T) {
	service, pool, _ := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	userID := seedUser(t, ctx, pool, "history@example.com")
	first, err := service.Create(ctx, userID, "Alpha")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Create(ctx, userID, "Beta")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("two creations returned the same workspace")
	}

	var projectID, sourceID ID
	if err := pool.QueryRow(ctx, "INSERT INTO projects (workspace_id, project_key, name) VALUES ($1,$2,$3) RETURNING id", first.ID, "synthetic-project", "Synthetic Project").Scan(&projectID); err != nil {
		t.Fatal(database.SafeError("seed project", err))
	}
	if err := pool.QueryRow(ctx, "INSERT INTO sources (workspace_id, platform, instance_key) VALUES ($1,$2,$3) RETURNING id", first.ID, "synthetic-platform", "synthetic-instance").Scan(&sourceID); err != nil {
		t.Fatal(database.SafeError("seed source", err))
	}
	var credentialID ID
	if err := pool.QueryRow(ctx, "INSERT INTO ingest_credentials (workspace_id, source_id, secret_hash) VALUES ($1,$2,$3) RETURNING id", first.ID, sourceID, []byte("synthetic-digest-32-bytes-........")).Scan(&credentialID); err != nil {
		t.Fatal(database.SafeError("seed credential", err))
	}

	for _, row := range []struct {
		name  string
		query string
		id    ID
	}{
		{"project", "SELECT workspace_id FROM projects WHERE id=$1", projectID},
		{"source", "SELECT workspace_id FROM sources WHERE id=$1", sourceID},
		{"ingest credential", "SELECT workspace_id FROM ingest_credentials WHERE id=$1", credentialID},
	} {
		var workspaceID ID
		if err := pool.QueryRow(ctx, row.query, row.id).Scan(&workspaceID); err != nil {
			t.Fatalf("%s row is gone after the second creation: %v", row.name, database.SafeError("load "+row.name, err))
		}
		if workspaceID != first.ID {
			t.Fatalf("%s row moved away from its workspace", row.name)
		}
	}
	var inSecond int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM projects WHERE workspace_id=$1)
+ (SELECT count(*) FROM sources WHERE workspace_id=$1)
+ (SELECT count(*) FROM ingest_credentials WHERE workspace_id=$1)`, second.ID).Scan(&inSecond); err != nil {
		t.Fatal(database.SafeError("count second rows", err))
	}
	if inSecond != 0 {
		t.Fatalf("the new workspace has %d copied business rows, want 0", inSecond)
	}
	// Both workspaces belong to the same account through separate owner rows.
	for _, workspaceID := range []ID{first.ID, second.ID} {
		if role, found, err := service.MembershipRole(ctx, userID, workspaceID); err != nil || !found || role != RoleOwner {
			t.Fatalf("workspace %v role=%q found=%t err=%v", workspaceID, role, found, err)
		}
	}
}

// TestMembershipRoleOnTerminatedConnectionIsUnavailable: a backend the server
// closed makes the membership lookup an unavailable-database error, never a
// "not a member" result.
func TestMembershipRoleOnTerminatedConnectionIsUnavailable(t *testing.T) {
	svc, pool, appURL := newWorkspaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(database.SafeError("acquire", err))
	}
	var pid int32
	if err := c.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(database.SafeError("pid", err))
	}
	c.Release()
	killer := openAppConn(t, ctx, appURL)
	if _, err := killer.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(database.SafeError("terminate", err))
	}
	_, found, err := svc.MembershipRole(ctx, ID{Bytes: [16]byte{1}, Valid: true}, ID{Bytes: [16]byte{2}, Valid: true})
	if found || !errors.Is(err, database.ErrUnavailable) {
		t.Fatalf("found=%v err=%v", found, err)
	}
}
