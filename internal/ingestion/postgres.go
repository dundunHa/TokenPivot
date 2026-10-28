package ingestion

import (
	"context"
	"errors"
	"time"

	"github.com/dundunHa/TokenPivot/internal/aggregation"
	"github.com/dundunHa/TokenPivot/internal/credentials"
	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SQL used by the ingestion store. Every statement scopes its work by the
// authenticated workspace, so a foreign row is unreachable even without a foreign
// key. No statement is built from request input.
const (
	// nowQuery is clock_timestamp(), not now(): now() is the transaction start
	// constant, which would hide a wait on a lock from the window re-check.
	nowQuery = `SELECT clock_timestamp()`

	projectExistsQuery = `SELECT 1 FROM projects WHERE workspace_id = $1 AND id = $2`

	// insertEventQuery relies on the identity unique constraint as the only
	// duplicate arbiter: no read-before-write and no overwrite. RETURNING tells the
	// caller whether a row was actually inserted.
	insertEventQuery = `INSERT INTO usage_events (
    workspace_id, source_id, source_event_id, project_id, provider, model, occurred_at, received_at,
    normalization_version, payload_hash,
    input_tokens, input_tokens_quality, output_tokens, output_tokens_quality,
    total_tokens, total_tokens_quality, cache_read_tokens, cache_read_tokens_quality,
    cache_write_tokens, cache_write_tokens_quality, reasoning_tokens, reasoning_tokens_quality,
    status, duration_ms
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)
ON CONFLICT ON CONSTRAINT uk_usage_events_workspace_id_source_id_source_event_id DO NOTHING
RETURNING 1` //nolint:lll // the SQL is written out column by column on purpose

	// existingEventQuery reads the stored comparison basis by its identity in its
	// own statement. Under Read Committed the INSERT snapshot cannot be assumed to
	// see a row another transaction just committed. Only the two comparison columns
	// are read: the stored normalization version decides comparability, and the
	// stored digest decides duplicate versus conflict.
	existingEventQuery = `SELECT normalization_version, payload_hash
FROM usage_events WHERE workspace_id = $1 AND source_id = $2 AND source_event_id = $3`

	// touchSourceQuery moves the source stamp forward only. GREATEST keeps an older
	// value from overtaking a newer one when requests finish out of order.
	touchSourceQuery = `UPDATE sources
SET last_received_at = GREATEST(coalesce(last_received_at, clock_timestamp()), clock_timestamp())
WHERE workspace_id = $1 AND id = $2`
)

// Postgres implements the ingestion store on the caller-owned pool. Errors leaving
// this type carry fixed text only: SQL, driver messages, event content and the
// connection string are never part of an error string.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns the ingestion store on the service pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

// BeginItem opens one item transaction at Read Committed, the isolation level the
// duplicate arbitration was designed for.
func (p *Postgres) BeginItem(ctx context.Context) (itemTx, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, database.SafeError("ingestion begin", err)
	}
	return &postgresTx{tx: tx}, nil
}

// TouchSource advances sources.last_received_at for one authenticated workspace and
// source. It runs outside every item transaction; a failure is only logged.
func (p *Postgres) TouchSource(ctx context.Context, workspaceID, sourceID credentials.ID) error {
	if _, err := p.pool.Exec(ctx, touchSourceQuery, workspaceID, sourceID); err != nil {
		return database.SafeError("ingestion source", err)
	}
	return nil
}

// postgresTx is the per-item transaction wrapper. Each method runs one statement in
// the caller's transaction and never commits on its own.
type postgresTx struct {
	tx pgx.Tx
}

// Now reads the server time from the database. It is a statement, not a
// transaction-start constant, so a wait on a lock is visible to the next sample.
func (t *postgresTx) Now(ctx context.Context) (time.Time, error) {
	var now time.Time
	if err := t.tx.QueryRow(ctx, nowQuery).Scan(&now); err != nil {
		return time.Time{}, database.SafeError("ingestion server time", err)
	}
	return now, nil
}

// ProjectExists reports whether the project belongs to the workspace. Archived
// projects still belong to it: archiving does not change the event's business
// identity.
func (t *postgresTx) ProjectExists(ctx context.Context, workspaceID, projectID credentials.ID) (bool, error) {
	var found int
	if err := t.tx.QueryRow(ctx, projectExistsQuery, workspaceID, projectID).Scan(&found); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, database.SafeError("ingestion project", err)
	}
	return true, nil
}

// InsertEvent inserts the detail row once. A false result means this identity
// already exists; any other failure is a real storage failure.
func (t *postgresTx) InsertEvent(ctx context.Context, event storedEvent) (bool, error) {
	var found int
	err := t.tx.QueryRow(ctx, insertEventQuery, eventArgs(event)...).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING RETURNING yields no row when the identity exists.
		return false, nil
	}
	if err != nil {
		return false, database.SafeError("ingestion insert event", err)
	}
	return true, nil
}

// ExistingEvent reads the stored comparison basis of one identity, if any.
func (t *postgresTx) ExistingEvent(ctx context.Context, workspaceID, sourceID credentials.ID, sourceEventID string) (storedEvent, bool, error) {
	var existing storedEvent
	err := t.tx.QueryRow(ctx, existingEventQuery, workspaceID, sourceID, sourceEventID).Scan(
		&existing.NormalizationVersion, &existing.PayloadHash,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return storedEvent{}, false, nil
	}
	if err != nil {
		return storedEvent{}, false, database.SafeError("ingestion existing event", err)
	}
	return existing, true, nil
}

// ApplyMinute writes the minute increment through the aggregation package, inside
// this same transaction, so both writes commit or roll back together.
func (t *postgresTx) ApplyMinute(ctx context.Context, input aggregation.MinuteInput) error {
	return aggregation.ApplyRequest(ctx, t.tx, input)
}

// Commit commits the item. Its error is reported as indeterminate by the caller
// and is sanitized here, so no driver text can reach a log line or a response.
func (t *postgresTx) Commit(ctx context.Context) error {
	if err := t.tx.Commit(ctx); err != nil {
		return database.SafeError("ingestion commit", err)
	}
	return nil
}

// Rollback discards the item. A transaction the driver already finished reports
// pgx.ErrTxClosed, which is not a failure worth surfacing. Every other error is
// sanitized for the same reason as Commit.
func (t *postgresTx) Rollback(ctx context.Context) error {
	err := t.tx.Rollback(ctx)
	if err == nil || errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return database.SafeError("ingestion rollback", err)
}

// eventArgs lists the insert parameters in the column order of insertEventQuery.
func eventArgs(event storedEvent) []any {
	return []any{
		event.WorkspaceID, event.SourceID, event.SourceEventID, event.ProjectID,
		event.Provider, event.Model, event.OccurredAt, event.ReceivedAt,
		event.NormalizationVersion, event.PayloadHash,
		metricValue(event.Usage.Input), string(event.Usage.Input.Quality),
		metricValue(event.Usage.Output), string(event.Usage.Output.Quality),
		metricValue(event.Usage.Total), string(event.Usage.Total.Quality),
		metricValue(event.Usage.CacheRead), string(event.Usage.CacheRead.Quality),
		metricValue(event.Usage.CacheWrite), string(event.Usage.CacheWrite.Quality),
		metricValue(event.Usage.Reasoning), string(event.Usage.Reasoning.Quality),
		event.Status, event.DurationMS,
	}
}

// metricValue turns a metric into its nullable column value: a missing metric is
// NULL, never zero.
func metricValue(metric aggregation.Metric) any {
	if metric.Value == nil || metric.Quality == aggregation.QualityMissing {
		return nil
	}
	return *metric.Value
}

// Ensure the store and transaction wrappers satisfy the narrow interfaces the
// service declares.
var (
	_ store  = (*Postgres)(nil)
	_ itemTx = (*postgresTx)(nil)
)
