//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dundunHa/TokenPivot/db/migrations"
	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
)

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
		// Prevent URL query parameters from overriding checked coordinates or loading service files.
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
	if err != nil || unsafe || appRole != urls[1].User.Username() {
		if closeErr := app.Close(ctx); closeErr != nil {
			t.Error("close app failed")
		}
		t.Fatal("unsafe test service role")
	}
	// Migrations grant table privileges to the fixed tokenpivot_app role, so the
	// service test account must be that role or inherit it without SET ROLE.
	var appPrivileges bool
	err = app.QueryRow(ctx, "SELECT pg_has_role(current_user, 'tokenpivot_app', 'USAGE')").Scan(&appPrivileges)
	closeErr := app.Close(ctx)
	if err != nil || closeErr != nil || !appPrivileges {
		t.Fatal("test service role must have tokenpivot_app privileges")
	}
	schema := "t050_" + fmt.Sprintf("%x", rand.Text()[:12])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := owner.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(database.SafeError("create owned schema", err))
	}
	t.Logf("owned schema=%s database=%s migrator=%s app=%s", schema, db, role, appRole)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var actual string
		err := owner.QueryRow(cleanupCtx, "SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname=$1", schema).Scan(&actual)
		if err != nil || actual != role || !strings.HasPrefix(schema, "t050_") {
			t.Errorf("preserving schema %s: ownership check failed", schema)
			return
		}
		if _, err := owner.Exec(cleanupCtx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("preserving schema %s: cleanup failed", schema)
		}
	})
	if _, err := owner.Exec(ctx, "GRANT USAGE ON SCHEMA "+quoted+" TO "+pgx.Identifier{appRole}.Sanitize()); err != nil {
		t.Fatal(database.SafeError("grant usage", err))
	}
	for i, u := range urls {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		values[i] = u.String()
	}
	return values[0], values[1]
}

func testProvider(t *testing.T, value string, files fs.FS) (*goose.Provider, *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := openDB(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(database.SafeError("close", err))
		}
	})
	p, err := newProvider(db, files)
	if err != nil {
		t.Fatal(err)
	}
	if db.Stats().MaxOpenConnections != 1 {
		t.Fatal("migration pool is not one connection")
	}
	return p, db
}

func TestSingleConnectionMatrix(t *testing.T) {
	for _, first := range []string{"status", "up"} {
		t.Run(first, func(t *testing.T) {
			value, appURL := integrationSchema(t)
			p, db := testProvider(t, value, migrations.Files)
			// Derive the expectations from the embedded migrations so later
			// migrations do not need to edit this test again.
			sources := p.ListSources()
			if len(sources) < 2 {
				t.Fatal("fewer than two migration sources")
			}
			latest := sources[len(sources)-1].Version
			previous := sources[len(sources)-2].Version
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var before, after int
			if err := db.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&before); err != nil {
				t.Fatal("pid query failed")
			}
			var out bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&out, nil))
			for _, command := range []string{first, "up", "up", "down-one", "up"} {
				start := time.Now()
				if err := execute(ctx, p, command, logger); err != nil {
					t.Fatal(err)
				}
				version, err := p.GetDBVersion(ctx)
				if err != nil {
					t.Fatal(database.SafeError("version", err))
				}
				want := latest
				switch command {
				case "status":
					want = 0
				case "down-one":
					want = previous
				}
				if version != want {
					t.Fatalf("version=%d want=%d", version, want)
				}
				// The auth tables exist only while migration 00005 is applied, and its
				// down-one must leave every earlier table in place.
				var authTables, usageMinuteStats, usageEvents, foundation bool
				if err := db.QueryRowContext(ctx, "SELECT to_regclass(current_schema()||'.users') IS NOT NULL, to_regclass(current_schema()||'.usage_minute_stats') IS NOT NULL, to_regclass(current_schema()||'.usage_events') IS NOT NULL, to_regclass(current_schema()||'.workspaces') IS NOT NULL").Scan(&authTables, &usageMinuteStats, &usageEvents, &foundation); err != nil {
					t.Fatal(database.SafeError("table set", err))
				}
				switch command {
				case "status":
					if authTables || usageMinuteStats || usageEvents || foundation {
						t.Fatal("business tables exist before up")
					}
				case "down-one":
					if authTables || !usageMinuteStats || !usageEvents || !foundation {
						t.Fatal("down-one did not only remove the auth tables")
					}
				default:
					if !authTables || !usageMinuteStats || !usageEvents || !foundation {
						t.Fatal("up did not create the auth, minute stats, usage event and foundation tables")
					}
				}
				t.Logf("command=%s exit=0 duration=%s version=%d", command, time.Since(start), version)
			}
			if !strings.Contains(out.String(), "no change") {
				t.Fatal("missing no change result")
			}
			if err := db.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&after); err != nil {
				t.Fatal("pid query failed")
			}
			stats := db.Stats()
			if before != after || stats.OpenConnections != 1 || stats.Idle != 1 || stats.WaitCount != 0 {
				t.Fatal("operation borrowed another connection or waited")
			}
			t.Logf("backend_pid=%d open=%d idle=%d wait_count=%d", after, stats.OpenConnections, stats.Idle, stats.WaitCount)
			app, err := pgx.Connect(ctx, appURL)
			if err != nil {
				t.Fatal(database.SafeError("app", err))
			}
			defer func() {
				if err := app.Close(context.Background()); err != nil {
					t.Error("close app failed")
				}
			}()
			for _, query := range []string{"CREATE TABLE forbidden (id bigint)", "INSERT INTO goose_db_version(version_id,is_applied) VALUES(999,true)"} {
				if _, err := app.Exec(ctx, query); err == nil {
					t.Fatal("service permissions too broad")
				}
			}
		})
	}
}

func fixture(sqlText string) fstest.MapFS {
	return fstest.MapFS{"00001_ok.sql": {Data: []byte("-- +goose Up\nSELECT 1;\n-- +goose Down\nSELECT 1;\n")}, "00002_probe.sql": {Data: []byte("-- +goose Up\n" + sqlText + "\n-- +goose Down\nSELECT 1;\n")}}
}

func TestTransactionRollbackAndLockConnection(t *testing.T) {
	value, _ := integrationSchema(t)
	files := fixture("CREATE TABLE rollback_probe(id bigint); SELECT 'synthetic-sensitive-marker'::integer;")
	p, db := testProvider(t, value, files)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	err := execute(ctx, p, "up", slog.New(slog.NewJSONHandler(&out, nil)))
	if err == nil || strings.Contains(err.Error()+out.String(), "synthetic-sensitive-marker") {
		t.Fatal("SQL failure missing or leaked")
	}
	var exists bool
	var version int
	if err := db.QueryRowContext(ctx, "SELECT to_regclass(current_schema()||'.rollback_probe') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatal("failed transaction object survived")
	}
	if err := db.QueryRowContext(ctx, "SELECT max(version_id) FROM goose_db_version").Scan(&version); err != nil || version != 1 {
		t.Fatal("incorrect failed transaction history")
	}
	// This SQL runs inside Provider's migration transaction, not on an assertion connection.
	probe := fixture("CREATE TABLE lock_probe AS SELECT pg_backend_pid() AS pid, EXISTS (SELECT 1 FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory' AND granted AND objid=4097083626::oid) AS locked;")
	p2, err := newProvider(db, probe)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p2.Up(ctx); err != nil {
		t.Fatal(database.SafeError("probe", err))
	}
	var pid, current int
	var locked bool
	if err := db.QueryRowContext(ctx, "SELECT pid,locked,pg_backend_pid() FROM lock_probe").Scan(&pid, &locked, &current); err != nil || !locked || pid != current || db.Stats().WaitCount != 0 {
		t.Fatal("SQL and session lock did not use same connection")
	}
	t.Logf("migration SQL and session lock backend_pid=%d wait_count=0", pid)
}

func TestLockContentionAndCancellation(t *testing.T) {
	value, _ := integrationSchema(t)
	p1, db1 := testProvider(t, value, fixture("SELECT pg_sleep(10);"))
	p2, db2 := testProvider(t, value, fixture("SELECT 1;"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p1.Up(ctx); done <- err }()
	waitForMigration(t, db2)
	start := time.Now()
	lockCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := p2.Up(lockCtx)
	stop()
	if err == nil {
		t.Fatal("competing migration unexpectedly acquired lock")
	}
	t.Logf("contender exit=1 duration=%s", time.Since(start))
	start = time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled migration succeeded")
		}
		t.Logf("cancel exit=1 duration=%s", time.Since(start))
	case <-time.After(10 * time.Second):
		t.Fatal("cancel cleanup did not finish")
	}
	if err := db1.Close(); err != nil {
		t.Fatal("canceled DB close failed")
	}
	waitForNoMigrationLock(t, db2)
	recoveryCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	var version int
	if err := db2.QueryRowContext(recoveryCtx, "SELECT max(version_id) FROM goose_db_version").Scan(&version); err != nil || version != 1 {
		t.Fatal("canceled migration history survived")
	}
	if _, err := p2.Up(recoveryCtx); err != nil {
		t.Fatal(database.SafeError("recover", err))
	}
	if err := db2.QueryRowContext(recoveryCtx, "SELECT max(version_id) FROM goose_db_version").Scan(&version); err != nil || version != 2 {
		t.Fatal("recovery migration did not apply")
	}
	if db2.Stats().WaitCount != 0 {
		t.Fatal("contender waited for second pool connection")
	}
}

func waitForMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var found bool
		err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid WHERE a.usename=current_user AND a.datname=current_database() AND a.pid<>pg_backend_pid() AND l.locktype='advisory' AND l.granted AND a.query LIKE '%pg_sleep(10)%' AND a.state='active')").Scan(&found)
		if err != nil {
			t.Fatal(database.SafeError("observe migration", err))
		}
		if found {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("migration did not reach sleep while holding lock")
		}
	}
}

func TestMigrationProcess(t *testing.T) {
	if os.Getenv("TOKENPIVOT_MIGRATION_TEST_PROCESS") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	db, err := openDB(ctx, os.Getenv("TOKENPIVOT_MIGRATION_DATABASE_URL"))
	if err != nil {
		os.Exit(1)
	}
	p, err := newProvider(db, fixture("CREATE TABLE killed_probe(id bigint); SELECT pg_sleep(10);"))
	if err != nil {
		os.Exit(1)
	}
	_, err = p.Up(ctx)
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestKilledMigrationReleasesLock(t *testing.T) {
	value, _ := integrationSchema(t)
	p, db := testProvider(t, value, migrations.Files)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMigrationProcess$")
	cmd.Env = []string{"TOKENPIVOT_MIGRATION_TEST_PROCESS=1", "TOKENPIVOT_MIGRATION_DATABASE_URL=" + value, "PGPASSFILE=/dev/null", "PGSERVICEFILE=/dev/null"}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal("start migration child failed")
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Error("child cleanup failed")
			}
			_ = cmd.Wait()
		}
	})
	waitForMigration(t, db)
	start := time.Now()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal("kill owned migration child failed")
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed child exited successfully")
	}
	waitForNoMigrationLock(t, db)
	if _, err := p.Up(ctx); err != nil {
		t.Fatal(database.SafeError("post kill recovery", err))
	}
	var exists bool
	if err := db.QueryRowContext(ctx, "SELECT to_regclass(current_schema()||'.killed_probe') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatal("killed migration object survived")
	}
	t.Logf("killed child recovery exit=0 duration=%s", time.Since(start))
}

func waitForNoMigrationLock(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var locked bool
		if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE a.usename=current_user AND a.datname=current_database() AND l.locktype='advisory' AND l.granted)").Scan(&locked); err != nil {
			t.Fatal("lock observation failed")
		}
		if !locked {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("owned migration lock not released")
		}
	}
}
