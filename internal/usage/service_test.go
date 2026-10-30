package usage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dundunHa/TokenPivot/internal/aggregation"
	"github.com/jackc/pgx/v5/pgtype"
)

var testWorkspace = pgtype.UUID{Bytes: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, Valid: true}

const testNow = "2026-10-28T12:34:56.123456789Z"

func at(value string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("tz database unavailable for %s: %v", name, err)
	}
	return loc
}

// fakeStore records calls and returns canned rows.
type fakeStore struct {
	aggregateCalls int
	rankingsCalls  int
	aggregate      AggregateParams
	rankings       RankingsParams
	aggregateRows  AggregateRows
	rankingsRows   RankingsRows
	err            error
	optionsCalls   int
	options        OptionsParams
	optionsRows    OptionsRows
}

func (f *fakeStore) Options(ctx context.Context, p OptionsParams) (OptionsRows, error) {
	f.optionsCalls++
	f.options = p
	return f.optionsRows, f.err
}

func (f *fakeStore) Aggregate(ctx context.Context, p AggregateParams) (AggregateRows, error) {
	f.aggregateCalls++
	f.aggregate = p
	return f.aggregateRows, f.err
}

func (f *fakeStore) Rankings(ctx context.Context, p RankingsParams) (RankingsRows, error) {
	f.rankingsCalls++
	f.rankings = p
	return f.rankingsRows, f.err
}

// newTestService returns a service with a counting clock fixed at testNow.
func newTestService(store Store) (*Service, *int, *time.Time) {
	now := at(testNow)
	calls := 0
	s := New(store)
	s.now = func() time.Time { calls++; return now }
	return s, &calls, &now
}

func raw(request, sum, known, estimated, missing string) RawStats {
	m := RawMetric{Sum: sum, Known: known, Estimated: estimated, Missing: missing}
	return RawStats{RequestCount: request, Metrics: [metricCount]RawMetric{m, m, m, m, m, m}}
}

func aggValues(extra ...string) url.Values {
	v := url.Values{"start": {"2026-10-28T00:00:00Z"}, "end": {"2026-10-28T03:00:00Z"}, "timezone": {"UTC"}, "granularity": {"hour"}}
	for i := 0; i+1 < len(extra); i += 2 {
		if extra[i+1] == "\x00delete" {
			v.Del(extra[i])
			continue
		}
		v.Add(extra[i], extra[i+1])
	}
	return v
}

func set(v url.Values, key string, values ...string) url.Values {
	v[key] = values
	return v
}

func TestAggregateRejectsInvalidArgumentsWithoutQuery(t *testing.T) {
	many := make([]string, 51)
	for i := range many {
		many[i] = "p" + string(rune('a'+i%26)) + strings.Repeat("x", i)
	}
	tests := map[string]url.Values{
		"unknown parameter":      aggValues("extra", "1"),
		"repeated start":         set(aggValues(), "start", "2026-10-28T00:00:00Z", "2026-10-28T00:01:00Z"),
		"repeated timezone":      set(aggValues(), "timezone", "UTC", "UTC"),
		"missing start":          aggValues("start", "\x00delete"),
		"missing timezone":       aggValues("timezone", "\x00delete"),
		"missing granularity":    aggValues("granularity", "\x00delete"),
		"empty end":              set(aggValues(), "end", ""),
		"empty provider":         aggValues("provider", ""),
		"empty model":            aggValues("model", ""),
		"51 distinct providers":  set(aggValues(), "provider", many...),
		"51 distinct models":     set(aggValues(), "model", many...),
		"bad project uuid":       aggValues("project", "not-a-uuid"),
		"uppercase unassigned":   aggValues("project", "Unassigned"),
		"empty timezone":         set(aggValues(), "timezone", ""),
		"Local timezone":         set(aggValues(), "timezone", "Local"),
		"fixed offset timezone":  set(aggValues(), "timezone", "+08:00"),
		"unknown timezone":       set(aggValues(), "timezone", "Mars/Olympus"),
		"bad granularity":        set(aggValues(), "granularity", "week"),
		"start with seconds":     set(aggValues(), "start", "2026-10-28T00:00:01Z"),
		"start with fraction":    set(aggValues(), "start", "2026-10-28T00:00:00.5Z"),
		"end with seconds":       set(aggValues(), "end", "2026-10-28T03:00:30Z"),
		"start without offset":   set(aggValues(), "start", "2026-10-28T00:00:00"),
		"start equals end":       set(aggValues(), "end", "2026-10-28T00:00:00Z"),
		"start after end":        set(aggValues(), "end", "2026-10-27T00:00:00Z"),
		"future end":             set(aggValues(), "end", "2026-10-28T12:36:00Z"),
		"invalid utf8 value":     aggValues("provider", "bad\xff"),
		"NUL in value":           aggValues("model", "a\x00b"),
		"invalid utf8 parameter": aggValues("bad\xff", "1"),
		"cursor on aggregate":    aggValues("cursor", "abc"),
		"group_by on aggregate":  aggValues("group_by", "model"),
	}
	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{}
			s, _, _ := newTestService(store)
			_, err := s.Aggregate(context.Background(), testWorkspace, values)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v, want ErrInvalidArgument", err)
			}
			if store.aggregateCalls != 0 {
				t.Fatal("a rejected request must not reach the store")
			}
			if strings.Contains(err.Error(), "bad") || strings.Contains(err.Error(), "Mars") {
				t.Fatal("error repeats a request value")
			}
		})
	}
}

func TestFilterIsDeduplicatedAndPassedToStore(t *testing.T) {
	store := &fakeStore{}
	s, _, _ := newTestService(store)
	project := "018f3c5e-1a2b-7c3d-8e9f-0a1b2c3d4e5f"
	values := aggValues("provider", "b", "provider", "a", "provider", "b", "model", "m1", "project", project, "project", project, "project", "unassigned")
	if _, err := s.Aggregate(context.Background(), testWorkspace, values); err != nil {
		t.Fatal(err)
	}
	got := store.aggregate
	if got.WorkspaceID != testWorkspace {
		t.Fatal("store was not given the authorised workspace")
	}
	if strings.Join(got.Filter.Providers, ",") != "a,b" || strings.Join(got.Filter.Models, ",") != "m1" || !got.Filter.Unassigned || len(got.Filter.Projects) != 1 || got.Filter.Projects[0].String() != project {
		t.Fatalf("filter = %+v", got.Filter)
	}
	fifty := make([]string, 0, 100)
	for i := 0; i < 50; i++ {
		fifty = append(fifty, "v"+strings.Repeat("y", i), "v"+strings.Repeat("y", i))
	}
	if _, err := s.Aggregate(context.Background(), testWorkspace, set(aggValues(), "provider", fifty...)); err != nil {
		t.Fatalf("50 distinct values (with duplicates) must be accepted: %v", err)
	}
}

func TestRetentionBoundary(t *testing.T) {
	cutoff := at("2025-09-28T12:34:00Z") // now 2026-10-28T12:34:56
	s, _, _ := newTestService(&fakeStore{})
	ok := set(aggValues(), "start", cutoff.Format(time.RFC3339))
	if _, err := s.Aggregate(context.Background(), testWorkspace, set(ok, "granularity", "day")); err != nil {
		t.Fatalf("start == cutoff must be allowed: %v", err)
	}
	store := &fakeStore{}
	s, _, _ = newTestService(store)
	early := set(aggValues(), "start", cutoff.Add(-time.Minute).Format(time.RFC3339))
	_, err := s.Aggregate(context.Background(), testWorkspace, set(early, "granularity", "day"))
	var outside *OutsideRetentionError
	if !errors.As(err, &outside) || !outside.Cutoff.Equal(cutoff) {
		t.Fatalf("error = %v, want OutsideRetentionError with cutoff %s", err, cutoff)
	}
	if store.aggregateCalls != 0 {
		t.Fatal("retention violation must not reach the store")
	}
}

func TestOmittedEndSamplesClockOnceAndMarksCurrentBucketsPartial(t *testing.T) {
	for _, g := range []string{"minute", "hour", "day"} {
		t.Run(g, func(t *testing.T) {
			store := &fakeStore{}
			s, calls, _ := newTestService(store)
			values := set(set(aggValues(), "granularity", g), "start", "2026-10-28T12:00:00Z")
			values.Del("end")
			response, err := s.Aggregate(context.Background(), testWorkspace, values)
			if err != nil {
				t.Fatal(err)
			}
			if *calls != 1 {
				t.Fatalf("clock sampled %d times, want 1", *calls)
			}
			if response.Range.End != response.Range.AsOf || response.Range.End != "2026-10-28T12:34:56.123456Z" {
				t.Fatalf("range = %+v", response.Range)
			}
			if !store.aggregate.End.Equal(at("2026-10-28T12:34:56.123456Z")) {
				t.Fatalf("SQL upper bound = %s", store.aggregate.End)
			}
			last := store.aggregate.Buckets[len(store.aggregate.Buckets)-1]
			if !last.Partial {
				t.Fatalf("current %s bucket must be partial", g)
			}
		})
	}
}

func TestExplicitEndKeepsAsOfAtSamplingInstant(t *testing.T) {
	s, calls, _ := newTestService(&fakeStore{})
	response, err := s.Aggregate(context.Background(), testWorkspace, aggValues())
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || response.Range.AsOf != "2026-10-28T12:34:56.123456Z" || response.Range.End != "2026-10-28T03:00:00Z" {
		t.Fatalf("range = %+v calls=%d", response.Range, *calls)
	}
	if response.Range.RetentionCutoff != "2025-09-28T12:34:00Z" {
		t.Fatalf("retention_cutoff = %s", response.Range.RetentionCutoff)
	}
}

func TestMinuteTrendIsTimezoneIndependent(t *testing.T) {
	var starts [2][]time.Time
	for i, zone := range []string{"UTC", "Australia/Lord_Howe"} {
		store := &fakeStore{}
		s, _, _ := newTestService(store)
		values := set(set(aggValues(), "granularity", "minute"), "timezone", zone)
		values = set(values, "end", "2026-10-28T00:05:00Z")
		if _, err := s.Aggregate(context.Background(), testWorkspace, values); err != nil {
			t.Fatal(err)
		}
		for _, b := range store.aggregate.Buckets {
			starts[i] = append(starts[i], b.Start)
		}
	}
	if len(starts[0]) != 5 || len(starts[0]) != len(starts[1]) {
		t.Fatalf("bucket counts %d/%d", len(starts[0]), len(starts[1]))
	}
	for i := range starts[0] {
		if !starts[0][i].Equal(starts[1][i]) {
			t.Fatal("minute buckets must not depend on the display time zone")
		}
	}
}

func TestStartAcceptsOffsetAndOnlyChangesInstantRepresentation(t *testing.T) {
	store := &fakeStore{}
	s, _, _ := newTestService(store)
	values := set(aggValues(), "start", "2026-10-28T08:00:00+08:00")
	if _, err := s.Aggregate(context.Background(), testWorkspace, values); err != nil {
		t.Fatal(err)
	}
	if !store.aggregate.Start.Equal(at("2026-10-28T00:00:00Z")) {
		t.Fatalf("start = %s", store.aggregate.Start)
	}
}

type want struct {
	start, end string
	partial    bool
}

func checkBuckets(t *testing.T, got []Bucket, expected []want) {
	t.Helper()
	if len(got) != len(expected) {
		for _, b := range got {
			t.Logf("got %s -> %s partial=%v", b.Start.Format(time.RFC3339), b.End.Format(time.RFC3339), b.Partial)
		}
		t.Fatalf("bucket count = %d, want %d", len(got), len(expected))
	}
	for i, e := range expected {
		if !got[i].Start.Equal(at(e.start)) || !got[i].End.Equal(at(e.end)) || got[i].Partial != e.partial {
			t.Fatalf("bucket %d = %s -> %s partial=%v, want %+v", i, got[i].Start.Format(time.RFC3339), got[i].End.Format(time.RFC3339), got[i].Partial, e)
		}
	}
}

func TestHourBucketsNewYorkSpringForwardSkipsHour(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	// 2026-03-08 02:00 EST jumps to 03:00 EDT at 07:00Z; local 02:00 never exists.
	got, tooMany, err := enumerateBuckets(at("2026-03-08T05:00:00Z"), at("2026-03-08T09:00:00Z"), Hour, loc, MaxPoints, nil)
	if err != nil || tooMany {
		t.Fatal(err, tooMany)
	}
	checkBuckets(t, got, []want{
		{"2026-03-08T05:00:00Z", "2026-03-08T06:00:00Z", false}, // 00:00 EST
		{"2026-03-08T06:00:00Z", "2026-03-08T07:00:00Z", false}, // 01:00 EST
		{"2026-03-08T07:00:00Z", "2026-03-08T08:00:00Z", false}, // 03:00 EDT, no empty 02:00 bucket
		{"2026-03-08T08:00:00Z", "2026-03-08T09:00:00Z", false}, // 04:00 EDT
	})
}

func TestHourBucketsNewYorkFallBackKeepsRepeatedHourApart(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	// 2026-11-01 02:00 EDT falls back to 01:00 EST at 06:00Z: local 01:00 happens twice.
	got, _, err := enumerateBuckets(at("2026-11-01T04:00:00Z"), at("2026-11-01T08:00:00Z"), Hour, loc, MaxPoints, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkBuckets(t, got, []want{
		{"2026-11-01T04:00:00Z", "2026-11-01T05:00:00Z", false}, // 00:00 EDT
		{"2026-11-01T05:00:00Z", "2026-11-01T06:00:00Z", false}, // 01:00 EDT
		{"2026-11-01T06:00:00Z", "2026-11-01T07:00:00Z", false}, // 01:00 EST, a separate bucket
		{"2026-11-01T07:00:00Z", "2026-11-01T08:00:00Z", false}, // 02:00 EST
	})
}

func TestDayBucketsNewYorkAre23And25Hours(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	spring, _, err := enumerateBuckets(at("2026-03-08T05:00:00Z"), at("2026-03-09T04:00:00Z"), Day, loc, MaxPoints, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkBuckets(t, spring, []want{{"2026-03-08T05:00:00Z", "2026-03-09T04:00:00Z", false}}) // 23 hours
	fall, _, err := enumerateBuckets(at("2026-11-01T04:00:00Z"), at("2026-11-02T05:00:00Z"), Day, loc, MaxPoints, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkBuckets(t, fall, []want{{"2026-11-01T04:00:00Z", "2026-11-02T05:00:00Z", false}}) // 25 hours
}

func TestHourBucketsLordHoweHalfHourShifts(t *testing.T) {
	loc := mustLoc(t, "Australia/Lord_Howe")
	// Start of DST 2026-10-04 02:00 +10:30 -> 02:30 +11:00 at 2026-10-03T15:30Z.
	spring, _, err := enumerateBuckets(at("2026-10-03T14:30:00Z"), at("2026-10-03T17:00:00Z"), Hour, loc, MaxPoints, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkBuckets(t, spring, []want{
		{"2026-10-03T14:30:00Z", "2026-10-03T15:30:00Z", false}, // 01:00 +10:30
		{"2026-10-03T15:30:00Z", "2026-10-03T16:00:00Z", false}, // 02:30..03:00 +11:00, a 30 minute hour
		{"2026-10-03T16:00:00Z", "2026-10-03T17:00:00Z", false}, // 03:00 +11:00
	})
	// End of DST 2026-04-05 02:00 +11:00 -> 01:30 +10:30 at 2026-04-04T15:00Z.
	fall, _, err := enumerateBuckets(at("2026-04-04T14:00:00Z"), at("2026-04-04T16:30:00Z"), Hour, loc, MaxPoints, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkBuckets(t, fall, []want{
		{"2026-04-04T14:00:00Z", "2026-04-04T15:00:00Z", false}, // 01:00 +11:00
		{"2026-04-04T15:00:00Z", "2026-04-04T15:30:00Z", false}, // 01:30..02:00 +10:30, offset change splits the local hour
		{"2026-04-04T15:30:00Z", "2026-04-04T16:30:00Z", false}, // 02:00 +10:30
	})
	for _, b := range append(spring, fall...) {
		if b.Start.Minute()%30 != 0 {
			t.Fatalf("unexpected boundary %s", b.Start)
		}
	}
}

func TestPartialEdgesAndTotalsIndependentOfGranularity(t *testing.T) {
	loc := mustLoc(t, "Australia/Lord_Howe")
	start, end := at("2026-10-03T14:45:00Z"), at("2026-10-03T16:15:00Z")
	hour, _, err := enumerateBuckets(start, end, Hour, loc, MaxPoints, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkBuckets(t, hour, []want{
		{"2026-10-03T14:30:00Z", "2026-10-03T15:30:00Z", true},
		{"2026-10-03T15:30:00Z", "2026-10-03T16:00:00Z", false},
		{"2026-10-03T16:00:00Z", "2026-10-03T17:00:00Z", true},
	})
	// Every granularity covers exactly [start,end): the first starts no later than
	// start, the last ends no earlier than end, and neighbours touch.
	for _, g := range []Granularity{Minute, Hour, Day} {
		buckets, _, err := enumerateBuckets(start, end, g, loc, MaxPoints, nil)
		if err != nil {
			t.Fatal(err)
		}
		if buckets[0].Start.After(start) || buckets[len(buckets)-1].End.Before(end) {
			t.Fatalf("%s buckets do not cover the range", g)
		}
		for i := 1; i < len(buckets); i++ {
			if !buckets[i-1].End.Equal(buckets[i].Start) {
				t.Fatalf("%s buckets are not contiguous", g)
			}
		}
	}
}

func TestExactlyMaxPointsAllowedAndNextBucketStopsEnumeration(t *testing.T) {
	utc := time.UTC
	start := at("2026-10-01T00:00:00Z")
	got, tooMany, err := enumerateBuckets(start, start.Add(1000*time.Minute), Minute, utc, MaxPoints, nil)
	if err != nil || tooMany || len(got) != 1000 {
		t.Fatalf("1000 buckets: len=%d tooMany=%v err=%v", len(got), tooMany, err)
	}
	if _, tooMany, _ = enumerateBuckets(start, start.Add(1001*time.Minute), Minute, utc, MaxPoints, nil); !tooMany {
		t.Fatal("1001 buckets must be too many")
	}
	if _, tooMany, _ = enumerateBuckets(start, start.Add(1000*time.Hour), Hour, utc, MaxPoints, nil); tooMany {
		t.Fatal("1000 hour buckets must be allowed")
	}
	if _, tooMany, _ = enumerateBuckets(start, start.Add(1001*time.Hour), Hour, utc, MaxPoints, nil); !tooMany {
		t.Fatal("1001 hour buckets must be too many")
	}
}

func TestEnumerationStopsEarlyOnVeryLongRanges(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	start := at("2000-01-01T00:00:00Z")
	end := at("2026-01-01T00:00:00Z") // about 227,000 hour buckets
	for _, g := range []Granularity{Hour, Day} {
		probes := 0
		_, tooMany, err := enumerateBuckets(start, end, g, loc, MaxPoints, &probes)
		if err != nil || !tooMany {
			t.Fatalf("%s: tooMany=%v err=%v", g, tooMany, err)
		}
		// A full scan would need millions of key probes; early exit stays near
		// 1001 buckets times the per-bucket search.
		if probes > 120000 {
			t.Fatalf("%s: %d probes, enumeration did not stop at the 1001st bucket", g, probes)
		}
	}
}

func TestTooManyPointsSuggestsFinestCoarserGranularity(t *testing.T) {
	// Real limits: 1001 minutes -> hour fits.
	store := &fakeStore{}
	s, _, _ := newTestService(store)
	values := set(set(set(aggValues(), "granularity", "minute"), "start", "2026-10-27T00:00:00Z"), "end", "2026-10-27T16:41:00Z") // 1001 minutes
	_, err := s.Aggregate(context.Background(), testWorkspace, values)
	assertSuggestion(t, err, MaxPoints, Hour)
	// 1001 hours -> day fits.
	values = set(set(aggValues(), "start", "2026-09-01T00:00:00Z"), "end", "2026-10-12T17:00:00Z")
	_, err = s.Aggregate(context.Background(), testWorkspace, values)
	assertSuggestion(t, err, MaxPoints, Day)
	if store.aggregateCalls != 0 {
		t.Fatal("too many points must not reach the store")
	}
	// Controllable small limit: only day fits, and nothing fits.
	s.maxPoints = 30
	values = set(set(aggValues(), "start", "2026-10-01T00:00:00Z"), "end", "2026-10-28T00:00:00Z") // 648 hours, 27 days
	_, err = s.Aggregate(context.Background(), testWorkspace, values)
	assertSuggestion(t, err, 30, Day)
	s.maxPoints = 5
	_, err = s.Aggregate(context.Background(), testWorkspace, values)
	assertSuggestion(t, err, 5, "")
	// A day request is never suggested a finer granularity.
	_, err = s.Aggregate(context.Background(), testWorkspace, set(values, "granularity", "day"))
	assertSuggestion(t, err, 5, "")
}

func assertSuggestion(t *testing.T, err error, max int, suggested Granularity) {
	t.Helper()
	var tooMany *TooManyPointsError
	if !errors.As(err, &tooMany) || tooMany.MaxPoints != max {
		t.Fatalf("error = %v, want TooManyPointsError(%d)", err, max)
	}
	if suggested == "" {
		if tooMany.Suggested != nil {
			t.Fatalf("suggested = %s, want nil", *tooMany.Suggested)
		}
		return
	}
	if tooMany.Suggested == nil || *tooMany.Suggested != suggested {
		t.Fatalf("suggested = %v, want %s", tooMany.Suggested, suggested)
	}
}

func TestAggregateMapsRowsToPrecomputedBoundariesWithoutZeroFill(t *testing.T) {
	huge := "123456789012345678901234567890" // larger than int64
	store := &fakeStore{aggregateRows: AggregateRows{
		Overall: &RawStats{},
		Points:  []RawPoint{{Index: 2, Stats: raw("3", huge, "1", "1", "1")}, {Index: 0, Stats: raw("2", "7", "2", "0", "0")}},
	}}
	overall := raw("5", "9007199254740993", "3", "1", "1")
	store.aggregateRows.Overall = &overall
	s, _, _ := newTestService(store)
	response, err := s.Aggregate(context.Background(), testWorkspace, aggValues())
	if err != nil {
		t.Fatal(err)
	}
	if len(store.aggregate.Buckets) != 3 || len(response.Points) != 2 {
		t.Fatalf("buckets=%d points=%d", len(store.aggregate.Buckets), len(response.Points))
	}
	if response.Points[0].BucketStart != "2026-10-28T00:00:00Z" || response.Points[1].BucketStart != "2026-10-28T02:00:00Z" || response.Points[1].BucketEnd != "2026-10-28T03:00:00Z" {
		t.Fatalf("points = %+v", response.Points)
	}
	if *response.Points[1].Stats.TotalTokens.Value != huge || response.Points[1].Stats.RequestCount != "3" {
		t.Fatal("beyond-int64 sum was altered")
	}
	if *response.Totals.InputTokens.Value != "9007199254740993" {
		t.Fatal("beyond 2^53-1 sum was altered")
	}
}

func TestAggregateEmptyResultIsZeroedWithEmptyPoints(t *testing.T) {
	s, _, _ := newTestService(&fakeStore{})
	response, err := s.Aggregate(context.Background(), testWorkspace, aggValues())
	if err != nil {
		t.Fatal(err)
	}
	if response.Points == nil || len(response.Points) != 0 {
		t.Fatal("points must be an empty array, not null")
	}
	totals := response.Totals
	if totals.RequestCount != "0" || totals.TotalTokens.Value != nil || totals.TotalTokens.KnownCount != "0" || totals.ReasoningTokens.MissingCount != "0" {
		t.Fatalf("totals = %+v", totals)
	}
}

func TestMetricQualityMapping(t *testing.T) {
	cases := []struct {
		name                     string
		sum, known, est, missing string
		wantValue                *string
	}{
		{"known zero is a real zero", "0", "2", "0", "0", ptr("0")},
		{"estimated zero is a real zero", "0", "0", "1", "0", ptr("0")},
		{"all missing has no value", "0", "0", "0", "4", nil},
		{"partial missing keeps the sum", "30", "1", "0", "1", ptr("30")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stats, err := convertStats(raw("1", tc.sum, tc.known, tc.est, tc.missing))
			if err != nil {
				t.Fatal(err)
			}
			got := stats.OutputTokens
			if (got.Value == nil) != (tc.wantValue == nil) || (got.Value != nil && *got.Value != *tc.wantValue) {
				t.Fatalf("value = %v, want %v", got.Value, tc.wantValue)
			}
			if got.KnownCount != tc.known || got.EstimatedCount != tc.est || got.MissingCount != tc.missing {
				t.Fatalf("counts = %+v", got)
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestConvertStatsRejectsNonCanonicalStoredText(t *testing.T) {
	for _, bad := range []string{"", "01", "-1", "1.5", "1e3", " 1", "+1"} {
		if _, err := convertStats(raw("1", bad, "1", "0", "0")); err == nil {
			t.Fatalf("%q accepted", bad)
		}
		if _, err := convertStats(raw(bad, "1", "1", "0", "0")); err == nil {
			t.Fatalf("request count %q accepted", bad)
		}
	}
}

func TestSubsetMetricsDoNotChangeTotal(t *testing.T) {
	stats := RawStats{RequestCount: "1", Metrics: [metricCount]RawMetric{
		{"100", "1", "0", "0"}, {"20", "1", "0", "0"}, {"120", "1", "0", "0"}, {"60", "1", "0", "0"}, {"0", "0", "0", "1"}, {"8", "1", "0", "0"},
	}}
	got, err := convertStats(stats)
	if err != nil {
		t.Fatal(err)
	}
	if *got.TotalTokens.Value != "120" || *got.OutputTokens.Value != "20" || *got.CacheReadTokens.Value != "60" || *got.ReasoningTokens.Value != "8" || got.CacheWriteTokens.Value != nil {
		t.Fatalf("stats = %+v", got)
	}
}

func TestShare(t *testing.T) {
	cases := []struct {
		name         string
		value, total *string
		want         *float64
	}{
		{"normal", ptr("30"), ptr("120"), fptr(0.25)},
		{"whole", ptr("120"), ptr("120"), fptr(1)},
		{"zero over positive is a real zero", ptr("0"), ptr("120"), fptr(0)},
		{"zero over zero", ptr("0"), ptr("0"), nil},
		{"overall missing", ptr("5"), nil, nil},
		{"group missing", nil, ptr("120"), nil},
	}
	for _, tc := range cases {
		got := share(tc.value, tc.total)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Fatalf("%s: share = %v, want %v", tc.name, got, tc.want)
		}
	}
	huge := ptr("340282366920938463463374607431768211456")
	if got := share(huge, huge); got == nil || *got != 1 {
		t.Fatal("beyond-int64 values must still divide exactly")
	}
}

func fptr(f float64) *float64 { return &f }

func rankValues(extra ...string) url.Values {
	v := url.Values{"start": {"2026-10-28T00:00:00Z"}, "end": {"2026-10-28T03:00:00Z"}, "group_by": {"model"}}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	return v
}

func TestRankingsRejectsInvalidArgumentsWithoutQuery(t *testing.T) {
	tests := map[string]url.Values{
		"missing group_by":    func() url.Values { v := rankValues(); v.Del("group_by"); return v }(),
		"bad group_by":        rankValues("group_by", "source"),
		"limit zero":          rankValues("limit", "0"),
		"limit 51":            rankValues("limit", "51"),
		"limit negative":      rankValues("limit", "-1"),
		"limit text":          rankValues("limit", "ten"),
		"limit empty":         rankValues("limit", ""),
		"limit huge":          rankValues("limit", "99999999999999999999"),
		"empty cursor":        rankValues("cursor", ""),
		"garbage cursor":      rankValues("cursor", "!!!"),
		"valid b64 not json":  rankValues("cursor", "AAAA"),
		"timezone on ranking": rankValues("timezone", "UTC"),
		"repeated group_by":   set(rankValues(), "group_by", "model", "project"),
	}
	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{}
			s, _, _ := newTestService(store)
			if _, err := s.Rankings(context.Background(), testWorkspace, values); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v", err)
			}
			if store.rankingsCalls != 0 {
				t.Fatal("rejected request reached the store")
			}
		})
	}
}

func rankRow(a, b, total string) RawGroup {
	return RawGroup{Key1: ptr(a), Key2: ptr(b), Stats: raw("1", total, "1", "0", "0")}
}

func TestRankingsFirstPageOtherAndShare(t *testing.T) {
	other := raw("4", "40", "4", "0", "0")
	store := &fakeStore{rankingsRows: RankingsRows{
		Overall: raw("6", "100", "6", "0", "0"),
		Groups:  []RawGroup{rankRow("p1", "m", "40"), rankRow("p2", "m", "20")},
		Other:   &other,
	}}
	s, _, _ := newTestService(store)
	response, err := s.Rankings(context.Background(), testWorkspace, rankValues("limit", "2"))
	if err != nil {
		t.Fatal(err)
	}
	if store.rankings.Offset != 0 || store.rankings.Limit != 2 || store.rankings.GroupBy != GroupModel {
		t.Fatalf("params = %+v", store.rankings)
	}
	if len(response.Groups) != 2 || response.Other == nil || response.NextCursor == nil {
		t.Fatalf("response = %+v", response)
	}
	if *response.Groups[0].Share != 0.4 || *response.Other.Share != 0.4 {
		t.Fatalf("shares = %v %v", *response.Groups[0].Share, *response.Other.Share)
	}
	key := response.Groups[0].Key
	if *key["provider"] != "p1" || *key["model"] != "m" || len(key) != 2 {
		t.Fatalf("model key = %v", key)
	}
	// The default limit is 10.
	if _, err := s.Rankings(context.Background(), testWorkspace, rankValues()); err != nil || store.rankings.Limit != 10 {
		t.Fatalf("default limit = %d err=%v", store.rankings.Limit, err)
	}
}

func TestRankingsLastPageHasNoOtherAndNoCursor(t *testing.T) {
	store := &fakeStore{rankingsRows: RankingsRows{Overall: raw("1", "10", "1", "0", "0"), Groups: []RawGroup{rankRow("p", "m", "10")}}}
	s, _, _ := newTestService(store)
	response, err := s.Rankings(context.Background(), testWorkspace, rankValues())
	if err != nil {
		t.Fatal(err)
	}
	if response.Other != nil || response.NextCursor != nil {
		t.Fatal("no remaining groups means other=null and next_cursor=null")
	}
	empty, err := (func() (*RankingsResponse, error) {
		s2, _, _ := newTestService(&fakeStore{rankingsRows: RankingsRows{Overall: raw("0", "0", "0", "0", "0")}})
		return s2.Rankings(context.Background(), testWorkspace, rankValues())
	})()
	if err != nil || empty.Groups == nil || len(empty.Groups) != 0 || empty.Overall.TotalTokens.Value != nil || empty.Other != nil {
		t.Fatalf("empty = %+v err=%v", empty, err)
	}
}

func TestProjectGroupKeyAllowsNull(t *testing.T) {
	store := &fakeStore{rankingsRows: RankingsRows{Overall: raw("2", "10", "2", "0", "0"), Groups: []RawGroup{
		{Key1: ptr("018f3c5e-1a2b-7c3d-8e9f-0a1b2c3d4e5f"), Stats: raw("1", "10", "1", "0", "0")},
		{Key1: nil, Stats: raw("1", "0", "0", "0", "1")},
	}}}
	s, _, _ := newTestService(store)
	response, err := s.Rankings(context.Background(), testWorkspace, rankValues("group_by", "project"))
	if err != nil {
		t.Fatal(err)
	}
	if *response.Groups[0].Key["project_id"] != "018f3c5e-1a2b-7c3d-8e9f-0a1b2c3d4e5f" || response.Groups[1].Key["project_id"] != nil {
		t.Fatalf("keys = %v %v", response.Groups[0].Key, response.Groups[1].Key)
	}
	if response.Groups[1].Share != nil {
		t.Fatal("an all-missing group has a null share, not 0%")
	}
}

func pagedService(t *testing.T) (*Service, *fakeStore, *time.Time) {
	t.Helper()
	other := raw("1", "1", "1", "0", "0")
	store := &fakeStore{rankingsRows: RankingsRows{Overall: raw("3", "3", "3", "0", "0"), Groups: []RawGroup{rankRow("p", "m", "1")}, Other: &other}}
	s, _, now := newTestService(store)
	return s, store, now
}

func TestCursorSecondPageUsesOffsetAndOriginalAsOf(t *testing.T) {
	s, store, now := pagedService(t)
	values := rankValues("limit", "1")
	values.Set("start", "2026-10-28T10:00:00Z")
	values.Del("end") // open ended: as_of is sampled
	first, err := s.Rankings(context.Background(), testWorkspace, values)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(5 * time.Minute) // time passes before the next page
	values.Set("cursor", *first.NextCursor)
	second, err := s.Rankings(context.Background(), testWorkspace, values)
	if err != nil {
		t.Fatal(err)
	}
	if store.rankings.Offset != 1 || second.Range.AsOf != first.Range.AsOf || second.Range.End != first.Range.End {
		t.Fatalf("offset=%d asOf %s vs %s", store.rankings.Offset, second.Range.AsOf, first.Range.AsOf)
	}
	if !store.rankings.End.Equal(at(first.Range.AsOf)) {
		t.Fatal("SQL upper bound must stay at the original as_of")
	}
	if second.Range.RetentionCutoff == first.Range.RetentionCutoff {
		t.Fatal("the retention cutoff must be recomputed for the new page")
	}
	third, err := s.Rankings(context.Background(), testWorkspace, func() url.Values { values.Set("cursor", *second.NextCursor); return values }())
	if err != nil || store.rankings.Offset != 2 || third.Range.AsOf != first.Range.AsOf {
		t.Fatalf("third page offset=%d err=%v", store.rankings.Offset, err)
	}
}

func TestCursorRechecksRetentionOnLaterPage(t *testing.T) {
	s, store, now := pagedService(t)
	values := rankValues("limit", "1")
	values.Set("start", "2025-09-28T12:34:00Z") // exactly the first page's cutoff
	first, err := s.Rankings(context.Background(), testWorkspace, values)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Minute) // the cutoff moves past start
	values.Set("cursor", *first.NextCursor)
	calls := store.rankingsCalls
	_, err = s.Rankings(context.Background(), testWorkspace, values)
	var outside *OutsideRetentionError
	if !errors.As(err, &outside) || !outside.Cutoff.Equal(at("2025-09-28T12:36:00Z")) {
		t.Fatalf("error = %v", err)
	}
	if store.rankingsCalls != calls {
		t.Fatal("the stale page must not query")
	}
}

func TestCursorRejectsChangedQuery(t *testing.T) {
	s, store, _ := pagedService(t)
	base := rankValues("limit", "1")
	first, err := s.Rankings(context.Background(), testWorkspace, base)
	if err != nil {
		t.Fatal(err)
	}
	cursor := *first.NextCursor
	changes := map[string]func(url.Values){
		"limit":       func(v url.Values) { v.Set("limit", "2") },
		"group_by":    func(v url.Values) { v.Set("group_by", "provider") },
		"start":       func(v url.Values) { v.Set("start", "2026-10-28T00:01:00Z") },
		"end":         func(v url.Values) { v.Set("end", "2026-10-28T04:00:00Z") },
		"provider":    func(v url.Values) { v.Set("provider", "p") },
		"model":       func(v url.Values) { v.Set("model", "m") },
		"project":     func(v url.Values) { v.Set("project", "unassigned") },
		"end omitted": func(v url.Values) { v.Del("end") },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			values := rankValues("limit", "1")
			change(values)
			values.Set("cursor", cursor)
			calls := store.rankingsCalls
			if _, err := s.Rankings(context.Background(), testWorkspace, values); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v", err)
			}
			if store.rankingsCalls != calls {
				t.Fatal("rejected cursor reached the store")
			}
		})
	}
	// A cursor of one workspace is useless in another.
	values := rankValues("limit", "1")
	values.Set("cursor", cursor)
	other := pgtype.UUID{Bytes: [16]byte{9}, Valid: true}
	if _, err := s.Rankings(context.Background(), other, values); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("cross-workspace cursor error = %v", err)
	}
}

func TestForgedCursorsCannotBypassRangeChecks(t *testing.T) {
	s, _, now := pagedService(t)
	values := rankValues("limit", "1")
	q, err := parseQuery(values)
	if err != nil {
		t.Fatal(err)
	}
	digest := queryDigest(testWorkspace, q, GroupModel, 1)
	forge := func(p cursorPayload) string {
		cursor, err := encodeCursor(p)
		if err != nil {
			t.Fatal(err)
		}
		return cursor
	}
	cases := map[string]cursorPayload{
		"future as_of":     {Version: 1, Digest: digest, AsOf: now.Add(time.Hour).UnixMicro(), Offset: 1},
		"as_of before end": {Version: 1, Digest: digest, AsOf: at("2026-10-28T02:00:00Z").UnixMicro(), Offset: 1},
		"offset zero":      {Version: 1, Digest: digest, AsOf: now.UnixMicro(), Offset: 0},
		"negative offset":  {Version: 1, Digest: digest, AsOf: now.UnixMicro(), Offset: -1},
		"unaligned offset": {Version: 1, Digest: digest, AsOf: now.UnixMicro(), Offset: 1},
		"wrong version":    {Version: 2, Digest: digest, AsOf: now.UnixMicro(), Offset: 1},
		"short digest":     {Version: 1, Digest: "abc", AsOf: now.UnixMicro(), Offset: 1},
	}
	delete(cases, "unaligned offset") // offset 1 is aligned for limit 1; covered by the limit-2 case below
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			values := rankValues("limit", "1")
			values.Set("cursor", forge(payload))
			if _, err := s.Rankings(context.Background(), testWorkspace, values); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	// Unknown cursor fields and trailing data are not tolerated.
	for _, text := range []string{"eyJ2IjoxLCJ6IjoxfQ", "e30"} {
		values := rankValues("limit", "1")
		values.Set("cursor", text)
		if _, err := s.Rankings(context.Background(), testWorkspace, values); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("cursor %q error = %v", text, err)
		}
	}
	// An offset that is not a multiple of limit is rejected.
	q2, _ := parseQuery(rankValues("limit", "2"))
	values = rankValues("limit", "2")
	values.Set("cursor", forge(cursorPayload{Version: 1, Digest: queryDigest(testWorkspace, q2, GroupModel, 2), AsOf: now.UnixMicro(), Offset: 3}))
	if _, err := s.Rankings(context.Background(), testWorkspace, values); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("misaligned offset error = %v", err)
	}
}

func TestStoreFailuresAreNotHiddenAsEmpty(t *testing.T) {
	boom := errors.New("boom")
	s, _, _ := newTestService(&fakeStore{err: boom})
	if _, err := s.Aggregate(context.Background(), testWorkspace, aggValues()); !errors.Is(err, boom) {
		t.Fatalf("aggregate error = %v", err)
	}
	if _, err := s.Rankings(context.Background(), testWorkspace, rankValues()); !errors.Is(err, boom) {
		t.Fatalf("rankings error = %v", err)
	}
	s, _, _ = newTestService(&fakeStore{aggregateRows: AggregateRows{Overall: ptrStats(raw("1", "1", "1", "0", "0")), Points: []RawPoint{{Index: 99, Stats: raw("1", "1", "1", "0", "0")}}}})
	if _, err := s.Aggregate(context.Background(), testWorkspace, aggValues()); err == nil || errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("out of range bucket index error = %v", err)
	}
}

func ptrStats(s RawStats) *RawStats { return &s }

func TestMinuteBoundaryRejectsAnyNonZeroFractionDigit(t *testing.T) {
	for _, field := range []string{"start", "end"} {
		for _, bad := range []string{
			"2026-10-28T00:00:00.0000000001Z", // 10th digit
			"2026-10-28T00:00:00.00000000001Z",
			"2026-10-28T00:00:00.000000000000001Z",
			"2026-10-28T00:00:00.5Z",
			"2026-10-28T0:00:00.0000000001Z",  // one-digit hour, long fraction
			"2026-10-28T0:00:00Z",             // one-digit hour
			"2026-10-28T00:00:00,0000000001Z", // comma fraction, non-zero
			"2026-10-28T00:00:00,0Z",          // comma fraction, all zero
			"2026-10-28T00:00:00.Z",           // empty fraction
			"2026-10-28T00:00:00+24:00",       // illegal offset hour
			"2026-10-28T00:00:00+00:60",       // illegal offset minute
			"2026-10-28T00:00:00.0000000001+08:00",
			"2026-10-28t00:00:00Z",
			"2026-10-28T00:00:00z",
			"2026-10-28T00:00:00",
		} {
			store := &fakeStore{}
			s, _, _ := newTestService(store)
			if _, err := s.Aggregate(context.Background(), testWorkspace, set(aggValues(), field, bad)); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("%s=%q error = %v, want ErrInvalidArgument", field, bad, err)
			}
			if store.aggregateCalls != 0 {
				t.Fatalf("%s=%q reached the store", field, bad)
			}
		}
	}
	for _, ok := range []string{
		"2026-10-28T00:00:00.0Z", "2026-10-28T00:00:00.000000000000Z",
		"2026-10-28T08:00:00.0000000000000+08:00", "2026-10-28T00:00:00-23:59",
		"2026-10-28T05:30:00+05:30", "2024-02-29T00:00:00Z",
	} {
		if _, err := parseMinute(ok); err != nil {
			t.Fatalf("valid minute %q rejected: %v", ok, err)
		}
	}
	if _, err := parseMinute("2026-02-29T00:00:00Z"); err == nil {
		t.Fatal("non-leap Feb 29 accepted")
	}
}

func TestProjectFilterIsDistinctByUUIDIdentity(t *testing.T) {
	uuidAt := func(i int, upper bool) string {
		s := fmt.Sprintf("018f3c5e-1a2b-7c3d-8e9f-%012x", i)
		if upper {
			s = strings.ToUpper(s)
		}
		return s
	}
	var ok []string
	for i := 0; i < 50; i++ {
		ok = append(ok, uuidAt(i, false), uuidAt(i, true))
	}
	store := &fakeStore{}
	s, _, _ := newTestService(store)
	if _, err := s.Aggregate(context.Background(), testWorkspace, set(aggValues(), "project", ok...)); err != nil {
		t.Fatalf("50 distinct UUIDs with case variants must be accepted: %v", err)
	}
	if got := len(store.aggregate.Filter.Projects); got != 50 {
		t.Fatalf("projects = %d, want 50", got)
	}
	// 51 truly distinct UUIDs, and 50 UUIDs plus unassigned, are rejected.
	for name, list := range map[string][]string{
		"51 UUIDs":            append(append([]string{}, ok...), uuidAt(50, false)),
		"50 UUIDs unassigned": append(append([]string{}, ok...), "unassigned"),
	} {
		store := &fakeStore{}
		s, _, _ := newTestService(store)
		if _, err := s.Aggregate(context.Background(), testWorkspace, set(aggValues(), "project", list...)); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s error = %v", name, err)
		}
		if store.aggregateCalls != 0 {
			t.Fatalf("%s reached the store", name)
		}
	}
	// Equivalent filters share one digest, so a cursor survives case changes.
	a, err := parseQuery(set(rankValues("limit", "1"), "project", uuidAt(1, false), uuidAt(2, false)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseQuery(set(rankValues("limit", "1"), "project", uuidAt(2, true), uuidAt(1, true), uuidAt(1, false)))
	if err != nil {
		t.Fatal(err)
	}
	if queryDigest(testWorkspace, a, GroupModel, 1) != queryDigest(testWorkspace, b, GroupModel, 1) {
		t.Fatal("equivalent project filters produced different digests")
	}
}

func TestDecodeCursorRejectsTrailingData(t *testing.T) {
	good := cursorPayload{Version: 1, Digest: strings.Repeat("a", 64), AsOf: 1, Offset: 1}
	enc, err := encodeCursor(good)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeCursor(enc); err != nil {
		t.Fatalf("valid cursor rejected: %v", err)
	}
	raw, _ := json.Marshal(good)
	for name, tail := range map[string]string{
		"closing brace":   "}",
		"closing bracket": "]",
		"second json":     "{}",
		"garbage":         "xyz",
	} {
		text := base64.RawURLEncoding.EncodeToString(append(append([]byte{}, raw...), tail...))
		if _, err := decodeCursor(text); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: error = %v, want ErrInvalidArgument", name, err)
		}
	}
}

func TestOptionsSamplesClockOnceAndUsesRetentionWindow(t *testing.T) {
	store := &fakeStore{}
	s, calls, now := newTestService(store)
	got, err := s.Options(context.Background(), testWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatalf("clock sampled %d times", *calls)
	}
	asOf := now.Truncate(time.Microsecond)
	if !store.options.End.Equal(asOf) || !store.options.Start.Equal(aggregation.RetentionCutoff(asOf)) || store.options.WorkspaceID != testWorkspace {
		t.Fatalf("params %+v", store.options)
	}
	if got.Providers == nil || got.Models == nil || len(got.Providers) != 0 || len(got.Models) != 0 {
		t.Fatalf("empty result must be non-nil empty slices: %+v", got)
	}
}

func TestOptionsDeduplicatesAndSorts(t *testing.T) {
	store := &fakeStore{optionsRows: OptionsRows{
		Providers: []string{"openai", "anthropic", "openai"},
		Models:    []string{"b", "B", "a", "b"},
	}}
	s, _, _ := newTestService(store)
	got, err := s.Options(context.Background(), testWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Providers, ",") != "anthropic,openai" || strings.Join(got.Models, ",") != "B,a,b" {
		t.Fatalf("got %+v", got)
	}
}

func TestOptionsStoreErrorPassesThrough(t *testing.T) {
	boom := errors.New("boom")
	s, _, _ := newTestService(&fakeStore{err: boom})
	if _, err := s.Options(context.Background(), testWorkspace); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}
