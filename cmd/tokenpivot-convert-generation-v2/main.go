// Command tokenpivot-convert-generation-v2 converts stored usage events from the
// retired seven element normalization to the nine element one.
//
// It runs as a check by default and only writes with an explicit --apply. It reads
// its connection from TOKENPIVOT_MIGRATION_DATABASE_URL and never falls back to a
// service role, because the service role has no UPDATE privilege on usage_events by
// design. It never prints a DSN, an event or a credential, and it never runs
// migrations.
//
// The conversion changes exactly two columns of a v1 row, normalization_version and
// payload_hash, and recomputes the digest through the same shared implementation the
// request path uses. Every other column, the minute aggregate and any existing v2
// row must come out of the transaction unchanged; otherwise it rolls back.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dundunHa/TokenPivot/internal/aggregation"
	"github.com/dundunHa/TokenPivot/internal/config"
	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/dundunHa/TokenPivot/internal/ingestion"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
)

const (
	// operationTimeout bounds the whole run: this is a local, one-off operation.
	operationTimeout = 60 * time.Second
	// retiredVersion is the seven element normalization this command converts.
	retiredVersion = "tokenpivot-usage-event/v1"
	// maxConvertRows bounds one transaction. Every converted row is held in memory
	// so the pre-commit check can compare it column by column; a larger set belongs
	// in a deliberate, reviewed run rather than an accidental one.
	maxConvertRows = 100000
	// commitUnknownStatus is the status a caller must treat as "stop writing and
	// verify with a fresh read-only check".
	commitUnknownStatus = "commit_unknown"
	// maxSafeInteger is the largest integer the ingestion API accepts (2^53-1). A
	// stored value outside [0, maxSafeInteger] could not have come from a valid
	// event, so the conversion refuses it instead of hashing it.
	maxSafeInteger int64 = 9007199254740991
)

// exitCommitUnknown is the exit code that distinguishes an unconfirmed COMMIT from
// an ordinary failure, so an operator script cannot read it as success.
const exitCommitUnknown = 2

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := run(ctx, os.Args[1:], logger)
	if err == nil {
		return
	}
	var unknown *commitUnknownError
	if errors.As(err, &unknown) {
		logger.Error("commit result unknown: keep the writers stopped and verify with a fresh read-only check",
			"stage", "conversion", "status", commitUnknownStatus, "err", unknown.cause)
		os.Exit(exitCommitUnknown)
	}
	if errors.Is(err, errHelp) {
		fmt.Fprint(os.Stderr, usage)
		return
	}
	logger.Error("failed", "stage", "conversion", "err", err)
	os.Exit(1)
}

const usage = `usage: tokenpivot-convert-generation-v2 [--apply]

Without arguments the command only checks the stored rows and reports what a
conversion would do. --apply converts every row of the retired version in one
transaction and verifies the result before committing.
`

// errHelp reports a request for the usage text, which is not a failure.
var errHelp = errors.New("help requested")

// commitUnknownError reports a COMMIT whose outcome the server could not confirm. The
// transaction may or may not be applied, so the caller must not report success, must
// not retry the apply automatically and must verify with a new connection.
type commitUnknownError struct{ cause error }

func (e *commitUnknownError) Error() string {
	return "commit result unknown: the transaction may have been applied"
}

func (e *commitUnknownError) Unwrap() error { return e.cause }

// run executes one check or one apply and reports it as structured, content free
// counts.
func run(ctx context.Context, args []string, logger *slog.Logger) (retErr error) {
	apply, err := parseArgs(args)
	if err != nil {
		return err
	}
	cfg, err := config.LoadMigration()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	db, err := openDB(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, database.SafeError("close", db.Close())) }()
	c := &converter{db: db, logger: logger}
	if !apply {
		return c.check(ctx)
	}
	return c.apply(ctx)
}

// parseArgs accepts no argument for a check and exactly --apply for a conversion.
func parseArgs(args []string) (bool, error) {
	switch {
	case len(args) == 0:
		return false, nil
	case len(args) == 1 && args[0] == "--apply":
		return true, nil
	case len(args) == 1 && (args[0] == "--help" || args[0] == "-h"):
		return false, errHelp
	default:
		return false, errors.New("usage: tokenpivot-convert-generation-v2 [--apply]")
	}
}

// openDB opens one connection with the same conservative timeouts as the migration
// command, so a stuck lock or statement fails fast instead of holding writers.
func openDB(ctx context.Context, value string) (*sql.DB, error) {
	if err := database.ValidateURL(value); err != nil {
		return nil, err
	}
	params, err := pgx.ParseConfig(value)
	if err != nil {
		return nil, database.SafeError("parse", err)
	}
	params.ConnectTimeout = database.ConnectTimeout
	params.RuntimeParams["lock_timeout"] = "5s"
	params.RuntimeParams["statement_timeout"] = "30s"
	db := stdlib.OpenDB(*params)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	pingCtx, cancel := context.WithTimeout(ctx, database.ConnectTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		return nil, errors.Join(database.SafeError("ping", err), database.SafeError("close", db.Close()))
	}
	return db, nil
}

// converter owns the two modes over one connection.
type converter struct {
	db     *sql.DB
	logger *slog.Logger
	// commit performs the transaction commit. Production leaves it nil and uses
	// (*sql.Tx).Commit. It is a field because a COMMIT can be applied by the server
	// while the client sees an error, and only a test can reproduce both halves of
	// that state without a fault injecting driver.
	commit func(*sql.Tx) error
}

// commitTx commits through the configured seam, defaulting to the real driver call.
func (c *converter) commitTx(tx *sql.Tx) error {
	if c.commit != nil {
		return c.commit(tx)
	}
	return tx.Commit()
}

// state is the count-only picture of the stored rows, before and after a run.
type state struct {
	V1Rows int64 `json:"v1_rows"`
	V2Rows int64 `json:"v2_rows"`
	Rows   int64 `json:"rows"`
}

// check reports what a conversion would do without writing anything: the preflight,
// every retired row and its recomputed digest are read in one read-only transaction,
// exactly as apply reads them. A stored value the conversion would refuse therefore
// fails here too, with the same zero changes.
func (c *converter) check(ctx context.Context) error {
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return database.SafeError("begin read only", err)
	}
	defer func() { _ = tx.Rollback() }()
	before, err := c.preflight(ctx, tx)
	if err != nil {
		return err
	}
	if before.V1Rows != 0 {
		// loading validates every value/quality pair and recomputes DigestV2 for the
		// same rows apply would rewrite.
		if _, err := c.loadRetiredRows(ctx, tx, before.V1Rows); err != nil {
			return err
		}
	}
	c.logger.Info("report", "mode", "check", "v1_rows", before.V1Rows, "v2_rows", before.V2Rows,
		"rows", before.Rows, "pending_conversion", before.V1Rows, "state", checkState(before))
	return nil
}

// checkState names whether a conversion is needed, in one word.
func checkState(before state) string {
	if before.V1Rows == 0 {
		return "converted"
	}
	return "needs_conversion"
}

// apply converts every retired row in one transaction and verifies the result before
// committing. With no retired row it writes nothing at all.
func (c *converter) apply(ctx context.Context) (retErr error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return database.SafeError("begin", err)
	}
	defer func() {
		if retErr != nil {
			_ = tx.Rollback()
		}
	}()
	before, err := c.preflight(ctx, tx)
	if err != nil {
		return err
	}
	if before.V1Rows == 0 {
		// Nothing to convert: no UPDATE, no backup, no change to any row.
		if err := tx.Rollback(); err != nil {
			return database.SafeError("rollback empty", err)
		}
		c.logger.Info("report", "mode", "apply", "converted", 0, "v1_rows", 0, "v2_rows", before.V2Rows,
			"rows", before.Rows, "state", "converted")
		return nil
	}
	rows, err := c.loadRetiredRows(ctx, tx, before.V1Rows)
	if err != nil {
		return err
	}
	// Snapshot what the conversion must not touch: the digest of the rows already
	// under the current version, the digest of the minute aggregate and the total
	// row count.
	v2Count, v2Digest, err := c.digest(ctx, tx, v2DigestQuery, ingestion.NormalizationVersionV2, "")
	if err != nil {
		return err
	}
	minutesCount, minutesDigest, err := c.digest(ctx, tx, minutesDigestQuery)
	if err != nil {
		return err
	}
	converted := make([]string, 0, len(rows))
	for _, row := range rows {
		if err := c.convertRow(ctx, tx, row); err != nil {
			return err
		}
		converted = append(converted, row.idText)
	}
	if err := c.verify(ctx, tx, before, rows, converted, minutesCount, minutesDigest, v2Count, v2Digest); err != nil {
		return err
	}
	if err := c.commitTx(tx); err != nil {
		return &commitUnknownError{cause: database.SafeError("commit", err)}
	}
	retErr = nil
	c.logger.Info("report", "mode", "apply", "converted", len(converted), "v1_rows", 0,
		"v2_rows", before.V2Rows+int64(len(converted)), "rows", before.Rows, "verified", true, "state", "converted")
	return nil
}

// preflight proves the stored shape is the one this command understands, then counts
// the rows. Any anomaly fails the whole run: a partial conversion is worse than none.
func (c *converter) preflight(ctx context.Context, tx *sql.Tx) (state, error) {
	var columns int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = 'usage_events'
  AND column_name IN ('generation_duration_ms', 'ttft_ms')`).Scan(&columns); err != nil {
		return state{}, database.SafeError("read columns", err)
	}
	if columns != 2 {
		return state{}, errors.New("usage_events is missing generation_duration_ms or ttft_ms: apply migration 00009 first")
	}
	var unknownVersions int64
	var current state
	rows, err := tx.QueryContext(ctx, `SELECT normalization_version, count(*) FROM usage_events GROUP BY 1`)
	if err != nil {
		return state{}, database.SafeError("read versions", err)
	}
	defer rows.Close()
	for rows.Next() {
		var version string
		var count int64
		if err := rows.Scan(&version, &count); err != nil {
			return state{}, database.SafeError("scan versions", err)
		}
		switch version {
		case retiredVersion:
			current.V1Rows = count
		case ingestion.NormalizationVersionV2:
			current.V2Rows = count
		default:
			unknownVersions += count
		}
		current.Rows += count
	}
	if err := rows.Err(); err != nil {
		return state{}, database.SafeError("read versions rows", err)
	}
	if unknownVersions != 0 {
		return state{}, errors.New("stored rows carry an unexpected normalization version")
	}
	var timed int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM usage_events
WHERE normalization_version = $1 AND (generation_duration_ms IS NOT NULL OR ttft_ms IS NOT NULL)`,
		retiredVersion).Scan(&timed); err != nil {
		return state{}, database.SafeError("read retired timings", err)
	}
	if timed != 0 {
		return state{}, errors.New("a retired row already carries a timing value")
	}
	var malformed int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM usage_events
WHERE payload_hash IS NULL OR length(payload_hash) <> 32`).Scan(&malformed); err != nil {
		return state{}, database.SafeError("read digests", err)
	}
	if malformed != 0 {
		return state{}, errors.New("a stored digest is missing or is not a 32 byte SHA-256")
	}
	if current.V1Rows > maxConvertRows {
		return state{}, errors.New("more retired rows than one bounded conversion allows")
	}
	return current, nil
}

// retiredRow is one stored row the conversion rewrites. It carries the shared
// normalization input rebuilt from the retained columns, the digest that input
// produces, and the whole row as JSON without the two columns the conversion owns.
// The JSON snapshot is compared as a whole, so an identity, received_at or any other
// column cannot drift outside the comparison.
type retiredRow struct {
	id       pgtype.UUID
	idText   string
	input    ingestion.NormalizationInput
	digest   [sha256.Size]byte
	snapshot string
}

// loadRetiredRows reads every retired row ordered by time and identity, so a rerun
// walks the same order. Every metric must carry a value and quality the API could have
// produced, and the digest is recomputed here: check calls this too, so a stored row
// apply would refuse fails the read-only check first, before anything is written.
func (c *converter) loadRetiredRows(ctx context.Context, tx *sql.Tx, expected int64) ([]retiredRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT t.id,
    t.occurred_at, t.project_id, t.provider, t.model,
    t.input_tokens, t.input_tokens_quality,
    t.output_tokens, t.output_tokens_quality,
    t.total_tokens, t.total_tokens_quality,
    t.cache_read_tokens, t.cache_read_tokens_quality,
    t.cache_write_tokens, t.cache_write_tokens_quality,
    t.reasoning_tokens, t.reasoning_tokens_quality,
    t.status, t.duration_ms, t.generation_duration_ms, t.ttft_ms,
    (to_jsonb(t) - 'normalization_version' - 'payload_hash')::text
FROM usage_events t WHERE t.normalization_version = $1
ORDER BY t.occurred_at, t.id`, retiredVersion)
	if err != nil {
		return nil, database.SafeError("read retired rows", err)
	}
	defer rows.Close()
	out := make([]retiredRow, 0, expected)
	for rows.Next() {
		var row retiredRow
		var projectID pgtype.UUID
		var values [6]sql.NullInt64
		var qualities [6]sql.NullString
		var status sql.NullString
		var duration, generation, ttft sql.NullInt64
		if err := rows.Scan(&row.id,
			&row.input.OccurredAt, &projectID, &row.input.Provider, &row.input.Model,
			&values[0], &qualities[0], &values[1], &qualities[1], &values[2], &qualities[2],
			&values[3], &qualities[3], &values[4], &qualities[4], &values[5], &qualities[5],
			&status, &duration, &generation, &ttft, &row.snapshot); err != nil {
			return nil, database.SafeError("scan retired row", err)
		}
		if generation.Valid || ttft.Valid {
			return nil, errors.New("a retired row carries a timing value")
		}
		row.input.ProjectID = projectID
		metrics := []*aggregation.Metric{&row.input.InputTokens, &row.input.OutputTokens, &row.input.TotalTokens,
			&row.input.CacheReadTokens, &row.input.CacheWriteTokens, &row.input.ReasoningTokens}
		for i, metric := range metrics {
			stored, err := storedMetric(values[i], qualities[i])
			if err != nil {
				return nil, err
			}
			*metric = stored
		}
		if status.Valid {
			value := status.String
			row.input.Status = &value
		}
		if duration.Valid {
			value := duration.Int64
			row.input.DurationMS = &value
		}
		row.idText = uuidText(row.id)
		row.digest = ingestion.DigestV2(row.input)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, database.SafeError("read retired rows", err)
	}
	if int64(len(out)) != expected {
		return nil, errors.New("the retired row count changed during the run")
	}
	return out, nil
}

// storedQuality maps a stored quality string onto the aggregation quality, refusing
// anything this release does not know: an unknown value is never guessed.
func storedQuality(value sql.NullString) (aggregation.Quality, error) {
	if !value.Valid {
		return "", errors.New("a stored usage quality is missing")
	}
	switch quality := aggregation.Quality(value.String); quality {
	case aggregation.QualityKnown, aggregation.QualityEstimated, aggregation.QualityMissing:
		return quality, nil
	default:
		return "", errors.New("a stored usage quality is not recognized")
	}
}

// storedMetric rebuilds one usage metric from its stored value and quality, refusing
// any pair the API could not have produced: known and estimated carry a value in
// [0, 2^53-1], missing carries none. DigestV2 renders a nil value as missing whatever
// the stored quality says, so an unchecked pair would silently hash a different event.
func storedMetric(value sql.NullInt64, quality sql.NullString) (aggregation.Metric, error) {
	storedQualityValue, err := storedQuality(quality)
	if err != nil {
		return aggregation.Metric{}, err
	}
	if storedQualityValue == aggregation.QualityMissing {
		if value.Valid {
			return aggregation.Metric{}, errors.New("a missing usage quality carries a value")
		}
		return aggregation.Metric{Quality: storedQualityValue}, nil
	}
	if !value.Valid {
		return aggregation.Metric{}, errors.New("a known or estimated usage quality has no value")
	}
	if value.Int64 < 0 || value.Int64 > maxSafeInteger {
		return aggregation.Metric{}, errors.New("a stored usage value is outside the accepted range")
	}
	metric := aggregation.Metric{Quality: storedQualityValue}
	metric.Value = &value.Int64
	return metric, nil
}

// convertRow rewrites the version and digest of one row by its identity and proves
// that nothing else in that row moved.
func (c *converter) convertRow(ctx context.Context, tx *sql.Tx, row retiredRow) error {
	result, err := tx.ExecContext(ctx, `UPDATE usage_events
SET normalization_version = $2, payload_hash = $3
WHERE id = $1 AND normalization_version = $4`,
		row.id, ingestion.NormalizationVersionV2, row.digest[:], retiredVersion)
	if err != nil {
		return database.SafeError("update row", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return database.SafeError("update row count", err)
	}
	if affected != 1 {
		return errors.New("a retired row changed during the run")
	}
	after, err := c.rowSnapshot(ctx, tx, row.id)
	if err != nil {
		return err
	}
	if after != row.snapshot {
		return errors.New("a converted row changed a column the conversion does not own")
	}
	return nil
}

// rowSnapshot re-reads one row as JSON without the two columns the conversion owns.
// Comparing the whole row covers every column the normalization input never carries:
// the identities, received_at and the stored quality text itself.
func (c *converter) rowSnapshot(ctx context.Context, tx *sql.Tx, id pgtype.UUID) (string, error) {
	var snapshot string
	if err := tx.QueryRowContext(ctx, `SELECT (to_jsonb(t) - 'normalization_version' - 'payload_hash')::text
FROM usage_events t WHERE t.id = $1`, id).Scan(&snapshot); err != nil {
		return "", database.SafeError("read converted row", err)
	}
	return snapshot, nil
}

// verify runs the pre-commit checks: no retired row left, the counted rows to the
// expected version and digest, every converted row's digest equal to the shared
// DigestV2 result, the pre-existing v2 rows, the minute aggregate and the total row
// count all unchanged.
func (c *converter) verify(ctx context.Context, tx *sql.Tx, before state, rows []retiredRow,
	converted []string, minutesCount int64, minutesDigest string, v2Count int64, v2Digest string) error {
	var remaining int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM usage_events WHERE normalization_version = $1`,
		retiredVersion).Scan(&remaining); err != nil {
		return database.SafeError("count retired rows", err)
	}
	if remaining != 0 {
		return errors.New("a retired row is still stored after the conversion")
	}
	var total int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM usage_events`).Scan(&total); err != nil {
		return database.SafeError("count rows", err)
	}
	if total != before.Rows {
		return errors.New("the row count changed during the conversion")
	}
	currentCount, currentDigest, err := c.digest(ctx, tx, v2DigestQuery, ingestion.NormalizationVersionV2, strings.Join(converted, ","))
	if err != nil {
		return err
	}
	if currentCount != v2Count || currentDigest != v2Digest {
		return errors.New("a row already under the current version changed")
	}
	afterMinutesCount, afterMinutesDigest, err := c.digest(ctx, tx, minutesDigestQuery)
	if err != nil {
		return err
	}
	if afterMinutesCount != minutesCount || afterMinutesDigest != minutesDigest {
		return errors.New("the minute aggregate changed")
	}
	var convertedRows int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM usage_events WHERE normalization_version = $1`,
		ingestion.NormalizationVersionV2).Scan(&convertedRows); err != nil {
		return database.SafeError("count current rows", err)
	}
	if convertedRows != before.V2Rows+int64(len(converted)) {
		return errors.New("the converted row count does not match")
	}
	for _, row := range rows {
		var hash []byte
		if err := tx.QueryRowContext(ctx, `SELECT payload_hash FROM usage_events WHERE id = $1`, row.id).Scan(&hash); err != nil {
			return database.SafeError("read converted digest", err)
		}
		if want := ingestion.DigestV2(row.input); string(hash) != string(want[:]) {
			return errors.New("a converted digest is not the shared DigestV2 result")
		}
	}
	return nil
}

// v2DigestQuery digests every row under the current version, optionally excluding the
// identities the caller names as a comma separated text list. Naming them as text
// keeps the parameter a single scalar.
const v2DigestQuery = `SELECT count(*), coalesce(md5(string_agg(h, '' ORDER BY h)), '')
FROM (SELECT t::text AS h FROM usage_events t
      WHERE t.normalization_version = $1
        AND NOT (t.id::text = ANY(string_to_array($2, ',')))) s`

// minutesDigestQuery digests the whole minute aggregate, which the conversion must
// never touch.
const minutesDigestQuery = `SELECT count(*), coalesce(md5(string_agg(h, '' ORDER BY h)), '')
FROM (SELECT t::text AS h FROM usage_minute_stats t) s`

// digest runs one of the digest queries above and returns its count and digest.
func (c *converter) digest(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, string, error) {
	var count int64
	var digest string
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&count, &digest); err != nil {
		return 0, "", database.SafeError("digest", err)
	}
	return count, digest, nil
}

// uuidText renders an identity in the lower case hyphenated form.
func uuidText(id pgtype.UUID) string {
	var raw [16]byte = id.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}
