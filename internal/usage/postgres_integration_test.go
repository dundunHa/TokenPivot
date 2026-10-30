//go:build integration

package usage

import (
	"context"
	"crypto/rand"
	"fmt"
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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// integrationSchema creates a fresh schema in the dedicated _test database,
// applies every released migration and returns the migration-owner and service
// role URLs pinned to it. Missing or unsafe variables fail the test.
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
	var unsafe, member bool
	err = app.QueryRow(ctx, "SELECT current_user, rolsuper OR rolcreaterole OR rolcreatedb OR has_database_privilege(current_user,current_database(),'CREATE'), pg_has_role(current_user, 'tokenpivot_app', 'USAGE') FROM pg_roles WHERE rolname=current_user").Scan(&appRole, &unsafe, &member)
	_ = app.Close(ctx)
	if err != nil {
		t.Fatal(database.SafeError("service role identity", err))
	}
	if unsafe || !member || appRole != urls[1].User.Username() {
		t.Fatal("unsafe test service role")
	}
	schema := "t055_" + fmt.Sprintf("%x", rand.Text()[:12])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := owner.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(database.SafeError("create owned schema", err))
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var actual string
		err := owner.QueryRow(cleanupCtx, "SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname=$1", schema).Scan(&actual)
		if err != nil || actual != role || !strings.HasPrefix(schema, "t055_") {
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
	applyMigrations(t, values[0])
	return values[0], values[1]
}

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

// fixture is a store on the service-role pool. Rows are inserted through the same
// pool, which only holds SELECT, INSERT and UPDATE, so the tests never use the
// owner role to bypass the application grants.
type fixture struct {
	store *Postgres
	pool  *pgxpool.Pool
	ctx   context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	_, appURL := integrationSchema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := database.Open(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &fixture{store: NewPostgres(pool), pool: pool, ctx: ctx}
}

func uid(b byte) pgtype.UUID {
	var raw [16]byte
	raw[0], raw[15] = 0x01, b
	return pgtype.UUID{Bytes: raw, Valid: true}
}

// row is one usage_minute_stats row; every metric shares the same quality triple
// so the tests can reason about it, and total/others differ by the multipliers.
type row struct {
	workspace pgtype.UUID
	minute    time.Time
	source    pgtype.UUID
	project   pgtype.UUID // invalid means NULL
	provider  string
	model     string
	events    int64
	input     int64
	output    int64
	total     int64
	known     int64
	estimated int64
	missing   int64
}

func (f *fixture) insert(t *testing.T, rows ...row) {
	t.Helper()
	const stmt = `INSERT INTO usage_minute_stats (workspace_id,bucket_start,source_id,project_id,provider,model,event_count,
input_tokens_sum,input_tokens_known_count,input_tokens_estimated_count,input_tokens_missing_count,
output_tokens_sum,output_tokens_known_count,output_tokens_estimated_count,output_tokens_missing_count,
total_tokens_sum,total_tokens_known_count,total_tokens_estimated_count,total_tokens_missing_count,
cache_read_tokens_sum,cache_read_tokens_known_count,cache_read_tokens_estimated_count,cache_read_tokens_missing_count,
cache_write_tokens_sum,cache_write_tokens_known_count,cache_write_tokens_estimated_count,cache_write_tokens_missing_count,
reasoning_tokens_sum,reasoning_tokens_known_count,reasoning_tokens_estimated_count,reasoning_tokens_missing_count)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$11,$12,$13,$9,$11,$12,$13,$10,$11,$12,$13,0,0,0,$14,0,0,0,$14,0,0,0,$14)`
	for _, r := range rows {
		var project any
		if r.project.Valid {
			project = r.project
		}
		if _, err := f.pool.Exec(f.ctx, stmt, r.workspace, r.minute, r.source, project, r.provider, r.model, r.events,
			r.input, r.output, r.total, r.known, r.estimated, r.missing, r.missing); err != nil {
			t.Fatal(database.SafeError("insert", err))
		}
	}
}

var (
	ws      = uid(1)
	otherWS = uid(2)
	srcA    = uid(10)
	projA   = uid(21)
	projB   = uid(22)
	base    = time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
)

func rowAt(offset time.Duration, provider, model string, project pgtype.UUID, input, output, total int64) row {
	return row{workspace: ws, minute: base.Add(offset), source: srcA, project: project, provider: provider, model: model, events: 1, input: input, output: output, total: total, known: 1}
}

func buckets(starts ...time.Time) []Bucket {
	out := make([]Bucket, len(starts)-1)
	for i := range out {
		out[i] = Bucket{Start: starts[i], End: starts[i+1]}
	}
	return out
}

func TestIntegrationAggregateBucketsByPrecomputedBoundaries(t *testing.T) {
	f := newFixture(t)
	f.insert(t,
		rowAt(0, "p", "m", projA, 10, 5, 15),
		rowAt(29*time.Minute, "p", "m", projA, 1, 1, 2),
		rowAt(30*time.Minute, "p", "m", projA, 100, 50, 150), // half-hour boundary supplied by the caller
		rowAt(2*time.Hour, "p", "m", projA, 7, 7, 14),        // bucket index 3; index 2 stays empty
		rowAt(3*time.Hour, "p", "m", projA, 999, 999, 1998),  // at End: excluded
		row{workspace: otherWS, minute: base, source: srcA, provider: "p", model: "m", events: 1, input: 5000, output: 5000, total: 10000, known: 1},
	)
	bounds := buckets(base, base.Add(30*time.Minute), base.Add(time.Hour), base.Add(2*time.Hour), base.Add(3*time.Hour))
	rows, err := f.store.Aggregate(f.ctx, AggregateParams{WorkspaceID: ws, Start: base, End: base.Add(3 * time.Hour), Buckets: bounds})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Points) != 3 || rows.Points[0].Index != 0 || rows.Points[1].Index != 1 || rows.Points[2].Index != 3 {
		t.Fatalf("points = %+v: the empty bucket must be absent and nothing zero-filled", rows.Points)
	}
	first := rows.Points[0].Stats
	if first.RequestCount != "2" || first.Metrics[2].Sum != "17" || first.Metrics[2].Known != "2" {
		t.Fatalf("first bucket = %+v", first)
	}
	// 17 + 150 + 14: the overall total equals the sum of the points.
	if rows.Overall == nil || rows.Overall.RequestCount != "4" || rows.Overall.Metrics[2].Sum != "181" {
		t.Fatalf("overall = %+v", rows.Overall)
	}
}

func TestIntegrationAggregateFiltersAndTenancy(t *testing.T) {
	f := newFixture(t)
	f.insert(t,
		rowAt(0, "p1", "m1", projA, 1, 1, 2),
		rowAt(1*time.Minute, "p1", "m2", projB, 2, 2, 4),
		rowAt(2*time.Minute, "p2", "m1", pgtype.UUID{}, 4, 4, 8),
		row{workspace: otherWS, minute: base, source: srcA, project: projA, provider: "p1", model: "m1", events: 1, input: 100, output: 100, total: 200, known: 1},
	)
	run := func(filter Filter) *RawStats {
		rows, err := f.store.Aggregate(f.ctx, AggregateParams{WorkspaceID: ws, Start: base, End: base.Add(time.Hour), Filter: filter, Buckets: buckets(base, base.Add(time.Hour))})
		if err != nil {
			t.Fatal(err)
		}
		return rows.Overall
	}
	totalOf := func(s *RawStats) string {
		if s == nil {
			return "none"
		}
		return s.Metrics[2].Sum
	}
	cases := []struct {
		name   string
		filter Filter
		want   string
	}{
		{"no filter only the authorised workspace", Filter{}, "14"},
		{"provider", Filter{Providers: []string{"p1"}}, "6"},
		{"provider OR", Filter{Providers: []string{"p1", "p2"}}, "14"},
		{"model", Filter{Models: []string{"m1"}}, "10"},
		{"provider AND model", Filter{Providers: []string{"p1"}, Models: []string{"m1"}}, "2"},
		{"project", Filter{Projects: []pgtype.UUID{projA}}, "2"},
		{"unassigned", Filter{Unassigned: true}, "8"},
		{"unassigned OR project", Filter{Unassigned: true, Projects: []pgtype.UUID{projB}}, "12"},
		{"case sensitive", Filter{Providers: []string{"P1"}}, "none"},
		{"foreign workspace project id", Filter{Projects: []pgtype.UUID{uid(99)}}, "none"},
	}
	for _, tc := range cases {
		if got := totalOf(run(tc.filter)); got != tc.want {
			t.Errorf("%s: total = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestIntegrationQualityAndLargeNumbers(t *testing.T) {
	f := newFixture(t)
	const max = int64(9223372036854775807)
	a := rowAt(0, "p", "m", projA, max, max, max)
	b := rowAt(time.Minute, "p", "m", projA, max, max, max)
	c := rowAt(2*time.Minute, "p", "m", projA, 0, 0, 0)
	c.known, c.missing = 0, 2 // everything missing
	f.insert(t, a, b, c)
	rows, err := f.store.Aggregate(f.ctx, AggregateParams{WorkspaceID: ws, Start: base, End: base.Add(time.Hour), Buckets: buckets(base, base.Add(time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := convertStats(*rows.Overall)
	if err != nil {
		t.Fatal(err)
	}
	if *stats.TotalTokens.Value != "18446744073709551614" {
		t.Fatalf("sum beyond int64 = %s", *stats.TotalTokens.Value)
	}
	if stats.RequestCount != "3" || stats.TotalTokens.KnownCount != "2" || stats.TotalTokens.MissingCount != "2" || stats.CacheWriteTokens.Value != nil {
		t.Fatalf("stats = %+v", stats)
	}
	// A bucket whose rows are all missing has no value but is still returned.
	only := rowAt(10*time.Minute, "q", "n", projA, 0, 0, 0)
	only.known, only.missing = 0, 1
	f.insert(t, only)
	rows, err = f.store.Aggregate(f.ctx, AggregateParams{WorkspaceID: ws, Start: base.Add(10 * time.Minute), End: base.Add(11 * time.Minute), Buckets: buckets(base.Add(10*time.Minute), base.Add(11*time.Minute))})
	if err != nil || len(rows.Points) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	one, _ := convertStats(rows.Points[0].Stats)
	if one.TotalTokens.Value != nil || one.RequestCount != "1" {
		t.Fatalf("all-missing bucket = %+v", one)
	}
}

// TestIntegrationCountsBeyondInt64 sums request_count and the known, estimated
// and missing counts past int64 across rows that each hold a valid bigint, and
// checks the decimal strings digit by digit in both Aggregate and Rankings.
func TestIntegrationCountsBeyondInt64(t *testing.T) {
	f := newFixture(t)
	const max = int64(9223372036854775807)
	var rows []row
	for i := 0; i < 6; i++ {
		r := rowAt(time.Duration(i)*time.Minute, "p", "m", projA, 0, 0, 0)
		r.events, r.known, r.estimated, r.missing = max, 0, 0, 0
		switch i / 2 {
		case 0:
			r.known = max
		case 1:
			r.estimated = max
		default:
			r.missing = max
		}
		rows = append(rows, r)
	}
	f.insert(t, rows...)
	// The shared helper leaves the cache and reasoning metrics' missing counts at
	// r.missing; make them add up to request_count so the quality counts stay valid.
	if _, err := f.pool.Exec(f.ctx, `UPDATE usage_minute_stats SET cache_read_tokens_missing_count=event_count,
cache_write_tokens_missing_count=event_count, reasoning_tokens_missing_count=event_count WHERE workspace_id=$1`, ws); err != nil {
		t.Fatal(database.SafeError("fix missing counts", err))
	}
	check := func(name string, raw RawStats) {
		t.Helper()
		s, err := convertStats(raw)
		if err != nil {
			t.Fatal(err)
		}
		const twice = "18446744073709551614"
		m := s.TotalTokens
		if s.RequestCount != "55340232221128654842" || m.KnownCount != twice || m.EstimatedCount != twice || m.MissingCount != twice {
			t.Fatalf("%s: request=%s known=%s estimated=%s missing=%s", name, s.RequestCount, m.KnownCount, m.EstimatedCount, m.MissingCount)
		}
		for _, o := range []MetricStats{s.CacheReadTokens, s.CacheWriteTokens, s.ReasoningTokens} {
			if o.KnownCount != "0" || o.EstimatedCount != "0" || o.MissingCount != s.RequestCount {
				t.Fatalf("%s: other metric known=%s estimated=%s missing=%s", name, o.KnownCount, o.EstimatedCount, o.MissingCount)
			}
		}
	}
	agg, err := f.store.Aggregate(f.ctx, AggregateParams{WorkspaceID: ws, Start: base, End: base.Add(time.Hour), Buckets: buckets(base, base.Add(time.Hour))})
	if err != nil || agg.Overall == nil {
		t.Fatalf("aggregate rows=%+v err=%v", agg, err)
	}
	check("aggregate overall", *agg.Overall)
	rk := rankings(t, f, GroupModel, 0, 1, Filter{})
	if len(rk.Groups) != 1 {
		t.Fatalf("groups = %s", keys(rk.Groups))
	}
	check("rankings overall", rk.Overall)
	check("rankings group", rk.Groups[0].Stats)
}

func TestIntegrationAggregateEmptyAndHalfOpenRange(t *testing.T) {
	f := newFixture(t)
	f.insert(t, rowAt(0, "p", "m", projA, 1, 1, 2))
	rows, err := f.store.Aggregate(f.ctx, AggregateParams{WorkspaceID: ws, Start: base.Add(time.Minute), End: base.Add(time.Hour), Buckets: buckets(base.Add(time.Minute), base.Add(time.Hour))})
	if err != nil || rows.Overall != nil || len(rows.Points) != 0 {
		t.Fatalf("empty result = %+v err=%v", rows, err)
	}
	// [start,end): a row exactly at end is excluded, at start included.
	rows, err = f.store.Aggregate(f.ctx, AggregateParams{WorkspaceID: ws, Start: base, End: base.Add(time.Minute), Buckets: buckets(base, base.Add(time.Minute))})
	if err != nil || rows.Overall == nil || rows.Overall.RequestCount != "1" {
		t.Fatalf("half open = %+v err=%v", rows, err)
	}
}

func seedRanking(t *testing.T, f *fixture) {
	t.Helper()
	f.insert(t,
		rowAt(0, "pa", "shared", projA, 1, 1, 100),
		rowAt(time.Minute, "pb", "shared", projB, 1, 1, 100), // same model name, other provider, tied total
		rowAt(2*time.Minute, "pa", "small", projA, 1, 1, 30),
		rowAt(3*time.Minute, "pa", "tiny", pgtype.UUID{}, 1, 1, 5),
		ghostRow(),
		row{workspace: otherWS, minute: base, source: srcA, provider: "pz", model: "zz", events: 1, total: 99999, known: 1},
	)
}

// ghostRow is a request whose token values are all missing.
func ghostRow() row {
	r := rowAt(4*time.Minute, "pc", "ghost", pgtype.UUID{}, 0, 0, 0)
	r.known, r.missing = 0, 1
	return r
}

func rankings(t *testing.T, f *fixture, by GroupBy, offset, limit int, filter Filter) RankingsRows {
	t.Helper()
	rows, err := f.store.Rankings(f.ctx, RankingsParams{WorkspaceID: ws, Start: base, End: base.Add(time.Hour), Filter: filter, GroupBy: by, Offset: offset, Limit: limit})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func keys(rows []RawGroup) string {
	out := make([]string, len(rows))
	for i, g := range rows {
		a, b := "<null>", ""
		if g.Key1 != nil {
			a = *g.Key1
		}
		if g.Key2 != nil {
			b = "/" + *g.Key2
		}
		out[i] = a + b
	}
	return strings.Join(out, ",")
}

func TestIntegrationRankingsModelKeysTiesAndOther(t *testing.T) {
	f := newFixture(t)
	seedRanking(t, f)
	page := rankings(t, f, GroupModel, 0, 2, Filter{})
	if keys(page.Groups) != "pa/shared,pb/shared" {
		t.Fatalf("page 1 = %s (same model under two providers must stay apart, ties by key)", keys(page.Groups))
	}
	if page.Overall.RequestCount != "5" || page.Overall.Metrics[2].Sum != "235" {
		t.Fatalf("overall = %+v", page.Overall)
	}
	if page.Other == nil || page.Other.RequestCount != "3" || page.Other.Metrics[2].Sum != "35" {
		t.Fatalf("other = %+v", page.Other)
	}
	second := rankings(t, f, GroupModel, 2, 2, Filter{})
	if keys(second.Groups) != "pa/small,pa/tiny" || second.Other == nil || second.Other.RequestCount != "1" {
		t.Fatalf("page 2 = %s other=%+v", keys(second.Groups), second.Other)
	}
	if second.Overall.Metrics[2].Sum != "235" {
		t.Fatal("overall must cover every group on every page")
	}
	last := rankings(t, f, GroupModel, 4, 2, Filter{})
	if keys(last.Groups) != "pc/ghost" || last.Other != nil {
		t.Fatalf("last page = %s other=%+v", keys(last.Groups), last.Other)
	}
	ghost, _ := convertStats(last.Groups[0].Stats)
	if ghost.TotalTokens.Value != nil || ghost.RequestCount != "1" {
		t.Fatalf("all-missing group must stay, with no value: %+v", ghost)
	}
	// Static data: pages are disjoint and complete.
	seen := map[string]bool{}
	for offset := 0; offset < 5; offset++ {
		for _, g := range rankings(t, f, GroupModel, offset, 1, Filter{}).Groups {
			seen[keys([]RawGroup{g})] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("one-by-one paging saw %d groups, want 5", len(seen))
	}
}

func TestIntegrationRankingsProjectNullLastAndProvider(t *testing.T) {
	f := newFixture(t)
	seedRanking(t, f)
	projects := rankings(t, f, GroupProject, 0, 50, Filter{})
	if len(projects.Groups) != 3 || projects.Groups[2].Key1 != nil {
		t.Fatalf("projects = %s, NULL project must sort last", keys(projects.Groups))
	}
	if projects.Groups[0].Key2 != nil {
		t.Fatal("project groups have a single key")
	}
	providers := rankings(t, f, GroupProvider, 0, 50, Filter{})
	if keys(providers.Groups) != "pa,pb,pc" {
		t.Fatalf("providers = %s", keys(providers.Groups))
	}
	filtered := rankings(t, f, GroupProvider, 0, 50, Filter{Providers: []string{"pa"}})
	if keys(filtered.Groups) != "pa" || filtered.Overall.Metrics[2].Sum != "135" {
		t.Fatalf("filter must apply to groups and overall: %s %+v", keys(filtered.Groups), filtered.Overall)
	}
}

func TestIntegrationRankingsEmptyResult(t *testing.T) {
	f := newFixture(t)
	rows := rankings(t, f, GroupModel, 0, 10, Filter{})
	if len(rows.Groups) != 0 || rows.Other != nil || rows.Overall.RequestCount != "0" || rows.Overall.Metrics[2].Known != "0" {
		t.Fatalf("empty = %+v", rows)
	}
}

func TestIntegrationCollationIsByteOrder(t *testing.T) {
	f := newFixture(t)
	f.insert(t,
		rowAt(0, "b", "m", projA, 1, 1, 10),
		rowAt(time.Minute, "B", "m", projA, 1, 1, 10),
		rowAt(2*time.Minute, "a", "m", projA, 1, 1, 10),
	)
	rows := rankings(t, f, GroupProvider, 0, 10, Filter{})
	if keys(rows.Groups) != "B,a,b" {
		t.Fatalf("order = %s, want C collation B,a,b", keys(rows.Groups))
	}
}

func TestIntegrationCanceledContextIsNotEmptyResult(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.store.Aggregate(ctx, AggregateParams{WorkspaceID: ws, Start: base, End: base.Add(time.Hour), Buckets: buckets(base, base.Add(time.Hour))}); err == nil {
		t.Fatal("canceled aggregate returned no error")
	}
	if _, err := f.store.Rankings(ctx, RankingsParams{WorkspaceID: ws, Start: base, End: base.Add(time.Hour), GroupBy: GroupModel, Limit: 1}); err == nil {
		t.Fatal("canceled rankings returned no error")
	}
}

func TestIntegrationServiceEndToEndWithLordHoweBoundaries(t *testing.T) {
	f := newFixture(t)
	// Rows around the Lord Howe DST start (2026-10-03T15:30Z): a 30 minute bucket.
	day := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	for _, offset := range []time.Duration{14*time.Hour + 30*time.Minute, 15*time.Hour + 29*time.Minute, 15*time.Hour + 30*time.Minute, 15*time.Hour + 59*time.Minute, 16 * time.Hour} {
		r := rowAt(0, "p", "m", projA, 1, 1, 2)
		r.minute = day.Add(offset)
		f.insert(t, r)
	}
	s := New(f.store)
	s.now = func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }
	response, err := s.Aggregate(f.ctx, ws, url.Values{
		"start": {"2026-10-03T14:30:00Z"}, "end": {"2026-10-03T17:00:00Z"}, "timezone": {"Australia/Lord_Howe"}, "granularity": {"hour"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Points) != 3 || response.Totals.RequestCount != "5" {
		t.Fatalf("response = %+v", response)
	}
	counts := []string{response.Points[0].Stats.RequestCount, response.Points[1].Stats.RequestCount, response.Points[2].Stats.RequestCount}
	if strings.Join(counts, ",") != "2,2,1" || response.Points[1].BucketEnd != "2026-10-03T16:00:00Z" {
		t.Fatalf("counts=%v points=%+v: rows must follow the Go boundaries", counts, response.Points)
	}
}

// optionsClock fixes the service clock on a minute boundary so the window edges
// land exactly on stored bucket_start values.
func optionsService(f *fixture) (*Service, time.Time, time.Time) {
	asOf := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	s := New(f.store)
	s.now = func() time.Time { return asOf }
	return s, aggregation.RetentionCutoff(asOf), asOf
}

func optionRow(minute time.Time, workspace pgtype.UUID, provider, model string) row {
	r := rowAt(0, provider, model, projA, 1, 1, 2)
	r.workspace, r.minute = workspace, minute
	return r
}

func TestIntegrationOptionsWindowTenancyDedupAndOrder(t *testing.T) {
	f := newFixture(t)
	s, cutoff, asOf := optionsService(f)
	f.insert(t,
		optionRow(cutoff.Add(-time.Minute), ws, "before-p", "before-m"), // before cutoff: excluded
		optionRow(cutoff, ws, "b", "m2"),                                // equal to cutoff: included
		optionRow(asOf.Add(-time.Minute), ws, "a", "M1"),                // last minute before as_of: included
		optionRow(asOf, ws, "at-p", "at-m"),                             // == as_of: excluded
		optionRow(asOf.Add(time.Minute), ws, "after-p", "after-m"),      // after as_of: excluded
		optionRow(asOf.Add(-time.Hour), otherWS, "foreign-p", "foreign-m"),
		optionRow(asOf.Add(-time.Hour), ws, "B", "m2"), // case differs: a separate value, byte order puts it first
	)
	// Same provider/model under a second project and source: still one value.
	dup := optionRow(asOf.Add(-2*time.Hour), ws, "a", "M1")
	dup.project, dup.source = projB, uid(11)
	f.insert(t, dup)

	got, err := s.Options(f.ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if want := "B,a,b"; strings.Join(got.Providers, ",") != want {
		t.Fatalf("providers = %v want %s", got.Providers, want)
	}
	if want := "M1,m2"; strings.Join(got.Models, ",") != want {
		t.Fatalf("models = %v want %s", got.Models, want)
	}
	foreign, err := s.Options(f.ctx, otherWS)
	if err != nil || strings.Join(foreign.Providers, ",") != "foreign-p" || strings.Join(foreign.Models, ",") != "foreign-m" {
		t.Fatalf("other workspace = %+v err=%v", foreign, err)
	}
}

func TestIntegrationOptionsExcludeEmptyStrings(t *testing.T) {
	f := newFixture(t)
	s, _, asOf := optionsService(f)
	at := asOf.Add(-time.Hour)
	f.insert(t,
		optionRow(at, ws, "", "m"),
		optionRow(at.Add(time.Minute), ws, "p", ""),
		optionRow(at.Add(2*time.Minute), ws, "", ""),
		optionRow(at.Add(3*time.Minute), ws, "q", "n"),
	)
	got, err := s.Options(f.ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Providers, ",") != "p,q" || strings.Join(got.Models, ",") != "m,n" {
		t.Fatalf("got %+v want providers p,q and models m,n", got)
	}
}

func TestIntegrationOptionsEmptyIsNotAnError(t *testing.T) {
	f := newFixture(t)
	s, _, _ := optionsService(f)
	got, err := s.Options(f.ctx, ws)
	if err != nil || got.Providers == nil || got.Models == nil || len(got.Providers) != 0 || len(got.Models) != 0 {
		t.Fatalf("empty = %+v err=%v", got, err)
	}
}

func TestIntegrationOptionsCanceledContextIsNotEmptyResult(t *testing.T) {
	f := newFixture(t)
	s, _, _ := optionsService(f)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := s.Options(ctx, ws); err == nil {
		t.Fatal("canceled context returned no error")
	}
}
