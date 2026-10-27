//go:build integration

package credentials

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
	"github.com/jackc/pgx/v5/pgtype"
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
	schema := "t010_" + fmt.Sprintf("%x", rand.Text()[:12])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := owner.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		_ = app.Close(ctx)
		t.Fatal(database.SafeError("create owned schema", err))
	}
	if _, err := owner.Exec(ctx, "GRANT USAGE ON SCHEMA "+quoted+" TO "+pgx.Identifier{appRole}.Sanitize()); err != nil {
		_ = app.Close(ctx)
		t.Fatal(database.SafeError("grant usage", err))
	}
	t.Logf("owned schema=%s database=%s migrator=%s app=%s", schema, db, role, appRole)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var actual string
		err := owner.QueryRow(cleanupCtx, "SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname=$1", schema).Scan(&actual)
		if err != nil || actual != role || !strings.HasPrefix(schema, "t010_") {
			t.Errorf("preserving schema %s: ownership check failed", schema)
			return
		}
		if _, err := owner.Exec(cleanupCtx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("preserving schema %s: cleanup failed", schema)
		}
	})
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

func insertSource(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID ID) ID {
	t.Helper()
	var id ID
	if err := pool.QueryRow(ctx, "INSERT INTO sources (workspace_id, platform, instance_key) VALUES ($1,$2,$3) RETURNING id",
		workspaceID, "synthetic-platform", fmt.Sprintf("instance-%x", rand.Text()[:8])).Scan(&id); err != nil {
		t.Fatal(database.SafeError("insert source", err))
	}
	return id
}

// storedRow is the database view of one credential row.
type storedRow struct {
	WorkspaceID ID
	SourceID    ID
	RevokedAt   *time.Time
}

func loadRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, credentialID ID) storedRow {
	t.Helper()
	var row storedRow
	var revokedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, "SELECT workspace_id, source_id, revoked_at FROM ingest_credentials WHERE id=$1", credentialID).
		Scan(&row.WorkspaceID, &row.SourceID, &revokedAt); err != nil {
		t.Fatal(database.SafeError("load credential", err))
	}
	if revokedAt.Valid {
		stamp := revokedAt.Time
		row.RevokedAt = &stamp
	}
	return row
}

func TestAuthenticateAgainstPostgres(t *testing.T) {
	_, appURL := integrationSchema(t)
	pool := newPool(t, appURL)
	service := New(NewPostgres(pool))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workspaceID := insertWorkspace(t, ctx, pool, "synthetic-auth")
	sourceID := insertSource(t, ctx, pool, workspaceID)

	issued, err := service.Issue(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if issued.Token == "" || !strings.HasPrefix(issued.Token, tokenPrefix) {
		t.Fatal("issue did not return a token")
	}
	var storedLength int
	var storedDigest []byte
	if err := pool.QueryRow(ctx, "SELECT octet_length(secret_hash), secret_hash FROM ingest_credentials WHERE id=$1", issued.ID).Scan(&storedLength, &storedDigest); err != nil {
		t.Fatal(database.SafeError("read digest", err))
	}
	if storedLength != 32 {
		t.Fatalf("stored digest length=%d want=32", storedLength)
	}
	digest := secretHash(issued.Token)
	if !bytes.Equal(storedDigest, digest[:]) {
		t.Fatal("stored digest is not the SHA-256 of the full token")
	}
	if bytes.Contains(storedDigest, []byte(issued.Token)) {
		t.Fatal("stored digest contains the plaintext token")
	}

	authenticated, err := service.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	if authenticated.WorkspaceID.Bytes != workspaceID.Bytes || authenticated.SourceID.Bytes != sourceID.Bytes || authenticated.ID.Bytes != issued.ID.Bytes {
		t.Fatal("authenticate did not return the stored ownership")
	}
	if authenticated.RevokedAt != nil {
		t.Fatal("active credential reported as revoked")
	}

	if err := service.Revoke(ctx, workspaceID, issued.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, issued.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked token error=%v want ErrUnauthenticated", err)
	}
	if _, err := service.Authenticate(ctx, tokenPrefix+"synthetic-absent"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("unknown token error=%v want ErrUnauthenticated", err)
	}
	if _, err := service.Authenticate(ctx, "synthetic-no-prefix"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("malformed token error=%v want ErrUnauthenticated", err)
	}
	t.Logf("digest_bytes=%d authenticate/revoke paths verified", storedLength)
}

func TestListIsolationAgainstPostgres(t *testing.T) {
	_, appURL := integrationSchema(t)
	pool := newPool(t, appURL)
	service := New(NewPostgres(pool))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workspaceA := insertWorkspace(t, ctx, pool, "synthetic-list-a")
	sourceA := insertSource(t, ctx, pool, workspaceA)
	workspaceB := insertWorkspace(t, ctx, pool, "synthetic-list-b")
	sourceB := insertSource(t, ctx, pool, workspaceB)

	first, err := service.Issue(ctx, workspaceA, sourceA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Issue(ctx, workspaceA, sourceA)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := service.Issue(ctx, workspaceB, sourceB)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Revoke(ctx, workspaceA, first.ID); err != nil {
		t.Fatal(err)
	}

	list, err := service.List(ctx, workspaceA, sourceA)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("listed credentials=%d want=2", len(list))
	}
	seen := make(map[[16]byte]bool, len(list))
	for _, credential := range list {
		if credential.WorkspaceID.Bytes != workspaceA.Bytes || credential.SourceID.Bytes != sourceA.Bytes {
			t.Fatal("list returned a credential with foreign ownership")
		}
		seen[credential.ID.Bytes] = true
	}
	if !seen[second.ID.Bytes] {
		t.Fatal("list is missing the active credential")
	}
	if !seen[first.ID.Bytes] {
		t.Fatal("list is missing the revoked credential")
	}
	if seen[foreign.ID.Bytes] {
		t.Fatal("list returned another workspace's credential")
	}

	for _, scope := range []struct {
		name        string
		workspaceID ID
		sourceID    ID
	}{
		{"other workspace", workspaceB, sourceA},
		{"other source", workspaceA, sourceB},
	} {
		crossList, err := service.List(ctx, scope.workspaceID, scope.sourceID)
		if err != nil {
			t.Fatalf("%s list error=%v", scope.name, err)
		}
		if len(crossList) != 0 {
			t.Fatalf("%s list returned %d credentials", scope.name, len(crossList))
		}
	}
	t.Logf("listed=%d cross-space lists empty", len(list))
}

func TestCrossWorkspaceRotateRevokeAgainstPostgres(t *testing.T) {
	_, appURL := integrationSchema(t)
	pool := newPool(t, appURL)
	service := New(NewPostgres(pool))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workspaceA := insertWorkspace(t, ctx, pool, "synthetic-cross-a")
	sourceA := insertSource(t, ctx, pool, workspaceA)
	workspaceB := insertWorkspace(t, ctx, pool, "synthetic-cross-b")

	issued, err := service.Issue(ctx, workspaceA, sourceA)
	if err != nil {
		t.Fatal(err)
	}
	before := loadRow(t, ctx, pool, issued.ID)
	if before.RevokedAt != nil {
		t.Fatal("setup credential is already revoked")
	}
	if _, err := service.Rotate(ctx, workspaceB, issued.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-space rotate error=%v want ErrNotFound", err)
	}
	if err := service.Revoke(ctx, workspaceB, issued.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-space revoke error=%v want ErrNotFound", err)
	}
	after := loadRow(t, ctx, pool, issued.ID)
	if after.RevokedAt != nil || after.WorkspaceID.Bytes != workspaceA.Bytes || after.SourceID.Bytes != sourceA.Bytes {
		t.Fatal("cross-space request changed the original row")
	}
	var total int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM ingest_credentials WHERE workspace_id=$1", workspaceA).Scan(&total); err != nil {
		t.Fatal(database.SafeError("count credentials", err))
	}
	if total != 1 {
		t.Fatalf("credential rows=%d want=1 after rejected cross-space requests", total)
	}
	if _, err := service.Authenticate(ctx, issued.Token); err != nil {
		t.Fatalf("credential stopped working after rejected cross-space requests: %v", err)
	}
	if _, err := service.Issue(ctx, workspaceB, sourceA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign source issue error=%v want ErrNotFound", err)
	}
	t.Logf("cross-space operations returned not_found and left rows unchanged")
}

func TestConcurrentRevokeAgainstPostgres(t *testing.T) {
	_, appURL := integrationSchema(t)
	pool := newPool(t, appURL)
	service := New(NewPostgres(pool))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	workspaceID := insertWorkspace(t, ctx, pool, "synthetic-concurrent")
	sourceID := insertSource(t, ctx, pool, workspaceID)
	issued, err := service.Issue(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 8
	start := make(chan struct{})
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- service.Revoke(ctx, workspaceID, issued.ID)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent revoke error=%v", err)
		}
	}
	row := loadRow(t, ctx, pool, issued.ID)
	if row.RevokedAt == nil {
		t.Fatal("concurrent revoke did not stamp revoked_at")
	}
	first := *row.RevokedAt
	if err := service.Revoke(ctx, workspaceID, issued.ID); err != nil {
		t.Fatalf("repeated revoke error=%v", err)
	}
	if again := loadRow(t, ctx, pool, issued.ID); again.RevokedAt == nil || !again.RevokedAt.Equal(first) {
		t.Fatal("repeated revoke changed revoked_at")
	}
	if _, err := service.Authenticate(ctx, issued.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked token error=%v want ErrUnauthenticated", err)
	}
	t.Logf("concurrent workers=%d all succeeded with one revoked_at", workers)
}

func TestRevokeRotateCommitOrdersAgainstPostgres(t *testing.T) {
	_, appURL := integrationSchema(t)
	pool := newPool(t, appURL)
	service := New(NewPostgres(pool))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workspaceID := insertWorkspace(t, ctx, pool, "synthetic-order")
	sourceID := insertSource(t, ctx, pool, workspaceID)

	t.Run("rotate then revoke", func(t *testing.T) {
		old, err := service.Issue(ctx, workspaceID, sourceID)
		if err != nil {
			t.Fatal(err)
		}
		rotated, err := service.Rotate(ctx, workspaceID, old.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rotated.WorkspaceID.Bytes != old.WorkspaceID.Bytes || rotated.SourceID.Bytes != old.SourceID.Bytes {
			t.Fatal("rotation changed the fixed ownership")
		}
		if _, err := service.Authenticate(ctx, old.Token); err != nil {
			t.Fatalf("old token after rotation error=%v want nil", err)
		}
		if err := service.Revoke(ctx, workspaceID, old.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Authenticate(ctx, old.Token); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("old token after revoke error=%v want ErrUnauthenticated", err)
		}
		if _, err := service.Authenticate(ctx, rotated.Token); err != nil {
			t.Fatalf("new token error=%v want nil", err)
		}
	})

	t.Run("revoke then rotate", func(t *testing.T) {
		old, err := service.Issue(ctx, workspaceID, sourceID)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Revoke(ctx, workspaceID, old.ID); err != nil {
			t.Fatal(err)
		}
		revokedRow := loadRow(t, ctx, pool, old.ID)
		if revokedRow.RevokedAt == nil {
			t.Fatal("setup revoke did not stamp revoked_at")
		}
		firstRevokedAt := *revokedRow.RevokedAt
		rotated, err := service.Rotate(ctx, workspaceID, old.ID)
		if err != nil {
			t.Fatal(err)
		}
		afterRow := loadRow(t, ctx, pool, old.ID)
		if afterRow.RevokedAt == nil || !afterRow.RevokedAt.Equal(firstRevokedAt) {
			t.Fatal("rotation cleared or changed the old revoked_at")
		}
		if _, err := service.Authenticate(ctx, old.Token); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("old token error=%v want ErrUnauthenticated", err)
		}
		if _, err := service.Authenticate(ctx, rotated.Token); err != nil {
			t.Fatalf("new token error=%v want nil", err)
		}
	})
}

func TestRevokeKeepsSourceAndOtherRows(t *testing.T) {
	_, appURL := integrationSchema(t)
	pool := newPool(t, appURL)
	service := New(NewPostgres(pool))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workspaceID := insertWorkspace(t, ctx, pool, "synthetic-keep")
	sourceID := insertSource(t, ctx, pool, workspaceID)
	revoked, err := service.Issue(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	active, err := service.Issue(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Revoke(ctx, workspaceID, revoked.ID); err != nil {
		t.Fatal(err)
	}
	var sourceCount, credentialCount int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM sources WHERE id=$1 AND workspace_id=$2), (SELECT count(*) FROM ingest_credentials WHERE workspace_id=$2)", sourceID, workspaceID).
		Scan(&sourceCount, &credentialCount); err != nil {
		t.Fatal(database.SafeError("count rows", err))
	}
	if sourceCount != 1 {
		t.Fatalf("source rows=%d want=1", sourceCount)
	}
	if credentialCount != 2 {
		t.Fatalf("credential rows=%d want=2", credentialCount)
	}
	if row := loadRow(t, ctx, pool, active.ID); row.RevokedAt != nil {
		t.Fatal("revoking one credential also revoked another")
	}
	if _, err := service.Authenticate(ctx, active.Token); err != nil {
		t.Fatalf("remaining active token error=%v want nil", err)
	}
	t.Logf("revoke left source and the other credential intact")
}
