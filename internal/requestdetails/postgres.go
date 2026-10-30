package requestdetails

import (
	"context"

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

// eventsQuery pages newest first by (occurred_at, id). $1 workspace, $2 start,
// $3 end, $4..$7 filter, $8 has cursor, $9 cursor time, $10 cursor id, $11 limit+1.
var eventsQuery = `SELECT e.id, e.source_id, e.occurred_at, e.received_at, e.provider, e.model,
  e.project_id, p.project_key, p.name, s.platform, s.instance_key,
  e.input_tokens, e.input_tokens_quality, e.output_tokens, e.output_tokens_quality,
  e.total_tokens, e.total_tokens_quality, e.status, e.duration_ms
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
		return nil, database.SafeError("request events", err)
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var r EventRow
		if err := rows.Scan(&r.ID, &r.SourceID, &r.OccurredAt, &r.ReceivedAt, &r.Provider, &r.Model,
			&r.ProjectID, &r.ProjectKey, &r.ProjectName, &r.Platform, &r.InstanceKey,
			&r.Input.Value, &r.Input.Quality, &r.Output.Value, &r.Output.Quality,
			&r.Total.Value, &r.Total.Quality, &r.Status, &r.DurationMS); err != nil {
			return nil, database.SafeError("scan request event", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, database.SafeError("request events", err)
	}
	return out, nil
}

// rateQuery samples the database statement start once and aggregates the
// half-open window [as_of-60s, as_of) in the same statement. $1 workspace,
// $2 optional fixed as_of, $3..$6 filter.
var rateQuery = `WITH p AS (SELECT COALESCE($2::timestamptz, statement_timestamp()) AS as_of)
SELECT p.as_of,
  (sum(e.total_tokens::numeric) FILTER (WHERE e.total_tokens_quality IN ('known','estimated')))::text,
  count(e.id),
  count(*) FILTER (WHERE e.total_tokens_quality = 'known'),
  count(*) FILTER (WHERE e.total_tokens_quality = 'estimated'),
  count(*) FILTER (WHERE e.total_tokens_quality = 'missing'),
  max(e.received_at)
FROM p
LEFT JOIN usage_events e
  ON e.workspace_id = $1
 AND e.occurred_at >= p.as_of - interval '60 seconds'
 AND e.occurred_at < p.as_of
 ` + filterSQL("$3", "$4", "$5", "$6") + `
GROUP BY p.as_of`

// Rate aggregates the rolling 60 second window.
func (s *Postgres) Rate(ctx context.Context, params RateParams) (RateRow, error) {
	var asOf any
	if params.AsOf != nil {
		asOf = *params.AsOf
	}
	args := append([]any{params.WorkspaceID, asOf}, filterArgs(params.Filter)...)
	var r RateRow
	if err := s.pool.QueryRow(ctx, rateQuery, args...).Scan(&r.AsOf, &r.Sum, &r.Events, &r.Known, &r.Estimated, &r.Missing, &r.LastReceived); err != nil {
		return RateRow{}, database.SafeError("request rate", err)
	}
	return r, nil
}

// CountProjects counts how many of ids are projects of the workspace.
func (s *Postgres) CountProjects(ctx context.Context, workspaceID pgtype.UUID, ids []pgtype.UUID) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM projects WHERE workspace_id = $1 AND id = ANY($2::uuid[])`, workspaceID, ids).Scan(&n); err != nil {
		return 0, database.SafeError("count projects", err)
	}
	return n, nil
}
