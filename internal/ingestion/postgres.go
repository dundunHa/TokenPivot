package ingestion

import (
	"context"
	"errors"
	"time"

	"github.com/dundunHa/TokenPivot/internal/aggregation"
	"github.com/dundunHa/TokenPivot/internal/credentials"
	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// projectLookup is the ownership query one prefetch hint asks for.
type projectLookup struct {
	workspaceID credentials.ID
	projectID   credentials.ID
}

// projectCheckResult is the ownership answer read from a prefetched round trip and
// held until ProjectExists consumes it.
type projectCheckResult struct {
	lookup projectLookup
	exists bool
	err    error
}

// sampledTime is a server time read from the second statement of a pipelined write
// and held until the following Now consumes it.
type sampledTime struct {
	at  time.Time
	err error
}

// postgresTx is the per-item transaction wrapper. Each method runs in the caller's
// transaction, never commits on its own, and merges its own statement with the
// adjacent independent statement into one pipelined round trip. The pending and
// buffered fields hold the pipelined answers until their consumer asks for them.
type postgresTx struct {
	tx              pgx.Tx
	pendingProject  *projectLookup
	bufferedProject *projectCheckResult
	sampledNow      *sampledTime
}

// PrefetchProject announces that the next server time sample may be sent together
// with this ownership query. It only records the hint: the query is queued by Now,
// so a window that ends the item early never adds it to the batch.
func (t *postgresTx) PrefetchProject(workspaceID, projectID credentials.ID) {
	t.pendingProject = &projectLookup{workspaceID: workspaceID, projectID: projectID}
	t.bufferedProject = nil
}

// Now reads the server time from the database. It is a statement, not a
// transaction-start constant, so a wait on a lock is visible to the next sample.
// A pending project prefetch turns this sample and the ownership query into one
// round trip; a time already sampled by the previous write is returned without any
// round trip.
func (t *postgresTx) Now(ctx context.Context) (time.Time, error) {
	if sampled := t.sampledNow; sampled != nil {
		t.sampledNow = nil
		return sampled.at, sampled.err
	}
	if t.pendingProject != nil {
		lookup := *t.pendingProject
		t.pendingProject = nil
		var batch pgx.Batch
		batch.Queue(nowQuery)
		batch.Queue(projectExistsQuery, lookup.workspaceID, lookup.projectID)
		br := t.tx.SendBatch(ctx, &batch)
		defer func() { _ = br.Close() }()

		var now time.Time
		if err := br.QueryRow().Scan(&now); err != nil {
			return time.Time{}, database.SafeError("ingestion server time", err)
		}
		// The ownership answer is buffered, including its failure: an item that the
		// window already ended must report expired or FUTURE_EVENT, never a project
		// lookup error.
		var found int
		switch err := br.QueryRow().Scan(&found); {
		case err == nil:
			t.bufferedProject = &projectCheckResult{lookup: lookup, exists: true}
		case errors.Is(err, pgx.ErrNoRows):
			t.bufferedProject = &projectCheckResult{lookup: lookup}
		default:
			t.bufferedProject = &projectCheckResult{lookup: lookup, err: database.SafeError("ingestion project", err)}
		}
		if closeErr := br.Close(); closeErr != nil && t.bufferedProject.err == nil {
			return time.Time{}, database.SafeError("ingestion server time", closeErr)
		}
		return now, nil
	}
	var now time.Time
	if err := t.tx.QueryRow(ctx, nowQuery).Scan(&now); err != nil {
		return time.Time{}, database.SafeError("ingestion server time", err)
	}
	return now, nil
}

// ProjectExists reports whether the project belongs to the workspace. Archived
// projects still belong to it: archiving does not change the event's business
// identity. A prefetched answer for exactly this pair is used as is; every other
// call runs the query on its own.
func (t *postgresTx) ProjectExists(ctx context.Context, workspaceID, projectID credentials.ID) (bool, error) {
	t.pendingProject = nil
	if buffered := t.bufferedProject; buffered != nil {
		t.bufferedProject = nil
		if buffered.lookup.workspaceID == workspaceID && buffered.lookup.projectID == projectID {
			return buffered.exists, buffered.err
		}
	}
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
// already exists; any other failure is a real storage failure. The clock sample
// queued right behind the insert therefore observes the server time after the
// insert's lock wait, not before it.
func (t *postgresTx) InsertEvent(ctx context.Context, event storedEvent) (bool, error) {
	t.sampledNow = nil
	var batch pgx.Batch
	batch.Queue(insertEventQuery, eventArgs(event)...)
	batch.Queue(nowQuery)
	br := t.tx.SendBatch(ctx, &batch)
	defer func() { _ = br.Close() }()

	inserted := true
	var found int
	err := br.QueryRow().Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING RETURNING yields no row when the identity exists.
		inserted = false
	} else if err != nil {
		return false, database.SafeError("ingestion insert event", err)
	}
	t.captureSampledNow(br)
	return inserted, nil
}

// ExistingEvent reads the stored comparison basis of one identity, if any. The
// clock sample of the same round trip feeds the window re-check that follows the
// read.
func (t *postgresTx) ExistingEvent(ctx context.Context, workspaceID, sourceID credentials.ID, sourceEventID string) (storedEvent, bool, error) {
	t.sampledNow = nil
	var batch pgx.Batch
	batch.Queue(existingEventQuery, workspaceID, sourceID, sourceEventID)
	batch.Queue(nowQuery)
	br := t.tx.SendBatch(ctx, &batch)
	defer func() { _ = br.Close() }()

	var existing storedEvent
	found := true
	err := br.QueryRow().Scan(&existing.NormalizationVersion, &existing.PayloadHash)
	if errors.Is(err, pgx.ErrNoRows) {
		existing = storedEvent{}
		found = false
	} else if err != nil {
		return storedEvent{}, false, database.SafeError("ingestion existing event", err)
	}
	t.captureSampledNow(br)
	return existing, found, nil
}

// ApplyMinute writes the minute increment through the aggregation package, inside
// this same transaction, so both writes commit or roll back together. The write is
// pipelined with the following clock sample by minuteBatchTx.
func (t *postgresTx) ApplyMinute(ctx context.Context, input aggregation.MinuteInput) error {
	t.sampledNow = nil
	return aggregation.ApplyRequest(ctx, &minuteBatchTx{Tx: t.tx, parent: t}, input)
}

// minuteBatchTx is the aggregation writer's transaction view for one item: it
// forwards every method to the item transaction and turns its single Exec into a
// pipelined round trip that also samples the server time after the statement.
type minuteBatchTx struct {
	pgx.Tx
	parent *postgresTx
}

// Exec runs the minute upsert and the clock sample as one round trip. Any failure
// of the statement is returned as is, so the aggregation package still owns its
// stage name; the sample is only read once the statement itself succeeded.
func (m *minuteBatchTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	var batch pgx.Batch
	batch.Queue(sql, arguments...)
	batch.Queue(nowQuery)
	br := m.Tx.SendBatch(ctx, &batch)
	defer func() { _ = br.Close() }()
	tag, err := br.Exec()
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	m.parent.captureSampledNow(br)
	return tag, nil
}

// captureSampledNow reads the clock sample queued behind a pipelined statement and
// holds its value or its sanitized error for the next Now.
func (t *postgresTx) captureSampledNow(br pgx.BatchResults) {
	var now time.Time
	if err := br.QueryRow().Scan(&now); err != nil {
		t.sampledNow = &sampledTime{err: database.SafeError("ingestion server time", err)}
		_ = br.Close()
		return
	}
	if err := br.Close(); err != nil {
		t.sampledNow = &sampledTime{err: database.SafeError("ingestion server time", err)}
		return
	}
	t.sampledNow = &sampledTime{at: now}
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
	_ store             = (*Postgres)(nil)
	_ itemTx            = (*postgresTx)(nil)
	_ projectPrefetcher = (*postgresTx)(nil)
)
