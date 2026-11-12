package requestdetails

import (
	"context"
	"time"

	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres reads usage_events through the service role pool.
type Postgres struct{ pool *pgxpool.Pool }

// NewPostgres returns a store on pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// filterSQL is shared by both statements. Parameters are the arrays and flag
// numbered by the caller; a NULL array leaves a dimension unfiltered.
func filterSQL(providers, models, projects, unassigned string) string {
	return `AND (` + providers + `::text[] IS NULL OR e.provider = ANY(` + providers + `::text[]))
  AND (` + models + `::text[] IS NULL OR e.model = ANY(` + models + `::text[]))
  AND ((` + projects + `::uuid[] IS NULL AND NOT ` + unassigned + `::boolean)
       OR e.project_id = ANY(` + projects + `::uuid[])
       OR (` + unassigned + `::boolean AND e.project_id IS NULL))`
}

func filterArgs(f Filter) []any {
	var providers, models, projects any
	if len(f.Providers) > 0 {
		providers = f.Providers
	}
	if len(f.Models) > 0 {
		models = f.Models
	}
	if len(f.Projects) > 0 {
		projects = f.Projects
	}
	return []any{providers, models, projects, f.Unattributed}
}

// speedExpr is the unrounded per request generation speed in tokens/s: the
// decode span covers every output token but the first one, so N-1 tokens are
// spread over generation_duration_ms. It is numeric, never a stored float.
const speedExpr = `((e.output_tokens - 1)::numeric * 1000) / e.generation_duration_ms`

// speedValid is the only condition under which speedExpr has a value. The final
// known output tokens must exceed one, and a single chunk output (zero decode
// span) yields no speed rather than a zero or an infinity.
const speedValid = `e.output_tokens_quality = 'known' AND e.output_tokens > 1 AND e.generation_duration_ms > 0`

// eventsQuery pages newest first by (occurred_at, id). $1 workspace, $2 start,
// $3 end, $4..$7 filter, $8 has cursor, $9 cursor time, $10 cursor id, $11 limit+1.
var eventsQuery = `SELECT e.id, e.source_id, e.occurred_at, e.received_at, e.provider, e.model,
  e.project_id, p.project_key, p.name, s.platform, s.instance_key,
  e.input_tokens, e.input_tokens_quality, e.output_tokens, e.output_tokens_quality,
  e.total_tokens, e.total_tokens_quality, e.status, e.duration_ms,
  CASE WHEN ` + speedValid + ` THEN round(` + speedExpr + `, 6)::text END,
  e.ttft_ms
FROM usage_events e
JOIN sources s ON s.workspace_id = e.workspace_id AND s.id = e.source_id
LEFT JOIN projects p ON p.workspace_id = e.workspace_id AND p.id = e.project_id
WHERE e.workspace_id = $1
  AND e.occurred_at >= $2 AND e.occurred_at < $3
  ` + filterSQL("$4", "$5", "$6", "$7") + `
  AND (NOT $8::boolean OR (e.occurred_at, e.id) < ($9::timestamptz, $10::uuid))
ORDER BY e.occurred_at DESC, e.id DESC
LIMIT $11`

// Events returns up to Limit+1 rows.
func (s *Postgres) Events(ctx context.Context, params EventsParams) ([]EventRow, error) {
	args := []any{params.WorkspaceID, params.Start, params.End}
	args = append(args, filterArgs(params.Filter)...)
	var cursorTime, cursorID any
	if params.HasCursor {
		cursorTime, cursorID = params.CursorTime, params.CursorID
	}
	args = append(args, params.HasCursor, cursorTime, cursorID, params.Limit+1)
	rows, err := s.pool.Query(ctx, eventsQuery, args...)
	if err != nil {
		return nil, database.SafeStoreError("request events", err)
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var r EventRow
		if err := rows.Scan(&r.ID, &r.SourceID, &r.OccurredAt, &r.ReceivedAt, &r.Provider, &r.Model,
			&r.ProjectID, &r.ProjectKey, &r.ProjectName, &r.Platform, &r.InstanceKey,
			&r.Input.Value, &r.Input.Quality, &r.Output.Value, &r.Output.Quality,
			&r.Total.Value, &r.Total.Quality, &r.Status, &r.DurationMS,
			&r.GenerationSpeedTPS, &r.TTFTMS); err != nil {
			return nil, database.SafeStoreError("scan request event", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, database.SafeStoreError("request events", err)
	}
	return out, nil
}

// rateQuery samples the database statement start once and aggregates the
// half-open window [as_of-60s, as_of) in the same statement. $1 workspace,
// $2 optional fixed as_of, $3..$6 filter. The two generation metrics are
// arithmetic means over their own valid samples, never token or duration
// weighted: each request counts once. The average is taken over the unrounded
// per request speed and rounded only once, at the end.
var rateQuery = `WITH p AS (SELECT COALESCE($2::timestamptz, statement_timestamp()) AS as_of)
SELECT p.as_of,
  (sum(e.total_tokens::numeric) FILTER (WHERE e.total_tokens_quality IN ('known','estimated')))::text,
  count(e.id),
  count(*) FILTER (WHERE e.total_tokens_quality = 'known'),
  count(*) FILTER (WHERE e.total_tokens_quality = 'estimated'),
  count(*) FILTER (WHERE e.total_tokens_quality = 'missing'),
  max(e.received_at),
  round(avg(` + speedExpr + `) FILTER (WHERE ` + speedValid + `), 6)::text,
  count(*) FILTER (WHERE ` + speedValid + `),
  round(avg(e.ttft_ms) FILTER (WHERE e.ttft_ms IS NOT NULL), 6)::text,
  count(e.ttft_ms)
FROM p
LEFT JOIN usage_events e
  ON e.workspace_id = $1
 AND e.occurred_at >= p.as_of - interval '60 seconds'
 AND e.occurred_at < p.as_of
 ` + filterSQL("$3", "$4", "$5", "$6") + `
GROUP BY p.as_of`

// speedQuery measures generation speed from the events that can be measured: a known
// or estimated output_tokens and a positive duration_ms. As in rateQuery the database
// statement start is sampled once as as_of.
//
// It returns one row per UTC minute (grouping(bucket)=0) from the minute that contains
// as_of-60min up to as_of, so the first bucket is whole, and one overall row
// (grouping(bucket)=1). The overall row's win_* columns cover only the rolling 60 seconds
// [as_of-60s, as_of); the per-minute rows' sums are the minute series. Empty minutes have
// no row. $1 workspace, $2 optional fixed as_of, $3..$6 filter.
var speedQuery = `WITH p AS (SELECT COALESCE($2::timestamptz, statement_timestamp()) AS as_of),
m AS (
  SELECT p.as_of, e.occurred_at, e.output_tokens_quality, e.output_tokens, e.duration_ms,
    (e.output_tokens_quality IN ('known','estimated') AND e.duration_ms IS NOT NULL AND e.duration_ms > 0) AS usable
  FROM p
  LEFT JOIN usage_events e
    ON e.workspace_id = $1
   AND e.occurred_at >= date_bin('1 minute', p.as_of - interval '60 minutes', TIMESTAMPTZ '2000-01-01 00:00:00+00')
   AND e.occurred_at < p.as_of
   ` + filterSQL("$3", "$4", "$5", "$6") + `
)
SELECT as_of,
  date_bin('1 minute', occurred_at, TIMESTAMPTZ '2000-01-01 00:00:00+00') AS bucket,
  grouping(date_bin('1 minute', occurred_at, TIMESTAMPTZ '2000-01-01 00:00:00+00')) AS is_total,
  (sum(output_tokens::numeric) FILTER (WHERE usable))::text,
  (sum(duration_ms::numeric) FILTER (WHERE usable))::text,
  count(occurred_at), count(*) FILTER (WHERE usable),
  count(*) FILTER (WHERE usable AND output_tokens_quality = 'estimated'),
  (sum(output_tokens::numeric) FILTER (WHERE usable AND occurred_at >= as_of - interval '60 seconds'))::text,
  (sum(duration_ms::numeric) FILTER (WHERE usable AND occurred_at >= as_of - interval '60 seconds'))::text,
  count(*) FILTER (WHERE occurred_at >= as_of - interval '60 seconds'),
  count(*) FILTER (WHERE usable AND occurred_at >= as_of - interval '60 seconds'),
  count(*) FILTER (WHERE usable AND output_tokens_quality = 'estimated' AND occurred_at >= as_of - interval '60 seconds')
FROM m
GROUP BY GROUPING SETS ((as_of, date_bin('1 minute', occurred_at, TIMESTAMPTZ '2000-01-01 00:00:00+00')), (as_of))
ORDER BY is_total, bucket`

// Speed returns the current window and the minute series.
func (s *Postgres) Speed(ctx context.Context, params SpeedParams) (SpeedRow, error) {
	var asOf any
	if params.AsOf != nil {
		asOf = *params.AsOf
	}
	args := append([]any{params.WorkspaceID, asOf}, filterArgs(params.Filter)...)
	rows, err := s.pool.Query(ctx, speedQuery, args...)
	if err != nil {
		return SpeedRow{}, database.SafeStoreError("request speed", err)
	}
	defer rows.Close()
	var out SpeedRow
	for rows.Next() {
		var (
			at                  time.Time
			bucket              *time.Time
			isTotal             int32
			allOut, allDur      *string
			events, meas, estim int64
			winOut, winDur      *string
			wEvents, wMeas, wEs int64
		)
		if err := rows.Scan(&at, &bucket, &isTotal, &allOut, &allDur, &events, &meas, &estim, &winOut, &winDur, &wEvents, &wMeas, &wEs); err != nil {
			return SpeedRow{}, database.SafeStoreError("scan request speed", err)
		}
		out.AsOf = at
		if isTotal == 1 {
			out.Current = SpeedSums{OutputTokens: winOut, DurationMS: winDur, Events: wEvents, Measured: wMeas, Estimated: wEs}
			continue
		}
		if bucket != nil && meas > 0 {
			out.Points = append(out.Points, SpeedPoint{BucketStart: *bucket, SpeedSums: SpeedSums{OutputTokens: allOut, DurationMS: allDur, Events: events, Measured: meas, Estimated: estim}})
		}
	}
	if err := rows.Err(); err != nil {
		return SpeedRow{}, database.SafeStoreError("request speed", err)
	}
	return out, nil
}

// Rate aggregates the rolling 60 second window.
func (s *Postgres) Rate(ctx context.Context, params RateParams) (RateRow, error) {
	var asOf any
	if params.AsOf != nil {
		asOf = *params.AsOf
	}
	args := append([]any{params.WorkspaceID, asOf}, filterArgs(params.Filter)...)
	var r RateRow
	if err := s.pool.QueryRow(ctx, rateQuery, args...).Scan(&r.AsOf, &r.Sum, &r.Events, &r.Known, &r.Estimated, &r.Missing, &r.LastReceived,
		&r.SpeedAvg, &r.SpeedSamples, &r.TTFTAvg, &r.TTFTSamples); err != nil {
		return RateRow{}, database.SafeStoreError("request rate", err)
	}
	return r, nil
}

// CountProjects counts how many of ids are projects of the workspace.
func (s *Postgres) CountProjects(ctx context.Context, workspaceID pgtype.UUID, ids []pgtype.UUID) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM projects WHERE workspace_id = $1 AND id = ANY($2::uuid[])`, workspaceID, ids).Scan(&n); err != nil {
		return 0, database.SafeStoreError("count projects", err)
	}
	return n, nil
}
