//go:build integration

package requestdetails

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
	// Optional speed inputs; an event without outQuality has a missing output.
	out        *int64
	outQuality string
	duration   *int64
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
	if e.outQuality == "" {
		e.outQuality = "missing"
	}
	f.exec(t, `INSERT INTO usage_events (id,workspace_id,source_id,source_event_id,project_id,provider,model,occurred_at,received_at,normalization_version,payload_hash,
input_tokens,input_tokens_quality,output_tokens,output_tokens_quality,total_tokens,total_tokens_quality,
cache_read_tokens_quality,cache_write_tokens_quality,reasoning_tokens_quality,duration_ms)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'v1','\x00',NULL,'missing',$12,$13,$10,$11,'missing','missing','missing',$14)`,
		e.id, e.ws, srcFor(e.ws), fmt.Sprint("e", nextID), project, e.provider, e.model, e.at, e.received, e.total, e.quality, e.out, e.outQuality, e.duration)
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

func (f *fixture) speed(t *testing.T, w pgtype.UUID, q url.Values) *SpeedResponse {
	t.Helper()
	r, err := f.svc.SpeedAt(f.ctx, w, q, asOf)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func sp(out, dur int64, quality string) ev {
	return ev{out: n(out), outQuality: quality, duration: n(dur), total: n(out), quality: "known"}
}

// Only events with a usable output and a positive duration are measured, the rolling
// window is exactly [as_of-60s, as_of), and the series groups by UTC minute.
func TestSpeedWindowMeasuredEventsAndSeries(t *testing.T) {
	f := newFixture(t)
	at := func(d time.Duration, e ev) ev { e.at = asOf.Add(d); return e }
	// In the 60 second window: 100 tokens/2s known, 50 tokens/1s estimated.
	f.add(t, at(-10*time.Second, sp(100, 2000, "known")))
	f.add(t, at(-20*time.Second, sp(50, 1000, "estimated")))
	// In the window but not measurable: unknown duration, zero duration, missing output.
	f.add(t, at(-30*time.Second, ev{out: n(500), outQuality: "known", total: n(500)}))
	f.add(t, at(-31*time.Second, ev{out: n(500), outQuality: "known", duration: n(0), total: n(500)}))
	f.add(t, at(-32*time.Second, ev{duration: n(900), total: n(900)}))
	// Window boundaries: start is included, as_of and just before start are not.
	f.add(t, at(-time.Minute, sp(7, 700, "known")))
	f.add(t, at(-time.Minute-time.Microsecond, sp(1000, 1000, "known")))
	f.add(t, at(0, sp(1000, 1000, "known")))
	// The first series minute is whole: 11:00:10 is before as_of-60min (11:00:30) but in the 11:00 bucket.
	f.add(t, at(-60*time.Minute-20*time.Second, sp(40, 400, "known")))
	// Earlier in the hour, and outside the hour.
	f.add(t, at(-10*time.Minute, sp(300, 3000, "known")))
	f.add(t, at(-61*time.Minute, sp(9000, 9000, "known")))
	// Another workspace must not leak in.
	f.add(t, ev{ws: otherWS, at: asOf.Add(-5 * time.Second), out: n(9999), outQuality: "known", duration: n(1), total: n(9999)})

	r := f.speed(t, ws, nil)
	c := r.Current
	if c.OutputTokens == nil || *c.OutputTokens != "157" || c.DurationMS == nil || *c.DurationMS != "3700" {
		t.Fatalf("current sums = %+v", c)
	}
	// 6 events completed in the window (the one exactly at as_of-60s included, the one at as_of
	// excluded); 3 are measurable, 1 of them estimated.
	if c.EventCount != 6 || c.MeasuredCount != 3 || c.EstimatedCount != 1 {
		t.Fatalf("current counts = %+v", c)
	}
	if r.WindowSeconds != 60 || r.Unit != "tokens/s" || r.AsOf != formatTime(asOf) {
		t.Fatalf("shape = %+v", r)
	}
	if r.SeriesStart != formatTime(asOf.Add(-60*time.Minute).Truncate(time.Minute)) {
		t.Fatalf("series start = %s", r.SeriesStart)
	}
	// The series groups by UTC calendar minute over the last 60 minutes, so unlike the rolling
	// window it keeps the event at as_of-60s-1µs (11:59:29.999999): 11:59 = 7 + 1000 tokens.
	// 12:00 holds the two measurable events at -10s and -20s; the event at as_of is excluded.
	got := map[string]SpeedMinute{}
	for _, p := range r.Points {
		got[p.BucketStart] = p
	}
	m := func(h, min int) string { return formatTime(time.Date(2026, 10, 28, h, min, 0, 0, time.UTC)) }
	val := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	for _, want := range []struct {
		at             string
		out, dur       string
		measured, estd int64
	}{
		{m(11, 0), "40", "400", 1, 0},
		{m(11, 50), "300", "3000", 1, 0},
		{m(11, 59), "1007", "1700", 2, 0},
		{m(12, 0), "150", "3000", 2, 1},
	} {
		p, ok := got[want.at]
		if !ok || val(p.OutputTokens) != want.out || val(p.DurationMS) != want.dur || p.MeasuredCount != want.measured || p.EstimatedCount != want.estd {
			t.Fatalf("%s = out %s dur %s measured %d estimated %d (present=%v), want %s/%s/%d/%d",
				want.at, val(p.OutputTokens), val(p.DurationMS), p.MeasuredCount, p.EstimatedCount, ok, want.out, want.dur, want.measured, want.estd)
		}
	}
	if len(got) != 4 {
		t.Fatalf("expected exactly the 11:00, 11:50, 11:59 and 12:00 minutes, got %d: %+v", len(got), got)
	}
	// Minutes with no measurable event are absent, never zero rows.
	for _, p := range r.Points {
		if p.MeasuredCount == 0 {
			t.Fatalf("empty minute returned: %+v", p)
		}
	}
}

func TestSpeedEmptyAndFilters(t *testing.T) {
	f := newFixture(t)
	r := f.speed(t, ws, nil)
	if r.Current.OutputTokens != nil || r.Current.DurationMS != nil || r.Current.EventCount != 0 || len(r.Points) != 0 {
		t.Fatalf("empty = %+v", r)
	}
	at := asOf.Add(-5 * time.Second)
	a := sp(100, 1000, "known")
	a.at, a.model, a.provider, a.project = at, "m1", "p1", projA
	b := sp(200, 4000, "known")
	b.at, b.model, b.provider = at.Add(-time.Second), "m2", "p2"
	f.add(t, a)
	f.add(t, b)
	cases := []struct {
		q        url.Values
		out, dur string
	}{
		{nil, "300", "5000"},
		{url.Values{"model": {"m1"}}, "100", "1000"},
		{url.Values{"provider": {"p2"}}, "200", "4000"},
		{url.Values{"project": {"unattributed"}}, "200", "4000"},
		{url.Values{"project": {projA.String()}}, "100", "1000"},
	}
	for _, c := range cases {
		got := f.speed(t, ws, c.q).Current
		if got.OutputTokens == nil || *got.OutputTokens != c.out || *got.DurationMS != c.dur {
			t.Fatalf("%v: %+v, want %s/%s", c.q, got, c.out, c.dur)
		}
	}
	if _, err := f.svc.SpeedAt(f.ctx, ws, url.Values{"project": {projX.String()}}, asOf); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("foreign project err = %v", err)
	}
	if _, err := f.svc.SpeedAt(f.ctx, ws, url.Values{"limit": {"5"}}, asOf); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unknown parameter err = %v", err)
	}
}
