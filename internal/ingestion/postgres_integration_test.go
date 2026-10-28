//go:build integration

package ingestion

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

	// Exactly on the lower bound is still accepted; one microsecond under is not.
	now := time.Now().UTC().Truncate(time.Microsecond)
	edge := now.Add(-retainedWindow)
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
