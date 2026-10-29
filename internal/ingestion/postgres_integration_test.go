//go:build integration

package ingestion

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dundunHa/TokenPivot/db/migrations"
	"github.com/dundunHa/TokenPivot/internal/aggregation"
	"github.com/dundunHa/TokenPivot/internal/credentials"
	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/dundunHa/TokenPivot/internal/projects"
	"github.com/dundunHa/TokenPivot/internal/sources"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// minuteColumns is the fixed metric order used when reading a minute bucket back.
var minuteColumns = []string{"input_tokens", "output_tokens", "total_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"}

// integrationEnv is one per-test schema in the dedicated test database. Only
// non-sensitive metadata is ever logged: the schema, database and role names.
type integrationEnv struct {
	ownerURL string
	appURL   string
	schema   string
	owner    *pgx.Conn
	appRole  string
}

// newIntegrationEnv creates a random schema, verifies both roles, applies every
// released migration and grants the service role schema usage. Missing or unsafe
// integration variables fail the test; nothing is skipped.
func newIntegrationEnv(t *testing.T) *integrationEnv {
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
		parsed, err := url.Parse(value)
		if err != nil {
			t.Fatal("invalid test URL")
		}
		ip := net.ParseIP(parsed.Hostname())
		if ip == nil || !ip.IsLoopback() || !strings.HasSuffix(strings.TrimPrefix(parsed.Path, "/"), "_test") {
			t.Fatal("test database must use a loopback IP and _test database suffix")
		}
		for key := range parsed.Query() {
			if key != "sslmode" && key != "search_path" {
				t.Fatal("test URL has unsupported query option")
			}
		}
		urls[i] = parsed
	}
	if urls[0].Host != urls[1].Host || urls[0].Path != urls[1].Path || urls[0].User.Username() == urls[1].User.Username() {
		t.Fatal("test roles must be distinct in the same database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
	var dbName, ownerRole string
	var unsafe bool
	if err := owner.QueryRow(ctx, "SELECT current_database(),current_user,rolsuper OR rolcreaterole FROM pg_roles WHERE rolname=current_user").Scan(&dbName, &ownerRole, &unsafe); err != nil {
		t.Fatal(database.SafeError("identity", err))
	}
	if dbName != strings.TrimPrefix(urls[0].Path, "/") || ownerRole != urls[0].User.Username() || unsafe {
		t.Fatal("unsafe test migration role")
	}
	appRole := verifyIngestionAppRole(t, ctx, values[1], urls[1].User.Username())
	schema := "t013_" + hex.EncodeToString(randomBytes(t, 6))
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := owner.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(database.SafeError("create owned schema", err))
	}
	// Registered right after CREATE SCHEMA succeeds so a later failure still tries a
	// checked cleanup; the closure only uses values already in scope.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var actual string
		err := owner.QueryRow(cleanupCtx, "SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname=$1", schema).Scan(&actual)
		if err != nil || actual != ownerRole || !strings.HasPrefix(schema, "t013_") {
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
	t.Logf("owned schema=%s database=%s migrator=%s app=%s", schema, dbName, ownerRole, appRole)
	for i, parsed := range urls {
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		values[i] = parsed.String()
	}
	applyIngestionMigrations(t, values[0])
	// The owner connection predates the search_path override, so it is pinned here
	// explicitly. Every helper below relies on unqualified table names resolving to
	// the test schema, exactly like the application connection does.
	if _, err := owner.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		t.Fatal(database.SafeError("pin owner search path", err))
	}
	return &integrationEnv{ownerURL: values[0], appURL: values[1], schema: schema, owner: owner, appRole: appRole}
}

// verifyIngestionAppRole connects as the service test role and checks it is the
// expected unprivileged role that inherits tokenpivot_app.
func verifyIngestionAppRole(t *testing.T, ctx context.Context, appURL, wantRole string) string {
	t.Helper()
	app, err := pgx.Connect(ctx, appURL)
	if err != nil {
		t.Fatal(database.SafeError("service role connect", err))
	}
	defer func() {
		if err := app.Close(context.Background()); err != nil {
			t.Error(database.SafeError("app close", err))
		}
	}()
	var appRole string
	var unsafe bool
	if err := app.QueryRow(ctx, "SELECT current_user, rolsuper OR rolcreaterole OR rolcreatedb OR has_database_privilege(current_user,current_database(),'CREATE') FROM pg_roles WHERE rolname=current_user").Scan(&appRole, &unsafe); err != nil {
		t.Fatal(database.SafeError("service role identity", err))
	}
	if unsafe || appRole != wantRole {
		t.Fatal("unsafe test service role")
	}
	var inherited bool
	if err := app.QueryRow(ctx, "SELECT pg_has_role(current_user, 'tokenpivot_app', 'USAGE')").Scan(&inherited); err != nil {
		t.Fatal(database.SafeError("service role privileges", err))
	}
	if !inherited {
		t.Fatal("test service role must have tokenpivot_app privileges")
	}
	return appRole
}

// applyIngestionMigrations runs every released migration inside the pinned schema
// on one connection.
func applyIngestionMigrations(t *testing.T, migrationURL string) {
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

// randomBytes returns cryptographically random test material.
func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal("random bytes failed")
	}
	return b
}

// newIntegrationID returns a random version 4 UUID; the value is an opaque test
// identity, never a claim about a real row.
func newIntegrationID(t *testing.T) pgtype.UUID {
	t.Helper()
	b := randomBytes(t, 16)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var id pgtype.UUID
	id.Bytes = [16]byte(b)
	id.Valid = true
	return id
}

// pool opens one service-role pool for the schema and closes it at test end.
func (e *integrationEnv) pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(e.appURL)
	if err != nil {
		t.Fatal(database.SafeError("parse app URL", err))
	}
	config.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(database.SafeError("open app pool", err))
	}
	t.Cleanup(pool.Close)
	return pool
}

// service builds the ingestion service on the service-role pool. The logger writes
// to a buffer the caller can inspect for leaks.
func (e *integrationEnv) service(t *testing.T, pool *pgxpool.Pool, logs *syncBuffer) *Service {
	t.Helper()
	return New(NewPostgres(pool), &fakeAuthenticator{token: "tpk_integration"}, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// seedSource creates one workspace and one source and returns their identities.
func (e *integrationEnv) seedSource(t *testing.T, ctx context.Context) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	workspaceID := newIntegrationID(t)
	sourceID := newIntegrationID(t)
	if _, err := e.owner.Exec(ctx, "INSERT INTO workspaces (id, name) VALUES ($1,$2)", workspaceID, "synthetic workspace"); err != nil {
		t.Fatal(database.SafeError("seed workspace", err))
	}
	if _, err := e.owner.Exec(ctx, "INSERT INTO sources (id, workspace_id, platform, instance_key) VALUES ($1,$2,$3,$4)",
		sourceID, workspaceID, "synthetic-platform", "synthetic-instance-"+hex.EncodeToString(randomBytes(t, 4))); err != nil {
		t.Fatal(database.SafeError("seed source", err))
	}
	return workspaceID, sourceID
}

// seedProject creates one project in the workspace. A non-nil archivedAt marks it
// archived, which must not change ingestion behavior.
func (e *integrationEnv) seedProject(t *testing.T, ctx context.Context, workspaceID pgtype.UUID, archivedAt *time.Time) pgtype.UUID {
	t.Helper()
	projectID := newIntegrationID(t)
	if _, err := e.owner.Exec(ctx, "INSERT INTO projects (id, workspace_id, project_key, name, archived_at) VALUES ($1,$2,$3,$4,$5)",
		projectID, workspaceID, "project-"+hex.EncodeToString(randomBytes(t, 4)), "synthetic project", archivedAt); err != nil {
		t.Fatal(database.SafeError("seed project", err))
	}
	return projectID
}

// credential is the identity a batch is attributed to.
func credentialFor(workspaceID, sourceID pgtype.UUID) credentials.Credential {
	return credentials.Credential{WorkspaceID: workspaceID, SourceID: sourceID}
}

// storedRow is one usage_events row as the tests read it back.
type storedRow struct {
	projectID   pgtype.UUID
	provider    string
	model       string
	occurredAt  time.Time
	receivedAt  time.Time
	version     string
	payloadHash []byte
	values      [6]*int64
	qualities   [6]string
	status      *string
	durationMS  *int64
}

// eventRowSQL reads one detail row by its identity with every frozen column.
const eventRowSQL = `SELECT project_id, provider, model, occurred_at, received_at, normalization_version, payload_hash,
    input_tokens, input_tokens_quality, output_tokens, output_tokens_quality,
    total_tokens, total_tokens_quality, cache_read_tokens, cache_read_tokens_quality,
    cache_write_tokens, cache_write_tokens_quality, reasoning_tokens, reasoning_tokens_quality,
    status, duration_ms
FROM usage_events WHERE workspace_id=$1 AND source_id=$2 AND source_event_id=$3`

// readRow reads one stored detail row, failing when it is absent.
func (e *integrationEnv) readRow(t *testing.T, ctx context.Context, workspaceID, sourceID pgtype.UUID, sourceEventID string) storedRow {
	t.Helper()
	var row storedRow
	err := e.owner.QueryRow(ctx, eventRowSQL, workspaceID, sourceID, sourceEventID).Scan(
		&row.projectID, &row.provider, &row.model, &row.occurredAt, &row.receivedAt, &row.version, &row.payloadHash,
		&row.values[0], &row.qualities[0], &row.values[1], &row.qualities[1],
		&row.values[2], &row.qualities[2], &row.values[3], &row.qualities[3],
		&row.values[4], &row.qualities[4], &row.values[5], &row.qualities[5],
		&row.status, &row.durationMS,
	)
	if err != nil {
		t.Fatal(database.SafeError("read event row", err))
	}
	return row
}

// countRows counts the detail rows of one identity, so an accidental second insert
// is visible.
func (e *integrationEnv) countRows(t *testing.T, ctx context.Context, workspaceID, sourceID pgtype.UUID, sourceEventID string) int {
	t.Helper()
	var count int
	if err := e.owner.QueryRow(ctx, "SELECT count(*) FROM usage_events WHERE workspace_id=$1 AND source_id=$2 AND source_event_id=$3",
		workspaceID, sourceID, sourceEventID).Scan(&count); err != nil {
		t.Fatal(database.SafeError("count event rows", err))
	}
	return count
}

// bucketSelectSQL reads the minute bucket of one dimension set. The column list is
// a compile time constant, so no caller input reaches the SQL text.
var bucketSelectSQL = func() string {
	columns := []string{"event_count"}
	for _, metric := range minuteColumns {
		columns = append(columns, metric+"_sum", metric+"_known_count", metric+"_estimated_count", metric+"_missing_count")
	}
	return "SELECT " + strings.Join(columns, ",") + " FROM usage_minute_stats WHERE workspace_id=$1 AND bucket_start=$2 AND source_id=$3 AND project_id IS NOT DISTINCT FROM $4 AND provider=$5 AND model=$6"
}()

// minuteBucket is one stored aggregation row.
type minuteBucket struct {
	eventCount int64
	sum        [6]int64
	known      [6]int64
	estimated  [6]int64
	missing    [6]int64
}

// readBucket reads one minute bucket, failing when it is absent.
func (e *integrationEnv) readBucket(t *testing.T, ctx context.Context, workspaceID, sourceID, projectID pgtype.UUID, provider, model string, occurredAt time.Time) minuteBucket {
	t.Helper()
	var bucket minuteBucket
	targets := []any{&bucket.eventCount}
	for i := range minuteColumns {
		targets = append(targets, &bucket.sum[i], &bucket.known[i], &bucket.estimated[i], &bucket.missing[i])
	}
	err := e.owner.QueryRow(ctx, bucketSelectSQL,
		workspaceID, occurredAt.UTC().Truncate(time.Minute), sourceID, projectID, provider, model).Scan(targets...)
	if err != nil {
		t.Fatal(database.SafeError("read minute bucket", err))
	}
	return bucket
}

// countBuckets counts the minute rows of one workspace and source.
func (e *integrationEnv) countBuckets(t *testing.T, ctx context.Context, workspaceID, sourceID pgtype.UUID) int {
	t.Helper()
	var count int
	if err := e.owner.QueryRow(ctx, "SELECT count(*) FROM usage_minute_stats WHERE workspace_id=$1 AND source_id=$2",
		workspaceID, sourceID).Scan(&count); err != nil {
		t.Fatal(database.SafeError("count minute buckets", err))
	}
	return count
}

// lastReceivedAt reads the maintained source stamp.
func (e *integrationEnv) lastReceivedAt(t *testing.T, ctx context.Context, workspaceID, sourceID pgtype.UUID) *time.Time {
	t.Helper()
	var stamp *time.Time
	if err := e.owner.QueryRow(ctx, "SELECT last_received_at FROM sources WHERE workspace_id=$1 AND id=$2",
		workspaceID, sourceID).Scan(&stamp); err != nil {
		t.Fatal(database.SafeError("read source stamp", err))
	}
	return stamp
}

// setLastReceivedAt pins the source stamp so the GREATEST rule can be observed.
func (e *integrationEnv) setLastReceivedAt(t *testing.T, ctx context.Context, workspaceID, sourceID pgtype.UUID, stamp time.Time) {
	t.Helper()
	if _, err := e.owner.Exec(ctx, "UPDATE sources SET last_received_at=$1 WHERE workspace_id=$2 AND id=$3", stamp, workspaceID, sourceID); err != nil {
		t.Fatal(database.SafeError("pin source stamp", err))
	}
}

// acceptedEventJSON is one valid event with a known input and output value.
func acceptedEventJSON(sourceEventID string, occurredAt time.Time, projectID pgtype.UUID) string {
	project := "null"
	if projectID.Valid {
		project = `"` + uuidText(projectID) + `"`
	}
	return fmt.Sprintf(`{"source_event_id":%q,"project_id":%s,"provider":"example-provider","model":"example-model","occurred_at":%q,"usage":{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"}},"status":"success","duration_ms":1250}`,
		sourceEventID, project, occurredAt.UTC().Format(microLayout))
}

// uuidText renders an identity in the lower case hyphenated form.
func uuidText(id pgtype.UUID) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", id.Bytes[0:4], id.Bytes[4:6], id.Bytes[6:8], id.Bytes[8:10], id.Bytes[10:16])
}

// runBatch decodes and processes one batch, which is exactly what the handler does
// after its batch-wide gates.
func runBatch(t *testing.T, service *Service, credential credentials.Credential, body string) []result {
	t.Helper()
	items, detail, ok := decodeBatch([]byte(body))
	if !ok {
		t.Fatalf("batch decode failed: %+v", detail)
	}
	return service.process(context.Background(), credential, items)
}

// decodeOneBatch decodes one batch on the test goroutine, so a concurrency test can
// hand the result to a writer goroutine without a Fatalf there.
func decodeOneBatch(t *testing.T, body string) []item {
	t.Helper()
	items, detail, ok := decodeBatch([]byte(body))
	if !ok {
		t.Fatalf("batch decode failed: %+v", detail)
	}
	return items
}

func TestBatchAcceptsNewEvents(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	projectID := e.seedProject(t, ctx, workspaceID, nil)
	pool := e.pool(t)
	logs := &syncBuffer{}
	service := e.service(t, pool, logs)
	credential := credentialFor(workspaceID, sourceID)
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	results := runBatch(t, service, credential,
		encodedBatch(t, acceptedEventJSON("evt-new-1", occurredAt, projectID), acceptedEventJSON("evt-new-2", occurredAt.Add(time.Second), pgtype.UUID{})))
	requireResultStatuses(t, results, statusAccepted, statusAccepted)

	row := e.readRow(t, ctx, workspaceID, sourceID, "evt-new-1")
	if row.version != normalizationVersion {
		t.Fatalf("version=%q want=%q", row.version, normalizationVersion)
	}
	if row.projectID != projectID {
		t.Fatalf("project=%s want=%s", uuidText(row.projectID), uuidText(projectID))
	}
	if !row.occurredAt.UTC().Equal(occurredAt) {
		t.Fatalf("occurred_at=%s want=%s", row.occurredAt.UTC(), occurredAt)
	}
	if row.receivedAt.Before(row.occurredAt) {
		t.Fatalf("received_at=%s precedes occurred_at=%s", row.receivedAt, row.occurredAt)
	}
	if decoded := mustDecodeEvent(t, acceptedEventJSON("evt-new-1", occurredAt, projectID)); string(row.payloadHash) != string(decoded.PayloadHash[:]) {
		t.Fatal("stored payload_hash differs from the recomputed digest")
	}
	if row.status == nil || *row.status != "success" || row.durationMS == nil || *row.durationMS != 1250 {
		t.Fatalf("status=%v duration_ms=%v", row.status, row.durationMS)
	}
	// The derived total is stored as a real 120 value, not zero and not NULL.
	if row.values[2] == nil || *row.values[2] != 120 || row.qualities[2] != string(aggregation.QualityKnown) {
		t.Fatalf("total=%v quality=%q want 120/known", row.values[2], row.qualities[2])
	}
	unattributed := e.readRow(t, ctx, workspaceID, sourceID, "evt-new-2")
	if unattributed.projectID.Valid {
		t.Fatal("an absent project must stay NULL")
	}

	bucket := e.readBucket(t, ctx, workspaceID, sourceID, projectID, "example-provider", "example-model", occurredAt)
	if bucket.eventCount != 1 || bucket.sum[0] != 100 || bucket.sum[1] != 20 || bucket.sum[2] != 120 {
		t.Fatalf("bucket=%+v want event_count=1 sums 100/20/120", bucket)
	}
	if stamp := e.lastReceivedAt(t, ctx, workspaceID, sourceID); stamp == nil {
		t.Fatal("last_received_at was not maintained")
	}
	if logs.String() != "" {
		t.Fatalf("unexpected log output: %s", logs.String())
	}
}

func TestBatchMixedItemsIsolateFailures(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	foreignProject := e.seedProject(t, ctx, e.newWorkspace(t, ctx), nil)
	pool := e.pool(t)
	service := e.service(t, pool, &syncBuffer{})
	credential := credentialFor(workspaceID, sourceID)
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	results := runBatch(t, service, credential, encodedBatch(t,
		acceptedEventJSON("evt-mixed-1", occurredAt, pgtype.UUID{}),
		`{"source_event_id":"evt-mixed-bad","provider":"p","model":"m","occurred_at":"`+occurredAt.Format(microLayout)+`","usage":{"input_tokens":{"value":1.0,"quality":"known"}}}`,
		`{"source_event_id":"evt-mixed-foreign","project_id":"`+uuidText(foreignProject)+`","provider":"p","model":"m","occurred_at":"`+occurredAt.Format(microLayout)+`","usage":{}}`,
		acceptedEventJSON("evt-mixed-2", occurredAt, pgtype.UUID{}),
	))
	requireResultStatuses(t, results, statusAccepted, statusRejected, statusRejected, statusAccepted)
	requireResultCodes(t, results, "", codeInvalidArgument, codeProjectInvalid, "")
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-mixed-bad"); count != 0 {
		t.Fatalf("rejected item was stored %d times", count)
	}
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-mixed-foreign"); count != 0 {
		t.Fatalf("foreign project item was stored %d times", count)
	}
	if bucket := e.readBucket(t, ctx, workspaceID, sourceID, pgtype.UUID{}, "example-provider", "example-model", occurredAt); bucket.eventCount != 2 {
		t.Fatalf("event_count=%d want=2", bucket.eventCount)
	}
}

// newWorkspace creates one extra workspace so cross-workspace rules can be tested.
func (e *integrationEnv) newWorkspace(t *testing.T, ctx context.Context) pgtype.UUID {
	t.Helper()
	workspaceID := newIntegrationID(t)
	if _, err := e.owner.Exec(ctx, "INSERT INTO workspaces (id, name) VALUES ($1,$2)", workspaceID, "synthetic other workspace"); err != nil {
		t.Fatal(database.SafeError("seed other workspace", err))
	}
	return workspaceID
}

func TestDuplicateWithinAndAcrossBatches(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	projectID := e.seedProject(t, ctx, workspaceID, nil)
	pool := e.pool(t)
	service := e.service(t, pool, &syncBuffer{})
	credential := credentialFor(workspaceID, sourceID)
	occurredAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	pinned := time.Now().UTC().Add(-90 * time.Minute)
	e.setLastReceivedAt(t, ctx, workspaceID, sourceID, pinned)

	// The same identity twice in one batch: exactly one accepted, one duplicate,
	// and only one minute increment.
	event := acceptedEventJSON("evt-dup", occurredAt, projectID)
	results := runBatch(t, service, credential, encodedBatch(t, event, event))
	requireResultStatuses(t, results, statusAccepted, statusDuplicate)
	row := e.readRow(t, ctx, workspaceID, sourceID, "evt-dup")
	storedReceived := row.receivedAt
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-dup"); count != 1 {
		t.Fatalf("rows=%d want=1", count)
	}
	if bucket := e.readBucket(t, ctx, workspaceID, sourceID, projectID, "example-provider", "example-model", occurredAt); bucket.eventCount != 1 {
		t.Fatalf("event_count=%d want=1", bucket.eventCount)
	}
	moved := e.lastReceivedAt(t, ctx, workspaceID, sourceID)
	if moved == nil || !moved.After(pinned) {
		t.Fatalf("last_received_at=%v did not move past the pinned %s", moved, pinned)
	}

	// A later batch with the same identity and the same content: duplicate, no new
	// row, no new minute increment, and received_at is not renewed.
	results = runBatch(t, service, credential, encodedBatch(t, event))
	requireResultStatuses(t, results, statusDuplicate)
	if again := e.readRow(t, ctx, workspaceID, sourceID, "evt-dup"); !again.receivedAt.Equal(storedReceived) {
		t.Fatalf("received_at=%s want the original %s", again.receivedAt, storedReceived)
	}
	if bucket := e.readBucket(t, ctx, workspaceID, sourceID, projectID, "example-provider", "example-model", occurredAt); bucket.eventCount != 1 {
		t.Fatalf("event_count=%d want=1 after the retransmission", bucket.eventCount)
	}
	// A duplicate-only batch must not touch the source stamp.
	if stamp := e.lastReceivedAt(t, ctx, workspaceID, sourceID); stamp == nil || !stamp.Equal(*moved) {
		t.Fatalf("last_received_at=%v want the unchanged %s", stamp, moved)
	}
}

func TestSameIdentityDifferentContentIsConflict(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	pool := e.pool(t)
	service := e.service(t, pool, &syncBuffer{})
	credential := credentialFor(workspaceID, sourceID)
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	results := runBatch(t, service, credential, encodedBatch(t,
		acceptedEventJSON("evt-conflict", occurredAt, pgtype.UUID{}),
		`{"source_event_id":"evt-conflict","provider":"example-provider","model":"example-model","occurred_at":"`+occurredAt.Format(microLayout)+`","usage":{"input_tokens":{"value":101,"quality":"known"},"output_tokens":{"value":20,"quality":"known"}}}`,
	))
	requireResultStatuses(t, results, statusAccepted, statusConflict)
	// The stored row is never overwritten by the conflicting candidate.
	row := e.readRow(t, ctx, workspaceID, sourceID, "evt-conflict")
	if row.values[0] == nil || *row.values[0] != 100 {
		t.Fatalf("stored input=%v want the original 100", row.values[0])
	}
	if bucket := e.readBucket(t, ctx, workspaceID, sourceID, pgtype.UUID{}, "example-provider", "example-model", occurredAt); bucket.eventCount != 1 {
		t.Fatalf("event_count=%d want=1", bucket.eventCount)
	}
}

func TestExpiryPrecedesIdentityLookup(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	pool := e.pool(t)
	service := e.service(t, pool, &syncBuffer{})
	credential := credentialFor(workspaceID, sourceID)
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	// Seed the identity with a valid event, then resend it with an occurred_at
	// below the lower bound. If the duplicate lookup ran first this would be a
	// conflict; the frozen order makes it expired.
	results := runBatch(t, service, credential, encodedBatch(t, acceptedEventJSON("evt-expired", occurredAt, pgtype.UUID{})))
	requireResultStatuses(t, results, statusAccepted)
	stale := time.Now().UTC().Add(-retainedWindow - time.Hour).Truncate(time.Microsecond)
	results = runBatch(t, service, credential, encodedBatch(t, acceptedEventJSON("evt-expired", stale, pgtype.UUID{})))
	requireResultStatuses(t, results, statusExpired)
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-expired"); count != 1 {
		t.Fatalf("rows=%d want=1", count)
	}
}

func TestWindowBoundariesAgainstServerClock(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	pool := e.pool(t)
	service := e.service(t, pool, &syncBuffer{})
	credential := credentialFor(workspaceID, sourceID)

	results := runBatch(t, service, credential, encodedBatch(t,
		acceptedEventJSON("evt-future", time.Now().UTC().Add(time.Minute), pgtype.UUID{}),
	))
	requireResultStatuses(t, results, statusRejected)
	requireResultCodes(t, results, codeFutureEvent)
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-future"); count != 0 {
		t.Fatalf("future event was stored %d times", count)
	}

	// This checks the neighborhood of the lower bound, not the exact boundary: the
	// accepted event sits two seconds inside the bound and the expired one two
	// microseconds under it, so a small host/database clock difference cannot flip
	// either verdict. Exact inclusivity (exactly on the bound is accepted, one
	// microsecond under is expired) is covered by TestWindowBoundariesAreInclusive in
	// handler_test.go, which controls the server clock exactly.
	//
	// The bound is derived from the database clock, because that is the clock the
	// service compares against; a host-side now would be off by the host/database
	// clock difference on a non-native instance.
	var serverNow time.Time
	if err := e.owner.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&serverNow); err != nil {
		t.Fatal(database.SafeError("read server clock", err))
	}
	edge := serverNow.UTC().Truncate(time.Microsecond).Add(-retainedWindow)
	results = runBatch(t, service, credential, encodedBatch(t,
		acceptedEventJSON("evt-edge-inside", edge.Add(2*time.Second), pgtype.UUID{}),
		acceptedEventJSON("evt-edge-under", edge.Add(-2*time.Microsecond), pgtype.UUID{}),
	))
	requireResultStatuses(t, results, statusAccepted, statusExpired)
}

func TestConcurrentIdenticalEventsAcceptExactlyOnce(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	projectID := e.seedProject(t, ctx, workspaceID, nil)
	pool := e.pool(t)
	service := e.service(t, pool, &syncBuffer{})
	credential := credentialFor(workspaceID, sourceID)
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	event := acceptedEventJSON("evt-concurrent", occurredAt, projectID)

	// Both writers are released together by a channel; no sleep is involved. The
	// batch is decoded before the goroutines start so only the service runs there.
	items := decodeOneBatch(t, encodedBatch(t, event))
	start := make(chan struct{})
	results := make([][]result, 2)
	var wait sync.WaitGroup
	for i := range results {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			results[index] = service.process(context.Background(), credential, items)
		}(i)
	}
	close(start)
	wait.Wait()

	accepted, duplicate := 0, 0
	for _, batch := range results {
		for _, outcome := range batch {
			switch outcome.status {
			case statusAccepted:
				accepted++
			case statusDuplicate:
				duplicate++
			default:
				t.Fatalf("unexpected concurrent status %+v", outcome)
			}
		}
	}
	if accepted != 1 || duplicate != 1 {
		t.Fatalf("accepted=%d duplicate=%d want 1 and 1", accepted, duplicate)
	}
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-concurrent"); count != 1 {
		t.Fatalf("rows=%d want=1", count)
	}
	if bucket := e.readBucket(t, ctx, workspaceID, sourceID, projectID, "example-provider", "example-model", occurredAt); bucket.eventCount != 1 {
		t.Fatalf("event_count=%d want=1", bucket.eventCount)
	}
}

func TestConcurrentDifferentEventsShareOneMinuteBucket(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	projectID := e.seedProject(t, ctx, workspaceID, nil)
	pool := e.pool(t)
	service := e.service(t, pool, &syncBuffer{})
	credential := credentialFor(workspaceID, sourceID)
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute).Add(30 * time.Second)

	const writers = 4
	batches := make([][]item, writers)
	for i := range batches {
		batches[i] = decodeOneBatch(t, encodedBatch(t,
			acceptedEventJSON(fmt.Sprintf("evt-bucket-%d", i), occurredAt, projectID)))
	}
	start := make(chan struct{})
	outcomes := make([][]result, writers)
	var wait sync.WaitGroup
	for i := range outcomes {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			outcomes[index] = service.process(context.Background(), credential, batches[index])
		}(i)
	}
	close(start)
	wait.Wait()
	for i, batch := range outcomes {
		if len(batch) != 1 || batch[0].status != statusAccepted {
			t.Fatalf("writer %d: %+v", i, batch)
		}
	}

	bucket := e.readBucket(t, ctx, workspaceID, sourceID, projectID, "example-provider", "example-model", occurredAt)
	if bucket.eventCount != writers {
		t.Fatalf("event_count=%d want=%d", bucket.eventCount, writers)
	}
	if bucket.sum[0] != 100*writers || bucket.sum[1] != 20*writers || bucket.sum[2] != 120*writers {
		t.Fatalf("sums=%v want %d/%d/%d", bucket.sum[:3], 100*writers, 20*writers, 120*writers)
	}
}

func TestWorkspacesAreIsolated(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceA, sourceA := e.seedSource(t, ctx)
	workspaceB, sourceB := e.seedSource(t, ctx)
	projectB := e.seedProject(t, ctx, workspaceB, nil)
	pool := e.pool(t)
	service := e.service(t, pool, &syncBuffer{})
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	// The same source_event_id under two workspaces is two different events.
	results := runBatch(t, service, credentialFor(workspaceA, sourceA),
		encodedBatch(t, acceptedEventJSON("evt-shared", occurredAt, pgtype.UUID{})))
	requireResultStatuses(t, results, statusAccepted)
	results = runBatch(t, service, credentialFor(workspaceB, sourceB),
		encodedBatch(t, acceptedEventJSON("evt-shared", occurredAt, pgtype.UUID{})))
	requireResultStatuses(t, results, statusAccepted)

	// A project of the other workspace must not be attachable.
	results = runBatch(t, service, credentialFor(workspaceA, sourceA),
		encodedBatch(t, acceptedEventJSON("evt-cross-project", occurredAt, projectB)))
	requireResultStatuses(t, results, statusRejected)
	requireResultCodes(t, results, codeProjectInvalid)
	if count := e.countRows(t, ctx, workspaceA, sourceA, "evt-cross-project"); count != 0 {
		t.Fatalf("cross workspace item was stored %d times", count)
	}
}

func TestArchivedProjectStillAccepts(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	archivedAt := time.Now().UTC().Add(-24 * time.Hour)
	projectID := e.seedProject(t, ctx, workspaceID, &archivedAt)
	pool := e.pool(t)
	service := e.service(t, pool, &syncBuffer{})
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	results := runBatch(t, service, credentialFor(workspaceID, sourceID),
		encodedBatch(t, acceptedEventJSON("evt-archived", occurredAt, projectID)))
	requireResultStatuses(t, results, statusAccepted)
	if row := e.readRow(t, ctx, workspaceID, sourceID, "evt-archived"); row.projectID != projectID {
		t.Fatal("archived project was not retained on the row")
	}
}

func TestSourceStampMovesForwardOnly(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	pool := e.pool(t)
	service := e.service(t, pool, &syncBuffer{})
	credential := credentialFor(workspaceID, sourceID)
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	old := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	e.setLastReceivedAt(t, ctx, workspaceID, sourceID, old)
	results := runBatch(t, service, credential, encodedBatch(t, acceptedEventJSON("evt-stamp-1", occurredAt, pgtype.UUID{})))
	requireResultStatuses(t, results, statusAccepted)
	moved := e.lastReceivedAt(t, ctx, workspaceID, sourceID)
	if moved == nil || !moved.After(old) {
		t.Fatalf("last_received_at=%v did not move past %s", moved, old)
	}

	// A future stamp is never moved backwards by a later batch.
	future := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Microsecond)
	e.setLastReceivedAt(t, ctx, workspaceID, sourceID, future)
	results = runBatch(t, service, credential, encodedBatch(t, acceptedEventJSON("evt-stamp-2", occurredAt, pgtype.UUID{})))
	requireResultStatuses(t, results, statusAccepted)
	if stamp := e.lastReceivedAt(t, ctx, workspaceID, sourceID); stamp == nil || !stamp.Equal(future) {
		t.Fatalf("last_received_at=%v want the pinned future %s", stamp, future)
	}
}

func TestHandlerServesRealTransactionThroughHTTP(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	projectID := e.seedProject(t, ctx, workspaceID, nil)
	pool := e.pool(t)
	// The authenticator resolves the seeded identity, so the whole HTTP path runs
	// against real rows.
	service := New(NewPostgres(pool), &fixedAuthenticator{credential: credentialFor(workspaceID, sourceID)}, slog.New(slog.NewJSONHandler(&syncBuffer{}, nil)))
	mux := http.NewServeMux()
	NewHandler(service).Register(mux)
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	request := httptest.NewRequest(http.MethodPost, ingestionPath,
		strings.NewReader(encodedBatch(t, acceptedEventJSON("evt-http", occurredAt, projectID))))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer tpk_integration")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 body=%s", recorder.Code, recorder.Body.String())
	}
	var body itemResults
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	requireStatuses(t, body, statusAccepted)
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-http"); count != 1 {
		t.Fatalf("rows=%d want=1", count)
	}
}

// fixedAuthenticator resolves one pre-seeded credential, so an integration test can
// go through the real handler without a token table.
type fixedAuthenticator struct {
	credential credentials.Credential
}

func (a *fixedAuthenticator) Authenticate(ctx context.Context, token string) (credentials.Credential, error) {
	if token != "tpk_integration" {
		return credentials.Credential{}, credentials.ErrUnauthenticated
	}
	return a.credential, nil
}

// requireResultStatuses asserts the per-item statuses of a processed batch.
func requireResultStatuses(t *testing.T, results []result, want ...string) {
	t.Helper()
	if len(results) != len(want) {
		t.Fatalf("results=%d want=%d", len(results), len(want))
	}
	for i := range want {
		if results[i].status != want[i] {
			t.Fatalf("statuses[%d]=%q want=%q (all: %+v)", i, results[i].status, want[i], results)
		}
	}
}

// requireResultCodes asserts the per-item error codes of a processed batch.
func requireResultCodes(t *testing.T, results []result, want ...string) {
	t.Helper()
	if len(results) != len(want) {
		t.Fatalf("results=%d want=%d", len(results), len(want))
	}
	for i := range want {
		if results[i].code != want[i] {
			t.Fatalf("codes[%d]=%q want=%q (all: %+v)", i, results[i].code, want[i], results)
		}
	}
}

// probeConn opens one extra owner connection that is only used for observation and
// for holding explicit locks, so a blocked item transaction never blocks the test's
// own reads.
func (e *integrationEnv) probeConn(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), e.ownerURL)
	if err != nil {
		t.Fatal(database.SafeError("probe connect", err))
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Error(database.SafeError("probe close", err))
		}
	})
	return conn
}

// waitForWaitingBackend waits until some backend is blocked on an ungranted lock.
// The dedicated test database is exclusive to this run, so the blocked backend can
// only be the item under test. No sleep is used to guess the schedule: the lock
// itself serializes the two sides.
func waitForWaitingBackend(t *testing.T, ctx context.Context, observer *pgx.Conn) int {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pid int
		err := observer.QueryRow(deadline, "SELECT pid FROM pg_locks WHERE NOT granted LIMIT 1").Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(database.SafeError("observe waiting backend", err))
		}
		select {
		case <-ticker.C:
		case <-deadline.Done():
			t.Fatal("no backend became blocked on the held lock")
		}
	}
}

// holdLock runs fn inside one owner transaction that is always rolled back, so a
// test can create a real lock without committing any fixture row.
func (e *integrationEnv) holdLock(t *testing.T, ctx context.Context, fn func(ctx context.Context, tx pgx.Tx)) func() {
	t.Helper()
	tx, err := e.owner.Begin(ctx)
	if err != nil {
		t.Fatal(database.SafeError("begin lock holder", err))
	}
	fn(ctx, tx)
	once := sync.Once{}
	release := func() {
		once.Do(func() {
			if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Error(database.SafeError("release lock holder", err))
			}
		})
	}
	t.Cleanup(release)
	return release
}

// holdIdentityLock inserts the target identity in an uncommitted transaction, so the
// item's own INSERT has to wait on the unique identity key.
func holdIdentityLock(t *testing.T, ctx context.Context, tx pgx.Tx, workspaceID, sourceID pgtype.UUID, sourceEventID string, at time.Time) {
	t.Helper()
	// Only the identity and the mandatory columns matter here; the content is
	// obviously synthetic and is never committed.
	_, err := tx.Exec(ctx, `INSERT INTO usage_events (
    workspace_id, source_id, source_event_id, provider, model, occurred_at, received_at,
    normalization_version, payload_hash,
    input_tokens_quality, output_tokens_quality, total_tokens_quality,
    cache_read_tokens_quality, cache_write_tokens_quality, reasoning_tokens_quality)
VALUES ($1,$2,$3,'lock-holder','lock-holder',$4,$4,$5,'\x00'::bytea,
    'missing','missing','missing','missing','missing','missing')`,
		workspaceID, sourceID, sourceEventID, at, normalizationVersion)
	if err != nil {
		t.Fatal(database.SafeError("hold identity row", err))
	}
}

// holdMinuteLock locks the bucket row the item's minute increment will update.
func holdMinuteLock(t *testing.T, ctx context.Context, tx pgx.Tx, workspaceID, sourceID, projectID pgtype.UUID, provider, model string, occurredAt time.Time) {
	t.Helper()
	var found int
	err := tx.QueryRow(ctx, `SELECT 1 FROM usage_minute_stats
WHERE workspace_id=$1 AND bucket_start=$2 AND source_id=$3
  AND project_id IS NOT DISTINCT FROM $4 AND provider=$5 AND model=$6 FOR UPDATE`,
		workspaceID, occurredAt.UTC().Truncate(time.Minute), sourceID, projectID, provider, model).Scan(&found)
	if err != nil {
		t.Fatal(database.SafeError("hold minute row", err))
	}
}

// lowerBoundBase is a scripted server time whose lower window bound
// (base-retainedWindow) is inside one minute and at least one second away from the
// minute end. A seed event one second later therefore shares the target's minute
// bucket, which a raw time.Now() cannot guarantee: the target would fall into the
// next minute whenever the wall clock happened to be in the last second of a minute.
func lowerBoundBase() time.Time {
	return time.Now().UTC().Truncate(time.Minute).Add(30 * time.Second)
}

// steppingClock returns scripted server times in order and repeats the last one. It
// is only reachable through the hookStore Now hook; the production path always reads
// clock_timestamp().
type steppingClock struct {
	mu    sync.Mutex
	times []time.Time
	call  int
}

func (c *steppingClock) next(context.Context) (time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.times) == 0 {
		return time.Time{}, errors.New("synthetic: empty clock")
	}
	index := c.call
	if index >= len(c.times) {
		index = len(c.times) - 1
	}
	c.call++
	return c.times[index], nil
}

// startDetailBatch processes one batch on a goroutine, so a lock-based test can
// assert the blocked state meanwhile. Only service.process runs there: the test's
// assertions stay on the test goroutine.
func startDetailBatch(service *Service, credential credentials.Credential, items []item) <-chan []result {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	done := make(chan []result, 1)
	go func() {
		defer cancel()
		done <- service.process(ctx, credential, items)
	}()
	return done
}

// awaitDetailBatch waits for a started batch and fails the test on a hang.
func awaitDetailBatch(t *testing.T, done <-chan []result) []result {
	t.Helper()
	select {
	case results := <-done:
		return results
	case <-time.After(30 * time.Second):
		t.Fatal("batch did not finish within 30s")
		return nil
	}
}

// TestAggregationFailureRollsBackTheWholeItem makes the real minute upsert fail on
// the real database. The detail row and the minute increment must disappear
// together, the neighbours must be unaffected and a retry of the same identity must
// be accepted once the fault is removed.
func TestAggregationFailureRollsBackTheWholeItem(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	projectID := e.seedProject(t, ctx, workspaceID, nil)
	pool := e.pool(t)
	service := e.service(t, pool, &syncBuffer{})
	credential := credentialFor(workspaceID, sourceID)
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	// The constraint lives only in this test's schema and only bites this
	// synthetic provider, so the real aggregation statement is what fails.
	if _, err := e.owner.Exec(ctx, "ALTER TABLE usage_minute_stats ADD CONSTRAINT t011_aggregation_fail CHECK (provider <> 'aggregation-failure')"); err != nil {
		t.Fatal(database.SafeError("add aggregation fault constraint", err))
	}
	failing := `{"source_event_id":"evt-agg-fail","project_id":"` + uuidText(projectID) + `","provider":"aggregation-failure","model":"example-model","occurred_at":"` + occurredAt.Format(microLayout) + `","usage":{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"}}}`
	results := runBatch(t, service, credential, encodedBatch(t,
		acceptedEventJSON("evt-agg-neighbour-1", occurredAt, projectID),
		failing,
		acceptedEventJSON("evt-agg-neighbour-2", occurredAt, projectID),
	))
	requireResultStatuses(t, results, statusAccepted, statusRetryable, statusAccepted)
	requireResultCodes(t, results, "", codeUnavailable, "")

	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-agg-fail"); count != 0 {
		t.Fatalf("failed aggregation left %d detail rows", count)
	}
	if count := e.countBuckets(t, ctx, workspaceID, sourceID); count != 1 {
		t.Fatalf("buckets=%d want=1 (only the healthy dimension set)", count)
	}
	healthy := e.readBucket(t, ctx, workspaceID, sourceID, projectID, "example-provider", "example-model", occurredAt)
	if healthy.eventCount != 2 {
		t.Fatalf("healthy event_count=%d want=2", healthy.eventCount)
	}
	// While the fault is still present the same identity may simply be retried.
	results = runBatch(t, service, credential, encodedBatch(t, failing))
	requireResultStatuses(t, results, statusRetryable)
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-agg-fail"); count != 0 {
		t.Fatalf("failed aggregation left %d detail rows on the second attempt", count)
	}

	// Remove the fault and retry the same identity: it is accepted exactly once and
	// then counted exactly once.
	if _, err := e.owner.Exec(ctx, "ALTER TABLE usage_minute_stats DROP CONSTRAINT t011_aggregation_fail"); err != nil {
		t.Fatal(database.SafeError("drop aggregation fault constraint", err))
	}
	results = runBatch(t, service, credential, encodedBatch(t, failing))
	requireResultStatuses(t, results, statusAccepted)
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-agg-fail"); count != 1 {
		t.Fatalf("rows=%d want=1 after the retry", count)
	}
	failedBucket := e.readBucket(t, ctx, workspaceID, sourceID, projectID, "aggregation-failure", "example-model", occurredAt)
	if failedBucket.eventCount != 1 || failedBucket.sum[0] != 100 {
		t.Fatalf("retried bucket=%+v want event_count=1 input=100", failedBucket)
	}
	// A retransmission of the now-stored event is a duplicate, never a second count.
	results = runBatch(t, service, credential, encodedBatch(t, failing))
	requireResultStatuses(t, results, statusDuplicate)
	if bucket := e.readBucket(t, ctx, workspaceID, sourceID, projectID, "aggregation-failure", "example-model", occurredAt); bucket.eventCount != 1 {
		t.Fatalf("event_count=%d want=1 after the duplicate", bucket.eventCount)
	}
}

// TestLockWaitCrossesLowerBoundInsideInsert blocks the detail INSERT on the identity
// key, then releases it with the injected clock past the lower bound. The item must
// be expired and neither write may survive.
func TestLockWaitCrossesLowerBoundInsideInsert(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	projectID := e.seedProject(t, ctx, workspaceID, nil)
	pool := e.pool(t)
	observer := e.probeConn(t)
	logs := &syncBuffer{}
	// The event sits exactly on the lower bound at T0 and one microsecond late at
	// T1, which is the documented boundary example. Only Now is scripted; the
	// transaction, the lock wait and the window re-checks stay real.
	base := lowerBoundBase()
	occurredAt := base.Add(-retainedWindow)
	store := &hookStore{
		inner: NewPostgres(pool),
		now:   (&steppingClock{times: []time.Time{base, base.Add(time.Microsecond)}}).next,
	}
	service := New(store, &fakeAuthenticator{token: "tpk_integration"}, slog.New(slog.NewJSONHandler(logs, nil)))
	credential := credentialFor(workspaceID, sourceID)
	items := decodeOneBatch(t, encodedBatch(t, acceptedEventJSON("evt-lock-insert", occurredAt, projectID)))

	release := e.holdLock(t, ctx, func(ctx context.Context, tx pgx.Tx) {
		holdIdentityLock(t, ctx, tx, workspaceID, sourceID, "evt-lock-insert", base)
	})
	done := startDetailBatch(service, credential, items)
	waitForWaitingBackend(t, ctx, observer)
	release()
	results := awaitDetailBatch(t, done)
	requireResultStatuses(t, results, statusExpired)
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-lock-insert"); count != 0 {
		t.Fatalf("expired item left %d detail rows", count)
	}
	if count := e.countBuckets(t, ctx, workspaceID, sourceID); count != 0 {
		t.Fatalf("expired item left %d minute buckets", count)
	}
}

// TestLockWaitCrossesLowerBoundInsideMinute blocks the minute increment, then
// releases it with the clock past the lower bound. Both writes must roll back.
func TestLockWaitCrossesLowerBoundInsideMinute(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	projectID := e.seedProject(t, ctx, workspaceID, nil)
	pool := e.pool(t)
	observer := e.probeConn(t)
	// The seed event must land in the same minute bucket as the target and still be
	// inside the window, so the target time is placed away from a minute end.
	base := lowerBoundBase()
	occurredAt := base.Add(-retainedWindow)
	// The seed item runs on the same scripted clock, so neither its bucket nor its
	// window depends on when the test happens to execute.
	seed := New(&hookStore{
		inner: NewPostgres(pool),
		now:   (&steppingClock{times: []time.Time{base}}).next,
	}, &fakeAuthenticator{token: "tpk_integration"}, slog.New(slog.NewJSONHandler(&syncBuffer{}, nil)))
	credential := credentialFor(workspaceID, sourceID)
	results := runBatch(t, seed, credential, encodedBatch(t,
		acceptedEventJSON("evt-lock-minute-seed", occurredAt.Add(time.Second), projectID)))
	requireResultStatuses(t, results, statusAccepted)
	seedAt := occurredAt.Add(time.Second)
	if !seedAt.Truncate(time.Minute).Equal(occurredAt.Truncate(time.Minute)) {
		t.Fatalf("seed %s and target %s are in different minute buckets", seedAt, occurredAt)
	}

	store := &hookStore{
		inner: NewPostgres(pool),
		now:   (&steppingClock{times: []time.Time{base, base, base.Add(time.Microsecond)}}).next,
	}
	logs := &syncBuffer{}
	service := New(store, &fakeAuthenticator{token: "tpk_integration"}, slog.New(slog.NewJSONHandler(logs, nil)))
	items := decodeOneBatch(t, encodedBatch(t, acceptedEventJSON("evt-lock-minute", occurredAt, projectID)))

	release := e.holdLock(t, ctx, func(ctx context.Context, tx pgx.Tx) {
		holdMinuteLock(t, ctx, tx, workspaceID, sourceID, projectID, "example-provider", "example-model", occurredAt)
	})
	done := startDetailBatch(service, credential, items)
	waitForWaitingBackend(t, ctx, observer)
	release()
	results = awaitDetailBatch(t, done)
	requireResultStatuses(t, results, statusExpired)
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-lock-minute"); count != 0 {
		t.Fatalf("expired item left %d detail rows", count)
	}
	bucket := e.readBucket(t, ctx, workspaceID, sourceID, projectID, "example-provider", "example-model", occurredAt)
	if bucket.eventCount != 1 || bucket.sum[0] != 100 {
		t.Fatalf("bucket=%+v want the unchanged seed event_count=1 input=100", bucket)
	}
}

// hookStore wraps the real store and lets a test act at one exact point of the item
// transaction, so a race is created by ordering and never by a sleep.
type hookStore struct {
	inner store
	// now overrides the server time sample. Every other call still goes to the
	// real transaction, so the lock and the window re-check stay real while the
	// clock is controlled.
	now          func(ctx context.Context) (time.Time, error)
	afterInsert  func(ctx context.Context, event storedEvent, inserted bool) error
	beforeCommit func(ctx context.Context, tx itemTx) error
}

func (s *hookStore) BeginItem(ctx context.Context) (itemTx, error) {
	tx, err := s.inner.BeginItem(ctx)
	if err != nil {
		return nil, err
	}
	return &hookTx{inner: tx, hooks: s}, nil
}

func (s *hookStore) TouchSource(ctx context.Context, workspaceID, sourceID credentials.ID) error {
	return s.inner.TouchSource(ctx, workspaceID, sourceID)
}

// hookTx delegates every statement and calls the optional hooks in between.
type hookTx struct {
	inner itemTx
	hooks *hookStore
}

func (t *hookTx) Now(ctx context.Context) (time.Time, error) {
	if t.hooks.now != nil {
		return t.hooks.now(ctx)
	}
	return t.inner.Now(ctx)
}

func (t *hookTx) ProjectExists(ctx context.Context, workspaceID, projectID credentials.ID) (bool, error) {
	return t.inner.ProjectExists(ctx, workspaceID, projectID)
}

func (t *hookTx) InsertEvent(ctx context.Context, event storedEvent) (bool, error) {
	inserted, err := t.inner.InsertEvent(ctx, event)
	if err == nil && t.hooks.afterInsert != nil {
		if hookErr := t.hooks.afterInsert(ctx, event, inserted); hookErr != nil {
			return false, hookErr
		}
	}
	return inserted, err
}

func (t *hookTx) ExistingEvent(ctx context.Context, workspaceID, sourceID credentials.ID, sourceEventID string) (storedEvent, bool, error) {
	return t.inner.ExistingEvent(ctx, workspaceID, sourceID, sourceEventID)
}

func (t *hookTx) ApplyMinute(ctx context.Context, input aggregation.MinuteInput) error {
	return t.inner.ApplyMinute(ctx, input)
}

func (t *hookTx) Commit(ctx context.Context) error {
	if t.hooks.beforeCommit != nil {
		if err := t.hooks.beforeCommit(ctx, t.inner); err != nil {
			return err
		}
	}
	return t.inner.Commit(ctx)
}

func (t *hookTx) Rollback(ctx context.Context) error { return t.inner.Rollback(ctx) }

// TestVanishedRowBetweenInsertAndReadIsRetryable deletes the stored row with
// controlled SQL in the window between the conflicting INSERT and the comparison
// SELECT. The item must be retryable, with no re-insert and no recount.
func TestVanishedRowBetweenInsertAndReadIsRetryable(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	projectID := e.seedProject(t, ctx, workspaceID, nil)
	pool := e.pool(t)
	credential := credentialFor(workspaceID, sourceID)
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	event := acceptedEventJSON("evt-vanish", occurredAt, projectID)
	results := runBatch(t, e.service(t, pool, &syncBuffer{}), credential, encodedBatch(t, event))
	requireResultStatuses(t, results, statusAccepted)

	// The row is removed by the owner connection exactly between the two
	// statements of the item transaction, which is the documented race.
	deleting := &hookStore{inner: NewPostgres(pool)}
	deleting.afterInsert = func(ctx context.Context, _ storedEvent, inserted bool) error {
		if inserted {
			t.Error("the second attempt must conflict, not insert")
			return nil
		}
		if _, err := e.owner.Exec(ctx, "DELETE FROM usage_events WHERE workspace_id=$1 AND source_id=$2 AND source_event_id=$3",
			workspaceID, sourceID, "evt-vanish"); err != nil {
			return database.SafeError("delete stored row", err)
		}
		return nil
	}
	logs := &syncBuffer{}
	service := New(deleting, &fakeAuthenticator{token: "tpk_integration"}, slog.New(slog.NewJSONHandler(logs, nil)))
	results = runBatch(t, service, credential, encodedBatch(t, event))
	requireResultStatuses(t, results, statusRetryable)
	requireResultCodes(t, results, codeInternal)
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-vanish"); count != 0 {
		t.Fatalf("rows=%d want=0: the item must not re-insert a vanished identity", count)
	}
	bucket := e.readBucket(t, ctx, workspaceID, sourceID, projectID, "example-provider", "example-model", occurredAt)
	if bucket.eventCount != 1 {
		t.Fatalf("event_count=%d want=1: the item must not recount", bucket.eventCount)
	}
}

// TestRealCredentialRotationRevokeAndResend drives the whole path with the real
// credential service: create, authenticate, ingest, rotate, resend, revoke.
func TestRealCredentialRotationRevokeAndResend(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	projectID := e.seedProject(t, ctx, workspaceID, nil)
	pool := e.pool(t)
	credentialService := credentials.New(credentials.NewPostgres(pool))
	service := New(NewPostgres(pool), credentialService, slog.New(slog.NewJSONHandler(&syncBuffer{}, nil)))
	mux := http.NewServeMux()
	NewHandler(service).Register(mux)
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	event := acceptedEventJSON("evt-credential", occurredAt, projectID)

	issued, err := credentialService.Issue(ctx, credentials.ID(workspaceID), credentials.ID(sourceID))
	if err != nil {
		t.Fatal(database.SafeError("issue credential", err))
	}
	post := func(token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, ingestionPath, strings.NewReader(encodedBatch(t, event)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		return recorder
	}
	var body itemResults
	if err := json.Unmarshal(post(issued.Token).Body.Bytes(), &body); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	requireStatuses(t, body, statusAccepted)

	rotated, err := credentialService.Rotate(ctx, credentials.ID(workspaceID), issued.ID)
	if err != nil {
		t.Fatal(database.SafeError("rotate credential", err))
	}
	if rotated.Token == issued.Token {
		t.Fatal("rotation returned the old token")
	}
	if err := json.Unmarshal(post(rotated.Token).Body.Bytes(), &body); err != nil {
		t.Fatalf("decode retransmission response: %v", err)
	}
	// The rotated credential keeps the same source, so the same identity is a
	// duplicate: one row and one count.
	requireStatuses(t, body, statusDuplicate)
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-credential"); count != 1 {
		t.Fatalf("rows=%d want=1", count)
	}
	if bucket := e.readBucket(t, ctx, workspaceID, sourceID, projectID, "example-provider", "example-model", occurredAt); bucket.eventCount != 1 {
		t.Fatalf("event_count=%d want=1", bucket.eventCount)
	}

	if err := credentialService.Revoke(ctx, credentials.ID(workspaceID), rotated.ID); err != nil {
		t.Fatal(database.SafeError("revoke credential", err))
	}
	recorder := post(rotated.Token)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("revoked credential status=%d want=401 body=%s", recorder.Code, recorder.Body.String())
	}
	newEvent := acceptedEventJSON("evt-credential-revoked", occurredAt, projectID)
	request := httptest.NewRequest(http.MethodPost, ingestionPath, strings.NewReader(encodedBatch(t, newEvent)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rotated.Token)
	revokedRecorder := httptest.NewRecorder()
	mux.ServeHTTP(revokedRecorder, request)
	if revokedRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("revoked credential status=%d want=401", revokedRecorder.Code)
	}
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-credential-revoked"); count != 0 {
		t.Fatalf("revoked credential wrote %d rows", count)
	}
	if count := e.countBuckets(t, ctx, workspaceID, sourceID); count != 1 {
		t.Fatalf("buckets=%d want=1", count)
	}
}

// TestCommitWithLostConnectionDoesNotDoubleCount destroys the item's backend
// connection right before Commit, which is the testable form of an indeterminate
// commit result: the server aborts the transaction. The retransmission must resolve
// the identity without counting it twice.
func TestCommitWithLostConnectionDoesNotDoubleCount(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	projectID := e.seedProject(t, ctx, workspaceID, nil)
	pool := e.pool(t)
	credential := credentialFor(workspaceID, sourceID)
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	event := acceptedEventJSON("evt-commit-lost", occurredAt, projectID)

	losing := &hookStore{inner: NewPostgres(pool)}
	var closed bool
	losing.beforeCommit = func(ctx context.Context, tx itemTx) error {
		postgresItem, ok := tx.(*postgresTx)
		if !ok {
			return errors.New("synthetic: unexpected transaction type")
		}
		closed = true
		// Closing the backend connection makes PostgreSQL abort the open
		// transaction, so the commit cannot land and its result is unknowable to
		// the client.
		return postgresItem.tx.Conn().Close(ctx)
	}
	logs := &syncBuffer{}
	service := New(losing, &fakeAuthenticator{token: "tpk_integration"}, slog.New(slog.NewJSONHandler(logs, nil)))
	results := runBatch(t, service, credential, encodedBatch(t, event))
	requireResultStatuses(t, results, statusRetryable)
	if results[0].status == statusAccepted {
		t.Fatal("a commit error must never be reported as accepted")
	}
	if !closed {
		t.Fatal("the commit hook did not run")
	}
	if strings.Contains(logs.String(), "usage_events") || strings.Contains(logs.String(), "INSERT INTO") {
		t.Fatalf("logs leaked SQL detail: %s", logs.String())
	}

	// The pool recovers a fresh connection and the retransmission resolves the same
	// identity exactly once.
	recovered := e.service(t, pool, &syncBuffer{})
	results = runBatch(t, recovered, credential, encodedBatch(t, event))
	if len(results) != 1 || (results[0].status != statusAccepted && results[0].status != statusDuplicate) {
		t.Fatalf("retransmission=%+v want accepted or duplicate", results)
	}
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-commit-lost"); count != 1 {
		t.Fatalf("rows=%d want=1", count)
	}
	bucket := e.readBucket(t, ctx, workspaceID, sourceID, projectID, "example-provider", "example-model", occurredAt)
	if bucket.eventCount != 1 || bucket.sum[0] != 100 {
		t.Fatalf("bucket=%+v want exactly one count of 100", bucket)
	}
}

// TestCommittedItemWithUnknownResultIsNotDoubleCounted covers the other half of the
// indeterminate commit: the commit really lands on the server but the caller only
// sees an error. The item must be retryable, and the retransmission must resolve the
// identity without a second count.
func TestCommittedItemWithUnknownResultIsNotDoubleCounted(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	projectID := e.seedProject(t, ctx, workspaceID, nil)
	pool := e.pool(t)
	credential := credentialFor(workspaceID, sourceID)
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	event := acceptedEventJSON("evt-commit-landed", occurredAt, projectID)

	// The hook commits the real transaction and then reports a failure, which is
	// exactly the state where the client cannot tell accepted from not written.
	uncertain := &hookStore{inner: NewPostgres(pool)}
	uncertain.beforeCommit = func(ctx context.Context, tx itemTx) error {
		postgresItem, ok := tx.(*postgresTx)
		if !ok {
			return errors.New("synthetic: unexpected transaction type")
		}
		if err := postgresItem.tx.Commit(ctx); err != nil {
			return err
		}
		return errors.New("synthetic: commit acknowledgement lost")
	}
	logs := &syncBuffer{}
	service := New(uncertain, &fakeAuthenticator{token: "tpk_integration"}, slog.New(slog.NewJSONHandler(logs, nil)))
	results := runBatch(t, service, credential, encodedBatch(t, event))
	requireResultStatuses(t, results, statusRetryable)
	requireResultCodes(t, results, codeInternal)
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-commit-landed"); count != 1 {
		t.Fatalf("rows=%d want=1: the commit did land on the server", count)
	}

	// The retransmission sees the committed row and answers duplicate: still one
	// row and one count.
	results = runBatch(t, e.service(t, pool, &syncBuffer{}), credential, encodedBatch(t, event))
	requireResultStatuses(t, results, statusDuplicate)
	if count := e.countRows(t, ctx, workspaceID, sourceID, "evt-commit-landed"); count != 1 {
		t.Fatalf("rows=%d want=1 after the retransmission", count)
	}
	bucket := e.readBucket(t, ctx, workspaceID, sourceID, projectID, "example-provider", "example-model", occurredAt)
	if bucket.eventCount != 1 || bucket.sum[0] != 100 {
		t.Fatalf("bucket=%+v want exactly one count of 100", bucket)
	}
}

// historySnapshot renders every usage_events and usage_minute_stats row of the
// workspace as one text line per row, covering ownership (workspace, source,
// project), provider/model, the token values and aggregate sums, and the
// timestamps, so a later snapshot can prove no row was rewritten or moved.
func (e *integrationEnv) historySnapshot(t *testing.T, ctx context.Context, workspaceID pgtype.UUID) (events, minutes []string) {
	t.Helper()
	read := func(query string) []string {
		rows, err := e.owner.Query(ctx, query, workspaceID)
		if err != nil {
			t.Fatal(database.SafeError("history snapshot", err))
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(database.SafeError("history scan", err))
			}
			lines = append(lines, line)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(database.SafeError("history rows", err))
		}
		return lines
	}
	events = read("SELECT concat_ws('|', source_event_id, workspace_id, source_id, coalesce(project_id::text,'-'), provider, model, occurred_at, received_at, coalesce(input_tokens::text,'-'), coalesce(output_tokens::text,'-'), coalesce(total_tokens::text,'-')) FROM usage_events WHERE workspace_id=$1 ORDER BY source_event_id")
	minutes = read("SELECT concat_ws('|', bucket_start, workspace_id, source_id, coalesce(project_id::text,'-'), provider, model, event_count, input_tokens_sum, output_tokens_sum, total_tokens_sum) FROM usage_minute_stats WHERE workspace_id=$1 ORDER BY bucket_start, provider, model")
	return events, minutes
}

// requireSubset fails unless every line of before is still present, unchanged, in
// after, and after has exactly extra additional lines.
func requireSubset(t *testing.T, name string, before, after []string, extra int) {
	t.Helper()
	present := make(map[string]int, len(after))
	for _, line := range after {
		present[line]++
	}
	for _, line := range before {
		if present[line] == 0 {
			t.Fatalf("%s: a pre-existing row was changed or removed: %s", name, line)
		}
		present[line]--
	}
	if len(after) != len(before)+extra {
		t.Fatalf("%s: rows %d -> %d want %d more", name, len(before), len(after), extra)
	}
}

// TestSourceArchiveKeepsHistoryAndInFlightRequest authenticates a real token,
// archives the source through the real sources.Service (which revokes the token)
// and then finishes the request that was already authenticated: it still writes
// history and moves last_received_at, while a fresh authentication with the same
// token fails and every earlier event and minute row is unchanged.
func TestSourceArchiveKeepsHistoryAndInFlightRequest(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	projectID := e.seedProject(t, ctx, workspaceID, nil)
	pool := e.pool(t)
	credentialService := credentials.New(credentials.NewPostgres(pool))
	sourceService := sources.New(sources.NewPostgres(pool))
	issued, err := credentialService.Issue(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	service := New(NewPostgres(pool), credentialService, slog.New(slog.NewJSONHandler(&syncBuffer{}, nil)))
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	authenticated, err := credentialService.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	requireResultStatuses(t, runBatch(t, service, authenticated, encodedBatch(t, acceptedEventJSON("evt-before", occurredAt, projectID))), statusAccepted)
	eventsBefore, minutesBefore := e.historySnapshot(t, ctx, workspaceID)
	if len(eventsBefore) != 1 || len(minutesBefore) != 1 {
		t.Fatalf("seed history events=%d minutes=%d want 1 and 1", len(eventsBefore), len(minutesBefore))
	}

	if err := sourceService.Archive(ctx, workspaceID, sourceID); err != nil {
		t.Fatal(err)
	}
	eventsMid, minutesMid := e.historySnapshot(t, ctx, workspaceID)
	requireSubset(t, "events after archive", eventsBefore, eventsMid, 0)
	requireSubset(t, "minutes after archive", minutesBefore, minutesMid, 0)
	var credentialRows int
	if err := e.owner.QueryRow(ctx, "SELECT count(*) FROM ingest_credentials WHERE workspace_id=$1 AND source_id=$2", workspaceID, sourceID).Scan(&credentialRows); err != nil || credentialRows != 1 {
		t.Fatalf("credential rows=%d err=%v want 1 kept", credentialRows, err)
	}
	e.setLastReceivedAt(t, ctx, workspaceID, sourceID, time.Now().UTC().Add(-time.Hour))

	// The request authenticated before the archive finishes in a different
	// minute, so the old bucket must stay untouched and a new one appears.
	inflightAt := occurredAt.Add(-time.Hour)
	requireResultStatuses(t, runBatch(t, service, authenticated, encodedBatch(t, acceptedEventJSON("evt-inflight", inflightAt, projectID))), statusAccepted)
	eventsAfter, minutesAfter := e.historySnapshot(t, ctx, workspaceID)
	requireSubset(t, "events after in-flight", eventsBefore, eventsAfter, 1)
	requireSubset(t, "minutes after in-flight", minutesBefore, minutesAfter, 1)
	if stamp := e.lastReceivedAt(t, ctx, workspaceID, sourceID); stamp == nil || time.Since(*stamp) > time.Minute {
		t.Fatal("in-flight request did not update last_received_at")
	}
	var archived bool
	if err := e.owner.QueryRow(ctx, "SELECT archived_at IS NOT NULL FROM sources WHERE id=$1 AND workspace_id=$2", sourceID, workspaceID).Scan(&archived); err != nil || !archived {
		t.Fatal("ingestion reactivated the archived source")
	}
	var active int
	if err := e.owner.QueryRow(ctx, "SELECT count(*) FROM ingest_credentials WHERE source_id=$1 AND revoked_at IS NULL", sourceID).Scan(&active); err != nil || active != 0 {
		t.Fatalf("active credentials=%d err=%v", active, err)
	}
	if _, err := credentialService.Authenticate(ctx, issued.Token); !errors.Is(err, credentials.ErrUnauthenticated) {
		t.Fatalf("new authentication error=%v want ErrUnauthenticated", err)
	}
}

// TestProjectArchiveKeepsHistory archives a project that already has events and
// minute stats through the real projects.Service: every earlier row is unchanged
// and the archived project keeps receiving events.
func TestProjectArchiveKeepsHistory(t *testing.T) {
	e := newIntegrationEnv(t)
	ctx := context.Background()
	workspaceID, sourceID := e.seedSource(t, ctx)
	projectID := e.seedProject(t, ctx, workspaceID, nil)
	pool := e.pool(t)
	service := e.service(t, pool, &syncBuffer{})
	projectService := projects.New(projects.NewPostgres(pool))
	occurredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	requireResultStatuses(t, runBatch(t, service, credentialFor(workspaceID, sourceID), encodedBatch(t, acceptedEventJSON("evt-p1", occurredAt, projectID))), statusAccepted)
	eventsBefore, minutesBefore := e.historySnapshot(t, ctx, workspaceID)
	if len(eventsBefore) != 1 || len(minutesBefore) != 1 {
		t.Fatalf("seed history events=%d minutes=%d want 1 and 1", len(eventsBefore), len(minutesBefore))
	}

	if err := projectService.Archive(ctx, workspaceID, projectID); err != nil {
		t.Fatal(err)
	}
	var archived bool
	if err := e.owner.QueryRow(ctx, "SELECT archived_at IS NOT NULL FROM projects WHERE id=$1 AND workspace_id=$2", projectID, workspaceID).Scan(&archived); err != nil || !archived {
		t.Fatal("project was not archived")
	}
	eventsMid, minutesMid := e.historySnapshot(t, ctx, workspaceID)
	requireSubset(t, "events after archive", eventsBefore, eventsMid, 0)
	requireSubset(t, "minutes after archive", minutesBefore, minutesMid, 0)

	// An archived project still accepts events, bound to the same project id.
	requireResultStatuses(t, runBatch(t, service, credentialFor(workspaceID, sourceID), encodedBatch(t, acceptedEventJSON("evt-p2", occurredAt.Add(-time.Hour), projectID))), statusAccepted)
	eventsAfter, minutesAfter := e.historySnapshot(t, ctx, workspaceID)
	requireSubset(t, "events after new event", eventsBefore, eventsAfter, 1)
	requireSubset(t, "minutes after new event", minutesBefore, minutesAfter, 1)
}
