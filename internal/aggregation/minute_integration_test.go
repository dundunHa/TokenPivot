//go:build integration

package aggregation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// minuteMetrics is the fixed metric order shared by the bucket reader and the
// assertions below.
var minuteMetrics = []string{"input_tokens", "output_tokens", "total_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"}

// metricIndex returns the position of a metric in minuteMetrics.
func metricIndex(name string) int {
	for i, metric := range minuteMetrics {
		if metric == name {
			return i
		}
	}
	panic("unknown metric " + name)
}

// env carries the two pinned per-schema URLs. Only non-sensitive metadata is ever
// logged: the schema name, the database name and the role names.
type env struct {
	ownerURL string
	appURL   string
	schema   string
}

// newEnv creates a fresh random schema in the dedicated test database, verifies
// the roles and the server version, applies every released migration through
// goose and adds the test-only detail probe table. Missing or unsafe integration
// variables fail the test; nothing is skipped.
func newEnv(t *testing.T) *env {
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
	var db, role string
	var super, createRole bool
	if err := owner.QueryRow(ctx, "SELECT current_database(),current_user,rolsuper,rolcreaterole FROM pg_roles WHERE rolname=current_user").Scan(&db, &role, &super, &createRole); err != nil {
		t.Fatal(database.SafeError("identity", err))
	}
	if db != strings.TrimPrefix(urls[0].Path, "/") || role != urls[0].User.Username() || super || createRole {
		t.Fatal("unsafe test migration role")
	}
	appRole := verifyAppRole(t, ctx, values[1], urls[1].User.Username())
	// NULLS NOT DISTINCT needs PostgreSQL 15 or newer. A lower server would silently
	// change the uniqueness rule, so stop instead of weakening it.
	var serverVersion string
	var versionNumber int
	if err := owner.QueryRow(ctx, "SELECT current_setting('server_version'), current_setting('server_version_num')::int").Scan(&serverVersion, &versionNumber); err != nil {
		t.Fatal(database.SafeError("server version", err))
	}
	if versionNumber < 150000 {
		t.Fatalf("server_version=%s does not support the NULLS NOT DISTINCT uniqueness this schema requires", serverVersion)
	}
	t.Logf("server_version=%s server_version_num=%d", serverVersion, versionNumber)
	schema := "t012_" + hex.EncodeToString(randomBytes(t, 6))
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
		if err != nil || actual != role || !strings.HasPrefix(schema, "t012_") {
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
	t.Logf("owned schema=%s database=%s migrator=%s app=%s", schema, db, role, appRole)
	for i, u := range urls {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		values[i] = u.String()
	}
	applyMigrations(t, values[0])
	// The detail probe table stands in for usage_events when verifying the same
	// transaction pattern: the identity key is the only duplicate arbiter and the
	// service role gets the same SELECT and INSERT it holds on usage_events. It has
	// no generated column, so the service role needs no sequence privilege either.
	quotedAppRole := pgx.Identifier{appRole}.Sanitize()
	probe := quoted + ".aggregation_detail_probe"
	if _, err := owner.Exec(ctx, `CREATE TABLE `+probe+` (
        workspace_id uuid NOT NULL,
        source_id uuid NOT NULL,
        source_event_id text COLLATE "C" NOT NULL,
        CONSTRAINT aggregation_detail_probe_pkey PRIMARY KEY (workspace_id, source_id, source_event_id)
    )`); err != nil {
		t.Fatal(database.SafeError("create detail probe", err))
	}
	if _, err := owner.Exec(ctx, "GRANT SELECT, INSERT ON "+probe+" TO "+quotedAppRole); err != nil {
		t.Fatal(database.SafeError("grant detail probe", err))
	}
	return &env{ownerURL: values[0], appURL: values[1], schema: schema}
}

// verifyAppRole connects as the service test role and returns its name after
// checking it is the expected, unprivileged role that inherits tokenpivot_app.
// The connection is closed before returning.
func verifyAppRole(t *testing.T, ctx context.Context, appURL, wantRole string) string {
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
	// The released migrations grant table privileges to the fixed tokenpivot_app
	// role, so the service test account must be that role or inherit it.
	var appPrivileges bool
	if err := app.QueryRow(ctx, "SELECT pg_has_role(current_user, 'tokenpivot_app', 'USAGE')").Scan(&appPrivileges); err != nil {
		t.Fatal(database.SafeError("service role privileges", err))
	}
	if !appPrivileges {
		t.Fatal("test service role must have tokenpivot_app privileges")
	}
	return appRole
}

// applyMigrations runs every released migration inside the pinned schema on one
// connection.
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

// openConn opens one connection that is closed when the test ends.
func openConn(t *testing.T, ctx context.Context, value, label string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, value)
	if err != nil {
		t.Fatal(database.SafeError("connect "+label, err))
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Error("close " + label + " failed")
		}
	})
	return conn
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal("random bytes failed")
	}
	return b
}

// newID returns a random version 4 UUID; the value is only an opaque test identity.
func newID(t *testing.T) pgtype.UUID {
	t.Helper()
	b := randomBytes(t, 16)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var id pgtype.UUID
	id.Bytes = [16]byte(b)
	id.Valid = true
	return id
}

// minuteInput builds one valid request-level input with six known zeros under a
// fresh workspace and source; its occurred_at carries sub-second digits.
func minuteInput(t *testing.T) MinuteInput {
	t.Helper()
	in := requestInput()
	in.WorkspaceID = newID(t)
	in.SourceID = newID(t)
	in.OccurredAt = time.Date(2026, 10, 27, 12, 34, 30, 123456000, time.UTC)
	return in
}

// knownMetric returns a known metric with the given value; it is a fresh pointer so
// each input owns its value.
func knownMetric(v int64) Metric {
	return Metric{Value: &v, Quality: QualityKnown}
}

func estimatedMetric(v int64) Metric {
	return Metric{Value: &v, Quality: QualityEstimated}
}

func missingMetric() Metric {
	return Metric{Quality: QualityMissing}
}

// probeInsertSQL uses the command tag instead of RETURNING, so the probe table only
// needs INSERT for the service role, exactly like a detail insert that reports
// duplicates through the row count.
const probeInsertSQL = `INSERT INTO aggregation_detail_probe (workspace_id, source_id, source_event_id)
VALUES ($1,$2,$3) ON CONFLICT (workspace_id, source_id, source_event_id) DO NOTHING`

// insertProbe inserts one detail probe row in the caller's transaction. A false
// result means the identity already exists, so the writer must not be called and
// no minute increment may happen.
func insertProbe(ctx context.Context, tx pgx.Tx, workspaceID, sourceID pgtype.UUID, eventID string) (bool, error) {
	tag, err := tx.Exec(ctx, probeInsertSQL, workspaceID, sourceID, eventID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// bucketRow is the stored minute bucket as the tests read it back.
type bucketRow struct {
	eventCount int64
	sum        [6]int64
	known      [6]int64
	estimated  [6]int64
	missing    [6]int64
}

// bucketSelect lists every minute column explicitly; the metric list is a compile
// time constant, so no caller input reaches the SQL text.
var bucketSelect = func() string {
	columns := []string{"event_count"}
	for _, metric := range minuteMetrics {
		columns = append(columns, metric+"_sum", metric+"_known_count", metric+"_estimated_count", metric+"_missing_count")
	}
	return "SELECT " + strings.Join(columns, ",") + " FROM usage_minute_stats WHERE workspace_id=$1 AND bucket_start=$2 AND source_id=$3 AND project_id IS NOT DISTINCT FROM $4 AND provider=$5 AND model=$6"
}()

// readBucket returns the stored bucket, failing when the row is absent.
func readBucket(t *testing.T, ctx context.Context, conn *pgx.Conn, in MinuteInput) bucketRow {
	t.Helper()
	var row bucketRow
	dest := []any{&row.eventCount}
	for i := range minuteMetrics {
		dest = append(dest, &row.sum[i], &row.known[i], &row.estimated[i], &row.missing[i])
	}
	err := conn.QueryRow(ctx, bucketSelect, in.WorkspaceID, bucketStart(in.OccurredAt), in.SourceID, in.ProjectID, in.Provider, in.Model).Scan(dest...)
	if err != nil {
		t.Fatal(database.SafeError("read minute bucket", err))
	}
	return row
}

// countBuckets returns how many minute rows exist for one workspace.
func countBuckets(t *testing.T, ctx context.Context, conn *pgx.Conn, workspaceID pgtype.UUID) int {
	t.Helper()
	var count int
	if err := conn.QueryRow(ctx, "SELECT count(*)::int FROM usage_minute_stats WHERE workspace_id=$1", workspaceID).Scan(&count); err != nil {
		t.Fatal(database.SafeError("count minute buckets", err))
	}
	return count
}

// countDetails returns how many detail probe rows exist for one workspace.
func countDetails(t *testing.T, ctx context.Context, conn *pgx.Conn, workspaceID pgtype.UUID) int {
	t.Helper()
	var count int
	if err := conn.QueryRow(ctx, "SELECT count(*)::int FROM aggregation_detail_probe WHERE workspace_id=$1", workspaceID).Scan(&count); err != nil {
		t.Fatal(database.SafeError("count detail probes", err))
	}
	return count
}

// requireNothingWritten asserts a workspace has neither a detail row nor a minute
// bucket, which is what every rolled back or rejected event must leave behind.
func requireNothingWritten(t *testing.T, ctx context.Context, conn *pgx.Conn, workspaceID pgtype.UUID, situation string) {
	t.Helper()
	if detail, minute := countDetails(t, ctx, conn, workspaceID), countBuckets(t, ctx, conn, workspaceID); detail != 0 || minute != 0 {
		t.Fatalf("%s left detail=%d minute=%d want 0 and 0", situation, detail, minute)
	}
}

// beginWithProbe begins a transaction on conn and inserts the event's detail
// probe row, the first step of the intended T-011 call order.
func beginWithProbe(t *testing.T, ctx context.Context, conn *pgx.Conn, in MinuteInput, eventID string) pgx.Tx {
	t.Helper()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(database.SafeError("begin", err))
	}
	inserted, err := insertProbe(ctx, tx, in.WorkspaceID, in.SourceID, eventID)
	if err != nil || !inserted {
		t.Fatal("detail probe insert failed")
	}
	return tx
}

// requireSanitized asserts a returned error keeps no SQL, parameter, driver detail
// or SQLSTATE.
func requireSanitized(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	text := err.Error()
	for _, leak := range []string{"22003", "23505", "SQLSTATE", "bigint", "usage_minute_stats", "$1", "aggregation_detail_probe", "INSERT INTO"} {
		if strings.Contains(text, leak) {
			t.Fatalf("error leaked %q: %s", leak, text)
		}
	}
}

// commitEvent inserts the detail probe row and applies the minute increment in one
// transaction, mirroring the intended T-011 call order.
func commitEvent(t *testing.T, ctx context.Context, conn *pgx.Conn, in MinuteInput, eventID string) {
	t.Helper()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(database.SafeError("begin", err))
	}
	defer func() {
		if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error("rollback failed")
		}
	}()
	inserted, err := insertProbe(ctx, tx, in.WorkspaceID, in.SourceID, eventID)
	if err != nil {
		t.Fatal(database.SafeError("insert detail probe", err))
	}
	if !inserted {
		t.Fatal("detail probe identity was not inserted")
	}
	if err := ApplyRequest(ctx, tx, in); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(database.SafeError("commit", err))
	}
}

// TestUsageMinuteStatsSchema verifies the migrated objects: the exact column
// shape, the single six-column NULLS NOT DISTINCT unique constraint and the
// minimal service role privileges.
func TestUsageMinuteStatsSchema(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	owner := openConn(t, ctx, e.ownerURL, "owner")
	app := openConn(t, ctx, e.appURL, "app")

	t.Run("columns", func(t *testing.T) {
		expected := map[string]struct{ dataType, nullable string }{
			"workspace_id": {"uuid", "NO"},
			"bucket_start": {"timestamp with time zone", "NO"},
			"source_id":    {"uuid", "NO"},
			"project_id":   {"uuid", "YES"},
			"provider":     {"text", "NO"},
			"model":        {"text", "NO"},
			"event_count":  {"bigint", "NO"},
		}
		for _, metric := range minuteMetrics {
			expected[metric+"_sum"] = struct{ dataType, nullable string }{"bigint", "NO"}
			expected[metric+"_known_count"] = struct{ dataType, nullable string }{"bigint", "NO"}
			expected[metric+"_estimated_count"] = struct{ dataType, nullable string }{"bigint", "NO"}
			expected[metric+"_missing_count"] = struct{ dataType, nullable string }{"bigint", "NO"}
		}
		actual := map[string]struct{ dataType, nullable, defaultText string }{}
		rows, err := owner.Query(ctx, "SELECT column_name::text, data_type::text, is_nullable::text, coalesce(column_default::text,'') FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='usage_minute_stats'")
		if err != nil {
			t.Fatal(database.SafeError("columns", err))
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var spec struct{ dataType, nullable, defaultText string }
			if err := rows.Scan(&name, &spec.dataType, &spec.nullable, &spec.defaultText); err != nil {
				t.Fatal(database.SafeError("scan column", err))
			}
			actual[name] = spec
		}
		if err := rows.Err(); err != nil {
			t.Fatal(database.SafeError("column rows", err))
		}
		if len(actual) != len(expected) {
			t.Fatalf("column count=%d want=%d", len(actual), len(expected))
		}
		for name, want := range expected {
			got, ok := actual[name]
			if !ok {
				t.Fatalf("missing column %s", name)
			}
			if got.dataType != want.dataType || got.nullable != want.nullable {
				t.Fatalf("column %s type=%q nullable=%q want type=%q nullable=%q", name, got.dataType, got.nullable, want.dataType, want.nullable)
			}
			if got.defaultText != "" {
				t.Fatalf("column %s has default %q, sums and counts must be explicit", name, got.defaultText)
			}
		}
		t.Logf("columns=%d all match", len(actual))
	})

	t.Run("unique constraint", func(t *testing.T) {
		var name string
		var unique, nullsNotDistinct bool
		var columns string
		err := owner.QueryRow(ctx, `SELECT i.relname::text, ix.indisunique, ix.indnullsnotdistinct,
            (SELECT string_agg(a.attname, ',' ORDER BY u.k)
               FROM unnest(string_to_array(ix.indkey::text, ' ')::int[]) WITH ORDINALITY AS u(attnum, k)
               JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum=u.attnum)::text
            FROM pg_index ix
            JOIN pg_class i ON i.oid=ix.indexrelid
            JOIN pg_class c ON c.oid=ix.indrelid
            JOIN pg_namespace n ON n.oid=c.relnamespace
            WHERE n.nspname=current_schema() AND c.relname='usage_minute_stats'`).Scan(&name, &unique, &nullsNotDistinct, &columns)
		if err != nil {
			t.Fatal(database.SafeError("unique constraint", err))
		}
		if name != "usage_minute_stats_key" || !unique || !nullsNotDistinct {
			t.Fatalf("index=%q unique=%t nulls_not_distinct=%t want usage_minute_stats_key unique with NULLS NOT DISTINCT", name, unique, nullsNotDistinct)
		}
		if columns != "workspace_id,bucket_start,source_id,project_id,provider,model" {
			t.Fatalf("unique columns=%q want the six-dimension key", columns)
		}
		var indexes int
		if err := owner.QueryRow(ctx, "SELECT count(*)::int FROM pg_index ix JOIN pg_class c ON c.oid=ix.indrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname='usage_minute_stats'").Scan(&indexes); err != nil {
			t.Fatal(database.SafeError("index count", err))
		}
		if indexes != 1 {
			t.Fatalf("index count=%d want=1 (only the unique constraint)", indexes)
		}
		for _, check := range []struct {
			name  string
			query string
		}{
			{"foreign keys and checks", "SELECT count(*)::int FROM pg_constraint con JOIN pg_class c ON c.oid=con.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname='usage_minute_stats' AND con.contype IN ('f','c')"},
			{"triggers", "SELECT count(*)::int FROM pg_trigger tg JOIN pg_class c ON c.oid=tg.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname='usage_minute_stats' AND NOT tg.tgisinternal"},
			{"row level security", "SELECT count(*)::int FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname='usage_minute_stats' AND (c.relrowsecurity OR c.relforcerowsecurity)"},
		} {
			var count int
			if err := owner.QueryRow(ctx, check.query).Scan(&count); err != nil {
				t.Fatal(database.SafeError(check.name, err))
			}
			if count != 0 {
				t.Fatalf("%s count=%d want=0", check.name, count)
			}
		}
	})

	t.Run("service role privileges", func(t *testing.T) {
		var privileges string
		if err := owner.QueryRow(ctx, "SELECT string_agg(privilege_type::text, ',' ORDER BY privilege_type)::text FROM information_schema.role_table_grants WHERE grantee='tokenpivot_app' AND table_schema=current_schema() AND table_name='usage_minute_stats'").Scan(&privileges); err != nil {
			t.Fatal(database.SafeError("grants", err))
		}
		if privileges != "INSERT,SELECT,UPDATE" {
			t.Fatalf("grants=%q want=INSERT,SELECT,UPDATE", privileges)
		}
		for _, privilege := range []string{"DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
			var granted bool
			if err := owner.QueryRow(ctx, "SELECT has_table_privilege('tokenpivot_app', current_schema()||'.usage_minute_stats', $1)", privilege).Scan(&granted); err != nil {
				t.Fatal(database.SafeError("privilege", err))
			}
			if granted {
				t.Fatalf("service role unexpectedly holds %s", privilege)
			}
		}
		for _, statement := range []string{
			"DELETE FROM usage_minute_stats",
			"TRUNCATE usage_minute_stats",
			"ALTER TABLE usage_minute_stats ADD COLUMN forbidden_probe integer",
			"DROP TABLE usage_minute_stats",
			"INSERT INTO goose_db_version (version_id, is_applied) VALUES (999, true)",
		} {
			_, err := app.Exec(ctx, statement)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Fatalf("statement %q error=%v want SQLSTATE 42501", statement, err)
			}
		}
		t.Log("service role denied DELETE, TRUNCATE, ALTER, DROP and goose history writes")
	})
}

// TestApplyRequestTransactionPattern covers commit/rollback visibility, the detail
// identity arbitration and concurrent increments of distinct events.
func TestApplyRequestTransactionPattern(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	owner := openConn(t, ctx, e.ownerURL, "owner")

	t.Run("commit visibility", func(t *testing.T) {
		in := minuteInput(t)
		connA := openConn(t, ctx, e.appURL, "A")
		observer := openConn(t, ctx, e.appURL, "observer")
		tx, err := connA.Begin(ctx)
		if err != nil {
			t.Fatal(database.SafeError("begin A", err))
		}
		defer func() {
			if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Error("rollback A failed")
			}
		}()
		inserted, err := insertProbe(ctx, tx, in.WorkspaceID, in.SourceID, "synthetic-commit")
		if err != nil || !inserted {
			t.Fatal("detail probe insert failed")
		}
		if err := ApplyRequest(ctx, tx, in); err != nil {
			t.Fatal(err)
		}
		if visible := countBuckets(t, ctx, observer, in.WorkspaceID); visible != 0 {
			t.Fatalf("uncommitted minute rows=%d want=0", visible)
		}
		if detailVisible := countDetails(t, ctx, observer, in.WorkspaceID); detailVisible != 0 {
			t.Fatalf("uncommitted detail rows=%d want=0", detailVisible)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(database.SafeError("commit A", err))
		}
		if detailVisible := countDetails(t, ctx, observer, in.WorkspaceID); detailVisible != 1 {
			t.Fatalf("committed detail rows=%d want=1", detailVisible)
		}
		row := readBucket(t, ctx, observer, in)
		if row.eventCount != 1 {
			t.Fatalf("committed event_count=%d want=1", row.eventCount)
		}
	})

	t.Run("rollback leaves nothing", func(t *testing.T) {
		in := minuteInput(t)
		conn := openConn(t, ctx, e.appURL, "rollback")
		tx := beginWithProbe(t, ctx, conn, in, "synthetic-rollback")
		if err := ApplyRequest(ctx, tx, in); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(database.SafeError("rollback", err))
		}
		requireNothingWritten(t, ctx, owner, in.WorkspaceID, "rollback")
	})

	t.Run("same identity conflict does not increment", func(t *testing.T) {
		in := minuteInput(t)
		const eventID = "synthetic-same-identity"
		connA := openConn(t, ctx, e.appURL, "A")
		connB := openConn(t, ctx, e.appURL, "B")
		txA, err := connA.Begin(ctx)
		if err != nil {
			t.Fatal(database.SafeError("begin A", err))
		}
		defer func() {
			if err := txA.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Error("rollback A failed")
			}
		}()
		inserted, err := insertProbe(ctx, txA, in.WorkspaceID, in.SourceID, eventID)
		if err != nil || !inserted {
			t.Fatal("A detail probe insert failed")
		}
		if err := ApplyRequest(ctx, txA, in); err != nil {
			t.Fatal(err)
		}
		txB, err := connB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			t.Fatal(database.SafeError("begin B", err))
		}
		t.Cleanup(func() {
			err := txB.Rollback(context.Background())
			if err != nil && !errors.Is(err, pgx.ErrTxClosed) && !errors.Is(err, pgconn.ErrConnClosed) {
				t.Error("rollback B failed")
			}
		})
		// B blocks on the detail identity key while A holds it uncommitted.
		var pidB int
		if err := connB.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pidB); err != nil {
			t.Fatal(database.SafeError("pid B", err))
		}
		type result struct {
			inserted bool
			err      error
		}
		done := make(chan result, 1)
		blockCtx, stopBlock := context.WithTimeout(ctx, 20*time.Second)
		var wg sync.WaitGroup
		t.Cleanup(func() {
			stopBlock()
			wg.Wait()
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			inserted, err := insertProbe(blockCtx, txB, in.WorkspaceID, in.SourceID, eventID)
			done <- result{inserted: inserted, err: err}
		}()
		waitForBlockedLock(t, ctx, owner, pidB)
		if err := txA.Commit(ctx); err != nil {
			t.Fatal(database.SafeError("commit A", err))
		}
		outcome := <-done
		if outcome.err != nil {
			t.Fatal(database.SafeError("blocked detail insert", outcome.err))
		}
		if outcome.inserted {
			t.Fatal("conflicting detail identity reported an inserted row")
		}
		// The losing caller must not call the writer at all.
		if err := txB.Rollback(ctx); err != nil {
			t.Fatal(database.SafeError("rollback B", err))
		}
		row := readBucket(t, ctx, owner, in)
		if row.eventCount != 1 {
			t.Fatalf("event_count=%d want=1 after one accepted event", row.eventCount)
		}
	})

	t.Run("distinct events increment concurrently", func(t *testing.T) {
		in := minuteInput(t)
		const events = 8
		conns := make([]*pgx.Conn, events)
		for i := range conns {
			conns[i] = openConn(t, ctx, e.appURL, fmt.Sprintf("contender-%d", i))
		}
		inputs := make([]MinuteInput, events)
		for i := range inputs {
			inputs[i] = in
			inputs[i].Input = knownMetric(int64(10 + i))
			inputs[i].Output = knownMetric(1)
			inputs[i].Total = knownMetric(int64(11 + i))
			inputs[i].CacheRead = missingMetric()
			// cache_write carries a distinct known value while cache_read stays missing,
			// so a swapped or dropped column between the two cache metrics cannot pass.
			inputs[i].CacheWrite = knownMetric(3)
			// One estimated metric per event proves the estimated count and its value
			// sum accumulate without ever being reclassified as known.
			inputs[i].Reasoning = estimatedMetric(2)
		}
		type result struct {
			index int
			err   error
		}
		results := make(chan result, events)
		release := make(chan struct{})
		contendCtx, stopContend := context.WithTimeout(ctx, 30*time.Second)
		var wg sync.WaitGroup
		t.Cleanup(func() {
			stopContend()
			wg.Wait()
		})
		wg.Add(events)
		for i := range inputs {
			go func(i int) {
				defer wg.Done()
				<-release
				conn := conns[i]
				tx, err := conn.BeginTx(contendCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				if err != nil {
					results <- result{index: i, err: err}
					return
				}
				inserted, err := insertProbe(contendCtx, tx, inputs[i].WorkspaceID, inputs[i].SourceID, fmt.Sprintf("synthetic-concurrent-%d", i))
				if err != nil || !inserted {
					_ = tx.Rollback(context.Background())
					if err == nil {
						err = errors.New("detail probe identity not inserted")
					}
					results <- result{index: i, err: err}
					return
				}
				if err := ApplyRequest(contendCtx, tx, inputs[i]); err != nil {
					_ = tx.Rollback(context.Background())
					results <- result{index: i, err: err}
					return
				}
				if err := tx.Commit(contendCtx); err != nil {
					results <- result{index: i, err: err}
					return
				}
				results <- result{index: i}
			}(i)
		}
		close(release)
		for range inputs {
			outcome := <-results
			if outcome.err != nil {
				t.Fatal(database.SafeError("concurrent event", outcome.err))
			}
		}
		row := readBucket(t, ctx, owner, in)
		if row.eventCount != events {
			t.Fatalf("event_count=%d want=%d", row.eventCount, events)
		}
		var wantInput, wantOutput, wantTotal, wantReasoning int64
		for _, input := range inputs {
			wantInput += *input.Input.Value
			wantOutput += *input.Output.Value
			wantTotal += *input.Total.Value
			wantReasoning += *input.Reasoning.Value
		}
		if wantInput != 108 || wantOutput != 8 || wantTotal != 116 || wantReasoning != 16 {
			t.Fatalf("fixture sums input=%d output=%d total=%d reasoning=%d, the explicit expectations below assume 108/8/116/16", wantInput, wantOutput, wantTotal, wantReasoning)
		}
		// Expected columns are written out independently of the writer's own delta
		// computation, so a wrong metric mapping cannot make the test agree with
		// itself. The eight inputs are: input known 10..17, output known 1, total
		// known 11..18, cache_read missing, cache_write known 3, reasoning estimated 2.
		type expectation struct {
			metric           string
			sum              int64
			known, estimated int64
			missing          int64
		}
		expected := []expectation{
			{"input_tokens", 108, 8, 0, 0},
			{"output_tokens", 8, 8, 0, 0},
			{"total_tokens", 116, 8, 0, 0},
			{"cache_read_tokens", 0, 0, 0, 8},
			{"cache_write_tokens", 24, 8, 0, 0},
			{"reasoning_tokens", 16, 0, 8, 0},
		}
		if len(expected) != len(minuteMetrics) {
			t.Fatalf("expectation list has %d metrics, want %d", len(expected), len(minuteMetrics))
		}
		for i, want := range expected {
			if minuteMetrics[i] != want.metric {
				t.Fatalf("expectation %d is %s but the column order says %s", i, want.metric, minuteMetrics[i])
			}
			if got := row.sum[i]; got != want.sum {
				t.Fatalf("%s sum=%d want=%d", want.metric, got, want.sum)
			}
			if got := row.known[i]; got != want.known {
				t.Fatalf("%s known=%d want=%d", want.metric, got, want.known)
			}
			if got := row.estimated[i]; got != want.estimated {
				t.Fatalf("%s estimated=%d want=%d", want.metric, got, want.estimated)
			}
			if got := row.missing[i]; got != want.missing {
				t.Fatalf("%s missing=%d want=%d", want.metric, got, want.missing)
			}
			if want.known+want.estimated+want.missing != events {
				t.Fatalf("%s expectation %d/%d/%d does not add up to %d events", want.metric, want.known, want.estimated, want.missing, events)
			}
		}
		if count := countBuckets(t, ctx, owner, in.WorkspaceID); count != 1 {
			t.Fatalf("buckets=%d want=1", count)
		}
		t.Logf("events=%d concurrent increments merged into one bucket with all six metric column groups matching", events)
	})
}

// waitForBlockedLock polls pg_locks until the given backend waits on an ungranted
// lock, so the test never guesses scheduling with sleeps.
func waitForBlockedLock(t *testing.T, ctx context.Context, conn *pgx.Conn, pid int) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := conn.QueryRow(deadline, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND NOT granted)", pid).Scan(&blocked); err != nil {
			t.Fatal(database.SafeError("observe blocked lock", err))
		}
		if blocked {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.Done():
			t.Fatal("competing detail insert did not block on the identity key")
		}
	}
}

// TestMinuteMetricsAndBuckets covers the documented acceptance rows: NULL project,
// six-dimension bucketing, known zero, estimated, missing metrics and the fact
// that total, cache and reasoning are never double counted.
func TestMinuteMetricsAndBuckets(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	owner := openConn(t, ctx, e.ownerURL, "owner")
	app := openConn(t, ctx, e.appURL, "app")

	t.Run("null project is one bucket", func(t *testing.T) {
		in := minuteInput(t)
		in.ProjectID = pgtype.UUID{}
		first := in
		first.Input = knownMetric(5)
		second := in
		second.Input = knownMetric(7)
		second.Output = missingMetric()
		commitEvent(t, ctx, app, first, "synthetic-null-project-1")
		commitEvent(t, ctx, app, second, "synthetic-null-project-2")
		row := readBucket(t, ctx, owner, in)
		if row.eventCount != 2 {
			t.Fatalf("event_count=%d want=2", row.eventCount)
		}
		if got := row.sum[metricIndex("input_tokens")]; got != 12 {
			t.Fatalf("input sum=%d want=12", got)
		}
		if got := row.known[metricIndex("output_tokens")]; got != 1 {
			t.Fatalf("output known=%d want=1", got)
		}
		if got := row.missing[metricIndex("output_tokens")]; got != 1 {
			t.Fatalf("output missing=%d want=1", got)
		}
		if buckets := countBuckets(t, ctx, owner, in.WorkspaceID); buckets != 1 {
			t.Fatalf("buckets=%d want=1 for NULL project", buckets)
		}
	})

	t.Run("six dimension bucketing", func(t *testing.T) {
		base := minuteInput(t)
		commitEvent(t, ctx, app, base, "synthetic-dimension-base")
		variants := map[string]MinuteInput{}
		variant := base
		variant.WorkspaceID = newID(t)
		variants["workspace"] = variant
		variant = base
		variant.SourceID = newID(t)
		variants["source"] = variant
		variant = base
		projectID := newID(t)
		variant.ProjectID = projectID
		variants["project"] = variant
		variant = base
		variant.Provider = "synthetic-other-provider"
		variants["provider"] = variant
		variant = base
		variant.Model = "synthetic-other-model"
		variants["model"] = variant
		variant = base
		variant.OccurredAt = base.OccurredAt.Add(time.Minute)
		variants["bucket_start"] = variant
		for name, input := range variants {
			commitEvent(t, ctx, app, input, "synthetic-dimension-"+name)
			if row := readBucket(t, ctx, owner, input); row.eventCount != 1 {
				t.Fatalf("%s variant event_count=%d want=1", name, row.eventCount)
			}
		}
		// Every dimension but workspace stays inside the base workspace, so its six
		// variants are six distinct buckets and none of them merged into the base.
		if buckets := countBuckets(t, ctx, owner, base.WorkspaceID); buckets != 6 {
			t.Fatalf("base workspace buckets=%d want=6 (base plus source, project, provider, model and next minute)", buckets)
		}
		// The aggregation key must not carry a credential dimension at all.
		var credentialColumns int
		if err := owner.QueryRow(ctx, "SELECT count(*)::int FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='usage_minute_stats' AND column_name LIKE '%credential%'").Scan(&credentialColumns); err != nil {
			t.Fatal(database.SafeError("credential columns", err))
		}
		if credentialColumns != 0 {
			t.Fatalf("credential columns=%d want=0", credentialColumns)
		}
	})

	t.Run("known zero is not missing", func(t *testing.T) {
		in := minuteInput(t)
		commitEvent(t, ctx, app, in, "synthetic-known-zero")
		row := readBucket(t, ctx, owner, in)
		for i, metric := range minuteMetrics {
			if row.sum[i] != 0 || row.known[i] != 1 || row.estimated[i] != 0 || row.missing[i] != 0 {
				t.Fatalf("%s sum=%d known=%d estimated=%d missing=%d want 0/1/0/0", metric, row.sum[i], row.known[i], row.estimated[i], row.missing[i])
			}
		}
	})

	t.Run("estimated and missing", func(t *testing.T) {
		in := minuteInput(t)
		in.Input = estimatedMetric(100)
		in.Output = knownMetric(20)
		in.Total = estimatedMetric(120)
		in.CacheRead = knownMetric(60)
		in.CacheWrite = missingMetric()
		in.Reasoning = missingMetric()
		commitEvent(t, ctx, app, in, "synthetic-estimated")
		row := readBucket(t, ctx, owner, in)
		if got := row.sum[metricIndex("input_tokens")]; got != 100 {
			t.Fatalf("input sum=%d want=100", got)
		}
		if got := row.estimated[metricIndex("input_tokens")]; got != 1 {
			t.Fatalf("input estimated=%d want=1", got)
		}
		if got := row.known[metricIndex("input_tokens")]; got != 0 {
			t.Fatalf("input known=%d want=0; an estimated value must not become known", got)
		}
		if got := row.sum[metricIndex("cache_write_tokens")]; got != 0 {
			t.Fatalf("cache_write sum=%d want=0", got)
		}
		if got := row.missing[metricIndex("cache_write_tokens")]; got != 1 {
			t.Fatalf("cache_write missing=%d want=1", got)
		}
		if got := row.missing[metricIndex("reasoning_tokens")]; got != 1 {
			t.Fatalf("reasoning missing=%d want=1", got)
		}
	})

	t.Run("all metrics missing still counts one event", func(t *testing.T) {
		in := minuteInput(t)
		in.Input = missingMetric()
		in.Output = missingMetric()
		in.Total = missingMetric()
		in.CacheRead = missingMetric()
		in.CacheWrite = missingMetric()
		in.Reasoning = missingMetric()
		commitEvent(t, ctx, app, in, "synthetic-all-missing")
		row := readBucket(t, ctx, owner, in)
		if row.eventCount != 1 {
			t.Fatalf("event_count=%d want=1; all missing is not a real zero request", row.eventCount)
		}
		for i, metric := range minuteMetrics {
			if row.sum[i] != 0 || row.known[i] != 0 || row.estimated[i] != 0 || row.missing[i] != 1 {
				t.Fatalf("%s sum=%d known=%d estimated=%d missing=%d want 0/0/0/1", metric, row.sum[i], row.known[i], row.estimated[i], row.missing[i])
			}
		}
	})

	t.Run("total is never recomputed", func(t *testing.T) {
		base := minuteInput(t)
		first := base
		first.Input = knownMetric(100)
		first.Output = knownMetric(20)
		first.Total = knownMetric(120)
		first.CacheRead = knownMetric(60)
		first.CacheWrite = missingMetric()
		first.Reasoning = missingMetric()
		second := base
		second.Input = missingMetric()
		second.Output = knownMetric(20)
		second.Total = missingMetric()
		second.CacheRead = missingMetric()
		second.CacheWrite = missingMetric()
		second.Reasoning = knownMetric(8)
		commitEvent(t, ctx, app, first, "synthetic-subset-1")
		commitEvent(t, ctx, app, second, "synthetic-subset-2")
		row := readBucket(t, ctx, owner, base)
		if row.eventCount != 2 {
			t.Fatalf("event_count=%d want=2", row.eventCount)
		}
		if got := row.sum[metricIndex("total_tokens")]; got != 120 {
			t.Fatalf("total sum=%d want=120; cache and reasoning are subsets", got)
		}
		if got := row.sum[metricIndex("output_tokens")]; got != 40 {
			t.Fatalf("output sum=%d want=40; reasoning is a subset of output", got)
		}
		if got := row.missing[metricIndex("total_tokens")]; got != 1 {
			t.Fatalf("total missing=%d want=1", got)
		}
		if got := row.sum[metricIndex("reasoning_tokens")]; got != 8 {
			t.Fatalf("reasoning sum=%d want=8", got)
		}
	})
}

// TestApplyRequestFailures covers the rejected inputs, cancellation, permission and
// bigint overflow paths: every failure leaves no detail row and no minute change.
func TestApplyRequestFailures(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	owner := openConn(t, ctx, e.ownerURL, "owner")
	app := openConn(t, ctx, e.appURL, "app")

	t.Run("non request level", func(t *testing.T) {
		in := minuteInput(t)
		in.Level = LevelSummary
		tx := beginWithProbe(t, ctx, app, in, "synthetic-summary")
		if err := ApplyRequest(ctx, tx, in); !errors.Is(err, ErrNonRequestLevel) {
			t.Fatalf("error=%v want ErrNonRequestLevel", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(database.SafeError("rollback", err))
		}
		requireNothingWritten(t, ctx, owner, in.WorkspaceID, "summary input")
	})

	t.Run("invalid metric representation", func(t *testing.T) {
		cases := map[string]func(*MinuteInput){
			"known without value":     func(in *MinuteInput) { in.Output = Metric{Quality: QualityKnown} },
			"estimated without value": func(in *MinuteInput) { in.Total = Metric{Quality: QualityEstimated} },
			"missing with value":      func(in *MinuteInput) { in.CacheRead = Metric{Value: value(0), Quality: QualityMissing} },
			"unknown quality":         func(in *MinuteInput) { in.Reasoning = Metric{Value: value(1), Quality: "synthetic-quality"} },
		}
		for name, mutate := range cases {
			in := minuteInput(t)
			mutate(&in)
			if err := ApplyRequest(ctx, nil, in); !errors.Is(err, ErrInvalidMetric) {
				t.Fatalf("%s error=%v want ErrInvalidMetric", name, err)
			}
			if buckets := countBuckets(t, ctx, owner, in.WorkspaceID); buckets != 0 {
				t.Fatalf("%s wrote %d minute buckets", name, buckets)
			}
		}
	})

	// The service role is denied UPDATE in this schema only, so the writer fails at
	// the statement instead of succeeding. The grant is restored by a cleanup that
	// also runs on t.Fatal, and the restore is proven before the subtest returns, so
	// the following subtests keep their normal privileges.
	t.Run("permission denied", func(t *testing.T) {
		table := pgx.Identifier{e.schema}.Sanitize() + ".usage_minute_stats"
		restore := func(ctx context.Context) error {
			_, err := owner.Exec(ctx, "GRANT SELECT, INSERT, UPDATE ON "+table+" TO tokenpivot_app")
			return err
		}
		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := restore(cleanupCtx); err != nil {
				t.Error(database.SafeError("restore grace", err))
			}
		})

		in := minuteInput(t)
		tx := beginWithProbe(t, ctx, app, in, "synthetic-permission-denied")
		if _, err := owner.Exec(ctx, "REVOKE INSERT, UPDATE ON "+table+" FROM tokenpivot_app"); err != nil {
			t.Fatal(database.SafeError("revoke for probe", err))
		}
		// Confirm the revocation actually reached the role under test before relying
		// on it, so a no-op REVOKE cannot make this subtest pass for the wrong reason.
		for _, privilege := range []string{"INSERT", "UPDATE"} {
			var granted bool
			if err := owner.QueryRow(ctx, "SELECT has_table_privilege('tokenpivot_app', $1, $2)", table, privilege).Scan(&granted); err != nil {
				t.Fatal(database.SafeError("verify revocation", err))
			}
			if granted {
				t.Fatalf("revoke left %s granted on %s", privilege, e.schema+".usage_minute_stats")
			}
		}
		err := ApplyRequest(ctx, tx, in)
		if err == nil {
			t.Fatal("writer succeeded without INSERT and UPDATE")
		}
		if errors.Is(err, ErrNonRequestLevel) || errors.Is(err, ErrInvalidMetric) {
			t.Fatalf("error=%v looks like a validation failure, not a database denial", err)
		}
		requireSanitized(t, err)
		// The caller owns rollback: the detail probe it inserted must not survive
		// together with any minute row.
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) && !errors.Is(rollbackErr, pgconn.ErrConnClosed) {
			t.Fatal(database.SafeError("rollback after denial", rollbackErr))
		}
		requireNothingWritten(t, ctx, owner, in.WorkspaceID, "permission denied write")

		if err := restore(ctx); err != nil {
			t.Fatal(database.SafeError("restore grants", err))
		}
		for _, privilege := range []string{"SELECT", "INSERT", "UPDATE"} {
			var granted bool
			if err := owner.QueryRow(ctx, "SELECT has_table_privilege('tokenpivot_app', $1, $2)", table, privilege).Scan(&granted); err != nil {
				t.Fatal(database.SafeError("verify restore", err))
			}
			if !granted {
				t.Fatalf("restore did not return %s on %s", privilege, e.schema+".usage_minute_stats")
			}
		}
		// The identical input now succeeds, which proves the earlier error came from
		// the revoked privilege and not from the input itself.
		commitEvent(t, ctx, app, in, "synthetic-permission-restored")
		if row := readBucket(t, ctx, owner, in); row.eventCount != 1 {
			t.Fatalf("event_count=%d want=1 after the privilege was restored", row.eventCount)
		}
		t.Log("revoked INSERT and UPDATE denied the writer, rollback left nothing and the restored grant accepted the same input")
	})

	t.Run("cancellation", func(t *testing.T) {
		in := minuteInput(t)
		tx := beginWithProbe(t, ctx, app, in, "synthetic-cancel")
		canceled, stop := context.WithCancel(ctx)
		stop()
		err := ApplyRequest(canceled, tx, in)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error=%v want context.Canceled", err)
		}
		requireSanitized(t, err)
		// A canceled statement aborts the transaction; only unexpected errors fail.
		if rollbackErr := tx.Rollback(context.Background()); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) && !errors.Is(rollbackErr, pgconn.ErrConnClosed) {
			t.Fatal(database.SafeError("rollback after cancel", rollbackErr))
		}
		requireNothingWritten(t, ctx, owner, in.WorkspaceID, "canceled write")
	})

	t.Run("bigint overflow rolls back", func(t *testing.T) {
		in := minuteInput(t)
		in.Input = knownMetric(math.MaxInt64)
		commitEvent(t, ctx, app, in, "synthetic-overflow-seed")
		row := readBucket(t, ctx, owner, in)
		if got := row.sum[metricIndex("input_tokens")]; got != math.MaxInt64 {
			t.Fatalf("seed input sum=%d want max int64", got)
		}
		over := in
		over.Input = knownMetric(1)
		tx := beginWithProbe(t, ctx, app, over, "synthetic-overflow")
		err := ApplyRequest(ctx, tx, over)
		if err == nil {
			t.Fatal("overflow succeeded instead of failing")
		}
		requireSanitized(t, err)
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(database.SafeError("rollback", err))
		}
		var detail int
		if err := owner.QueryRow(ctx, "SELECT count(*)::int FROM aggregation_detail_probe WHERE workspace_id=$1 AND source_event_id=$2", over.WorkspaceID, "synthetic-overflow").Scan(&detail); err != nil {
			t.Fatal(database.SafeError("overflow detail", err))
		}
		if detail != 0 {
			t.Fatal("rolled back overflow left a detail row")
		}
		after := readBucket(t, ctx, owner, in)
		if after.eventCount != 1 || after.sum[metricIndex("input_tokens")] != math.MaxInt64 {
			t.Fatalf("bucket changed after overflow: event_count=%d sum=%d", after.eventCount, after.sum[metricIndex("input_tokens")])
		}
		t.Log("overflow error was sanitized and the existing bucket kept its value")
	})
}
