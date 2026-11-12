//go:build integration

package requestdetails

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"sort"
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
	schema := "t058_" + fmt.Sprintf("%x", rand.Text()[:12])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := owner.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(database.SafeError("create owned schema", err))
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var actual string
		err := owner.QueryRow(cleanupCtx, "SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname=$1", schema).Scan(&actual)
		if err != nil || actual != role || !strings.HasPrefix(schema, "t058_") {
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

type fixture struct {
	store *Postgres
	svc   *Service
	pool  *pgxpool.Pool
	ctx   context.Context
	url   string
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
	store := NewPostgres(pool)
	f := &fixture{store: store, svc: New(store), pool: pool, ctx: ctx, url: appURL}
	for _, w := range []pgtype.UUID{ws, otherWS} {
		f.exec(t, `INSERT INTO sources (id,workspace_id,platform,instance_key) VALUES ($1,$2,'p','i')`, srcFor(w), w)
	}
	f.exec(t, `INSERT INTO projects (id,workspace_id,project_key,name) VALUES ($1,$2,'a','Alpha'),($3,$2,'b','Beta'),($4,$5,'z','Foreign')`, projA, ws, projB, projX, otherWS)
	return f
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		t.Fatal(database.SafeError("exec", err))
	}
}

func uid(b byte) pgtype.UUID {
	var raw [16]byte
	raw[0], raw[15] = 0x01, b
	return pgtype.UUID{Bytes: raw, Valid: true}
}

func srcFor(w pgtype.UUID) pgtype.UUID { return uid(w.Bytes[15] + 100) }

var (
	ws      = uid(1)
	otherWS = uid(2)
	projA   = uid(21)
	projB   = uid(22)
	projX   = uid(23)
	asOf    = time.Date(2026, 10, 28, 12, 0, 30, 0, time.UTC)
	nextID  byte
)

type ev struct {
	ws       pgtype.UUID
	at       time.Time
	received time.Time
	project  pgtype.UUID
	provider string
	model    string
	total    *int64
	quality  string
	id       pgtype.UUID
	// out is the final output token count; outputQuality defaults to known and
	// must be set to estimated/missing with out nil for those rows. gen and ttft
	// are the raw client timings, nil when unknown.
	out           *int64
	outputQuality string
	gen           *int64
	ttft          *int64
}

func n(v int64) *int64 { return &v }

func (f *fixture) add(t *testing.T, e ev) pgtype.UUID {
	t.Helper()
	nextID++
	if !e.ws.Valid {
		e.ws = ws
	}
	if e.provider == "" {
		e.provider = "prov"
	}
	if e.model == "" {
		e.model = "mod"
	}
	if e.quality == "" {
		e.quality = "known"
	}
	if e.outputQuality == "" {
		if e.out != nil {
			e.outputQuality = "known"
		} else {
			e.outputQuality = "missing"
		}
	}
	if e.received.IsZero() {
		e.received = e.at
	}
	if !e.id.Valid {
		e.id = pgtype.UUID{Bytes: [16]byte{0x02, 15: nextID}, Valid: true}
	}
	var project any
	if e.project.Valid {
		project = e.project
	}
	f.exec(t, `INSERT INTO usage_events (id,workspace_id,source_id,source_event_id,project_id,provider,model,occurred_at,received_at,normalization_version,payload_hash,
input_tokens,input_tokens_quality,output_tokens,output_tokens_quality,total_tokens,total_tokens_quality,
cache_read_tokens_quality,cache_write_tokens_quality,reasoning_tokens_quality,generation_duration_ms,ttft_ms)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'v1','\x00',NULL,'missing',$12,$13,$10,$11,'missing','missing','missing',$14,$15)`,
		e.id, e.ws, srcFor(e.ws), fmt.Sprint("e", nextID), project, e.provider, e.model, e.at, e.received, e.total, e.quality,
		e.out, e.outputQuality, e.gen, e.ttft)
	return e.id
}

func (f *fixture) rate(t *testing.T, w pgtype.UUID, q url.Values) *RateResponse {
	t.Helper()
	r, err := f.svc.RateAt(f.ctx, w, q, asOf)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRateWindowBoundariesAndSums(t *testing.T) {
	f := newFixture(t)
	start := asOf.Add(-time.Minute)
	f.add(t, ev{at: start.Add(-time.Microsecond), total: n(1000)})
	f.add(t, ev{at: start, total: n(10)})
	f.add(t, ev{at: asOf.Add(-time.Microsecond), total: n(20), quality: "estimated", received: asOf.Add(5 * time.Second)})
	f.add(t, ev{at: asOf, total: n(4000)})
	f.add(t, ev{at: start.Add(time.Second), total: nil, quality: "missing"})
	// A late report that occurred before the window must not count, nor move freshness.
	f.add(t, ev{at: start.Add(-time.Hour), total: n(7), received: asOf.Add(20 * time.Second)})
	f.add(t, ev{ws: otherWS, at: start.Add(time.Second), total: n(5)})
	r := f.rate(t, ws, nil)
	tt := r.TotalTokens
	if tt.Value == nil || *tt.Value != "30" || tt.EventCount != 3 || tt.KnownCount != 1 || tt.EstimatedCount != 1 || tt.MissingCount != 1 {
		t.Fatalf("rate = %+v", tt)
	}
	if r.WindowLastReceived == nil || *r.WindowLastReceived != formatTime(asOf.Add(5*time.Second)) {
		t.Fatalf("fresh = %v", r.WindowLastReceived)
	}
	if r.Start != formatTime(start) || r.End != formatTime(asOf) || r.WindowSeconds != 60 || r.Unit != "tokens/min" {
		t.Fatalf("window = %+v", r)
	}
}

func TestRateNullsZerosAndOverflow(t *testing.T) {
	f := newFixture(t)
	r := f.rate(t, ws, nil)
	if r.TotalTokens.Value != nil || r.TotalTokens.EventCount != 0 || r.WindowLastReceived != nil {
		t.Fatalf("empty = %+v", r)
	}
	f.add(t, ev{at: asOf.Add(-time.Second), total: nil, quality: "missing"})
	r = f.rate(t, ws, nil)
	if r.TotalTokens.Value != nil || r.TotalTokens.EventCount != 1 || r.TotalTokens.MissingCount != 1 || r.WindowLastReceived == nil {
		t.Fatalf("missing only = %+v", r)
	}
	f.add(t, ev{at: asOf.Add(-2 * time.Second), total: n(0)})
	if v := f.rate(t, ws, nil).TotalTokens.Value; v == nil || *v != "0" {
		t.Fatalf("zero = %v", v)
	}
	const big = 1<<53 - 1
	f.add(t, ev{at: asOf.Add(-3 * time.Second), total: n(big)})
	f.add(t, ev{at: asOf.Add(-4 * time.Second), total: n(big)})
	if v := f.rate(t, ws, nil).TotalTokens.Value; v == nil || *v != "18014398509481982" {
		t.Fatalf("big = %v", v)
	}
}

func TestFiltersAndForeignProject(t *testing.T) {
	f := newFixture(t)
	at := asOf.Add(-time.Second)
	f.add(t, ev{at: at, project: projA, provider: "p1", model: "m1", total: n(1)})
	f.add(t, ev{at: at.Add(-time.Millisecond), project: projB, provider: "p2", model: "m1", total: n(2)})
	f.add(t, ev{at: at.Add(-2 * time.Millisecond), provider: "p1", model: "m2", total: n(4)})
	cases := []struct {
		q    url.Values
		want string
	}{
		{nil, "7"},
		{url.Values{"provider": {"p1"}}, "5"},
		{url.Values{"provider": {"p1", "p2"}, "model": {"m1"}}, "3"},
		{url.Values{"project": {"unattributed"}}, "4"},
		{url.Values{"project": {projA.String(), "unattributed"}}, "5"},
		{url.Values{"project": {projB.String()}, "provider": {"p1"}}, ""},
	}
	for _, c := range cases {
		r := f.rate(t, ws, c.q)
		if c.want == "" {
			if r.TotalTokens.EventCount != 0 {
				t.Fatalf("%v = %+v", c.q, r)
			}
			continue
		}
		if r.TotalTokens.Value == nil || *r.TotalTokens.Value != c.want {
			t.Fatalf("%v = %v want %s", c.q, r.TotalTokens.Value, c.want)
		}
		list, err := f.svc.Events(f.ctx, ws, withRange(c.q))
		if err != nil {
			t.Fatal(err)
		}
		var sum int64
		for _, item := range list.Items {
			sum += *item.TotalTokens.Value
		}
		if fmt.Sprint(sum) != c.want {
			t.Fatalf("events %v sum %d want %s", c.q, sum, c.want)
		}
	}
	if _, err := f.svc.RateAt(f.ctx, ws, url.Values{"project": {projX.String()}}, asOf); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("foreign project err = %v", err)
	}
	if _, err := f.svc.Events(f.ctx, otherWS, url.Values{"project": {projA.String()}}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("cross workspace project err = %v", err)
	}
}

func withRange(q url.Values) url.Values {
	out := url.Values{"start": {formatTime(asOf.Add(-time.Hour))}, "end": {formatTime(asOf)}}
	for k, v := range q {
		out[k] = v
	}
	return out
}

func TestEventsPagingStableAndWorkspaceScoped(t *testing.T) {
	f := newFixture(t)
	same := asOf.Add(-10 * time.Second)
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, f.add(t, ev{at: same, total: n(int64(i))}).String())
	}
	f.add(t, ev{at: same.Add(-time.Second), total: n(9), project: projA})
	f.add(t, ev{ws: otherWS, at: same, total: n(1), project: projX})
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	q := withRange(url.Values{"limit": {"2"}})
	var got []string
	var cursor string
	for page := 0; page < 5; page++ {
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		r, err := f.svc.Events(f.ctx, ws, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range r.Items {
			got = append(got, item.ID)
			if item.Project != nil && item.Project.Name != "Alpha" {
				t.Fatalf("project = %+v", item.Project)
			}
			if item.Status != nil || item.DurationMS != nil || item.InputTokens.Value != nil || item.InputTokens.Quality != "missing" {
				t.Fatalf("item = %+v", item)
			}
		}
		if !r.HasMore {
			if r.NextCursor != nil {
				t.Fatal("last page has cursor")
			}
			break
		}
		cursor = *r.NextCursor
		// A new head event and a same-time event after the cursor must not push or duplicate pages.
		if page == 0 {
			f.add(t, ev{at: asOf.Add(-time.Second), total: n(1)})
		}
	}
	if len(got) != 6 || !sort.SliceIsSorted(got[:5], func(i, j int) bool { return got[i] > got[j] }) {
		t.Fatalf("got %v", got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatal("duplicate id")
		}
		seen[id] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("missing %s", id)
		}
	}
}

func TestEventsDefaultRangeAndCursorValidation(t *testing.T) {
	f := newFixture(t)
	f.svc.now = func() time.Time { return asOf }
	f.add(t, ev{at: asOf.Add(-30 * time.Minute), total: n(1)})
	f.add(t, ev{at: asOf.Add(-90 * time.Minute), total: n(1)})
	f.add(t, ev{at: asOf.Add(-time.Minute), total: n(1)})
	r, err := f.svc.Events(f.ctx, ws, url.Values{"limit": {"1"}})
	if err != nil || len(r.Items) != 1 || !r.HasMore || r.Start != formatTime(asOf.Add(-time.Hour)) {
		t.Fatalf("first = %+v %v", r, err)
	}
	f.svc.now = func() time.Time { return asOf.Add(10 * time.Minute) }
	next, err := f.svc.Events(f.ctx, ws, url.Values{"limit": {"5"}, "cursor": {*r.NextCursor}})
	if err != nil || len(next.Items) != 1 || next.Start != r.Start || next.End != r.End {
		t.Fatalf("next = %+v %v", next, err)
	}
	bad := []url.Values{
		{"cursor": {"!!"}},
		{"cursor": {*r.NextCursor}, "start": {formatTime(asOf.Add(-2 * time.Hour))}, "end": {r.End}},
		{"limit": {"101"}}, {"limit": {"0"}}, {"limit": {"1", "2"}},
		{"start": {r.Start}}, {"start": {r.End}, "end": {r.Start}},
	}
	for _, q := range bad {
		if _, err := f.svc.Events(f.ctx, ws, q); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%v err = %v", q, err)
		}
	}
	// Late report whose occurred_at falls inside the default range shows up on a fresh first page.
	f.add(t, ev{at: asOf.Add(-20 * time.Minute), total: n(1), received: asOf.Add(9 * time.Minute)})
	f.svc.now = func() time.Time { return asOf }
	again, _ := f.svc.Events(f.ctx, ws, url.Values{"limit": {"100"}})
	if len(again.Items) != 3 {
		t.Fatalf("again = %d", len(again.Items))
	}
}

func TestConcurrentInsertsDuringReads(t *testing.T) {
	f := newFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := f.pool.Exec(f.ctx, `INSERT INTO usage_events (workspace_id,source_id,source_event_id,provider,model,occurred_at,received_at,normalization_version,payload_hash,input_tokens_quality,output_tokens_quality,total_tokens,total_tokens_quality,cache_read_tokens_quality,cache_write_tokens_quality,reasoning_tokens_quality)
VALUES ($1,$2,$3,'p','m',statement_timestamp(),statement_timestamp(),'v1','\x00','missing','missing',1,'known','missing','missing','missing')`, ws, srcFor(ws), fmt.Sprint("c", i)); err != nil {
				errs <- err
				return
			}
		}
	}()
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_, err := f.svc.Events(f.ctx, ws, url.Values{"limit": {"10"}})
			errs <- err
		}(i)
		go func() {
			defer wg.Done()
			_, err := f.svc.Rate(f.ctx, ws, nil)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestEventsPagingNewAndBackfilledEvents: after page one, a new event with the
// same occurred_at (larger id) and a backfill at an already passed position are
// not shown by later pages and nothing repeats; a backfill at a later position
// (older time, or same time with a smaller id) is. Restarting from the latest
// page then shows the earlier-hidden ones.
func TestEventsPagingNewAndBackfilledEvents(t *testing.T) {
	f := newFixture(t)
	same := asOf.Add(-10 * time.Second)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, f.add(t, ev{at: same, total: n(1)}).String())
	}
	old := f.add(t, ev{at: same.Add(-5 * time.Second), total: n(1)}).String()
	q := withRange(url.Values{"limit": {"2"}})
	first, err := f.svc.Events(f.ctx, ws, q)
	if err != nil || !first.HasMore || len(first.Items) != 2 {
		t.Fatalf("first = %+v err %v", first, err)
	}
	// ids[2], ids[1] are page one; the cursor is (same, ids[1]).
	newSame := f.add(t, ev{at: same, total: n(1)}).String()                                                    // larger id: before the cursor
	passed := f.add(t, ev{at: same.Add(time.Second), total: n(1)}).String()                                    // newer time: before the cursor
	lowID := f.add(t, ev{at: same, total: n(1), id: pgtype.UUID{Bytes: [16]byte{0x01}, Valid: true}}).String() // smaller id: after the cursor
	between := f.add(t, ev{at: same.Add(-time.Second), total: n(1)}).String()                                  // older time: after the cursor
	q.Set("cursor", *first.NextCursor)
	var later []string
	for {
		r, err := f.svc.Events(f.ctx, ws, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range r.Items {
			later = append(later, item.ID)
		}
		if !r.HasMore {
			break
		}
		q.Set("cursor", *r.NextCursor)
	}
	want := []string{ids[0], lowID, between, old} // same time by id DESC, then older times
	if fmt.Sprint(later) != fmt.Sprint(want) {
		t.Fatalf("later = %v want %v", later, want)
	}
	for _, hidden := range []string{newSame, passed} {
		for _, id := range later {
			if id == hidden {
				t.Fatalf("passed position %s shown on later page", hidden)
			}
		}
	}
	latest, err := f.svc.Events(f.ctx, ws, withRange(url.Values{"limit": {"100"}}))
	if err != nil {
		t.Fatal(err)
	}
	shown := map[string]bool{}
	for _, item := range latest.Items {
		shown[item.ID] = true
	}
	if !shown[newSame] || !shown[passed] || len(latest.Items) != 8 {
		t.Fatalf("restart shows %d items, newSame=%v passed=%v", len(latest.Items), shown[newSame], shown[passed])
	}
}

// killPoolConn terminates the pool's only idle backend from a second connection,
// so the next query runs on a connection the server already closed.
func (f *fixture) killPoolConn(t *testing.T) {
	t.Helper()
	c, err := f.pool.Acquire(f.ctx)
	if err != nil {
		t.Fatal(database.SafeError("acquire", err))
	}
	var pid int32
	if err := c.QueryRow(f.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(database.SafeError("pid", err))
	}
	c.Release()
	killer, err := pgx.Connect(f.ctx, f.url)
	if err != nil {
		t.Fatal(database.SafeError("connect", err))
	}
	defer killer.Close(context.Background())
	if _, err := killer.Exec(f.ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(database.SafeError("terminate", err))
	}
}

// TestQueriesOnTerminatedConnectionAreUnavailable: a connection closed by the
// server mid-life (admin shutdown, EOF) is a dependency failure for events and
// rate; a cancelled context and an ordinary statement error are not.
func TestQueriesOnTerminatedConnectionAreUnavailable(t *testing.T) {
	f := newFixture(t)
	f.killPoolConn(t)
	_, err := f.svc.Events(f.ctx, ws, withRange(nil))
	if !errors.Is(err, database.ErrUnavailable) {
		t.Fatalf("events err = %v", err)
	}
	f.killPoolConn(t)
	if _, err := f.svc.Rate(f.ctx, ws, nil); !errors.Is(err, database.ErrUnavailable) {
		t.Fatalf("rate err = %v", err)
	}
	cancelled, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.svc.Events(cancelled, ws, withRange(nil)); err == nil || errors.Is(err, database.ErrUnavailable) {
		t.Fatalf("cancelled err = %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `SELECT * FROM no_such_table`); err == nil || errors.Is(database.SafeStoreError("x", err), database.ErrUnavailable) {
		t.Fatalf("statement err = %v", err)
	}
}

// detailByID returns the request detail of one event, failing when it is absent.
func (f *fixture) detailByID(t *testing.T, id pgtype.UUID) Event {
	t.Helper()
	list, err := f.svc.Events(f.ctx, ws, withRange(url.Values{"limit": {"100"}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range list.Items {
		if item.ID == id.String() {
			return item
		}
	}
	t.Fatalf("event %s not in detail list", id.String())
	return Event{}
}

// TestEventGenerationMetrics: only a known final output above one token over a
// positive decode span yields a speed; every other shape stays unknown while an
// independent valid TTFT is still returned. Historic rows keep both NULL.
func TestEventGenerationMetrics(t *testing.T) {
	f := newFixture(t)
	at := asOf.Add(-10 * time.Second)
	standard := f.add(t, ev{at: at, total: n(120), out: n(100), gen: n(2000), ttft: n(8000)})
	singleChunk := f.add(t, ev{at: at, total: n(5), out: n(5), gen: n(0), ttft: n(300)})
	oneToken := f.add(t, ev{at: at, total: n(1), out: n(1), gen: n(1500), ttft: n(50)})
	estimated := f.add(t, ev{at: at, total: n(20), out: n(20), outputQuality: "estimated", gen: n(1000), ttft: n(70)})
	missingOut := f.add(t, ev{at: at, quality: "missing", outputQuality: "missing", gen: n(1000), ttft: n(90)})
	historic := f.add(t, ev{at: at, total: n(7)})
	ttftOnly := f.add(t, ev{at: at, total: n(50), out: n(50), ttft: n(120)})

	cases := []struct {
		name   string
		id     pgtype.UUID
		speed  *string
		hasTTF bool
		ttft   int64
	}{
		{"standard", standard, ptrString("49.500000"), true, 8000},
		{"single chunk", singleChunk, nil, true, 300},
		{"one token", oneToken, nil, true, 50},
		{"estimated output", estimated, nil, true, 70},
		{"missing output", missingOut, nil, true, 90},
		{"historic nulls", historic, nil, false, 0},
		{"valid ttft only", ttftOnly, nil, true, 120},
	}
	for _, c := range cases {
		item := f.detailByID(t, c.id)
		if (c.speed == nil) != (item.GenerationSpeedTPS == nil) || (c.speed != nil && *item.GenerationSpeedTPS != *c.speed) {
			t.Fatalf("%s speed = %v want %v", c.name, item.GenerationSpeedTPS, c.speed)
		}
		if (item.TTFTMS != nil) != c.hasTTF || (c.hasTTF && *item.TTFTMS != c.ttft) {
			t.Fatalf("%s ttft = %v want %d (present %v)", c.name, item.TTFTMS, c.ttft, c.hasTTF)
		}
	}
}

func ptrString(v string) *string { return &v }

// TestRateGenerationMetricsAverages: the two metrics average their own valid
// samples arithmetically, rounded once at the end. Two valid speeds with
// different token counts and decode spans separate the arithmetic mean (29.75)
// from the duration weighted (36.333333) and token weighted (45.585586) forms.
func TestRateGenerationMetricsAverages(t *testing.T) {
	f := newFixture(t)
	at := asOf.Add(-time.Second)
	// A: only a TTFT sample.
	f.add(t, ev{at: at, total: n(1), out: n(1), gen: n(1500), ttft: n(400)})
	// Two valid speed and TTFT rows, deliberately different token counts and spans.
	f.add(t, ev{at: at, total: n(120), out: n(100), gen: n(2000), ttft: n(1000)})
	f.add(t, ev{at: at, total: n(11), out: n(11), gen: n(1000), ttft: n(3000)})
	// C: nothing usable.
	f.add(t, ev{at: at})
	r := f.rate(t, ws, nil)
	if r.GenerationSpeed.Unit != "tokens/s" || r.GenerationSpeed.SampleCount != 2 || r.GenerationSpeed.Value == nil || *r.GenerationSpeed.Value != "29.750000" {
		t.Fatalf("speed = %+v", r.GenerationSpeed)
	}
	if r.TTFT.Unit != "ms" || r.TTFT.SampleCount != 3 || r.TTFT.Value == nil || *r.TTFT.Value != "1466.666667" {
		t.Fatalf("ttft = %+v", r.TTFT)
	}
	// The legacy tokens/min fields survive unchanged next to the new objects.
	if r.Unit != "tokens/min" || r.TotalTokens.Value == nil || *r.TotalTokens.Value != "132" {
		t.Fatalf("legacy = %+v %+v", r.Unit, r.TotalTokens)
	}
	// Averages over non terminating per request values round only once.
	f2 := newFixture(t)
	f2.add(t, ev{at: at, out: n(2), gen: n(3000), ttft: n(1)})
	f2.add(t, ev{at: at, out: n(2), gen: n(3000), ttft: n(2)})
	r2 := f2.rate(t, ws, nil)
	if r2.GenerationSpeed.Value == nil || *r2.GenerationSpeed.Value != "0.333333" {
		t.Fatalf("repeating speed = %+v", r2.GenerationSpeed)
	}
	if r2.TTFT.Value == nil || *r2.TTFT.Value != "1.500000" {
		t.Fatalf("repeating ttft = %+v", r2.TTFT)
	}
	// Rounding once at the end is observable. These two speeds are exactly
	// 0.9999996 and 1.0000011; their mean is 1.00000035 and rounds to 1.000000,
	// while averaging the already rounded 1.000000 and 1.000001 gives 1.0000005
	// and would round to 1.000001. The spans are large only so the ratio lands on
	// the boundary exactly, not because such timings are expected in traffic.
	f3 := newFixture(t)
	f3.add(t, ev{at: at, out: n(2500000), gen: n(2500000000)})
	f3.add(t, ev{at: at, out: n(10000012), gen: n(10000000000)})
	r3 := f3.rate(t, ws, nil)
	if r3.GenerationSpeed.SampleCount != 2 || r3.GenerationSpeed.Value == nil || *r3.GenerationSpeed.Value != "1.000000" {
		t.Fatalf("round once speed = %+v", r3.GenerationSpeed)
	}
}

// TestRateGenerationMetricsSamplesAndWindow: no event, unusable rows mixed with
// valid ones, the exact half-open boundary and a backfill outside the window
// each keep the sample counts honest, and received_at never stands in for
// occurred_at.
func TestRateGenerationMetricsSamplesAndWindow(t *testing.T) {
	f := newFixture(t)
	r := f.rate(t, ws, nil)
	if r.GenerationSpeed.Value != nil || r.GenerationSpeed.SampleCount != 0 || r.TTFT.Value != nil || r.TTFT.SampleCount != 0 {
		t.Fatalf("empty = %+v %+v", r.GenerationSpeed, r.TTFT)
	}
	start := asOf.Add(-time.Minute)
	// Left edge is inclusive, the right edge exclusive.
	f.add(t, ev{at: start, out: n(11), gen: n(1000), ttft: n(0)})
	f.add(t, ev{at: asOf, out: n(21), gen: n(1000), ttft: n(9999)})
	// Unusable rows only: single chunk, N=1 and estimated output.
	f.add(t, ev{at: start.Add(time.Second), out: n(9), gen: n(0), ttft: n(100)})
	f.add(t, ev{at: start.Add(time.Second), out: n(1), gen: n(500)})
	f.add(t, ev{at: start.Add(time.Second), out: n(9), outputQuality: "estimated", gen: n(500)})
	r = f.rate(t, ws, nil)
	if r.GenerationSpeed.SampleCount != 1 || r.GenerationSpeed.Value == nil || *r.GenerationSpeed.Value != "10.000000" {
		t.Fatalf("speed = %+v", r.GenerationSpeed)
	}
	if r.TTFT.SampleCount != 2 || r.TTFT.Value == nil || *r.TTFT.Value != "50.000000" {
		t.Fatalf("ttft = %+v", r.TTFT)
	}
	// A report whose occurred_at precedes the window must not move either average,
	// whichever way received_at points.
	f.add(t, ev{at: start.Add(-time.Microsecond), out: n(101), gen: n(1000), ttft: n(50000), received: asOf.Add(-time.Second)})
	f.add(t, ev{at: start.Add(-time.Hour), out: n(201), gen: n(1000), ttft: n(60000), received: asOf.Add(-2 * time.Second)})
	// A backfill inside the window counts at its original occurred_at.
	f.add(t, ev{at: start.Add(2 * time.Second), out: n(101), gen: n(1000), ttft: n(300), received: asOf.Add(-3 * time.Second)})
	r = f.rate(t, ws, nil)
	if r.GenerationSpeed.SampleCount != 2 || r.GenerationSpeed.Value == nil || *r.GenerationSpeed.Value != "55.000000" {
		t.Fatalf("backfill speed = %+v", r.GenerationSpeed)
	}
	if r.TTFT.SampleCount != 3 || r.TTFT.Value == nil || *r.TTFT.Value != "133.333333" {
		t.Fatalf("backfill ttft = %+v", r.TTFT)
	}
}

// TestRateGenerationMetricsAllSamplesInvalid: the window holds events, so the
// legacy counters stay non zero, yet none of them is a valid sample of either
// new metric. Both objects must then be value null with sample count zero,
// never a zero average, and the single LEFT JOIN row of an empty window must not
// be miscounted as an event.
func TestRateGenerationMetricsAllSamplesInvalid(t *testing.T) {
	f := newFixture(t)
	at := asOf.Add(-time.Second)
	// Single chunk: usable output size but a zero decode span.
	f.add(t, ev{at: at, total: n(9), out: n(9), gen: n(0)})
	// One output token: nothing to spread over the span.
	f.add(t, ev{at: at, total: n(1), out: n(1), gen: n(500)})
	// Estimated final output is not a known sample.
	f.add(t, ev{at: at, total: n(9), out: n(9), outputQuality: "estimated", gen: n(500)})
	// Missing output: no final count at all.
	f.add(t, ev{at: at, total: n(4), quality: "missing", outputQuality: "missing", gen: n(500)})
	// Known output but no decode timing at all.
	f.add(t, ev{at: at, total: n(2), out: n(2)})
	// No row carries a TTFT, so that sample set is empty for a second reason.
	r := f.rate(t, ws, nil)
	// The missing row still counts as an event but contributes no total.
	if r.TotalTokens.EventCount != 5 || r.TotalTokens.Value == nil || *r.TotalTokens.Value != "21" {
		t.Fatalf("legacy = %+v", r.TotalTokens)
	}
	if r.GenerationSpeed.Unit != "tokens/s" || r.GenerationSpeed.Value != nil || r.GenerationSpeed.SampleCount != 0 {
		t.Fatalf("speed = %+v", r.GenerationSpeed)
	}
	if r.TTFT.Unit != "ms" || r.TTFT.Value != nil || r.TTFT.SampleCount != 0 {
		t.Fatalf("ttft = %+v", r.TTFT)
	}
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"generation_speed":{"unit":"tokens/s","value":null,"sample_count":0}`, `"ttft":{"unit":"ms","value":null,"sample_count":0}`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("body misses %s: %s", want, body)
		}
	}
}

// TestRateGenerationMetricsFilters: provider, model and project narrowing apply
// the same way to both new objects and to the legacy total.
func TestRateGenerationMetricsFilters(t *testing.T) {
	f := newFixture(t)
	at := asOf.Add(-time.Second)
	f.add(t, ev{at: at, project: projA, provider: "p1", model: "m1", total: n(10), out: n(11), gen: n(1000), ttft: n(100)})
	f.add(t, ev{at: at, project: projB, provider: "p2", model: "m1", total: n(20), out: n(21), gen: n(1000), ttft: n(300)})
	f.add(t, ev{at: at, provider: "p1", model: "m2", total: n(30), out: n(31), gen: n(1000)})
	cases := []struct {
		q          url.Values
		speedValue string
		speedCount int64
		ttftValue  string
		ttftCount  int64
	}{
		{nil, "20.000000", 3, "200.000000", 2},
		{url.Values{"provider": {"p1"}}, "20.000000", 2, "100.000000", 1},
		{url.Values{"model": {"m1"}}, "15.000000", 2, "200.000000", 2},
		{url.Values{"project": {"unattributed"}}, "30.000000", 1, "", 0},
		{url.Values{"project": {projA.String()}}, "10.000000", 1, "100.000000", 1},
		{url.Values{"project": {projB.String()}, "provider": {"p1"}}, "", 0, "", 0},
	}
	for _, c := range cases {
		r := f.rate(t, ws, c.q)
		if r.GenerationSpeed.SampleCount != c.speedCount || r.TTFT.SampleCount != c.ttftCount {
			t.Fatalf("%v counts = %+v %+v", c.q, r.GenerationSpeed, r.TTFT)
		}
		if c.speedValue == "" {
			if r.GenerationSpeed.Value != nil {
				t.Fatalf("%v speed = %v want null", c.q, *r.GenerationSpeed.Value)
			}
		} else if r.GenerationSpeed.Value == nil || *r.GenerationSpeed.Value != c.speedValue {
			t.Fatalf("%v speed = %v want %s", c.q, r.GenerationSpeed.Value, c.speedValue)
		}
		if c.ttftValue == "" {
			if r.TTFT.Value != nil {
				t.Fatalf("%v ttft = %v want null", c.q, *r.TTFT.Value)
			}
		} else if r.TTFT.Value == nil || *r.TTFT.Value != c.ttftValue {
			t.Fatalf("%v ttft = %v want %s", c.q, r.TTFT.Value, c.ttftValue)
		}
	}
}
