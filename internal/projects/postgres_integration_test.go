//go:build integration

package projects

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
		// URL query parameters must not override the checked coordinates.
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
		if closeErr := app.Close(ctx); closeErr != nil {
			t.Error("close app failed")
		}
		t.Fatal(database.SafeError("service role identity", err))
	}
	if unsafe || appRole != urls[1].User.Username() {
		if closeErr := app.Close(ctx); closeErr != nil {
			t.Error("close app failed")
		}
		t.Fatal("unsafe test service role")
	}
	// The migrations grant table privileges to the fixed tokenpivot_app role, so
	// the service test account must be that role or inherit it.
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
	schema := "t054_" + fmt.Sprintf("%x", rand.Text()[:12])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := owner.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		_ = app.Close(ctx)
		t.Fatal(database.SafeError("create owned schema", err))
	}
	// Register the cleanup right after CREATE SCHEMA succeeds: any later t.Fatal
	// leaves the schema behind otherwise. The closure below only depends on
	// owner, schema, quoted and role, which are all in scope by now.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var actual string
		err := owner.QueryRow(cleanupCtx, "SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname=$1", schema).Scan(&actual)
		if err != nil || actual != role || !strings.HasPrefix(schema, "t054_") {
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
	t.Logf("owned schema=%s database=%s migrator=%s app=%s", schema, db, role, appRole)
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

// newPool opens a service-role pool pinned to the random schema.
func newPool(t *testing.T, value string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func insertWorkspace(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) ID {
	t.Helper()
	var id ID
	if err := pool.QueryRow(ctx, "INSERT INTO workspaces (name) VALUES ($1) RETURNING id", name).Scan(&id); err != nil {
		t.Fatal(database.SafeError("insert workspace", err))
	}
	return id
}

func TestProjectsCRUDAgainstPostgres(t *testing.T) {
	_, appURL := integrationSchema(t)
	pool := newPool(t, appURL)
	service := New(NewPostgres(pool))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workspaceA := insertWorkspace(t, ctx, pool, "synthetic-a")
	workspaceB := insertWorkspace(t, ctx, pool, "synthetic-b")

	list, err := service.List(ctx, workspaceA)
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("empty list=%v err=%v", list, err)
	}
	created, err := service.Create(ctx, workspaceA, "key-1", "First")
	if err != nil {
		t.Fatal(err)
	}
	if created.WorkspaceID != workspaceA || created.Key != "key-1" || created.Name != "First" || created.ArchivedAt != nil || created.CreatedAt.IsZero() {
		t.Fatalf("created=%+v", created)
	}
	// The same key conflicts in the same workspace, but not in another one.
	if _, err := service.Create(ctx, workspaceA, "key-1", "Again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate key error=%v want ErrConflict", err)
	}
	other, err := service.Create(ctx, workspaceB, "key-1", "Other space")
	if err != nil {
		t.Fatal(err)
	}
	// Update is workspace scoped: the foreign id is not found and stays unchanged.
	if _, err := service.Update(ctx, workspaceA, other.ID, "hijack"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign update error=%v want ErrNotFound", err)
	}
	if err := service.Archive(ctx, workspaceA, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign archive error=%v want ErrNotFound", err)
	}
	var name string
	var archived *time.Time
	if err := pool.QueryRow(ctx, "SELECT name, archived_at FROM projects WHERE id=$1", other.ID).Scan(&name, &archived); err != nil || name != "Other space" || archived != nil {
		t.Fatalf("foreign project changed: %q %v %v", name, archived, err)
	}
	updated, err := service.Update(ctx, workspaceA, created.ID, "Renamed")
	if err != nil || updated.Name != "Renamed" || updated.Key != "key-1" {
		t.Fatalf("update=%+v err=%v", updated, err)
	}
	if again, err := service.Update(ctx, workspaceA, created.ID, "Renamed"); err != nil || again.Name != "Renamed" {
		t.Fatalf("repeat update=%+v err=%v", again, err)
	}
	// Archive twice: the first timestamp survives and the row stays listed.
	if err := service.Archive(ctx, workspaceA, created.ID); err != nil {
		t.Fatal(err)
	}
	var first time.Time
	if err := pool.QueryRow(ctx, "SELECT archived_at FROM projects WHERE id=$1", created.ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := service.Archive(ctx, workspaceA, created.ID); err != nil {
		t.Fatal(err)
	}
	var second time.Time
	if err := pool.QueryRow(ctx, "SELECT archived_at FROM projects WHERE id=$1", created.ID).Scan(&second); err != nil || !first.Equal(second) {
		t.Fatalf("archive time changed: %v -> %v err=%v", first, second, err)
	}
	if _, err := service.Create(ctx, workspaceA, "key-1", "Recreate"); !errors.Is(err, ErrConflict) {
		t.Fatalf("recreate after archive error=%v want ErrConflict", err)
	}
	list, err = service.List(ctx, workspaceA)
	if err != nil || len(list) != 1 || list[0].ArchivedAt == nil {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	if err := service.Archive(ctx, workspaceA, ID{Bytes: [16]byte{9}, Valid: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing archive error=%v want ErrNotFound", err)
	}
}

func TestProjectsConcurrentCreateOneWinner(t *testing.T) {
	_, appURL := integrationSchema(t)
	pool := newPool(t, appURL)
	service := New(NewPostgres(pool))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workspaceID := insertWorkspace(t, ctx, pool, "synthetic-race")
	const workers = 6
	start := make(chan struct{})
	results := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			<-start
			_, results[slot] = service.Create(ctx, workspaceID, "same-key", "Name")
		}(i)
	}
	close(start)
	wg.Wait()
	wins, conflicts := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected error %v", err)
		}
	}
	if wins != 1 || conflicts != workers-1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}
