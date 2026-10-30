package usage

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// metricNames are the metric column prefixes of usage_minute_stats, in the order
// of RawStats.Metrics.
var metricNames = [metricCount]string{"input_tokens", "output_tokens", "total_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"}

// statColumns lists the 25 summed columns of one statistics tuple: request_count,
// then sum, known, estimated and missing for every metric. The names are fixed
// code constants; no request text is ever added to them.
var statColumns = func() []string {
	columns := []string{"event_count"}
	for _, name := range metricNames {
		columns = append(columns, name+"_sum", name+"_known_count", name+"_estimated_count", name+"_missing_count")
	}
	return columns
}()

// rawColumns are the output names of the summed columns; request_count replaces
// event_count so the SQL and the Go scan agree.
func rawColumns() []string {
	columns := append([]string(nil), statColumns...)
	columns[0] = "request_count"
	return columns
}

// filterClause is shared by both statements. A NULL array leaves a dimension
// unfiltered; the project filter is the UUID list OR (project_id IS NULL when
// unassigned was requested). Parameters: $4 providers, $5 models, $6 projects,
// $7 unassigned. Every statement also pins workspace_id = $1 and the half-open
// minute range [$2,$3).
const filterClause = `m.workspace_id = $1
  AND m.bucket_start >= $2 AND m.bucket_start < $3
  AND ($4::text[] IS NULL OR m.provider = ANY($4::text[]))
  AND ($5::text[] IS NULL OR m.model = ANY($5::text[]))
  AND (($6::uuid[] IS NULL AND NOT $7::boolean)
       OR m.project_id = ANY($6::uuid[])
       OR ($7::boolean AND m.project_id IS NULL))`

// sumList renders "sum(m.c) AS name" for every statistics column.
func sumList() string {
	columns, names := statColumns, rawColumns()
	parts := make([]string, len(columns))
	for i, column := range columns {
		parts[i] = "sum(m." + column + ") AS " + names[i]
	}
	return strings.Join(parts, ", ")
}

// textList renders "prefix+name::text" for every statistics column.
func textList(prefix string, window bool) string {
	names := rawColumns()
	parts := make([]string, len(names))
	for i, name := range names {
		expr := prefix + name
		if window {
			expr = "sum(" + expr + ") OVER ()"
		}
		parts[i] = "(" + expr + ")::text"
	}
	return strings.Join(parts, ", ")
}

// aggregateQuery sums the filtered minute rows into the caller's precomputed
// buckets and, in the same statement, the grand total over those buckets. Bucket
// membership comes only from the bound arrays; there is no date_trunc and no
// session time zone. Parameters $8..$10 are the bucket ordinals, starts and ends.
var aggregateQuery = `WITH bounds AS (
  SELECT b.ord, b.s, b.e FROM unnest($8::int4[], $9::timestamptz[], $10::timestamptz[]) AS b(ord, s, e)
), per_bucket AS (
  SELECT bounds.ord, ` + sumList() + `
  FROM bounds
  JOIN usage_minute_stats m
    ON m.bucket_start >= bounds.s AND m.bucket_start < bounds.e
  WHERE ` + filterClause + `
  GROUP BY bounds.ord
)
SELECT ord, ` + textList("per_bucket.", false) + `, ` + textList("per_bucket.", true) + `
FROM per_bucket
ORDER BY ord`

// groupExpressions returns, for each ranking dimension, the two key columns and
// the GROUP BY list. The dimension comes from the GroupBy enum, never from text.
func groupExpressions(groupBy GroupBy) (key1, key2, groupList string, err error) {
	switch groupBy {
	case GroupProject:
		return "m.project_id::text", "NULL::text", "m.project_id", nil
	case GroupProvider:
		return "m.provider", "NULL::text", "m.provider", nil
	case GroupModel:
		return "m.provider", "m.model", "m.provider, m.model", nil
	}
	return "", "", "", errors.New("usage: unknown group dimension")
}

// rankingsQuery groups the filtered rows, ranks them (total descending, then the
// keys in C collation with NULL last) and returns in one statement the page
// rows, the aggregate of every later group and the overall aggregate. Parameters
// $8 and $9 are the zero-based offset and offset+limit.
func rankingsQuery(groupBy GroupBy) (string, error) {
	key1, key2, groupList, err := groupExpressions(groupBy)
	if err != nil {
		return "", err
	}
	names := rawColumns()
	plain := make([]string, len(names))
	totals := make([]string, len(names))
	for i, name := range names {
		plain[i] = "(" + name + ")::text"
		totals[i] = "(sum(" + name + "))::text"
	}
	zeroed := make([]string, len(names))
	for i, name := range names {
		zeroed[i] = "(COALESCE(sum(" + name + "), 0))::text"
	}
	return `WITH grouped AS (
  SELECT ` + key1 + ` AS g1, ` + key2 + ` AS g2, ` + sumList() + `
  FROM usage_minute_stats m
  WHERE ` + filterClause + `
  GROUP BY ` + groupList + `
), ranked AS (
  SELECT grouped.*, row_number() OVER (ORDER BY total_tokens_sum DESC, g1 COLLATE "C" ASC NULLS LAST, g2 COLLATE "C" ASC NULLS LAST) AS rn
  FROM grouped
)
SELECT 'group'::text AS kind, rn, g1, g2, ` + strings.Join(plain, ", ") + `
FROM ranked WHERE rn > $8::bigint AND rn <= $9::bigint
UNION ALL
SELECT 'other'::text, 0::bigint, NULL::text, NULL::text, ` + strings.Join(totals, ", ") + `
FROM ranked WHERE rn > $9::bigint HAVING count(*) > 0
UNION ALL
SELECT 'overall'::text, 0::bigint, NULL::text, NULL::text, ` + strings.Join(zeroed, ", ") + `
FROM ranked
ORDER BY kind, rn`, nil
}

// optionsQuery lists the distinct provider and model values of the workspace in
// [$2,$3). The kind column keeps the two independent dimensions in one statement.
const optionsQuery = `SELECT kind, v FROM (
  SELECT 'provider'::text AS kind, v FROM (
    SELECT DISTINCT provider AS v FROM usage_minute_stats
    WHERE workspace_id = $1 AND bucket_start >= $2 AND bucket_start < $3 AND provider <> ''
  ) p
  UNION ALL
  SELECT 'model'::text, v FROM (
    SELECT DISTINCT model AS v FROM usage_minute_stats
    WHERE workspace_id = $1 AND bucket_start >= $2 AND bucket_start < $3 AND model <> ''
  ) m
) o
ORDER BY kind, v COLLATE "C"`

// querier is the read subset of a pool.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Postgres is the pool-bound Store. It only reads usage_minute_stats.
type Postgres struct{ q querier }

// NewPostgres returns the store on the caller-owned pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{q: pool} }

// filterArgs returns $4..$7. Empty lists become NULL so the dimension is unfiltered.
func filterArgs(f Filter) []any {
	var providers, models []string
	if len(f.Providers) > 0 {
		providers = f.Providers
	}
	if len(f.Models) > 0 {
		models = f.Models
	}
	var projects any
	if len(f.Projects) > 0 {
		projects = f.Projects
	}
	return []any{providers, models, projects, f.Unassigned}
}

// scanStats points at the 25 text columns of one RawStats.
func scanStats(s *RawStats) []any {
	out := []any{&s.RequestCount}
	for i := range s.Metrics {
		m := &s.Metrics[i]
		out = append(out, &m.Sum, &m.Known, &m.Estimated, &m.Missing)
	}
	return out
}

// Aggregate returns the non-empty buckets and the total of one statement.
func (p *Postgres) Aggregate(ctx context.Context, params AggregateParams) (AggregateRows, error) {
	ords := make([]int32, len(params.Buckets))
	starts := make([]time.Time, len(params.Buckets))
	ends := make([]time.Time, len(params.Buckets))
	for i, b := range params.Buckets {
		ords[i], starts[i], ends[i] = int32(i), b.Start, b.End
	}
	args := append([]any{params.WorkspaceID, params.Start, params.End}, filterArgs(params.Filter)...)
	args = append(args, ords, starts, ends)
	rows, err := p.q.Query(ctx, aggregateQuery, args...)
	if err != nil {
		return AggregateRows{}, database.SafeError("usage aggregate", err)
	}
	defer rows.Close()
	var result AggregateRows
	for rows.Next() {
		var ord int32
		var point RawPoint
		var overall RawStats
		dest := append(append([]any{&ord}, scanStats(&point.Stats)...), scanStats(&overall)...)
		if err := rows.Scan(dest...); err != nil {
			return AggregateRows{}, database.SafeError("usage aggregate scan", err)
		}
		point.Index = int(ord)
		result.Points = append(result.Points, point)
		if result.Overall == nil {
			o := overall
			result.Overall = &o
		}
	}
	if err := rows.Err(); err != nil {
		return AggregateRows{}, database.SafeError("usage aggregate rows", err)
	}
	return result, nil
}

// Rankings returns the page, the remainder and the overall aggregate of one
// statement.
func (p *Postgres) Rankings(ctx context.Context, params RankingsParams) (RankingsRows, error) {
	query, err := rankingsQuery(params.GroupBy)
	if err != nil {
		return RankingsRows{}, err
	}
	args := append([]any{params.WorkspaceID, params.Start, params.End}, filterArgs(params.Filter)...)
	args = append(args, int64(params.Offset), int64(params.Offset+params.Limit))
	rows, err := p.q.Query(ctx, query, args...)
	if err != nil {
		return RankingsRows{}, database.SafeError("usage rankings", err)
	}
	defer rows.Close()
	var result RankingsRows
	sawOverall := false
	for rows.Next() {
		var kind string
		var rank int64
		var group RawGroup
		dest := append([]any{&kind, &rank, &group.Key1, &group.Key2}, scanStats(&group.Stats)...)
		if err := rows.Scan(dest...); err != nil {
			return RankingsRows{}, database.SafeError("usage rankings scan", err)
		}
		switch kind {
		case "group":
			result.Groups = append(result.Groups, group)
		case "other":
			stats := group.Stats
			result.Other = &stats
		case "overall":
			result.Overall = group.Stats
			sawOverall = true
		}
	}
	if err := rows.Err(); err != nil {
		return RankingsRows{}, database.SafeError("usage rankings rows", err)
	}
	if !sawOverall {
		return RankingsRows{}, errors.New("usage rankings: overall row missing")
	}
	return result, nil
}

// Options returns the distinct provider and model values in the window.
func (p *Postgres) Options(ctx context.Context, params OptionsParams) (OptionsRows, error) {
	rows, err := p.q.Query(ctx, optionsQuery, params.WorkspaceID, params.Start, params.End)
	if err != nil {
		return OptionsRows{}, database.SafeError("usage options", err)
	}
	defer rows.Close()
	var result OptionsRows
	for rows.Next() {
		var kind, value string
		if err := rows.Scan(&kind, &value); err != nil {
			return OptionsRows{}, database.SafeError("usage options scan", err)
		}
		switch kind {
		case "provider":
			result.Providers = append(result.Providers, value)
		case "model":
			result.Models = append(result.Models, value)
		}
	}
	if err := rows.Err(); err != nil {
		return OptionsRows{}, database.SafeError("usage options rows", err)
	}
	return result, nil
}
