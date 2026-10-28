// Package aggregation owns the additive per-minute usage statistics writer. It
// never owns a transaction, a connection or a retry policy: the caller passes the
// same pgx.Tx that inserted the detail event, so both writes commit or roll back
// together.
package aggregation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Level is the explicit event level of one input. Only request-level final events
// may increment minute statistics; a summary must never look like a request.
type Level string

const (
	// LevelRequest marks a per-request final event, the only accepted level.
	LevelRequest Level = "request"
	// LevelSummary marks a source-level rollup. It is rejected here so a rollup
	// never fabricates request counts or token sums.
	LevelSummary Level = "summary"
)

// Quality is the per-metric completeness vocabulary. A real zero is known, a
// derived value is estimated and an unavailable value is missing.
type Quality string

const (
	// QualityKnown marks a value read from the source as is; zero is allowed.
	QualityKnown Quality = "known"
	// QualityEstimated marks a value derived rather than measured; zero is allowed.
	QualityEstimated Quality = "estimated"
	// QualityMissing marks an unavailable value, which must be nil so that
	// missing is never indistinguishable from a real zero.
	QualityMissing Quality = "missing"
)

var (
	// ErrNonRequestLevel reports an input whose level is not request-level. The
	// whole call fails; nothing is skipped and no token value is added.
	ErrNonRequestLevel = errors.New("aggregation: only request-level events are aggregated")
	// ErrInvalidMetric reports a metric whose quality and value disagree, or an
	// unknown quality. The minute counts would be wrong, so the call fails.
	ErrInvalidMetric = errors.New("aggregation: metric quality does not match its value")
)

// Metric is one token metric: a nullable non-negative value plus its quality.
// Value is nil exactly when Quality is QualityMissing. Non-negativity and the
// int64 range are validated once by the caller before the value reaches this
// package; this writer only checks the quality representation.
type Metric struct {
	Value   *int64
	Quality Quality
}

// MinuteInput is the minimal input one minute increment needs. It carries the
// already normalized occurred_at, the explicit event level and the six named
// metrics. It deliberately has no event ID: exactly-once incrementing relies on
// the detail table's unique key and call order, not on this struct.
type MinuteInput struct {
	WorkspaceID pgtype.UUID
	SourceID    pgtype.UUID
	// ProjectID is invalid (Valid false) when the event is not attributed to a
	// project. The unique constraint treats those rows as one bucket.
	ProjectID  pgtype.UUID
	Provider   string
	Model      string
	OccurredAt time.Time
	Level      Level
	Input      Metric
	Output     Metric
	Total      Metric
	CacheRead  Metric
	CacheWrite Metric
	Reasoning  Metric
}

// increments is one metric's contribution to a bucket: the value delta plus the
// three quality count deltas. Missing metrics contribute no value.
type increments struct {
	value     int64
	known     int64
	estimated int64
	missing   int64
}

// minuteUpsert adds one event to its bucket. Every column is spelled out and every
// conflict update adds the excluded delta to the stored value: a single atomic
// statement under the six-column unique constraint, with no read-modify-write, no
// WHERE clause and no retry.
const minuteUpsert = `INSERT INTO usage_minute_stats (
    workspace_id, bucket_start, source_id, project_id, provider, model, event_count,
    input_tokens_sum, input_tokens_known_count, input_tokens_estimated_count, input_tokens_missing_count,
    output_tokens_sum, output_tokens_known_count, output_tokens_estimated_count, output_tokens_missing_count,
    total_tokens_sum, total_tokens_known_count, total_tokens_estimated_count, total_tokens_missing_count,
    cache_read_tokens_sum, cache_read_tokens_known_count, cache_read_tokens_estimated_count, cache_read_tokens_missing_count,
    cache_write_tokens_sum, cache_write_tokens_known_count, cache_write_tokens_estimated_count, cache_write_tokens_missing_count,
    reasoning_tokens_sum, reasoning_tokens_known_count, reasoning_tokens_estimated_count, reasoning_tokens_missing_count
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31)
ON CONFLICT (workspace_id, bucket_start, source_id, project_id, provider, model) DO UPDATE SET
    event_count = usage_minute_stats.event_count + EXCLUDED.event_count,
    input_tokens_sum = usage_minute_stats.input_tokens_sum + EXCLUDED.input_tokens_sum,
    input_tokens_known_count = usage_minute_stats.input_tokens_known_count + EXCLUDED.input_tokens_known_count,
    input_tokens_estimated_count = usage_minute_stats.input_tokens_estimated_count + EXCLUDED.input_tokens_estimated_count,
    input_tokens_missing_count = usage_minute_stats.input_tokens_missing_count + EXCLUDED.input_tokens_missing_count,
    output_tokens_sum = usage_minute_stats.output_tokens_sum + EXCLUDED.output_tokens_sum,
    output_tokens_known_count = usage_minute_stats.output_tokens_known_count + EXCLUDED.output_tokens_known_count,
    output_tokens_estimated_count = usage_minute_stats.output_tokens_estimated_count + EXCLUDED.output_tokens_estimated_count,
    output_tokens_missing_count = usage_minute_stats.output_tokens_missing_count + EXCLUDED.output_tokens_missing_count,
    total_tokens_sum = usage_minute_stats.total_tokens_sum + EXCLUDED.total_tokens_sum,
    total_tokens_known_count = usage_minute_stats.total_tokens_known_count + EXCLUDED.total_tokens_known_count,
    total_tokens_estimated_count = usage_minute_stats.total_tokens_estimated_count + EXCLUDED.total_tokens_estimated_count,
    total_tokens_missing_count = usage_minute_stats.total_tokens_missing_count + EXCLUDED.total_tokens_missing_count,
    cache_read_tokens_sum = usage_minute_stats.cache_read_tokens_sum + EXCLUDED.cache_read_tokens_sum,
    cache_read_tokens_known_count = usage_minute_stats.cache_read_tokens_known_count + EXCLUDED.cache_read_tokens_known_count,
    cache_read_tokens_estimated_count = usage_minute_stats.cache_read_tokens_estimated_count + EXCLUDED.cache_read_tokens_estimated_count,
    cache_read_tokens_missing_count = usage_minute_stats.cache_read_tokens_missing_count + EXCLUDED.cache_read_tokens_missing_count,
    cache_write_tokens_sum = usage_minute_stats.cache_write_tokens_sum + EXCLUDED.cache_write_tokens_sum,
    cache_write_tokens_known_count = usage_minute_stats.cache_write_tokens_known_count + EXCLUDED.cache_write_tokens_known_count,
    cache_write_tokens_estimated_count = usage_minute_stats.cache_write_tokens_estimated_count + EXCLUDED.cache_write_tokens_estimated_count,
    cache_write_tokens_missing_count = usage_minute_stats.cache_write_tokens_missing_count + EXCLUDED.cache_write_tokens_missing_count,
    reasoning_tokens_sum = usage_minute_stats.reasoning_tokens_sum + EXCLUDED.reasoning_tokens_sum,
    reasoning_tokens_known_count = usage_minute_stats.reasoning_tokens_known_count + EXCLUDED.reasoning_tokens_known_count,
    reasoning_tokens_estimated_count = usage_minute_stats.reasoning_tokens_estimated_count + EXCLUDED.reasoning_tokens_estimated_count,
    reasoning_tokens_missing_count = usage_minute_stats.reasoning_tokens_missing_count + EXCLUDED.reasoning_tokens_missing_count`

// ApplyRequest adds one request-level event to its UTC minute bucket inside the
// caller's transaction. The bucket start is always in.OccurredAt.UTC() truncated
// to the minute: no session time zone, never received_at.
//
// The function does not begin, commit, roll back, retry or borrow a connection,
// and it never turns a failure into success. Any error means the caller must roll
// back the whole transaction, so the detail row and the minute increment share one
// fate.
func ApplyRequest(ctx context.Context, tx pgx.Tx, in MinuteInput) error {
	if in.Level != LevelRequest {
		return ErrNonRequestLevel
	}
	metrics := []struct {
		name   string
		metric Metric
	}{
		{"input_tokens", in.Input},
		{"output_tokens", in.Output},
		{"total_tokens", in.Total},
		{"cache_read_tokens", in.CacheRead},
		{"cache_write_tokens", in.CacheWrite},
		{"reasoning_tokens", in.Reasoning},
	}
	deltas := make([]increments, len(metrics))
	for i, entry := range metrics {
		delta, err := entry.metric.increments()
		if err != nil {
			return fmt.Errorf("%w: %s", err, entry.name)
		}
		deltas[i] = delta
	}
	args := []any{
		in.WorkspaceID, bucketStart(in.OccurredAt), in.SourceID, in.ProjectID,
		in.Provider, in.Model, int64(1),
	}
	for _, delta := range deltas {
		args = append(args, delta.value, delta.known, delta.estimated, delta.missing)
	}
	if _, err := tx.Exec(ctx, minuteUpsert, args...); err != nil {
		return database.SafeError("aggregation minute upsert", err)
	}
	return nil
}

// bucketStart returns the UTC minute start that owns an instant. It converts to
// UTC first, so the same instant always lands in the same bucket regardless of the
// time.Location or fixed offset the caller used, and DST never shifts a bucket.
func bucketStart(occurredAt time.Time) time.Time {
	return occurredAt.UTC().Truncate(time.Minute)
}

// increments returns this metric's value and quality count deltas. It only checks
// the representation the minute counts depend on; value range and source validity
// stay with the caller.
func (m Metric) increments() (increments, error) {
	switch m.Quality {
	case QualityKnown:
		if m.Value == nil {
			return increments{}, ErrInvalidMetric
		}
		return increments{value: *m.Value, known: 1}, nil
	case QualityEstimated:
		if m.Value == nil {
			return increments{}, ErrInvalidMetric
		}
		return increments{value: *m.Value, estimated: 1}, nil
	case QualityMissing:
		if m.Value != nil {
			return increments{}, ErrInvalidMetric
		}
		return increments{missing: 1}, nil
	default:
		return increments{}, ErrInvalidMetric
	}
}
