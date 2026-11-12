package requestdetails

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

type fakeStore struct {
	rows     []EventRow
	events   EventsParams
	rate     RateRow
	projects int
}

func (f *fakeStore) CountProjects(_ context.Context, _ pgtype.UUID, ids []pgtype.UUID) (int, error) {
	return len(ids) - f.projects, nil
}
func (f *fakeStore) Events(_ context.Context, p EventsParams) ([]EventRow, error) {
	f.events = p
	return f.rows, nil
}
func (f *fakeStore) Rate(context.Context, RateParams) (RateRow, error) { return f.rate, nil }

func TestEventsLimitPlusOneAndCursor(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rows := []EventRow{}
	for i := byte(1); i <= 3; i++ {
		rows = append(rows, EventRow{ID: pgtype.UUID{Bytes: [16]byte{15: i}, Valid: true}, OccurredAt: now.Add(-time.Duration(i) * time.Second)})
	}
	store := &fakeStore{rows: rows}
	svc := New(store)
	svc.now = func() time.Time { return now }
	r, err := svc.Events(context.Background(), pgtype.UUID{}, url.Values{"limit": {"2"}})
	if err != nil || store.events.Limit != 2 || len(r.Items) != 2 || !r.HasMore || r.NextCursor == nil {
		t.Fatalf("%+v %v", r, err)
	}
	if !store.events.End.Equal(now) || !store.events.Start.Equal(now.Add(-time.Hour)) {
		t.Fatalf("range %v %v", store.events.Start, store.events.End)
	}
	if _, err := svc.Events(context.Background(), pgtype.UUID{}, url.Values{"cursor": {*r.NextCursor}}); err != nil || !store.events.HasCursor {
		t.Fatalf("cursor: %v", err)
	}
}

func TestInvalidInputs(t *testing.T) {
	svc := New(&fakeStore{projects: 1})
	for _, q := range []url.Values{
		{"limit": {"01"}}, {"limit": {"x"}}, {"end": {"2026-01-01T00:00:00Z"}},
		{"start": {"2026-01-01T00:00:00.1234567Z"}, "end": {"2026-01-02T00:00:00Z"}},
		{"start": {"2026-01-01T00:00:00,123Z"}, "end": {"2026-01-02T00:00:00Z"}},
		{"start": {"2026-01-01T00:00:00,1234560Z"}, "end": {"2026-01-02T00:00:00Z"}},
		{"start": {"2026-01-01T00:00:00.1234560Z"}, "end": {"2026-01-02T00:00:00Z"}},
		{"start": {"2026-01-01T00:00:00.1234567Z"}, "end": {"2026-01-02T00:00:00Z"}},
		{"start": {"2026-01-01T00:00:00"}, "end": {"2026-01-02T00:00:00Z"}},
		{"project": {"0199a0b0-0000-7000-8000-00000000000a"}},
		{"model": {"a\x00b"}}, {"unknown": {"1"}},
	} {
		if _, err := svc.Events(context.Background(), pgtype.UUID{}, q); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%v: %v", q, err)
		}
	}
}

// TestEventGenerationMetricsShape: both generation keys are always emitted, a
// missing sample stays null and never becomes a zero, and the derived speed is
// passed through as the store returned it.
func TestEventGenerationMetricsShape(t *testing.T) {
	speed := "49.500000"
	ttft := int64(8000)
	rows := []EventRow{
		{ID: pgtype.UUID{Bytes: [16]byte{0x03, 15: 1}, Valid: true}, Output: TokenRow{Value: ptr(int64(100)), Quality: "known"}, GenerationSpeedTPS: &speed, TTFTMS: &ttft},
		{ID: pgtype.UUID{Bytes: [16]byte{0x03, 15: 2}, Valid: true}, Output: TokenRow{Quality: "missing"}},
	}
	store := &fakeStore{rows: rows}
	svc := New(store)
	r, err := svc.Events(context.Background(), pgtype.UUID{}, url.Values{"limit": {"10"}})
	if err != nil || len(r.Items) != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	body, err := json.Marshal(r.Items)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"generation_speed_tps":"49.500000"`) || !strings.Contains(string(body), `"ttft_ms":8000`) {
		t.Fatalf("present values missing: %s", body)
	}
	if strings.Count(string(body), `"generation_speed_tps":null`) != 1 || strings.Count(string(body), `"ttft_ms":null`) != 1 {
		t.Fatalf("missing values are not explicit nulls: %s", body)
	}
}

// TestRateGenerationMetricsShape: the two objects carry their own unit and
// sample count; no valid sample yields value=null with count 0, never "0".
func TestRateGenerationMetricsShape(t *testing.T) {
	speed, ttft := "29.750000", "4200.000000"
	store := &fakeStore{rate: RateRow{AsOf: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), SpeedAvg: &speed, SpeedSamples: 2, TTFTAvg: &ttft, TTFTSamples: 3}}
	svc := New(store)
	r, err := svc.Rate(context.Background(), pgtype.UUID{}, url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Unit != "tokens/min" || r.GenerationSpeed != (RateMetric{Unit: "tokens/s", Value: &speed, SampleCount: 2}) || r.TTFT != (RateMetric{Unit: "ms", Value: &ttft, SampleCount: 3}) {
		t.Fatalf("rate = %+v", r)
	}
	empty := New(&fakeStore{rate: RateRow{AsOf: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}})
	e, err := empty.Rate(context.Background(), pgtype.UUID{}, url.Values{})
	if err != nil || e.GenerationSpeed.Value != nil || e.GenerationSpeed.SampleCount != 0 || e.TTFT.Value != nil || e.TTFT.SampleCount != 0 {
		t.Fatalf("empty = %+v %v", e, err)
	}
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"generation_speed":{"unit":"tokens/s","value":null,"sample_count":0}`) ||
		!strings.Contains(string(body), `"ttft":{"unit":"ms","value":null,"sample_count":0}`) {
		t.Fatalf("empty body = %s", body)
	}
}

func ptr(v int64) *int64 { return &v }
