//go:build integration

package sources

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/dundunHa/TokenPivot/db/migrations"
	"github.com/dundunHa/TokenPivot/internal/credentials"
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

// credentialState is the database view of one credential row.
type credentialState struct {
	hash      []byte
	sourceID  ID
	revokedAt *time.Time
}

func insertCredential(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, sourceID ID, revoked bool) ID {
	t.Helper()
	var id ID
	query := "INSERT INTO ingest_credentials (workspace_id, source_id, secret_hash, revoked_at) VALUES ($1,$2,$3," + map[bool]string{true: "now() - interval '1 hour'", false: "NULL"}[revoked] + ") RETURNING id"
	if err := pool.QueryRow(ctx, query, workspaceID, sourceID, []byte(rand.Text()+rand.Text())).Scan(&id); err != nil {
		t.Fatal(database.SafeError("insert credential", err))
	}
	return id
}

func loadCredential(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id ID) credentialState {
	t.Helper()
	var state credentialState
	if err := pool.QueryRow(ctx, "SELECT secret_hash, source_id, revoked_at FROM ingest_credentials WHERE id=$1", id).Scan(&state.hash, &state.sourceID, &state.revokedAt); err != nil {
		t.Fatal(database.SafeError("load credential", err))
	}
	return state
}

func countActive(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sourceID ID) int {
	t.Helper()
	var total int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM ingest_credentials WHERE source_id=$1 AND revoked_at IS NULL", sourceID).Scan(&total); err != nil {
		t.Fatal(database.SafeError("count active", err))
	}
	return total
}

func TestSourcesCRUDAgainstPostgres(t *testing.T) {
	_, appURL := integrationSchema(t)
	pool := newPool(t, appURL)
	service := New(NewPostgres(pool))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workspaceA := insertWorkspace(t, ctx, pool, "synthetic-a")
	workspaceB := insertWorkspace(t, ctx, pool, "synthetic-b")

	if list, err := service.List(ctx, workspaceA); err != nil || list == nil || len(list) != 0 {
		t.Fatalf("empty list=%v err=%v", list, err)
	}
	created, err := service.Create(ctx, workspaceA, "dsh", "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	if created.WorkspaceID != workspaceA || created.LastReceivedAt != nil || created.ArchivedAt != nil || created.CreatedAt.IsZero() {
		t.Fatalf("created=%+v", created)
	}
	if _, err := service.Create(ctx, workspaceA, "dsh", "inst-1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate error=%v want ErrConflict", err)
	}
	other, err := service.Create(ctx, workspaceB, "dsh", "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Create(ctx, workspaceA, "pi", "inst-2")
	if err != nil {
		t.Fatal(err)
	}
	// Partial updates leave the other field and the identity alone.
	newPlatform := "pi-renamed"
	updated, err := service.Update(ctx, workspaceA, created.ID, &newPlatform, nil)
	if err != nil || updated.Platform != newPlatform || updated.InstanceKey != "inst-1" || updated.ID != created.ID {
		t.Fatalf("platform update=%+v err=%v", updated, err)
	}
	newKey := "inst-1b"
	updated, err = service.Update(ctx, workspaceA, created.ID, nil, &newKey)
	if err != nil || updated.Platform != newPlatform || updated.InstanceKey != newKey {
		t.Fatalf("key update=%+v err=%v", updated, err)
	}
	if again, err := service.Update(ctx, workspaceA, created.ID, &newPlatform, &newKey); err != nil || again.InstanceKey != newKey {
		t.Fatalf("repeat update=%+v err=%v", again, err)
	}
	// Moving onto another source identity of the same workspace conflicts.
	pi, inst2 := "pi", "inst-2"
	if _, err := service.Update(ctx, workspaceA, created.ID, &pi, &inst2); !errors.Is(err, ErrConflict) {
		t.Fatalf("update conflict error=%v want ErrConflict", err)
	}
	// Foreign ids are not found and untouched.
	if _, err := service.Update(ctx, workspaceA, other.ID, &pi, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign update error=%v want ErrNotFound", err)
	}
	if err := service.Archive(ctx, workspaceA, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign archive error=%v want ErrNotFound", err)
	}
	var platform string
	var archived *time.Time
	if err := pool.QueryRow(ctx, "SELECT platform, archived_at FROM sources WHERE id=$1", other.ID).Scan(&platform, &archived); err != nil || platform != "dsh" || archived != nil {
		t.Fatalf("foreign source changed: %q %v %v", platform, archived, err)
	}
	// last_received_at survives updates and archiving.
	stamp := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, "UPDATE sources SET last_received_at=$1 WHERE id=$2", stamp, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Archive(ctx, workspaceA, second.ID); err != nil {
		t.Fatal(err)
	}
	list, err := service.List(ctx, workspaceA)
	if err != nil || len(list) != 2 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	var archivedRow Source
	for _, row := range list {
		if row.ID == second.ID {
			archivedRow = row
		}
	}
	if archivedRow.ArchivedAt == nil || archivedRow.LastReceivedAt == nil || !archivedRow.LastReceivedAt.Equal(stamp) {
		t.Fatalf("archived row=%+v", archivedRow)
	}
	if _, err := service.Create(ctx, workspaceA, "pi", "inst-2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("recreate after archive error=%v want ErrConflict", err)
	}
}

func TestSourceArchiveRevokesOnlyActiveCredentials(t *testing.T) {
	_, appURL := integrationSchema(t)
	pool := newPool(t, appURL)
	service := New(NewPostgres(pool))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workspaceID := insertWorkspace(t, ctx, pool, "synthetic-archive")
	otherWorkspace := insertWorkspace(t, ctx, pool, "synthetic-other")
	source, err := service.Create(ctx, workspaceID, "dsh", "inst")
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := service.Create(ctx, workspaceID, "dsh", "sibling")
	if err != nil {
		t.Fatal(err)
	}
	active := insertCredential(t, ctx, pool, workspaceID, source.ID, false)
	alreadyRevoked := insertCredential(t, ctx, pool, workspaceID, source.ID, true)
	siblingCredential := insertCredential(t, ctx, pool, workspaceID, sibling.ID, false)
	beforeRevoked := loadCredential(t, ctx, pool, alreadyRevoked)
	beforeActive := loadCredential(t, ctx, pool, active)

	if err := service.Archive(ctx, workspaceID, source.ID); err != nil {
		t.Fatal(err)
	}
	afterActive := loadCredential(t, ctx, pool, active)
	if afterActive.revokedAt == nil || string(afterActive.hash) != string(beforeActive.hash) || afterActive.sourceID != source.ID {
		t.Fatalf("active credential after archive: %+v", afterActive)
	}
	afterRevoked := loadCredential(t, ctx, pool, alreadyRevoked)
	if afterRevoked.revokedAt == nil || !afterRevoked.revokedAt.Equal(*beforeRevoked.revokedAt) {
		t.Fatal("archive rewrote an earlier revoked_at")
	}
	if loadCredential(t, ctx, pool, siblingCredential).revokedAt != nil {
		t.Fatal("archive revoked another source's credential")
	}
	// A repeat keeps both stamps; a wrong-workspace call changes nothing.
	var firstArchive time.Time
	if err := pool.QueryRow(ctx, "SELECT archived_at FROM sources WHERE id=$1", source.ID).Scan(&firstArchive); err != nil {
		t.Fatal(err)
	}
	if err := service.Archive(ctx, otherWorkspace, source.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace archive error=%v want ErrNotFound", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := service.Archive(ctx, workspaceID, source.ID); err != nil {
		t.Fatal(err)
	}
	var secondArchive time.Time
	if err := pool.QueryRow(ctx, "SELECT archived_at FROM sources WHERE id=$1", source.ID).Scan(&secondArchive); err != nil || !firstArchive.Equal(secondArchive) {
		t.Fatalf("archive stamp changed: %v -> %v err=%v", firstArchive, secondArchive, err)
	}
	if got := loadCredential(t, ctx, pool, active).revokedAt; got == nil || !got.Equal(*afterActive.revokedAt) {
		t.Fatal("repeat archive reset revoked_at")
	}
	var credentialRows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM ingest_credentials WHERE source_id=$1", source.ID).Scan(&credentialRows); err != nil || credentialRows != 2 {
		t.Fatalf("credential rows=%d err=%v want 2 (history kept)", credentialRows, err)
	}
}

// failingTx fails one named step so the real transaction wrapper below can be
// proven to roll the earlier steps back.
type failingStep struct {
	archiveTx
	failAt string
}

func (f failingStep) markArchived(ctx context.Context, workspaceID, sourceID ID) error {
	if f.failAt == "mark" {
		return errors.New("synthetic failure")
	}
	return f.archiveTx.markArchived(ctx, workspaceID, sourceID)
}

func (f failingStep) revokeCredentials(ctx context.Context, workspaceID, sourceID ID) error {
	if f.failAt == "revoke" {
		return errors.New("synthetic failure")
	}
	return f.archiveTx.revokeCredentials(ctx, workspaceID, sourceID)
}

func TestSourceArchiveRollsBackOnStepFailure(t *testing.T) {
	_, appURL := integrationSchema(t)
	pool := newPool(t, appURL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workspaceID := insertWorkspace(t, ctx, pool, "synthetic-rollback")
	service := New(NewPostgres(pool))
	for _, failAt := range []string{"mark", "revoke"} {
		source, err := service.Create(ctx, workspaceID, "dsh", "inst-"+failAt)
		if err != nil {
			t.Fatal(err)
		}
		credential := insertCredential(t, ctx, pool, workspaceID, source.ID, false)
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			t.Fatal(err)
		}
		err = archive(ctx, failingStep{archiveTx: &pgTx{tx: tx}, failAt: failAt}, workspaceID, source.ID)
		if err == nil {
			t.Fatalf("%s: failure was swallowed", failAt)
		}
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
			t.Fatal(rollbackErr)
		}
		var archived *time.Time
		if err := pool.QueryRow(ctx, "SELECT archived_at FROM sources WHERE id=$1", source.ID).Scan(&archived); err != nil || archived != nil {
			t.Fatalf("%s: archived_at survived rollback: %v %v", failAt, archived, err)
		}
		if loadCredential(t, ctx, pool, credential).revokedAt != nil {
			t.Fatalf("%s: credential revoked despite rollback", failAt)
		}
	}
}

// insertGate pauses a credential INSERT after the transaction has already taken
// the source row lock. It is a test-side pgx query tracer on a dedicated pool, so
// no production hook or exported API is involved: Issue and Rotate run unchanged
// and simply stop between "lock the source" and "insert the credential".
type insertGate struct {
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newInsertGate() *insertGate {
	return &insertGate{reached: make(chan struct{}, 1), release: make(chan struct{})}
}

func (g *insertGate) arm() { g.armed.Store(true) }

// open lets the paused INSERT continue; it is safe to call more than once.
func (g *insertGate) open() { g.once.Do(func() { close(g.release) }) }

func (g *insertGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "INSERT INTO ingest_credentials") && g.armed.CompareAndSwap(true, false) {
		g.reached <- struct{}{}
		// A cancelled test context must not leave the connection parked here.
		select {
		case <-g.release:
		case <-ctx.Done():
		}
	}
	return ctx
}

func (g *insertGate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// newGatedPool opens a service-role pool whose connections run the gate tracer.
func newGatedPool(t *testing.T, value string, gate *insertGate) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(value)
	if err != nil {
		t.Fatal(database.SafeError("parse app URL", err))
	}
	config.MaxConns = 6
	config.ConnConfig.Tracer = gate
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(database.SafeError("open gated pool", err))
	}
	t.Cleanup(pool.Close)
	// Registered after the pool, so it runs first and never leaves a goroutine
	// parked on the gate while the pool closes.
	t.Cleanup(gate.open)
	return pool
}

// waitForLockWaiter blocks until a backend running a statement that starts with
// queryPrefix is waiting on a row lock, so the test knows that exact call is
// queued behind the lock holder.
func waitForLockWaiter(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queryPrefix string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE $1", queryPrefix+"%FOR UPDATE").Scan(&waiting); err != nil {
			t.Fatal(database.SafeError("lock waiters", err))
		}
		if waiting > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no backend running %q queued behind the source row lock", queryPrefix)
}

type issueOutcome struct {
	issued credentials.IssuedCredential
	err    error
}

// TestArchiveAndCredentialWriteLockOrders drives the four deterministic lock
// orders with the real credentials.Service (Issue and Rotate) and the real source
// archive: the credential write holds the source row lock first, or the archive
// does. Each order waits until the second call is observably queued before the
// lock is released.
func TestArchiveAndCredentialWriteLockOrders(t *testing.T) {
	_, appURL := integrationSchema(t)
	plain := newPool(t, appURL)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	workspaceID := insertWorkspace(t, ctx, plain, "synthetic-lock-orders")
	sourceService := New(NewPostgres(plain))
	plainCredentials := credentials.New(credentials.NewPostgres(plain))

	for _, op := range []string{"issue", "rotate"} {
		for _, order := range []string{"credential first", "archive first"} {
			t.Run(op+"/"+order, func(t *testing.T) {
				// Each scenario owns its gate, gated pool and service: the gate's
				// release channel closes once, so sharing it would let a later
				// scenario pass the pause point without ever holding the lock.
				gate := newInsertGate()
				gatedCredentials := credentials.New(credentials.NewPostgres(newGatedPool(t, appURL, gate)))
				source, err := sourceService.Create(ctx, workspaceID, "dsh", op+"-"+order)
				if err != nil {
					t.Fatal(err)
				}
				old, err := plainCredentials.Issue(ctx, workspaceID, source.ID)
				if err != nil {
					t.Fatal(err)
				}
				call := func(service *credentials.Service) issueOutcome {
					var out issueOutcome
					if op == "issue" {
						out.issued, out.err = service.Issue(ctx, workspaceID, source.ID)
					} else {
						out.issued, out.err = service.Rotate(ctx, workspaceID, old.ID)
					}
					return out
				}
				requireNoActive := func() {
					if got := countActive(t, ctx, plain, source.ID); got != 0 {
						t.Fatalf("active credentials after archive=%d want 0", got)
					}
				}
				requireUnauthenticated := func(token string) {
					if _, err := plainCredentials.Authenticate(ctx, token); !errors.Is(err, credentials.ErrUnauthenticated) {
						t.Fatalf("authenticate after archive error=%v want ErrUnauthenticated", err)
					}
				}

				if order == "credential first" {
					// The credential call locks the source and pauses before its
					// INSERT; the archive must queue on the same lock.
					gate.arm()
					written := make(chan issueOutcome, 1)
					go func() { written <- call(gatedCredentials) }()
					select {
					case <-gate.reached:
					case <-time.After(10 * time.Second):
						t.Fatal("credential call never reached its INSERT")
					}
					archived := make(chan error, 1)
					go func() { archived <- sourceService.Archive(ctx, workspaceID, source.ID) }()
					waitForLockWaiter(t, ctx, plain, "SELECT 1 FROM sources")
					select {
					case err := <-archived:
						t.Fatalf("archive finished while the credential write held the lock: %v", err)
					case <-time.After(150 * time.Millisecond):
					}
					gate.open()
					out := <-written
					if out.err != nil {
						t.Fatalf("%s error=%v want success (it locked first)", op, out.err)
					}
					if err := <-archived; err != nil {
						t.Fatal(err)
					}
					// The credential that won the race is revoked by the archive.
					requireNoActive()
					requireUnauthenticated(out.issued.Token)
					requireUnauthenticated(old.Token)
					return
				}

				// Archive first: the archive steps run in an open transaction that
				// keeps the lock; the credential call must queue on it.
				holder, err := plain.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = holder.Rollback(context.WithoutCancel(ctx)) }()
				if err := archive(ctx, &pgTx{tx: holder}, workspaceID, source.ID); err != nil {
					t.Fatal(err)
				}
				written := make(chan issueOutcome, 1)
				go func() { written <- call(plainCredentials) }()
				waitForLockWaiter(t, ctx, plain, "SELECT archived_at FROM sources")
				select {
				case out := <-written:
					t.Fatalf("%s finished while the archive held the lock: %v", op, out.err)
				case <-time.After(150 * time.Millisecond):
				}
				if err := holder.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				out := <-written
				if !errors.Is(out.err, credentials.ErrSourceArchived) {
					t.Fatalf("%s error=%v want ErrSourceArchived", op, out.err)
				}
				requireNoActive()
				var total int
				if err := plain.QueryRow(ctx, "SELECT count(*) FROM ingest_credentials WHERE source_id=$1", source.ID).Scan(&total); err != nil || total != 1 {
					t.Fatalf("credential rows=%d err=%v want 1 (nothing inserted)", total, err)
				}
				requireUnauthenticated(old.Token)
			})
		}
	}
}

// TestArchiveAndIssueConcurrentStress adds unordered pressure on top of the
// deterministic lock orders above; it does not replace them.
func TestArchiveAndIssueConcurrentStress(t *testing.T) {
	_, appURL := integrationSchema(t)
	pool := newPool(t, appURL)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	workspaceID := insertWorkspace(t, ctx, pool, "synthetic-stress")
	sourceService := New(NewPostgres(pool))
	credentialService := credentials.New(credentials.NewPostgres(pool))
	for round := 0; round < 10; round++ {
		source, err := sourceService.Create(ctx, workspaceID, "dsh", fmt.Sprintf("stress-%d", round))
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := credentialService.Issue(ctx, workspaceID, source.ID); err != nil && !errors.Is(err, credentials.ErrSourceArchived) {
					t.Errorf("issue error=%v", err)
				}
			}()
		}
		archiveErr := make(chan error, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			archiveErr <- sourceService.Archive(ctx, workspaceID, source.ID)
		}()
		close(start)
		wg.Wait()
		if err := <-archiveErr; err != nil {
			t.Fatal(err)
		}
		if got := countActive(t, ctx, pool, source.ID); got != 0 {
			t.Fatalf("round %d: active credentials after archive=%d want 0", round, got)
		}
	}
}
