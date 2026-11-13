//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dundunHa/TokenPivot/db/migrations"
	"github.com/dundunHa/TokenPivot/internal/aggregation"
	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/dundunHa/TokenPivot/internal/ingestion"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// retiredVersionValue mirrors the version this command converts. It is written as a
// literal because a conversion test must fail if the constant ever changes silently.
const retiredVersionValue = "tokenpivot-usage-event/v1"

// convertEnv is one per-test schema in the dedicated test database. It logs only the
// schema name, never the connection string.
type convertEnv struct {
	db     *sql.DB
	dsn    string
	schema string
}

// newConvertEnv creates a random schema, applies every released migration inside it
// and returns a connection pinned to that schema.
func newConvertEnv(t *testing.T) *convertEnv {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PG") {
			if err := os.Unsetenv(key); err != nil {
				t.Fatal("unset PG failed")
			}
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PGPASSFILE", "/dev/null")
	t.Setenv("PGSERVICEFILE", "/dev/null")
	value := os.Getenv("TOKENPIVOT_TEST_DATABASE_URL")
	if err := database.ValidateURL(value); err != nil {
		t.Fatal("explicit integration database variable required")
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
		if key != "sslmode" {
			t.Fatal("test URL has unsupported query option")
		}
	}
	schema := "t070_" + hex.EncodeToString(randomBytes(t, 6))
	quoted := pgx.Identifier{schema}.Sanitize()
	bootstrapCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	bootstrap, err := pgx.Connect(bootstrapCtx, value)
	if err != nil {
		t.Fatal(database.SafeError("test connect", err))
	}
	// Registered before the schema cleanup so it runs after it: the cleanup needs a
	// live connection to drop the schema it created.
	t.Cleanup(func() {
		if err := bootstrap.Close(context.Background()); err != nil {
			t.Error("test connection close failed")
		}
	})
	var owner string
	if err := bootstrap.QueryRow(bootstrapCtx, "SELECT current_user").Scan(&owner); err != nil {
		t.Fatal(database.SafeError("test identity", err))
	}
	if _, err := bootstrap.Exec(bootstrapCtx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(database.SafeError("create owned schema", err))
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var actual string
		err := bootstrap.QueryRow(cleanupCtx, "SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname=$1", schema).Scan(&actual)
		if err != nil || actual != owner || !strings.HasPrefix(schema, "t070_") {
			t.Errorf("preserving schema %s: ownership check failed", schema)
			return
		}
		if _, err := bootstrap.Exec(cleanupCtx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("preserving schema %s: cleanup failed", schema)
		}
	})
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	dsn := parsed.String()
	applyConvertMigrations(t, dsn)
	db, err := openPinned(t, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("owned schema=%s", schema)
	return &convertEnv{db: db, dsn: dsn, schema: schema}
}

// applyConvertMigrations runs every released migration inside the pinned schema.
func applyConvertMigrations(t *testing.T, dsn string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
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

// openPinned opens the connection the tests and the converter share.
func openPinned(t *testing.T, dsn string) (*sql.DB, error) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, database.SafeError("parse", err)
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(database.SafeError("close", err))
		}
	})
	return db, nil
}

// randomBytes returns cryptographic test material.
func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal("random bytes failed")
	}
	return b
}

// eventSeed is one row written before a conversion, described only by the values the
// conversion must respect.
type eventSeed struct {
	id            string
	sourceEventID string
	project       sql.NullString
	provider      string
	model         string
	occurredAt    time.Time
	receivedAt    time.Time
	version       string
	hash          []byte
	values        [6]sql.NullInt64
	qualities     [6]string
	status        sql.NullString
	duration      sql.NullInt64
	generation    sql.NullInt64
	ttft          sql.NullInt64
}

const convertWorkspaceID = "11111111-1111-4111-8111-111111111111"
const convertSourceID = "22222222-2222-4222-8222-222222222222"

const insertEventQuery = `INSERT INTO usage_events (
    id, workspace_id, source_id, source_event_id, project_id, provider, model,
    occurred_at, received_at, normalization_version, payload_hash,
    input_tokens, input_tokens_quality,
    output_tokens, output_tokens_quality,
    total_tokens, total_tokens_quality,
    cache_read_tokens, cache_read_tokens_quality,
    cache_write_tokens, cache_write_tokens_quality,
    reasoning_tokens, reasoning_tokens_quality,
    status, duration_ms, generation_duration_ms, ttft_ms)
VALUES ($1, $2::uuid, $3::uuid, $4, $5::uuid, $6, $7, $8, $9, $10, $11,
    $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27)`

// seedEvent inserts one stored event exactly as given.
func (e *convertEnv) seedEvent(t *testing.T, seed eventSeed) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := e.db.ExecContext(ctx, insertEventQuery,
		seed.id, convertWorkspaceID, convertSourceID, seed.sourceEventID, seed.project,
		seed.provider, seed.model, seed.occurredAt, seed.receivedAt, seed.version, seed.hash,
		seed.values[0], seed.qualities[0], seed.values[1], seed.qualities[1],
		seed.values[2], seed.qualities[2], seed.values[3], seed.qualities[3],
		seed.values[4], seed.qualities[4], seed.values[5], seed.qualities[5],
		seed.status, seed.duration, seed.generation, seed.ttft)
	if err != nil {
		t.Fatal(database.SafeError("seed event", err))
	}
}

// newID returns a random opaque identity for a test row.
func newID(t *testing.T) string {
	t.Helper()
	b := randomBytes(t, 16)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return uuidString(b)
}

// uuidString renders the hyphenated lower case form.
func uuidString(b []byte) string {
	var buf bytes.Buffer
	for i, value := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf.WriteByte('-')
		}
		buf.WriteString(hex.EncodeToString([]byte{value}))
	}
	return buf.String()
}

// nullInt returns a stored integer, or an absent one.
func nullInt(value int64) sql.NullInt64 { return sql.NullInt64{Int64: value, Valid: true} }

// nullStringValue returns a stored string, or an absent one.
func nullStringValue(value string) sql.NullString {
	return sql.NullString{String: value, Valid: true}
}

// knownMetric is the stored shape of a known value.
func knownMetric(value int64) (sql.NullInt64, string) { return nullInt(value), "known" }

// estimatedMetric is the stored shape of a derived value, which still carries a value.
func estimatedMetric(value int64) (sql.NullInt64, string) { return nullInt(value), "estimated" }

// missingMetric is the stored shape of an unobserved value.
func missingMetric() (sql.NullInt64, string) { return sql.NullInt64{}, "missing" }

// oneToOneEvent builds a retired row whose business content is fully described, so
// the expected shared digest can be computed in the test.
func oneToOneEvent(t *testing.T, sourceEventID string, occurredAt time.Time, project sql.NullString) eventSeed {
	t.Helper()
	seed := eventSeed{
		id: newID(t), sourceEventID: sourceEventID, project: project,
		provider: "anthropic", model: "claude-sonnet-4",
		occurredAt: occurredAt, receivedAt: occurredAt.Add(2 * time.Second),
		version: retiredVersionValue, hash: bytes.Repeat([]byte{0xAB}, 32),
		status: nullStringValue("ok"), duration: nullInt(2400),
	}
	seed.values[0], seed.qualities[0] = knownMetric(1200)
	seed.values[1], seed.qualities[1] = knownMetric(340)
	seed.values[2], seed.qualities[2] = knownMetric(1540)
	seed.values[3], seed.qualities[3] = missingMetric()
	seed.values[4], seed.qualities[4] = missingMetric()
	seed.values[5], seed.qualities[5] = knownMetric(512)
	return seed
}

// expectedDigest rebuilds the shared normalization input of a seeded retired row,
// with both timings unknown, exactly as the conversion must.
func expectedDigest(t *testing.T, seed eventSeed) []byte {
	t.Helper()
	input := ingestion.NormalizationInput{
		OccurredAt: seed.occurredAt, Provider: seed.provider, Model: seed.model,
	}
	if seed.project.Valid {
		id := mustUUID(t, seed.project.String)
		input.ProjectID = id
	}
	if seed.status.Valid {
		value := seed.status.String
		input.Status = &value
	}
	if seed.duration.Valid {
		value := seed.duration.Int64
		input.DurationMS = &value
	}
	metrics := []*aggregation.Metric{&input.InputTokens, &input.OutputTokens, &input.TotalTokens,
		&input.CacheReadTokens, &input.CacheWriteTokens, &input.ReasoningTokens}
	for i := range metrics {
		metrics[i].Quality = aggregation.Quality(seed.qualities[i])
		if seed.values[i].Valid {
			value := seed.values[i].Int64
			metrics[i].Value = &value
		}
	}
	digest := ingestion.DigestV2(input)
	out := make([]byte, len(digest))
	copy(out, digest[:])
	return out
}

// mustUUID parses a UUID literal into the shared identity type.
func mustUUID(t *testing.T, value string) (id pgtype.UUID) {
	t.Helper()
	if err := id.Scan(value); err != nil {
		t.Fatalf("parse uuid: %v", err)
	}
	return id
}

// rowSnapshot renders one row without the two columns the conversion owns.
func (e *convertEnv) rowSnapshot(t *testing.T, id string) string {
	t.Helper()
	var text sql.NullString
	if err := e.db.QueryRow(`SELECT (to_jsonb(t) - 'normalization_version' - 'payload_hash')::text
FROM usage_events t WHERE id = $1`, id).Scan(&text); err != nil {
		t.Fatal(database.SafeError("row snapshot", err))
	}
	return text.String
}

// tableDigest digests every stored row, including the owned columns.
func (e *convertEnv) tableDigest(t *testing.T) string {
	t.Helper()
	var digest string
	if err := e.db.QueryRow(`SELECT coalesce(md5(string_agg(t::text, '' ORDER BY id::text)), '')
FROM usage_events t`).Scan(&digest); err != nil {
		t.Fatal(database.SafeError("table digest", err))
	}
	return digest
}

// minutesDigest digests the whole minute aggregate.
func (e *convertEnv) minutesDigest(t *testing.T) string {
	t.Helper()
	var digest string
	if err := e.db.QueryRow(`SELECT coalesce(md5(string_agg(t::text, '' ORDER BY t::text)), '')
FROM usage_minute_stats t`).Scan(&digest); err != nil {
		t.Fatal(database.SafeError("minutes digest", err))
	}
	return digest
}

// countVersion counts the stored rows of one normalization version.
func (e *convertEnv) countVersion(t *testing.T, version string) int64 {
	t.Helper()
	var count int64
	if err := e.db.QueryRow(`SELECT count(*) FROM usage_events WHERE normalization_version = $1`, version).Scan(&count); err != nil {
		t.Fatal(database.SafeError("count version", err))
	}
	return count
}

// storedDigest reads the stored digest of one row.
func (e *convertEnv) storedDigest(t *testing.T, id string) []byte {
	t.Helper()
	var hash []byte
	if err := e.db.QueryRow(`SELECT payload_hash FROM usage_events WHERE id = $1`, id).Scan(&hash); err != nil {
		t.Fatal(database.SafeError("read digest", err))
	}
	return hash
}

// seedMinute writes one minute bucket so the tests can prove the conversion never
// touches the aggregate.
func (e *convertEnv) seedMinute(t *testing.T) {
	t.Helper()
	columns := `(workspace_id, bucket_start, source_id, project_id, provider, model, event_count,
 input_tokens_sum, input_tokens_known_count, input_tokens_estimated_count, input_tokens_missing_count,
 output_tokens_sum, output_tokens_known_count, output_tokens_estimated_count, output_tokens_missing_count,
 total_tokens_sum, total_tokens_known_count, total_tokens_estimated_count, total_tokens_missing_count,
 cache_read_tokens_sum, cache_read_tokens_known_count, cache_read_tokens_estimated_count, cache_read_tokens_missing_count,
 cache_write_tokens_sum, cache_write_tokens_known_count, cache_write_tokens_estimated_count, cache_write_tokens_missing_count,
 reasoning_tokens_sum, reasoning_tokens_known_count, reasoning_tokens_estimated_count, reasoning_tokens_missing_count)`
	values := `($1::uuid, '2026-11-13T10:00:00Z', $2::uuid, NULL, 'anthropic', 'claude-sonnet-4', 3,
 1540, 2, 1, 0, 340, 1, 0, 2, 1540, 1, 1, 1, 0, 0, 0, 3, 0, 0, 0, 3, 512, 1, 0, 2)`
	if _, err := e.db.Exec(`INSERT INTO usage_minute_stats `+columns+` VALUES `+values,
		convertWorkspaceID, convertSourceID); err != nil {
		t.Fatal(database.SafeError("seed minute", err))
	}
}

// newConverter builds the command under test around the pinned connection.
func (e *convertEnv) newConverter() *converter {
	return &converter{db: e.db, logger: discardLogger()}
}

// captureLog collects the structured reports a converter logs, so a test can read the
// state a check reported without adding a second output path to the command.
type captureLog struct {
	buffer bytes.Buffer
	logger *slog.Logger
}

// newCaptureLog builds a silent logger that keeps every line it logs.
func newCaptureLog() *captureLog {
	capture := &captureLog{}
	capture.logger = slog.New(slog.NewJSONHandler(&capture.buffer, nil))
	return capture
}

// report returns the single report line, refusing anything else.
func (c *captureLog) report(t *testing.T) map[string]any {
	t.Helper()
	if strings.TrimSpace(c.buffer.String()) == "" {
		t.Fatal("no report line was logged")
	}
	var report map[string]any
	for _, line := range strings.Split(strings.TrimSpace(c.buffer.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("a logged line is not JSON: %s", line)
		}
		if entry["msg"] != "report" {
			continue
		}
		if report != nil {
			t.Fatal("more than one report line was logged")
		}
		report = entry
	}
	if report == nil {
		t.Fatal("no report line was logged")
	}
	return report
}

// checkOnNewConnection runs the read-only check through a freshly opened connection and
// returns the state it reported. This is how an operator verifies an unknown COMMIT
// without trusting the connection that failed.
func (e *convertEnv) checkOnNewConnection(t *testing.T) string {
	t.Helper()
	db, err := openPinned(t, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	capture := newCaptureLog()
	if err := (&converter{db: db, logger: capture.logger}).check(context.Background()); err != nil {
		t.Fatalf("check on a new connection failed: %v", err)
	}
	state, ok := capture.report(t)["state"].(string)
	if !ok {
		t.Fatal("the report carries no state")
	}
	return state
}

func TestConvertCheckIsReadOnlyAndApplyOwnsOnlyTwoColumns(t *testing.T) {
	env := newConvertEnv(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	attributed := oneToOneEvent(t, "evt-attributed", at, sql.NullString{String: newID(t), Valid: true})
	unattributed := eventSeed{
		id: newID(t), sourceEventID: "evt-unattributed",
		provider: "openai", model: "gpt-5", occurredAt: at.Add(time.Minute),
		receivedAt: at.Add(time.Minute + time.Second),
		version:    retiredVersionValue, hash: bytes.Repeat([]byte{0xCD}, 32),
	}
	unattributed.values[0], unattributed.qualities[0] = missingMetric()
	unattributed.values[1], unattributed.qualities[1] = knownMetric(0)
	unattributed.values[2], unattributed.qualities[2] = estimatedMetric(1540)
	unattributed.values[3], unattributed.qualities[3] = knownMetric(0)
	unattributed.values[4], unattributed.qualities[4] = knownMetric(0)
	unattributed.values[5], unattributed.qualities[5] = missingMetric()
	current := oneToOneEvent(t, "evt-current", at.Add(2*time.Minute), sql.NullString{})
	current.id = newID(t)
	current.version = ingestion.NormalizationVersionV2
	current.hash = bytes.Repeat([]byte{0x11}, 32)
	current.generation = nullInt(1800)
	current.ttft = nullInt(120)
	for _, seed := range []eventSeed{attributed, unattributed, current} {
		env.seedEvent(t, seed)
	}
	env.seedMinute(t)

	beforeTable := env.tableDigest(t)
	beforeMinutes := env.minutesDigest(t)
	attributedSnapshot := env.rowSnapshot(t, attributed.id)
	unattributedSnapshot := env.rowSnapshot(t, unattributed.id)
	currentSnapshot := env.rowSnapshot(t, current.id)
	currentHash := env.storedDigest(t, current.id)

	if err := env.newConverter().check(ctx); err != nil {
		t.Fatalf("check failed: %v", err)
	}
	if got := env.tableDigest(t); got != beforeTable {
		t.Fatal("check changed the stored rows")
	}
	if got := env.minutesDigest(t); got != beforeMinutes {
		t.Fatal("check changed the minute aggregate")
	}

	if err := env.newConverter().apply(ctx); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if got := env.countVersion(t, retiredVersionValue); got != 0 {
		t.Fatalf("retired rows left = %d, want 0", got)
	}
	if got := env.countVersion(t, ingestion.NormalizationVersionV2); got != 3 {
		t.Fatalf("current rows = %d, want 3", got)
	}
	if got := env.rowSnapshot(t, attributed.id); got != attributedSnapshot {
		t.Fatal("the conversion changed a column it does not own")
	}
	if got := env.rowSnapshot(t, unattributed.id); got != unattributedSnapshot {
		t.Fatal("the conversion changed a column it does not own")
	}
	if got := env.rowSnapshot(t, current.id); got != currentSnapshot {
		t.Fatal("the conversion changed an existing current row")
	}
	if got := env.storedDigest(t, current.id); !bytes.Equal(got, currentHash) {
		t.Fatal("the conversion rewrote the digest of an existing current row")
	}
	if got := env.storedDigest(t, attributed.id); !bytes.Equal(got, expectedDigest(t, attributed)) {
		t.Fatal("the converted digest is not the shared DigestV2 result")
	}
	if got := env.storedDigest(t, unattributed.id); !bytes.Equal(got, expectedDigest(t, unattributed)) {
		t.Fatal("the converted digest is not the shared DigestV2 result")
	}
	if got := env.minutesDigest(t); got != beforeMinutes {
		t.Fatal("the conversion changed the minute aggregate")
	}

	// A second run must convert nothing and change nothing.
	afterTable := env.tableDigest(t)
	if err := env.newConverter().apply(ctx); err != nil {
		t.Fatalf("second apply failed: %v", err)
	}
	if got := env.tableDigest(t); got != afterTable {
		t.Fatal("a repeated conversion changed stored rows")
	}
	if err := env.newConverter().check(ctx); err != nil {
		t.Fatalf("check after conversion failed: %v", err)
	}
}

func TestConvertIsANoOpWithoutRetiredRows(t *testing.T) {
	env := newConvertEnv(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	seed := oneToOneEvent(t, "evt-current", at, sql.NullString{})
	seed.version = ingestion.NormalizationVersionV2
	seed.hash = bytes.Repeat([]byte{0x22}, 32)
	env.seedEvent(t, seed)
	env.seedMinute(t)
	before := env.tableDigest(t)
	beforeMinutes := env.minutesDigest(t)
	if err := env.newConverter().check(ctx); err != nil {
		t.Fatalf("check failed: %v", err)
	}
	if err := env.newConverter().apply(ctx); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if got := env.tableDigest(t); got != before {
		t.Fatal("a no-op conversion changed stored rows")
	}
	if got := env.minutesDigest(t); got != beforeMinutes {
		t.Fatal("a no-op conversion changed the minute aggregate")
	}
}

func TestConvertRefusesAnUnsafeStoredState(t *testing.T) {
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		build func(t *testing.T, env *convertEnv)
	}{
		{
			name: "unknown version",
			build: func(t *testing.T, env *convertEnv) {
				seed := oneToOneEvent(t, "evt-v3", at, sql.NullString{})
				seed.version = "tokenpivot-usage-event/v3"
				env.seedEvent(t, seed)
			},
		},
		{
			name: "retired row already carries a timing",
			build: func(t *testing.T, env *convertEnv) {
				seed := oneToOneEvent(t, "evt-timed", at, sql.NullString{})
				seed.generation = nullInt(900)
				env.seedEvent(t, seed)
			},
		},
		{
			name: "digest is not a thirty two byte sha256",
			build: func(t *testing.T, env *convertEnv) {
				seed := oneToOneEvent(t, "evt-short", at, sql.NullString{})
				seed.hash = bytes.Repeat([]byte{0x01}, 16)
				env.seedEvent(t, seed)
			},
		},
		{
			name: "estimated quality without a value",
			build: func(t *testing.T, env *convertEnv) {
				seed := oneToOneEvent(t, "evt-estimated-null", at, sql.NullString{})
				seed.values[2], seed.qualities[2] = sql.NullInt64{}, "estimated"
				env.seedEvent(t, seed)
			},
		},
		{
			name: "known quality without a value",
			build: func(t *testing.T, env *convertEnv) {
				seed := oneToOneEvent(t, "evt-known-null", at, sql.NullString{})
				seed.values[0], seed.qualities[0] = sql.NullInt64{}, "known"
				env.seedEvent(t, seed)
			},
		},
		{
			name: "missing quality with a value",
			build: func(t *testing.T, env *convertEnv) {
				seed := oneToOneEvent(t, "evt-missing-value", at, sql.NullString{})
				seed.values[0], seed.qualities[0] = nullInt(1200), "missing"
				env.seedEvent(t, seed)
			},
		},
		{
			name: "negative value",
			build: func(t *testing.T, env *convertEnv) {
				seed := oneToOneEvent(t, "evt-negative", at, sql.NullString{})
				seed.values[0], seed.qualities[0] = nullInt(-1), "known"
				env.seedEvent(t, seed)
			},
		},
		{
			name: "value beyond the safe integer",
			build: func(t *testing.T, env *convertEnv) {
				seed := oneToOneEvent(t, "evt-overflow", at, sql.NullString{})
				seed.values[0], seed.qualities[0] = nullInt(9007199254740992), "known"
				env.seedEvent(t, seed)
			},
		},
		{
			name: "missing timing column",
			build: func(t *testing.T, env *convertEnv) {
				seed := oneToOneEvent(t, "evt-column", at, sql.NullString{})
				env.seedEvent(t, seed)
				if _, err := env.db.Exec(`ALTER TABLE usage_events DROP COLUMN ttft_ms`); err != nil {
					t.Fatal(database.SafeError("drop column", err))
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newConvertEnv(t)
			tc.build(t, env)
			before := env.tableDigest(t)
			if err := env.newConverter().check(context.Background()); err == nil {
				t.Fatal("check accepted an unsafe stored state")
			}
			if err := env.newConverter().apply(context.Background()); err == nil {
				t.Fatal("apply accepted an unsafe stored state")
			}
			if got := env.tableDigest(t); got != before {
				t.Fatal("a refused run changed stored rows")
			}
		})
	}
}

func TestConvertRollsBackWhenOneRowFails(t *testing.T) {
	env := newConvertEnv(t)
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	first := oneToOneEvent(t, "evt-first", at, sql.NullString{})
	blocked := oneToOneEvent(t, "evt-blocked", at.Add(time.Minute), sql.NullString{})
	env.seedEvent(t, first)
	env.seedEvent(t, blocked)
	// A test-only trigger in the throwaway schema makes the second update fail after
	// the first one already succeeded inside the transaction.
	if _, err := env.db.Exec(`CREATE FUNCTION t070_reject() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.source_event_id = 'evt-blocked' THEN
        RAISE EXCEPTION 'blocked row';
    END IF;
    RETURN NEW;
END $$`); err != nil {
		t.Fatal(database.SafeError("create trigger function", err))
	}
	if _, err := env.db.Exec(`CREATE TRIGGER t070_reject BEFORE UPDATE ON usage_events
FOR EACH ROW EXECUTE FUNCTION t070_reject()`); err != nil {
		t.Fatal(database.SafeError("create trigger", err))
	}
	firstDigest := env.storedDigest(t, first.id)
	if err := env.newConverter().apply(context.Background()); err == nil {
		t.Fatal("apply reported success although a row failed")
	}
	if got := env.countVersion(t, retiredVersionValue); got != 2 {
		t.Fatalf("retired rows = %d, want 2: the transaction was not rolled back", got)
	}
	if got := env.storedDigest(t, first.id); !bytes.Equal(got, firstDigest) {
		t.Fatal("the first converted row survived a rollback")
	}
}

func TestConvertReportsAnUnknownCommit(t *testing.T) {
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

	t.Run("the commit failed and the rows stay retired", func(t *testing.T) {
		env := newConvertEnv(t)
		// Two rows carry identical business content but different identities, so they
		// share one digest. A deferred unique constraint turns that into a failure that
		// only surfaces at COMMIT: the transaction really is not applied.
		first := oneToOneEvent(t, "evt-same-a", at, sql.NullString{})
		second := first
		second.id = newID(t)
		second.sourceEventID = "evt-same-b"
		second.hash = bytes.Repeat([]byte{0xAC}, 32)
		env.seedEvent(t, first)
		env.seedEvent(t, second)
		if _, err := env.db.Exec(`ALTER TABLE usage_events
ADD CONSTRAINT t070_hash_unique UNIQUE (payload_hash) DEFERRABLE INITIALLY DEFERRED`); err != nil {
			t.Fatal(database.SafeError("add deferred constraint", err))
		}
		t.Setenv("TOKENPIVOT_MIGRATION_DATABASE_URL", env.dsn)
		capture := newCaptureLog()
		err := run(context.Background(), []string{"--apply"}, capture.logger)
		var unknown *commitUnknownError
		if !errors.As(err, &unknown) {
			t.Fatalf("err = %v, want the commit unknown status", err)
		}
		if strings.Contains(err.Error(), env.schema) {
			t.Fatalf("err = %q, want no schema detail", err)
		}
		if got := env.countVersion(t, retiredVersionValue); got != 2 {
			t.Fatalf("retired rows = %d, want 2: a failed commit must not be reported as applied", got)
		}
		if strings.TrimSpace(capture.buffer.String()) != "" {
			t.Fatalf("a failed commit logged %q, want no success report and no retry", capture.buffer.String())
		}
		// A fresh connection is the operator's only trustworthy confirmation.
		if got := env.checkOnNewConnection(t); got != "needs_conversion" {
			t.Fatalf("check state = %q, want needs_conversion", got)
		}
	})

	t.Run("the commit was applied before the error surfaced", func(t *testing.T) {
		env := newConvertEnv(t)
		first := oneToOneEvent(t, "evt-committed-a", at, sql.NullString{})
		second := oneToOneEvent(t, "evt-committed-b", at.Add(time.Minute), sql.NullString{})
		env.seedEvent(t, first)
		env.seedEvent(t, second)
		// The server applies the transaction, then the client sees a transport error. The
		// command must still report the unknown result and must not rewrite or restore
		// anything: only the fresh read-only check can tell what happened.
		capture := newCaptureLog()
		c := &converter{db: env.db, logger: capture.logger, commit: func(tx *sql.Tx) error {
			if err := tx.Commit(); err != nil {
				return err
			}
			return errors.New("transport closed after commit")
		}}
		err := c.apply(context.Background())
		var unknown *commitUnknownError
		if !errors.As(err, &unknown) {
			t.Fatalf("err = %v, want the commit unknown status", err)
		}
		if strings.TrimSpace(capture.buffer.String()) != "" {
			t.Fatalf("an unconfirmed commit logged %q, want no success report", capture.buffer.String())
		}
		if got := env.countVersion(t, retiredVersionValue); got != 0 {
			t.Fatalf("retired rows = %d, want 0: this commit really was applied", got)
		}
		if got := env.checkOnNewConnection(t); got != "converted" {
			t.Fatalf("check state = %q, want converted", got)
		}
	})
}

func TestConvertRollsBackWhenACommittedColumnDrifts(t *testing.T) {
	env := newConvertEnv(t)
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	first := oneToOneEvent(t, "evt-drift-first", at, sql.NullString{})
	second := oneToOneEvent(t, "evt-drift-second", at.Add(time.Minute), sql.NullString{})
	env.seedEvent(t, first)
	env.seedEvent(t, second)
	// A test-only trigger rewrites received_at, the identity and the workspace of the
	// second row during the UPDATE. The digest input carries none of them, so only the
	// whole row snapshot can detect the drift, and the transaction must roll back.
	if _, err := env.db.Exec(`CREATE FUNCTION t070_drift() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.source_event_id = 'evt-drift-second' THEN
        NEW.received_at = NEW.received_at + interval '1 second';
        NEW.source_event_id = 'evt-drift-moved';
        NEW.workspace_id = '33333333-3333-4333-8333-333333333333';
    END IF;
    RETURN NEW;
END $$`); err != nil {
		t.Fatal(database.SafeError("create drift function", err))
	}
	if _, err := env.db.Exec(`CREATE TRIGGER t070_drift BEFORE UPDATE ON usage_events
FOR EACH ROW EXECUTE FUNCTION t070_drift()`); err != nil {
		t.Fatal(database.SafeError("create drift trigger", err))
	}
	beforeTable := env.tableDigest(t)
	beforeDigests := [][]byte{env.storedDigest(t, first.id), env.storedDigest(t, second.id)}
	err := env.newConverter().apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "column the conversion does not own") {
		t.Fatalf("err = %v, want the whole row snapshot to refuse the drift", err)
	}
	if got := env.countVersion(t, retiredVersionValue); got != 2 {
		t.Fatalf("retired rows = %d, want 2: the drift was not rolled back", got)
	}
	if got := env.tableDigest(t); got != beforeTable {
		t.Fatal("a refused conversion changed stored rows")
	}
	for i, id := range []string{first.id, second.id} {
		if got := env.storedDigest(t, id); !bytes.Equal(got, beforeDigests[i]) {
			t.Fatalf("row %d kept a converted digest after a rollback", i)
		}
	}
	// The read-only check does not update anything, so the trigger stays quiet and the
	// stored rows are still convertible.
	if err := env.newConverter().check(context.Background()); err != nil {
		t.Fatalf("check failed on a drifter that only changes during UPDATE: %v", err)
	}
}
