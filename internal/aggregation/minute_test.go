package aggregation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func value(v int64) *int64 {
	return &v
}

// requestInput is a valid request-level input with six known zero metrics, matching
// the "known 0 + 0" case: a real zero is known, never missing.
func requestInput() MinuteInput {
	zero := value(0)
	return MinuteInput{
		Provider:   "synthetic-provider",
		Model:      "synthetic-model",
		OccurredAt: time.Date(2026, 10, 27, 12, 34, 30, 0, time.UTC),
		Level:      LevelRequest,
		Input:      Metric{Value: zero, Quality: QualityKnown},
		Output:     Metric{Value: zero, Quality: QualityKnown},
		Total:      Metric{Value: zero, Quality: QualityKnown},
		CacheRead:  Metric{Value: zero, Quality: QualityKnown},
		CacheWrite: Metric{Value: zero, Quality: QualityKnown},
		Reasoning:  Metric{Value: zero, Quality: QualityKnown},
	}
}

// TestMinuteBuckets pins the UTC minute start rule: the same instant expressed in
// any Location or fixed offset yields one bucket, and the second/nanosecond part
// never changes it while the next minute does.
func TestMinuteBuckets(t *testing.T) {
	instant := time.Date(2026, 10, 27, 12, 34, 30, 500, time.UTC)
	want := time.Date(2026, 10, 27, 12, 34, 0, 0, time.UTC)
	locations := map[string]time.Time{
		"utc":                    instant,
		"fixed west offset":      instant.In(time.FixedZone("synthetic-west", -7*60*60)),
		"fixed east offset":      instant.In(time.FixedZone("synthetic-east", 9*60*60)),
		"fixed half-hour offset": instant.In(time.FixedZone("synthetic-named", 5*60*60+30*60)),
	}
	for name, at := range locations {
		if got := bucketStart(at); !got.Equal(want) || got.Location() != time.UTC {
			t.Fatalf("%s bucket=%s want=%s in UTC", name, got, want)
		}
	}

	start := time.Date(2026, 10, 27, 12, 34, 0, 0, time.UTC)
	last := time.Date(2026, 10, 27, 12, 34, 59, 999999999, time.UTC)
	next := time.Date(2026, 10, 27, 12, 35, 0, 0, time.UTC)
	if !bucketStart(start).Equal(bucketStart(last)) {
		t.Fatalf("12:34:00 and 12:34:59.999999999 fell into different buckets")
	}
	if !bucketStart(next).After(bucketStart(last)) {
		t.Fatalf("12:35:00 did not move to the next bucket")
	}
	if got := bucketStart(last).Nanosecond(); got != 0 {
		t.Fatalf("bucket keeps sub-minute nanoseconds: %d", got)
	}

	// A local wall clock is never read: an instant already shifted to another zone
	// still maps to the same UTC minute as the same instant in UTC.
	local := time.Date(2026, 10, 27, 5, 34, 59, 0, time.UTC).In(time.FixedZone("synthetic-offset", -7*60*60))
	if !bucketStart(local).Equal(time.Date(2026, 10, 27, 5, 34, 0, 0, time.UTC)) {
		t.Fatalf("offset representation changed the bucket: %s", bucketStart(local))
	}
}

// TestRejectedInputs covers the only checks this writer owns: the event level and
// each metric's quality representation. Every rejection returns before the
// transaction is used, so a nil tx proves nothing was written or begun.
func TestRejectedInputs(t *testing.T) {
	for _, level := range []Level{LevelSummary, "", "synthetic-unknown"} {
		in := requestInput()
		in.Level = level
		err := ApplyRequest(context.Background(), nil, in)
		if !errors.Is(err, ErrNonRequestLevel) {
			t.Fatalf("level=%q error=%v want ErrNonRequestLevel", level, err)
		}
	}

	metrics := []struct {
		name string
		set  func(*MinuteInput, Metric)
	}{
		{"input_tokens", func(in *MinuteInput, m Metric) { in.Input = m }},
		{"output_tokens", func(in *MinuteInput, m Metric) { in.Output = m }},
		{"total_tokens", func(in *MinuteInput, m Metric) { in.Total = m }},
		{"cache_read_tokens", func(in *MinuteInput, m Metric) { in.CacheRead = m }},
		{"cache_write_tokens", func(in *MinuteInput, m Metric) { in.CacheWrite = m }},
		{"reasoning_tokens", func(in *MinuteInput, m Metric) { in.Reasoning = m }},
	}
	invalids := []struct {
		name   string
		metric Metric
	}{
		{"known without value", Metric{Quality: QualityKnown}},
		{"estimated without value", Metric{Quality: QualityEstimated}},
		{"missing with value", Metric{Value: value(0), Quality: QualityMissing}},
		{"missing with nonzero value", Metric{Value: value(7), Quality: QualityMissing}},
		{"empty quality", Metric{Value: value(1)}},
		{"unknown quality", Metric{Value: value(1), Quality: "synthetic-quality"}},
	}
	for _, metric := range metrics {
		for _, invalid := range invalids {
			in := requestInput()
			metric.set(&in, invalid.metric)
			err := ApplyRequest(context.Background(), nil, in)
			if !errors.Is(err, ErrInvalidMetric) {
				t.Fatalf("%s %s error=%v want ErrInvalidMetric", metric.name, invalid.name, err)
			}
			if !strings.Contains(err.Error(), metric.name) {
				t.Fatalf("%s %s error=%q does not name the metric", metric.name, invalid.name, err)
			}
		}
	}
}

// TestIncrements pins the per-metric value and quality deltas, including known
// zero and estimated values, and that total is never recomputed from the parts.
func TestIncrements(t *testing.T) {
	cases := []struct {
		name   string
		metric Metric
		want   increments
	}{
		{"known zero", Metric{Value: value(0), Quality: QualityKnown}, increments{value: 0, known: 1}},
		{"known nonzero", Metric{Value: value(120), Quality: QualityKnown}, increments{value: 120, known: 1}},
		{"estimated zero", Metric{Value: value(0), Quality: QualityEstimated}, increments{value: 0, estimated: 1}},
		{"estimated nonzero", Metric{Value: value(100), Quality: QualityEstimated}, increments{value: 100, estimated: 1}},
		{"missing", Metric{Quality: QualityMissing}, increments{missing: 1}},
	}
	for _, test := range cases {
		got, err := test.metric.increments()
		if err != nil {
			t.Fatalf("%s error=%v", test.name, err)
		}
		if got != test.want {
			t.Fatalf("%s increments=%+v want=%+v", test.name, got, test.want)
		}
	}

	// A complete request still counts one event per metric, so the invariant
	// known+estimated+missing == event_count holds for every accepted input.
	in := requestInput()
	in.Input = Metric{Value: value(100), Quality: QualityKnown}
	in.Output = Metric{Value: value(20), Quality: QualityKnown}
	in.Total = Metric{Value: value(120), Quality: QualityKnown}
	in.CacheRead = Metric{Value: value(60), Quality: QualityKnown}
	in.CacheWrite = Metric{Quality: QualityMissing}
	in.Reasoning = Metric{Quality: QualityMissing}
	var total, misses int64
	for _, metric := range []Metric{in.Input, in.Output, in.Total, in.CacheRead, in.CacheWrite, in.Reasoning} {
		delta, err := metric.increments()
		if err != nil {
			t.Fatal(err)
		}
		if delta.known+delta.estimated+delta.missing != 1 {
			t.Fatalf("metric deltas=%+v do not add up to one event", delta)
		}
		if delta.missing == 1 {
			misses++
		}
		if metric == in.Total {
			total = delta.value
		}
	}
	if total != 120 {
		t.Fatalf("total delta=%d want=120 taken from the input", total)
	}
	if misses != 2 {
		t.Fatalf("missing metrics=%d want=2", misses)
	}
}
