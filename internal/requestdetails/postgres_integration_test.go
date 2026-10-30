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
	f := &fixture{store: store, svc: New(store), pool: pool, ctx: ctx}
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
	f.exec(t, `INSERT INTO usage_events (id,workspace_id,source_id,source_event_id,project_id,provider,model,occurred_at,received_at,normalization_version,payload_hash,
input_tokens,input_tokens_quality,output_tokens,output_tokens_quality,total_tokens,total_tokens_quality,
cache_read_tokens_quality,cache_write_tokens_quality,reasoning_tokens_quality)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'v1','\x00',NULL,'missing',NULL,'missing',$10,$11,'missing','missing','missing')`,
		e.id, e.ws, srcFor(e.ws), fmt.Sprint("e", nextID), project, e.provider, e.model, e.at, e.received, e.total, e.quality)
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
