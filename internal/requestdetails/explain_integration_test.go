//go:build integration

package requestdetails

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// TestExplainRepresentativeVolume records EXPLAIN (ANALYZE, BUFFERS) plans on
// synthetic data in the isolated test schema. It asserts nothing about timing:
// the plans are evidence for review, not an SLA. Set TOKENPIVOT_EXPLAIN=1 and
// run with -v to see them.
func TestExplainRepresentativeVolume(t *testing.T) {
	if os.Getenv("TOKENPIVOT_EXPLAIN") != "1" {
		t.Skip("set TOKENPIVOT_EXPLAIN=1 to record query plans")
	}
	f := newFixture(t)
	end := asOf
	// 80k spread over 180 days, 9k in the last hour, 1k sharing one occurred_at,
	// 10k from another workspace in the last hour.
	seed := func(workspace pgtype.UUID, n int, from, to time.Time, sameTime bool) {
		f.exec(t, `INSERT INTO usage_events (workspace_id,source_id,source_event_id,project_id,provider,model,occurred_at,received_at,normalization_version,payload_hash,
 input_tokens,input_tokens_quality,output_tokens,output_tokens_quality,total_tokens,total_tokens_quality,cache_read_tokens_quality,cache_write_tokens_quality,reasoning_tokens_quality,status,duration_ms)
SELECT $1,$2,'x'||md5(random()::text||g::text||$6::text),
  CASE WHEN g%4=0 THEN NULL WHEN g%4=1 THEN $7::uuid WHEN g%4=2 THEN $8::uuid ELSE NULL END,
  'p'||(g%3), 'm'||(g%7),
  CASE WHEN $9::boolean THEN $4::timestamptz ELSE $4::timestamptz + (($5::timestamptz-$4::timestamptz) * g / $3) END,
  now(),'v1','\x00',
  100,'known',20,'known',
  CASE WHEN g%10=0 THEN NULL ELSE 120 END, CASE WHEN g%10=0 THEN 'missing' WHEN g%10=1 THEN 'estimated' ELSE 'known' END,
  'missing','missing','missing','success',500
FROM generate_series(1,$3::int) g`, workspace, srcFor(workspace), n, from, to, fmt.Sprint(workspace.Bytes[15], n), projA, projB, sameTime)
	}
	seed(ws, 80000, end.Add(-180*24*time.Hour), end.Add(-2*time.Hour), false)
	seed(ws, 9000, end.Add(-time.Hour), end, false)
	seed(ws, 1000, end.Add(-30*time.Second), end.Add(-30*time.Second), true)
	seed(otherWS, 10000, end.Add(-time.Hour), end, false)

	// Fresh bulk data has no planner statistics; refresh them like autovacuum would.
	// The service role may not own the table, so a refusal is only logged.
	if _, err := f.pool.Exec(f.ctx, `ANALYZE usage_events`); err != nil {
		t.Logf("ANALYZE refused for service role: %v", err)
	}
	var rows int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM usage_events`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	var mine int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM usage_events WHERE workspace_id=$1`, ws).Scan(&mine); err != nil {
		t.Fatal(err)
	}
	t.Logf("rows total=%d workspace=%d", rows, mine)

	explain := func(name, sql string, args ...any) {
		out, err := f.pool.Query(f.ctx, "EXPLAIN (ANALYZE, BUFFERS) "+sql, args...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		defer out.Close()
		var b strings.Builder
		for out.Next() {
			var line string
			if err := out.Scan(&line); err != nil {
				t.Fatal(err)
			}
			b.WriteString(line + "\n")
		}
		t.Logf("=== %s ===\n%s", name, b.String())
	}
	none := Filter{}
	cases := []struct {
		name string
		p    EventsParams
	}{
		{"latest page, no filter, 1h", EventsParams{WorkspaceID: ws, Start: end.Add(-time.Hour), End: end, Filter: none, Limit: 50}},
		{"latest page, provider+model", EventsParams{WorkspaceID: ws, Start: end.Add(-time.Hour), End: end, Filter: Filter{Providers: []string{"p1"}, Models: []string{"m3", "m4"}}, Limit: 50}},
		{"latest page, project+unattributed", EventsParams{WorkspaceID: ws, Start: end.Add(-time.Hour), End: end, Filter: Filter{Projects: []pgtype.UUID{projA}, Unattributed: true}, Limit: 50}},
		{"latest page, unattributed only, 7d", EventsParams{WorkspaceID: ws, Start: end.Add(-7 * 24 * time.Hour), End: end, Filter: Filter{Unattributed: true}, Limit: 50}},
		{"cursor at identical occurred_at cluster", EventsParams{WorkspaceID: ws, Start: end.Add(-time.Hour), End: end, Limit: 50, HasCursor: true, CursorTime: end.Add(-30 * time.Second), CursorID: pgtype.UUID{Bytes: [16]byte{0xff}, Valid: true}}},
		{"full 180d range, selective filter", EventsParams{WorkspaceID: ws, Start: end.Add(-180 * 24 * time.Hour), End: end, Filter: Filter{Providers: []string{"p2"}, Models: []string{"m6"}, Projects: []pgtype.UUID{projB}}, Limit: 50}},
	}
	for _, c := range cases {
		args := []any{c.p.WorkspaceID, c.p.Start, c.p.End}
		args = append(args, filterArgs(c.p.Filter)...)
		var ct, ci any
		if c.p.HasCursor {
			ct, ci = c.p.CursorTime, c.p.CursorID
		}
		args = append(args, c.p.HasCursor, ct, ci, c.p.Limit+1)
		explain("events: "+c.name, eventsQuery, args...)
	}
	for _, c := range []struct {
		name string
		f    Filter
	}{{"no filter", none}, {"provider+model", Filter{Providers: []string{"p1"}, Models: []string{"m3"}}}, {"unattributed", Filter{Unattributed: true}}} {
		args := append([]any{ws, end}, filterArgs(c.f)...)
		explain("rate: "+c.name, rateQuery, args...)
	}
}
