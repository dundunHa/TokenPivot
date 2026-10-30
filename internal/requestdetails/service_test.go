package requestdetails

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

type fakeStore struct {
	rows     []EventRow
	events   EventsParams
	projects int
}

func (f *fakeStore) CountProjects(_ context.Context, _ pgtype.UUID, ids []pgtype.UUID) (int, error) {
	return len(ids) - f.projects, nil
}
func (f *fakeStore) Events(_ context.Context, p EventsParams) ([]EventRow, error) {
	f.events = p
	return f.rows, nil
}
func (f *fakeStore) Rate(context.Context, RateParams) (RateRow, error) { return RateRow{}, nil }

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
		{"start": {"2026-01-01T00:00:00"}, "end": {"2026-01-02T00:00:00Z"}},
		{"project": {"0199a0b0-0000-7000-8000-00000000000a"}},
		{"model": {"a\x00b"}}, {"unknown": {"1"}},
	} {
		if _, err := svc.Events(context.Background(), pgtype.UUID{}, q); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%v: %v", q, err)
		}
	}
}
