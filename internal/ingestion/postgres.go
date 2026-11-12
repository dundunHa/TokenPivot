package ingestion

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dundunHa/TokenPivot/internal/aggregation"
	"github.com/dundunHa/TokenPivot/internal/credentials"
	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
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

	// projectByKeyQuery reads a project by its reserved key, archived rows included.
	projectByKeyQuery = `SELECT id FROM projects WHERE workspace_id = $1 AND project_key = $2`

	// createProjectQuery creates the project unless the workspace is at the limit or the key
	// was just created by a concurrent item (DO NOTHING: the caller reads it again). The name
	// starts as the key and can be renamed later through the management API.
	createProjectQuery = `INSERT INTO projects (workspace_id, project_key, name)
SELECT $1, $2, $2 WHERE (SELECT count(*) FROM projects WHERE workspace_id = $1) < $3
ON CONFLICT (workspace_id, project_key) DO NOTHING
RETURNING id`

	// insertEventQuery relies on the identity unique constraint as the only
	// duplicate arbiter: no read-before-write and no overwrite. RETURNING tells the
	// caller whether a row was actually inserted. The two client observed timings
	// are nullable and carry no default, so an unknown timing stays NULL.
	insertEventQuery = `INSERT INTO usage_events (
    workspace_id, source_id, source_event_id, project_id, provider, model, occurred_at, received_at,
    normalization_version, payload_hash,
    input_tokens, input_tokens_quality, output_tokens, output_tokens_quality,
    total_tokens, total_tokens_quality, cache_read_tokens, cache_read_tokens_quality,
    cache_write_tokens, cache_write_tokens_quality, reasoning_tokens, reasoning_tokens_quality,
    status, duration_ms, generation_duration_ms, ttft_ms
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26)
ON CONFLICT ON CONSTRAINT uk_usage_events_workspace_id_source_id_source_event_id DO NOTHING
RETURNING 1` //nolint:lll // the SQL is written out column by column on purpose

	// existingEventQuery reads the stored comparison basis by its identity in its
	// own statement. Under Read Committed the INSERT snapshot cannot be assumed to
	// see a row another transaction just committed.
	//
	// It reads every retained comparison column, not just the stored version and
	// digest: a v2 row's digest is not comparable with a v1 projection, so the
	// common version's digest has to be rebuilt from these columns.
	existingEventQuery = `SELECT normalization_version, payload_hash,
    occurred_at, project_id, provider, model,
    input_tokens, input_tokens_quality, output_tokens, output_tokens_quality,
    total_tokens, total_tokens_quality, cache_read_tokens, cache_read_tokens_quality,
    cache_write_tokens, cache_write_tokens_quality, reasoning_tokens, reasoning_tokens_quality,
    status, duration_ms, generation_duration_ms, ttft_ms
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

// sampledTime is a server time read from the second statement of a pipelined write
// and held until the following Now consumes it.
type sampledTime struct {
	at  time.Time
	err error
}

// pipelinedBatch groups independent statements of one item transaction into a single
// wire-protocol batch. Each statement is encoded and appended as Parse + Bind +
// Describe + Execute, and exec appends the one Sync that ends the group, so the whole
// group costs exactly one network round trip on a cold connection and on a warm one.
//
// It is deliberately not pgx.Batch: that path prepares a statement before binding it
// and therefore pays a second round trip while the statement cache is cold, and it
// aborts the whole group when a later statement fails to prepare. Here the server
// resolves and runs the statements strictly in order, so a later statement's failure
// can never suppress an earlier statement's already produced result.
//
// Every statement is unnamed. That skips the statement cache entirely, which is what
// keeps the round-trip count independent of connection age.
type pipelinedBatch struct {
	conn    *pgx.Conn
	typeMap *pgtype.Map
	eqb     pgx.ExtendedQueryBuilder
	batch   pgconn.Batch
}

// newPipelinedBatch prepares a batch on the connection the item transaction owns.
func newPipelinedBatch(tx pgx.Tx) *pipelinedBatch {
	conn := tx.Conn()
	return &pipelinedBatch{conn: conn, typeMap: conn.TypeMap()}
}

// add encodes one statement with its arguments and appends it to the group. A
// statement that cannot be encoded is left out, so the caller decides whether the
// rest of the group is still worth sending.
func (b *pipelinedBatch) add(sql string, args ...any) error {
	if err := b.eqb.Build(b.typeMap, nil, args); err != nil {
		return err
	}
	b.batch.ExecParams(sql, b.eqb.ParamValues, nil, b.eqb.ParamFormats, b.eqb.ResultFormats)
	return nil
}

// exec writes the encoded group with its single Sync. The caller must close the
// reader: closing drains every remaining message up to ReadyForQuery, so a failure
// on one statement never leaves the connection busy for the transaction's rollback.
func (b *pipelinedBatch) exec(ctx context.Context) *pgconn.MultiResultReader {
	return b.conn.PgConn().ExecBatch(ctx, &b.batch)
}

// readBatchRow reads the single row of the next statement result of a pipelined
// batch into dest. pgx.ErrNoRows reports a statement that produced no row; any other
// error reports a statement that failed, including the statement that failed before
// its own result began.
func readBatchRow(mrr *pgconn.MultiResultReader, typeMap *pgtype.Map, dest ...any) error {
	if !mrr.NextResult() {
		if err := mrr.Close(); err != nil {
			return err
		}
		return errors.New("no more results in batch")
	}
	rr := mrr.ResultReader()
	if !rr.NextRow() {
		if _, err := rr.Close(); err != nil {
			return err
		}
		return pgx.ErrNoRows
	}
	fields := rr.FieldDescriptions()
	values := rr.Values()
	if len(fields) != len(dest) || len(values) != len(dest) {
		_, _ = rr.Close()
		return fmt.Errorf("result has %d columns, want %d", len(values), len(dest))
	}
	for i := range dest {
		if err := typeMap.Scan(fields[i].DataTypeOID, fields[i].Format, values[i], dest[i]); err != nil {
			_, _ = rr.Close()
			return err
		}
	}
	_, err := rr.Close()
	return err
}

// readBatchExec reads the command tag of the next statement result of a pipelined
// batch. A statement that failed reports its own error here.
func readBatchExec(mrr *pgconn.MultiResultReader) (pgconn.CommandTag, error) {
	if !mrr.NextResult() {
		if err := mrr.Close(); err != nil {
			return pgconn.CommandTag{}, err
		}
		return pgconn.CommandTag{}, errors.New("no more results in batch")
	}
	return mrr.ResultReader().Close()
}

// postgresTx is the per-item transaction wrapper. Each method runs in the caller's
// transaction and never commits on its own. The three write methods merge their own
// statement with the adjacent independent clock sample into one pipelined round
// trip; sampledNow holds that sample until the following Now consumes it.
type postgresTx struct {
	tx         pgx.Tx
	sampledNow *sampledTime
}

// Now reads the server time from the database. It is a statement, not a
// transaction-start constant, so a wait on a lock is visible to the next sample. A
// time already sampled by the previous write is returned without any round trip.
//
// The first sample of an item is always its own statement: merging it with the
// ownership query would make an expired or future event wait for that query, and
// the window verdict must be decided before any other statement runs.
func (t *postgresTx) Now(ctx context.Context) (time.Time, error) {
	if sampled := t.sampledNow; sampled != nil {
		t.sampledNow = nil
		return sampled.at, sampled.err
	}
	var now time.Time
	// A single statement runs in the same unnamed extended-query mode as a pipelined
	// group: the default mode prepares a statement before binding it, which costs a
	// second round trip while the connection's statement cache is still cold.
	if err := t.tx.QueryRow(ctx, nowQuery, pgx.QueryExecModeExec).Scan(&now); err != nil {
		return time.Time{}, database.SafeError("ingestion server time", err)
	}
	return now, nil
}

// ProjectExists reports whether the project belongs to the workspace. Archived
// projects still belong to it: archiving does not change the event's business
// identity. It runs as its own statement, so it never delays the window verdict
// that the caller has already decided.
func (t *postgresTx) ProjectExists(ctx context.Context, workspaceID, projectID credentials.ID) (bool, error) {
	var found int
	// Same unnamed mode as the pipelined group, so a cold connection does not pay a
	// separate prepare round trip here.
	if err := t.tx.QueryRow(ctx, projectExistsQuery, pgx.QueryExecModeExec, workspaceID, projectID).Scan(&found); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, database.SafeError("ingestion project", err)
	}
	return true, nil
}

// EnsureProject finds or creates the project with this key. Three outcomes: found (the
// common case), created, or the workspace is full and the key is new. A concurrent item that
// creates the same key between the read and the insert is covered by DO NOTHING plus a
// second read.
func (t *postgresTx) EnsureProject(ctx context.Context, workspaceID credentials.ID, projectKey string) (credentials.ID, bool, error) {
	find := func() (credentials.ID, bool, error) {
		var id credentials.ID
		err := t.tx.QueryRow(ctx, projectByKeyQuery, pgx.QueryExecModeExec, workspaceID, projectKey).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return credentials.ID{}, false, nil
		}
		if err != nil {
			return credentials.ID{}, false, database.SafeError("ingestion project key", err)
		}
		return id, true, nil
	}
	if id, found, err := find(); err != nil || found {
		return id, found, err
	}
	var id credentials.ID
	err := t.tx.QueryRow(ctx, createProjectQuery, pgx.QueryExecModeExec, workspaceID, projectKey, MaxAutoProjects).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return credentials.ID{}, false, database.SafeError("ingestion project create", err)
	}
	// No row: a concurrent item created the key, or the workspace is full.
	return find()
}

// InsertEvent inserts the detail row once. A false result means this identity
// already exists; any other failure is a real storage failure. The clock sample
// queued right behind the insert therefore observes the server time after the
// insert's lock wait, not before it.
func (t *postgresTx) InsertEvent(ctx context.Context, event storedEvent) (bool, error) {
	t.sampledNow = nil
	batch := newPipelinedBatch(t.tx)
	if err := batch.add(insertEventQuery, eventArgs(event)...); err != nil {
		return false, database.SafeError("ingestion insert event", err)
	}
	if err := batch.add(nowQuery); err != nil {
		return false, database.SafeError("ingestion server time", err)
	}
	mrr := batch.exec(ctx)
	defer func() { _ = mrr.Close() }()

	inserted := true
	var found int
	err := readBatchRow(mrr, batch.typeMap, &found)
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING RETURNING yields no row when the identity exists.
		inserted = false
	} else if err != nil {
		return false, database.SafeError("ingestion insert event", err)
	}
	t.captureSampledNow(mrr, batch.typeMap)
	return inserted, nil
}

// ExistingEvent reads the stored comparison basis of one identity, if any. The
// whole retained comparison content is loaded so a cross-version comparison can
// rebuild the common version's digest without a second statement. The clock
// sample of the same round trip feeds the window re-check that follows the read.
func (t *postgresTx) ExistingEvent(ctx context.Context, workspaceID, sourceID credentials.ID, sourceEventID string) (storedEvent, bool, error) {
	t.sampledNow = nil
	batch := newPipelinedBatch(t.tx)
	if err := batch.add(existingEventQuery, workspaceID, sourceID, sourceEventID); err != nil {
		return storedEvent{}, false, database.SafeError("ingestion existing event", err)
	}
	if err := batch.add(nowQuery); err != nil {
		return storedEvent{}, false, database.SafeError("ingestion server time", err)
	}
	mrr := batch.exec(ctx)
	defer func() { _ = mrr.Close() }()

	var (
		existing                            storedEvent
		inputValue, outputValue             *int64
		totalValue, cacheReadValue          *int64
		cacheWriteValue, reasoningValue     *int64
		inputQuality, outputQuality         string
		totalQuality, cacheReadQuality      string
		cacheWriteQuality, reasoningQuality string
	)
	found := true
	err := readBatchRow(mrr, batch.typeMap,
		&existing.NormalizationVersion, &existing.PayloadHash,
		&existing.OccurredAt, &existing.ProjectID, &existing.Provider, &existing.Model,
		&inputValue, &inputQuality, &outputValue, &outputQuality,
		&totalValue, &totalQuality, &cacheReadValue, &cacheReadQuality,
		&cacheWriteValue, &cacheWriteQuality, &reasoningValue, &reasoningQuality,
		&existing.Status, &existing.DurationMS, &existing.GenerationDurationMS, &existing.TTFTMS,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		existing = storedEvent{}
		found = false
	} else if err != nil {
		return storedEvent{}, false, database.SafeError("ingestion existing event", err)
	}
	if found {
		existing.Usage = usage{
			Input:      metricFrom(inputValue, inputQuality),
			Output:     metricFrom(outputValue, outputQuality),
			Total:      metricFrom(totalValue, totalQuality),
			CacheRead:  metricFrom(cacheReadValue, cacheReadQuality),
			CacheWrite: metricFrom(cacheWriteValue, cacheWriteQuality),
			Reasoning:  metricFrom(reasoningValue, reasoningQuality),
		}
	}
	t.captureSampledNow(mrr, batch.typeMap)
	return existing, found, nil
}

// metricFrom rebuilds one stored metric from its retained value and quality
// columns. A NULL value is the missing quality's value, never a real zero.
func metricFrom(value *int64, quality string) aggregation.Metric {
	return aggregation.Metric{Value: value, Quality: aggregation.Quality(quality)}
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
	batch := newPipelinedBatch(m.Tx)
	if err := batch.add(sql, arguments...); err != nil {
		return pgconn.CommandTag{}, err
	}
	if err := batch.add(nowQuery); err != nil {
		return pgconn.CommandTag{}, err
	}
	mrr := batch.exec(ctx)
	defer func() { _ = mrr.Close() }()
	tag, err := readBatchExec(mrr)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	m.parent.captureSampledNow(mrr, batch.typeMap)
	return tag, nil
}

// captureSampledNow reads the clock sample queued behind a pipelined statement and
// holds its value or its sanitized error for the next Now.
func (t *postgresTx) captureSampledNow(mrr *pgconn.MultiResultReader, typeMap *pgtype.Map) {
	var now time.Time
	if err := readBatchRow(mrr, typeMap, &now); err != nil {
		t.sampledNow = &sampledTime{err: database.SafeError("ingestion server time", err)}
		_ = mrr.Close()
		return
	}
	if err := mrr.Close(); err != nil {
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
		event.Status, event.DurationMS, event.GenerationDurationMS, event.TTFTMS,
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
