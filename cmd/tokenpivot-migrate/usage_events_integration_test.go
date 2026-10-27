//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dundunHa/TokenPivot/db/migrations"
	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// usageMetric is one metric column pair: a nullable value and its NOT NULL quality.
// The pointer stays nil for a missing value so NULL and zero remain distinguishable.
type usageMetric struct {
	value   *int64
	quality string
}

// usageEventRow carries the columns the tests set explicitly. The id is always left
// to the uuidv7() default so the tests also exercise that default.
type usageEventRow struct {
	workspaceID          string
	sourceID             string
	sourceEventID        string
	projectID            *string
	provider             string
	model                string
	occurredAt           time.Time
	receivedAt           time.Time
	normalizationVersion string
	payloadHash          []byte
	input                usageMetric
	output               usageMetric
	total                usageMetric
	cacheRead            usageMetric
	cacheWrite           usageMetric
	reasoning            usageMetric
	status               *string
	durationMS           *int64
}

const insertUsageEventSQL = `INSERT INTO usage_events (
    workspace_id, source_id, source_event_id, project_id, provider, model, occurred_at, received_at,
    normalization_version, payload_hash,
    input_tokens, input_tokens_quality, output_tokens, output_tokens_quality,
    total_tokens, total_tokens_quality, cache_read_tokens, cache_read_tokens_quality,
    cache_write_tokens, cache_write_tokens_quality, reasoning_tokens, reasoning_tokens_quality,
    status, duration_ms
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)
ON CONFLICT (workspace_id, source_id, source_event_id) DO NOTHING
RETURNING id`

const usageEventSnapshotSQL = `SELECT concat_ws('|',
    id::text, workspace_id::text, source_id::text, source_event_id,
    coalesce(project_id::text, '<null>'), provider, model,
    occurred_at::text, received_at::text, normalization_version, encode(payload_hash, 'hex'),
    coalesce(input_tokens::text, '<null>'), input_tokens_quality,
    coalesce(output_tokens::text, '<null>'), output_tokens_quality,
    coalesce(total_tokens::text, '<null>'), total_tokens_quality,
    coalesce(cache_read_tokens::text, '<null>'), cache_read_tokens_quality,
    coalesce(cache_write_tokens::text, '<null>'), cache_write_tokens_quality,
    coalesce(reasoning_tokens::text, '<null>'), reasoning_tokens_quality,
    coalesce(status, '<null>'), coalesce(duration_ms::text, '<null>'))
FROM usage_events
WHERE workspace_id=$1 AND source_id=$2 AND source_event_id=$3`

func pointerTo[T any](value T) *T {
	return &value
}

func randomUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal("random uuid failed")
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func syntheticUsageEvent(t *testing.T) usageEventRow {
	t.Helper()
	occurredAt := time.Date(2026, 10, 27, 10, 20, 30, 123456000, time.UTC)
	return usageEventRow{
		workspaceID:          randomUUID(t),
		sourceID:             randomUUID(t),
		sourceEventID:        "synthetic-event-" + randomUUID(t),
		provider:             "synthetic-provider",
		model:                "synthetic-model",
		occurredAt:           occurredAt,
		receivedAt:           occurredAt.Add(time.Minute),
		normalizationVersion: "tokenpivot-usage-event/v1",
		payloadHash:          bytes.Repeat([]byte{0x11}, 32),
		input:                usageMetric{value: pointerTo(int64(100)), quality: "known"},
		output:               usageMetric{value: pointerTo(int64(20)), quality: "known"},
		total:                usageMetric{value: pointerTo(int64(120)), quality: "known"},
		cacheRead:            usageMetric{quality: "missing"},
		cacheWrite:           usageMetric{quality: "missing"},
		reasoning:            usageMetric{quality: "missing"},
		status:               pointerTo("success"),
		durationMS:           pointerTo(int64(1250)),
	}
}

func usageEventArgs(row usageEventRow) []any {
	return []any{
		row.workspaceID, row.sourceID, row.sourceEventID, row.projectID, row.provider, row.model,
		row.occurredAt, row.receivedAt, row.normalizationVersion, row.payloadHash,
		row.input.value, row.input.quality, row.output.value, row.output.quality,
		row.total.value, row.total.quality, row.cacheRead.value, row.cacheRead.quality,
		row.cacheWrite.value, row.cacheWrite.quality, row.reasoning.value, row.reasoning.quality,
		row.status, row.durationMS,
	}
}

// insertUsageEvent returns the new id and whether the row was inserted. A false
// result means the identity key already existed and the conflict action was DO NOTHING.
func insertUsageEvent(t *testing.T, ctx context.Context, conn *pgx.Conn, row usageEventRow) (string, bool) {
	t.Helper()
	var id string
	err := conn.QueryRow(ctx, insertUsageEventSQL, usageEventArgs(row)...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatal(database.SafeError("insert usage event", err))
	}
	return id, true
}

// usageEventSnapshot renders every column of the identity's row as one text value so
// a before/after comparison covers all columns, including the ones the caller did not set.
func usageEventSnapshot(t *testing.T, ctx context.Context, conn *pgx.Conn, row usageEventRow) string {
	t.Helper()
	var snapshot string
	if err := conn.QueryRow(ctx, usageEventSnapshotSQL, row.workspaceID, row.sourceID, row.sourceEventID).Scan(&snapshot); err != nil {
		t.Fatal(database.SafeError("snapshot usage event", err))
	}
	return snapshot
}

func usageEventCount(t *testing.T, ctx context.Context, conn *pgx.Conn, row usageEventRow) int {
	t.Helper()
	var count int
	err := conn.QueryRow(ctx, "SELECT count(*)::int FROM usage_events WHERE workspace_id=$1 AND source_id=$2 AND source_event_id=$3", row.workspaceID, row.sourceID, row.sourceEventID).Scan(&count)
	if err != nil {
		t.Fatal(database.SafeError("count usage events", err))
	}
	return count
}

func TestUsageEvents(t *testing.T) {
	migrationURL, appURL := integrationSchema(t)
	p, db := testProvider(t, migrationURL, migrations.Files)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := p.Up(ctx); err != nil {
		t.Fatal(database.SafeError("usage events up", err))
	}
	app, err := pgx.Connect(ctx, appURL)
	if err != nil {
		t.Fatal(database.SafeError("app connect", err))
	}
	defer func() {
		if err := app.Close(context.Background()); err != nil {
			t.Error("close app failed")
		}
	}()

	// 1. Structure: ordinary table, exact column shape, identity key column order,
	// C collation on source_event_id, exactly the four planned indexes, and none of
	// the deliberately omitted objects (foreign keys, CHECK, triggers, RLS).
	t.Run("structure", func(t *testing.T) {
		type column struct {
			dataType    string
			nullable    string
			defaultText string
		}
		expected := map[string]column{
			"id":                    {dataType: "uuid", nullable: "NO", defaultText: "uuidv7()"},
			"workspace_id":          {dataType: "uuid", nullable: "NO"},
			"source_id":             {dataType: "uuid", nullable: "NO"},
			"source_event_id":       {dataType: "text", nullable: "NO"},
			"project_id":            {dataType: "uuid", nullable: "YES"},
			"provider":              {dataType: "text", nullable: "NO"},
			"model":                 {dataType: "text", nullable: "NO"},
			"occurred_at":           {dataType: "timestamp with time zone", nullable: "NO"},
			"received_at":           {dataType: "timestamp with time zone", nullable: "NO"},
			"normalization_version": {dataType: "text", nullable: "NO"},
			"payload_hash":          {dataType: "bytea", nullable: "NO"},
			"status":                {dataType: "text", nullable: "YES"},
			"duration_ms":           {dataType: "bigint", nullable: "YES"},
		}
		for _, metric := range []string{"input_tokens", "output_tokens", "total_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"} {
			expected[metric] = column{dataType: "bigint", nullable: "YES"}
			expected[metric+"_quality"] = column{dataType: "text", nullable: "NO"}
		}
		rows, err := db.QueryContext(ctx, "SELECT column_name::text, data_type::text, is_nullable::text, coalesce(column_default::text,'') FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='usage_events'")
		if err != nil {
			t.Fatal(database.SafeError("columns", err))
		}
		defer func() {
			if err := rows.Close(); err != nil {
				t.Error("close column rows failed")
			}
		}()
		actual := make(map[string]column)
		for rows.Next() {
			var name string
			var spec column
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
			if want.defaultText == "" {
				if got.defaultText != "" {
					t.Fatalf("column %s has unexpected default %q", name, got.defaultText)
				}
				continue
			}
			if !strings.Contains(got.defaultText, want.defaultText) {
				t.Fatalf("column %s default=%q want contains %q", name, got.defaultText, want.defaultText)
			}
		}
		t.Logf("columns=%d all match", len(actual))

		// relkind 'r' is an ordinary table and 'p' a partitioned table, so this also
		// proves the first version does not ship any partitioning.
		var relkind string
		var isPartition bool
		err = db.QueryRowContext(ctx, "SELECT c.relkind::text, c.relispartition FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname='usage_events'").Scan(&relkind, &isPartition)
		if err != nil {
			t.Fatal(database.SafeError("relkind", err))
		}
		if relkind != "r" || isPartition {
			t.Fatalf("relkind=%q ispartition=%t want ordinary non-partitioned table", relkind, isPartition)
		}

		var collation string
		err = db.QueryRowContext(ctx, "SELECT coalesce(co.collname::text,'') FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_collation co ON co.oid=a.attcollation WHERE n.nspname=current_schema() AND c.relname='usage_events' AND a.attname='source_event_id' AND NOT a.attisdropped").Scan(&collation)
		if err != nil {
			t.Fatal(database.SafeError("collation", err))
		}
		if collation != "C" {
			t.Fatalf("source_event_id collation=%q want=C", collation)
		}

		type index struct {
			unique  bool
			columns string
		}
		expectedIndexes := map[string]index{
			"usage_events_pkey": {unique: true, columns: "id"},
			"uk_usage_events_workspace_id_source_id_source_event_id": {unique: true, columns: "workspace_id,source_id,source_event_id"},
			"idx_usage_events_workspace_id_occurred_at_id":           {unique: false, columns: "workspace_id,occurred_at,id"},
			"idx_usage_events_received_at":                           {unique: false, columns: "received_at"},
		}
		indexRows, err := db.QueryContext(ctx, "SELECT i.relname::text, ix.indisunique, (SELECT string_agg(a.attname, ',' ORDER BY u.k) FROM unnest(string_to_array(ix.indkey::text, ' ')::int[]) WITH ORDINALITY AS u(attnum, k) JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum=u.attnum)::text FROM pg_index ix JOIN pg_class i ON i.oid=ix.indexrelid JOIN pg_class c ON c.oid=ix.indrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname='usage_events'")
		if err != nil {
			t.Fatal(database.SafeError("indexes", err))
		}
		defer func() {
			if err := indexRows.Close(); err != nil {
				t.Error("close index rows failed")
			}
		}()
		actualIndexes := make(map[string]index)
		for indexRows.Next() {
			var name string
			var spec index
			if err := indexRows.Scan(&name, &spec.unique, &spec.columns); err != nil {
				t.Fatal(database.SafeError("scan index", err))
			}
			actualIndexes[name] = spec
		}
		if err := indexRows.Err(); err != nil {
			t.Fatal(database.SafeError("index rows", err))
		}
		if len(actualIndexes) != len(expectedIndexes) {
			t.Fatalf("index count=%d want=%d", len(actualIndexes), len(expectedIndexes))
		}
		for name, want := range expectedIndexes {
			got, ok := actualIndexes[name]
			if !ok {
				t.Fatalf("missing index %s", name)
			}
			if got.unique != want.unique || got.columns != want.columns {
				t.Fatalf("index %s unique=%t columns=%q want unique=%t columns=%q", name, got.unique, got.columns, want.unique, want.columns)
			}
		}
		t.Logf("indexes=%d all match", len(actualIndexes))

		checks := []struct {
			name  string
			query string
		}{
			{"foreign key and check constraints", "SELECT count(*)::int FROM pg_constraint con JOIN pg_class c ON c.oid=con.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname='usage_events' AND con.contype IN ('f','c')"},
			{"triggers", "SELECT count(*)::int FROM pg_trigger tg JOIN pg_class c ON c.oid=tg.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname='usage_events' AND NOT tg.tgisinternal"},
			{"row level security", "SELECT count(*)::int FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname='usage_events' AND (c.relrowsecurity OR c.relforcerowsecurity)"},
		}
		for _, check := range checks {
			var count int
			if err := db.QueryRowContext(ctx, check.query).Scan(&count); err != nil {
				t.Fatal(database.SafeError(check.name, err))
			}
			if count != 0 {
				t.Fatalf("%s count=%d want=0", check.name, count)
			}
		}
	})

	// 2. Permissions: the service role keeps SELECT and INSERT only. Every other
	// table privilege is denied, including the row lock forms and ON CONFLICT DO UPDATE.
	t.Run("permissions", func(t *testing.T) {
		var privileges string
		err := db.QueryRowContext(ctx, "SELECT string_agg(privilege_type::text, ',' ORDER BY privilege_type)::text FROM information_schema.role_table_grants WHERE grantee='tokenpivot_app' AND table_schema=current_schema() AND table_name='usage_events'").Scan(&privileges)
		if err != nil {
			t.Fatal(database.SafeError("grants", err))
		}
		if privileges != "INSERT,SELECT" {
			t.Fatalf("grants=%q want=INSERT,SELECT", privileges)
		}
		for _, privilege := range []string{"UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
			var granted bool
			err := db.QueryRowContext(ctx, "SELECT has_table_privilege('tokenpivot_app', current_schema()||'.usage_events', $1)", privilege).Scan(&granted)
			if err != nil {
				t.Fatal(database.SafeError("privilege", err))
			}
			if granted {
				t.Fatalf("service role unexpectedly holds %s", privilege)
			}
		}
		row := syntheticUsageEvent(t)
		if _, inserted := insertUsageEvent(t, ctx, app, row); !inserted {
			t.Fatal("seed insert was not inserted")
		}
		for _, statement := range []string{
			"UPDATE usage_events SET provider='synthetic-forbidden'",
			"DELETE FROM usage_events",
			"TRUNCATE usage_events",
			"INSERT INTO usage_events (workspace_id, source_id, source_event_id, provider, model, occurred_at, received_at, normalization_version, payload_hash, input_tokens_quality, output_tokens_quality, total_tokens_quality, cache_read_tokens_quality, cache_write_tokens_quality, reasoning_tokens_quality) VALUES ('" + row.workspaceID + "','" + row.sourceID + "','" + row.sourceEventID + "','synthetic-provider','synthetic-model',now(),now(),'tokenpivot-usage-event/v1','\\x00','missing','missing','missing','missing','missing','missing') ON CONFLICT (workspace_id, source_id, source_event_id) DO UPDATE SET provider='synthetic-forbidden'",
			"SELECT id FROM usage_events FOR SHARE",
		} {
			_, err := app.Exec(ctx, statement)
			requireSQLState(t, err, "42501")
		}
		t.Log("service role denied UPDATE, DELETE, TRUNCATE, ON CONFLICT DO UPDATE and FOR SHARE")
	})

	// 3. Triple uniqueness: the identity key arbitrates duplicates and the generated
	// id default produces a UUIDv7 for the service role.
	t.Run("identity uniqueness", func(t *testing.T) {
		first := syntheticUsageEvent(t)
		id, inserted := insertUsageEvent(t, ctx, app, first)
		if !inserted {
			t.Fatal("first insert was not inserted")
		}
		var version int
		if err := app.QueryRow(ctx, "SELECT uuid_extract_version(id) FROM usage_events WHERE id=$1", id).Scan(&version); err != nil {
			t.Fatal(database.SafeError("uuid version", err))
		}
		if version != 7 {
			t.Fatalf("uuid version=%d want=7", version)
		}
		second := first
		second.payloadHash = bytes.Repeat([]byte{0x22}, 32)
		second.receivedAt = first.receivedAt.Add(time.Hour)
		if _, inserted := insertUsageEvent(t, ctx, app, second); inserted {
			t.Fatal("duplicate identity was inserted")
		}
		if count := usageEventCount(t, ctx, app, first); count != 1 {
			t.Fatalf("rows=%d want=1", count)
		}
	})

	// 4. Workspace and source isolation: the same source_event_id is a different
	// identity in another workspace or under another source, and case matters bytewise.
	t.Run("identity scope", func(t *testing.T) {
		base := syntheticUsageEvent(t)
		if _, inserted := insertUsageEvent(t, ctx, app, base); !inserted {
			t.Fatal("base insert was not inserted")
		}
		otherWorkspace := base
		otherWorkspace.workspaceID = randomUUID(t)
		if _, inserted := insertUsageEvent(t, ctx, app, otherWorkspace); !inserted {
			t.Fatal("same event id in another workspace was rejected")
		}
		otherSource := base
		otherSource.sourceID = randomUUID(t)
		if _, inserted := insertUsageEvent(t, ctx, app, otherSource); !inserted {
			t.Fatal("same event id under another source was rejected")
		}
		upper := base
		upper.sourceEventID = strings.ToUpper(base.sourceEventID)
		if _, inserted := insertUsageEvent(t, ctx, app, upper); !inserted {
			t.Fatal("differently cased event id was treated as the same identity")
		}
		lower := base
		lower.sourceEventID = "synthetic-case"
		if _, inserted := insertUsageEvent(t, ctx, app, lower); !inserted {
			t.Fatal("lowercase variant insert was not inserted")
		}
		upperCase := lower
		upperCase.sourceEventID = "SYNTHETIC-CASE"
		if _, inserted := insertUsageEvent(t, ctx, app, upperCase); !inserted {
			t.Fatal("uppercase variant was treated as the same identity")
		}
		if count := usageEventCount(t, ctx, app, base); count != 1 {
			t.Fatalf("base rows=%d want=1", count)
		}
	})

	// 5. No overwrite: a conflicting retransmission with later received_at changes no
	// column of the original row, because the service role can only skip the conflict.
	t.Run("no overwrite", func(t *testing.T) {
		original := syntheticUsageEvent(t)
		if _, inserted := insertUsageEvent(t, ctx, app, original); !inserted {
			t.Fatal("original insert was not inserted")
		}
		before := usageEventSnapshot(t, ctx, app, original)
		changed := original
		changed.payloadHash = bytes.Repeat([]byte{0x33}, 32)
		changed.normalizationVersion = "synthetic-version/v2"
		changed.provider = "synthetic-other-provider"
		changed.input = usageMetric{value: pointerTo(int64(999)), quality: "estimated"}
		changed.status = pointerTo("error")
		changed.durationMS = pointerTo(int64(1))
		changed.receivedAt = original.receivedAt.Add(24 * time.Hour)
		if _, inserted := insertUsageEvent(t, ctx, app, changed); inserted {
			t.Fatal("conflicting retransmission was inserted")
		}
		after := usageEventSnapshot(t, ctx, app, original)
		if before != after {
			t.Fatalf("row changed after conflicting retransmission\nbefore=%s\nafter=%s", before, after)
		}
		if count := usageEventCount(t, ctx, app, original); count != 1 {
			t.Fatalf("rows=%d want=1", count)
		}
	})

	// 6. Concurrent commit: the blocked do-nothing insert returns zero rows, yet the
	// following SELECT in the same read committed transaction B sees the row A committed
	// after B started waiting on the identity key.
	t.Run("concurrent commit", func(t *testing.T) {
		row := syntheticUsageEvent(t)
		connA := connectTestConn(t, ctx, appURL, "A")
		connB := connectTestConn(t, ctx, appURL, "B")
		txA, err := connA.Begin(ctx)
		if err != nil {
			t.Fatal(database.SafeError("begin A", err))
		}
		defer func() {
			if err := txA.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Error("rollback A failed")
			}
		}()
		var idA string
		if err := txA.QueryRow(ctx, insertUsageEventSQL, usageEventArgs(row)...).Scan(&idA); err != nil {
			t.Fatal(database.SafeError("insert A", err))
		}
		pidB := backendPID(t, ctx, connB, "pid B")
		txB, err := connB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			t.Fatal(database.SafeError("begin B", err))
		}
		// Registered before the blocked insert starts, so the cleanup that cancels and
		// joins that goroutine runs first, and connB is only closed after txB is done.
		// Cancelling a blocked Exec closes its connection, so a closed connection here
		// means the server already discarded the transaction; only unexpected errors fail.
		t.Cleanup(func() {
			err := txB.Rollback(context.Background())
			if err != nil && !errors.Is(err, pgx.ErrTxClosed) && !errors.Is(err, pgconn.ErrConnClosed) {
				t.Error("rollback B failed")
			}
		})
		done := startBlockedInsert(t, ctx, txB, row)
		waitForBlockedLock(t, ctx, db, pidB)
		if err := txA.Commit(ctx); err != nil {
			t.Fatal(database.SafeError("commit A", err))
		}
		if awaitBlockedInsert(t, done) {
			t.Fatal("blocked insert reported an inserted row after the first commit")
		}
		var idB string
		if err := txB.QueryRow(ctx, "SELECT id FROM usage_events WHERE workspace_id=$1 AND source_id=$2 AND source_event_id=$3", row.workspaceID, row.sourceID, row.sourceEventID).Scan(&idB); err != nil {
			t.Fatal(database.SafeError("read after conflict", err))
		}
		if idB != idA {
			t.Fatalf("follow-up read id=%s want=%s", idB, idA)
		}
		if err := txB.Commit(ctx); err != nil {
			t.Fatal(database.SafeError("commit B", err))
		}
		if count := usageEventCount(t, ctx, app, row); count != 1 {
			t.Fatalf("rows=%d want=1", count)
		}
	})

	// 7. Concurrent rollback: once the first transaction rolls back, the blocked insert
	// succeeds and its own content is the stored row.
	t.Run("concurrent rollback", func(t *testing.T) {
		row := syntheticUsageEvent(t)
		connA := connectTestConn(t, ctx, appURL, "A")
		connB := connectTestConn(t, ctx, appURL, "B")
		txA, err := connA.Begin(ctx)
		if err != nil {
			t.Fatal(database.SafeError("begin A", err))
		}
		defer func() {
			if err := txA.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Error("rollback A failed")
			}
		}()
		if _, err := txA.Exec(ctx, insertUsageEventSQL, usageEventArgs(row)...); err != nil {
			t.Fatal(database.SafeError("insert A", err))
		}
		pidB := backendPID(t, ctx, connB, "pid B")
		rowB := row
		rowB.payloadHash = bytes.Repeat([]byte{0x44}, 32)
		rowB.provider = "synthetic-rollback-provider"
		done := startBlockedInsert(t, ctx, connB, rowB)
		waitForBlockedLock(t, ctx, db, pidB)
		if err := txA.Rollback(ctx); err != nil {
			t.Fatal(database.SafeError("rollback A", err))
		}
		if !awaitBlockedInsert(t, done) {
			t.Fatal("insert after rollback was not inserted")
		}
		var provider string
		if err := app.QueryRow(ctx, "SELECT provider FROM usage_events WHERE workspace_id=$1 AND source_id=$2 AND source_event_id=$3", row.workspaceID, row.sourceID, row.sourceEventID).Scan(&provider); err != nil {
			t.Fatal(database.SafeError("read stored row", err))
		}
		if provider != rowB.provider {
			t.Fatalf("stored provider=%q want=%q", provider, rowB.provider)
		}
		if count := usageEventCount(t, ctx, app, row); count != 1 {
			t.Fatalf("rows=%d want=1", count)
		}
	})

	// 8. Many contenders: eight released connections insert the same identity with
	// different content; exactly one insert wins and its content is the stored row.
	t.Run("concurrent contenders", func(t *testing.T) {
		base := syntheticUsageEvent(t)
		const contenders = 8
		rows := make([]usageEventRow, contenders)
		conns := make([]*pgx.Conn, contenders)
		for i := range rows {
			rows[i] = base
			rows[i].payloadHash = bytes.Repeat([]byte{byte(0x50 + i)}, 32)
			rows[i].provider = fmt.Sprintf("synthetic-contender-%d", i)
			conns[i] = connectTestConn(t, ctx, appURL, "contender")
		}
		type outcome struct {
			index    int
			inserted bool
			err      error
		}
		results := make(chan outcome, contenders)
		release := make(chan struct{})
		contendCtx, stopContend := context.WithTimeout(ctx, 30*time.Second)
		// Registered after the connections, so on every path this cancel-and-join cleanup
		// runs before the contenders' connections are closed.
		var wg sync.WaitGroup
		t.Cleanup(func() {
			stopContend()
			wg.Wait()
		})
		wg.Add(contenders)
		for i, conn := range conns {
			go func(i int, conn *pgx.Conn) {
				defer wg.Done()
				<-release
				tag, err := conn.Exec(contendCtx, insertUsageEventSQL, usageEventArgs(rows[i])...)
				if err != nil {
					results <- outcome{index: i, err: err}
					return
				}
				results <- outcome{index: i, inserted: tag.RowsAffected() == 1}
			}(i, conn)
		}
		close(release)
		insertedTotal := 0
		winner := -1
		for range rows {
			result := <-results
			if result.err != nil {
				t.Fatal(database.SafeError("contender insert", result.err))
			}
			if result.inserted {
				insertedTotal++
				winner = result.index
			}
		}
		if insertedTotal != 1 {
			t.Fatalf("inserted rows=%d want=1", insertedTotal)
		}
		var provider string
		if err := app.QueryRow(ctx, "SELECT provider FROM usage_events WHERE workspace_id=$1 AND source_id=$2 AND source_event_id=$3", base.workspaceID, base.sourceID, base.sourceEventID).Scan(&provider); err != nil {
			t.Fatal(database.SafeError("read winner", err))
		}
		if provider != rows[winner].provider {
			t.Fatalf("stored provider=%q want winner %q", provider, rows[winner].provider)
		}
		if count := usageEventCount(t, ctx, app, base); count != 1 {
			t.Fatalf("rows=%d want=1", count)
		}
		t.Logf("contenders=%d winner=%d", contenders, winner)
	})

	// 9. Missing versus zero: a missing metric reads back as NULL with the missing
	// quality, a real zero reads back as 0, and omitted metric columns are not defaulted.
	t.Run("missing and zero", func(t *testing.T) {
		row := syntheticUsageEvent(t)
		row.input = usageMetric{quality: "missing"}
		row.output = usageMetric{value: pointerTo(int64(0)), quality: "known"}
		row.total = usageMetric{quality: "missing"}
		row.cacheRead = usageMetric{quality: "missing"}
		row.cacheWrite = usageMetric{quality: "missing"}
		row.reasoning = usageMetric{quality: "missing"}
		row.projectID = nil
		row.status = nil
		row.durationMS = nil
		if _, inserted := insertUsageEvent(t, ctx, app, row); !inserted {
			t.Fatal("insert was not inserted")
		}
		var input, output *int64
		var inputQuality, outputQuality, totalQuality string
		err := app.QueryRow(ctx, "SELECT input_tokens, output_tokens, input_tokens_quality, output_tokens_quality, total_tokens_quality FROM usage_events WHERE workspace_id=$1 AND source_id=$2 AND source_event_id=$3", row.workspaceID, row.sourceID, row.sourceEventID).Scan(&input, &output, &inputQuality, &outputQuality, &totalQuality)
		if err != nil {
			t.Fatal(database.SafeError("read metrics", err))
		}
		if input != nil {
			t.Fatalf("missing input_tokens read back as %d", *input)
		}
		if output == nil || *output != 0 {
			t.Fatalf("real zero output_tokens lost: %v", output)
		}
		if inputQuality != "missing" || outputQuality != "known" || totalQuality != "missing" {
			t.Fatalf("qualities input=%q output=%q total=%q", inputQuality, outputQuality, totalQuality)
		}
		omitted := syntheticUsageEvent(t)
		_, err = app.Exec(ctx, "INSERT INTO usage_events (workspace_id, source_id, source_event_id, provider, model, occurred_at, received_at, normalization_version, payload_hash, input_tokens_quality, output_tokens_quality, total_tokens_quality, cache_read_tokens_quality, cache_write_tokens_quality, reasoning_tokens_quality) VALUES ($1,$2,$3,'synthetic-provider','synthetic-model',now(),now(),'tokenpivot-usage-event/v1','\\x01','missing','missing','missing','missing','missing','missing')", omitted.workspaceID, omitted.sourceID, omitted.sourceEventID)
		if err != nil {
			t.Fatal(database.SafeError("insert omitted metrics", err))
		}
		var nonNull int
		err = app.QueryRow(ctx, "SELECT count(*)::int FROM usage_events WHERE workspace_id=$1 AND source_id=$2 AND source_event_id=$3 AND (input_tokens IS NOT NULL OR output_tokens IS NOT NULL OR total_tokens IS NOT NULL OR cache_read_tokens IS NOT NULL OR cache_write_tokens IS NOT NULL OR reasoning_tokens IS NOT NULL)", omitted.workspaceID, omitted.sourceID, omitted.sourceEventID).Scan(&nonNull)
		if err != nil {
			t.Fatal(database.SafeError("check omitted metrics", err))
		}
		if nonNull != 0 {
			t.Fatalf("omitted metric columns were defaulted for %d rows", nonNull)
		}
	})

	// 10. Time: microsecond values round-trip unchanged and received_at has no default,
	// so omitting it fails instead of letting the database pick a clock.
	t.Run("time precision", func(t *testing.T) {
		row := syntheticUsageEvent(t)
		row.occurredAt = time.Date(2026, 10, 26, 23, 30, 0, 654321000, time.UTC)
		row.receivedAt = time.Date(2026, 10, 27, 0, 0, 0, 100001000, time.UTC)
		if _, inserted := insertUsageEvent(t, ctx, app, row); !inserted {
			t.Fatal("insert was not inserted")
		}
		var occurredAt, receivedAt time.Time
		err := app.QueryRow(ctx, "SELECT occurred_at, received_at FROM usage_events WHERE workspace_id=$1 AND source_id=$2 AND source_event_id=$3", row.workspaceID, row.sourceID, row.sourceEventID).Scan(&occurredAt, &receivedAt)
		if err != nil {
			t.Fatal(database.SafeError("read times", err))
		}
		if !occurredAt.Equal(row.occurredAt) || occurredAt.Nanosecond()%1000 != 0 {
			t.Fatalf("occurred_at=%s want=%s at microsecond precision", occurredAt.UTC(), row.occurredAt)
		}
		if !receivedAt.Equal(row.receivedAt) || receivedAt.Nanosecond()%1000 != 0 {
			t.Fatalf("received_at=%s want=%s at microsecond precision", receivedAt.UTC(), row.receivedAt)
		}
		if occurredAt.Nanosecond()/1000 != 654321 || receivedAt.Nanosecond()/1000 != 100001 {
			t.Fatalf("microseconds occurred=%d received=%d", occurredAt.Nanosecond()/1000, receivedAt.Nanosecond()/1000)
		}
		omitted := syntheticUsageEvent(t)
		_, err = app.Exec(ctx, "INSERT INTO usage_events (workspace_id, source_id, source_event_id, provider, model, occurred_at, normalization_version, payload_hash, input_tokens_quality, output_tokens_quality, total_tokens_quality, cache_read_tokens_quality, cache_write_tokens_quality, reasoning_tokens_quality) VALUES ($1,$2,$3,'synthetic-provider','synthetic-model',now(),'tokenpivot-usage-event/v1','\\x02','missing','missing','missing','missing','missing','missing')", omitted.workspaceID, omitted.sourceID, omitted.sourceEventID)
		requireSQLState(t, err, "23502")
	})

	// 10a. Identity length boundary: the maximum frozen encoded length (256 UTF-8 bytes)
	// must fit the real unique index and still deduplicate, including multi-byte and
	// incompressible values.
	t.Run("identity length boundary", func(t *testing.T) {
		samples := map[string]string{
			"incompressible ascii": string(randomIdentityBytes(t, 256)),
			"two byte runes":       strings.Repeat("é", 128),
			"three byte runes":     strings.Repeat("用", 85) + "a",
		}
		for name, sample := range samples {
			if got := len(sample); got != 256 {
				t.Fatalf("%s sample length=%d want=256 bytes", name, got)
			}
			row := syntheticUsageEvent(t)
			row.sourceEventID = sample
			if _, inserted := insertUsageEvent(t, ctx, app, row); !inserted {
				t.Fatalf("%s identity at 256 bytes was rejected", name)
			}
			var storedBytes int
			if err := app.QueryRow(ctx, "SELECT octet_length(source_event_id) FROM usage_events WHERE workspace_id=$1 AND source_id=$2 AND source_event_id=$3", row.workspaceID, row.sourceID, row.sourceEventID).Scan(&storedBytes); err != nil {
				t.Fatal(database.SafeError("read identity length", err))
			}
			if storedBytes != 256 {
				t.Fatalf("%s stored bytes=%d want=256", name, storedBytes)
			}
			duplicate := row
			duplicate.payloadHash = bytes.Repeat([]byte{0x66}, 32)
			if _, inserted := insertUsageEvent(t, ctx, app, duplicate); inserted {
				t.Fatalf("%s duplicate at 256 bytes was inserted", name)
			}
			if count := usageEventCount(t, ctx, app, row); count != 1 {
				t.Fatalf("%s rows=%d want=1", name, count)
			}
		}
		t.Logf("samples=%d at 256 byte identity", len(samples))
	})
}

// connectTestConn opens a dedicated connection and closes it when the subtest ends;
// name labels the connect and close failures.
func connectTestConn(t *testing.T, ctx context.Context, url, name string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(database.SafeError("connect "+name, err))
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Error("close " + name + " failed")
		}
	})
	return conn
}

func backendPID(t *testing.T, ctx context.Context, conn *pgx.Conn, op string) int {
	t.Helper()
	var pid int
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(database.SafeError(op, err))
	}
	return pid
}

type blockedInsertOutcome struct {
	inserted bool
	err      error
}

// usageEventInserter is the Exec subset shared by *pgx.Conn and pgx.Tx, so the blocked
// insert can run either in an explicit transaction or in autocommit.
type usageEventInserter interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// startBlockedInsert runs the do-nothing insert in a goroutine so it can wait on the
// identity lock while the test finishes the other transaction. The channel is buffered,
// so the goroutine can always deliver its outcome and exit. Cleanups run last in first
// out, so the registration below cancels the insert and joins its goroutine before any
// connection opened earlier by the caller is closed.
func startBlockedInsert(t *testing.T, parent context.Context, inserter usageEventInserter, row usageEventRow) <-chan blockedInsertOutcome {
	t.Helper()
	ctx, stopBlock := context.WithTimeout(parent, 20*time.Second)
	var wg sync.WaitGroup
	t.Cleanup(func() {
		stopBlock()
		wg.Wait()
	})
	done := make(chan blockedInsertOutcome, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		tag, err := inserter.Exec(ctx, insertUsageEventSQL, usageEventArgs(row)...)
		if err != nil {
			done <- blockedInsertOutcome{err: err}
			return
		}
		done <- blockedInsertOutcome{inserted: tag.RowsAffected() == 1}
	}()
	return done
}

// awaitBlockedInsert waits for startBlockedInsert and reports whether it inserted a row.
func awaitBlockedInsert(t *testing.T, done <-chan blockedInsertOutcome) bool {
	t.Helper()
	result := <-done
	if result.err != nil {
		t.Fatal(database.SafeError("blocked insert", result.err))
	}
	return result.inserted
}

// waitForBlockedLock polls the wait-side backend pid until pg_locks reports a lock it
// requested but has not been granted, so the test does not guess scheduling with sleeps.
func waitForBlockedLock(t *testing.T, ctx context.Context, db *sql.DB, pid int) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := db.QueryRowContext(deadline, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND NOT granted)", pid).Scan(&blocked); err != nil {
			t.Fatal(database.SafeError("observe blocked lock", err))
		}
		if blocked {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.Done():
			t.Fatal("contending insert did not block on the identity key")
		}
	}
}

// randomIdentityBytes returns n random non-control ASCII bytes: incompressible content
// that also satisfies the frozen source_event_id character rules.
func randomIdentityBytes(t *testing.T, n int) []byte {
	t.Helper()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal("random identity failed")
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return b
}
