// Package ingestion implements the synchronous batch ingestion endpoint frozen in
// docs/openapi/openapi.yaml. One Bearer-authenticated POST carries 1-100
// request-level usage events; every event is validated on its own and committed in
// its own Read Committed transaction, so a bad or failing item never affects the
// others.
//
// The service owns the per-item order of operations and never touches pgx: the
// storage side is a narrow interface implemented by Postgres. The detail event and
// the minute increment always share one transaction, and an item is only reported
// as accepted after that transaction committed.
package ingestion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/dundunHa/TokenPivot/internal/aggregation"
	"github.com/dundunHa/TokenPivot/internal/credentials"
	"github.com/dundunHa/TokenPivot/internal/database"
)

// Frozen contract limits and versions.
const (
	// normalizationVersion is the only comparison algorithm this release writes
	// and understands.
	normalizationVersion = "tokenpivot-usage-event/v1"
	// retainedWindow is the accepted occurred_at age; both ends are inclusive.
	retainedWindow = 180 * 24 * time.Hour
	// maxSafeInteger is the largest value a JSON integer may carry (2^53-1).
	maxSafeInteger int64 = 9007199254740991
	// maxBatchEvents is the per-batch event limit.
	maxBatchEvents = 100
	// maxBodyBytes is the per-request body limit.
	maxBodyBytes = 1 << 20
	// processingBudget bounds one request from the moment the handler starts.
	processingBudget = 8 * time.Second
	// rollbackBudget bounds the rollback that releases an item connection. It is
	// deliberately not the request context: a canceled request must still get the
	// connection back, and the rollback must not hang on a dead network.
	rollbackBudget = 2 * time.Second
)

// Per-item result statuses. The first four never carry an error code; the last
// two always do.
const (
	statusAccepted  = "accepted"
	statusDuplicate = "duplicate"
	statusConflict  = "conflict"
	statusExpired   = "expired"
	statusRejected  = "rejected"
	statusRetryable = "retryable"
)

// Stable error codes. The rejected codes are permanent; the retryable codes mean
// the same identity and content may be sent again.
const (
	codeInvalidArgument           = "INVALID_ARGUMENT"
	codeFutureEvent               = "FUTURE_EVENT"
	codeProjectInvalid            = "PROJECT_INVALID"
	codeUsageUnsupported          = "USAGE_UNSUPPORTED"
	codeUsageOverflow             = "USAGE_OVERFLOW"
	codeNormalizationIncompatible = "NORMALIZATION_INCOMPATIBLE"
	codeUnavailable               = "UNAVAILABLE"
	codeInternal                  = "INTERNAL"
	codeDeadlineExceeded          = "DEADLINE_EXCEEDED"
)

// errMissingEvent reports an internal inconsistency: one batch position carries
// neither a decoded event nor a decoding rejection.
var errMissingEvent = errors.New("ingestion: item has neither an event nor a rejection")

// authenticator is the narrow authentication surface the service needs. It is
// satisfied by the credential service.
type authenticator interface {
	Authenticate(ctx context.Context, token string) (credentials.Credential, error)
}

// store is the narrow persistence surface the service needs. Both methods scope
// their work by the authenticated workspace; neither decides item semantics.
type store interface {
	// BeginItem opens one Read Committed item transaction.
	BeginItem(ctx context.Context) (itemTx, error)
	// TouchSource moves sources.last_received_at forward for one workspace and
	// source. It runs outside every item transaction and is never authoritative
	// for an item's result.
	TouchSource(ctx context.Context, workspaceID, sourceID credentials.ID) error
}

// itemTx is the per-item transaction the service drives. Every method must run
// inside that transaction; Commit is the only way an item becomes visible.
type itemTx interface {
	// Now reads the server time from the database, never a transaction-start
	// constant and never a client value.
	Now(ctx context.Context) (time.Time, error)
	// ProjectExists reports whether the project belongs to the workspace.
	ProjectExists(ctx context.Context, workspaceID, projectID credentials.ID) (bool, error)
	// InsertEvent inserts the detail row, reporting false when the identity
	// already exists.
	InsertEvent(ctx context.Context, event storedEvent) (bool, error)
	// ExistingEvent reads the stored comparison basis of one identity.
	ExistingEvent(ctx context.Context, workspaceID, sourceID credentials.ID, sourceEventID string) (storedEvent, bool, error)
	// ApplyMinute writes the minute increment inside this transaction.
	ApplyMinute(ctx context.Context, input aggregation.MinuteInput) error
	// Commit commits the detail row and the minute increment together.
	Commit(ctx context.Context) error
	// Rollback discards the whole item.
	Rollback(ctx context.Context) error
}

// usage holds the six metrics in their frozen normalization order.
type usage struct {
	Input      aggregation.Metric
	Output     aggregation.Metric
	Total      aggregation.Metric
	CacheRead  aggregation.Metric
	CacheWrite aggregation.Metric
	Reasoning  aggregation.Metric
}

// event is one decoded request-level event: the single typed result that both the
// normalized comparison text and the stored columns come from.
type event struct {
	SourceEventID string
	// ProjectID is invalid when the event is not attributed to a project.
	ProjectID  credentials.ID
	Provider   string
	Model      string
	OccurredAt time.Time // UTC, microsecond precision
	Usage      usage
	Status     *string
	DurationMS *int64
	// PayloadHash is the raw SHA-256 digest of the normalized comparison text.
	PayloadHash [sha256.Size]byte
}

// storedEvent is one immutable detail row: the identity, the first-receive stamp
// and the typed content.
type storedEvent struct {
	WorkspaceID          credentials.ID
	SourceID             credentials.ID
	ProjectID            credentials.ID
	SourceEventID        string
	Provider             string
	Model                string
	OccurredAt           time.Time
	ReceivedAt           time.Time
	NormalizationVersion string
	PayloadHash          []byte
	Usage                usage
	Status               *string
	DurationMS           *int64
}

// rejection is one permanent per-item refusal decided before any database write.
type rejection struct {
	code    string
	message string
}

// item is one batch position: a decoded event or a permanent rejection.
type item struct {
	event     *event
	rejection *rejection
}

// result is one per-item outcome. code and message are set exactly for rejected
// and retryable.
type result struct {
	status  string
	code    string
	message string
}

// transientError marks a per-item failure that is safe to retry with the same
// identity and content. Its message never carries driver detail.
type transientError struct {
	code  string
	cause error
}

func (e *transientError) Error() string { return e.code }

func (e *transientError) Unwrap() error { return e.cause }

// transient wraps a failure with one of the retryable codes.
func transient(code string, cause error) error {
	return &transientError{code: code, cause: cause}
}

// Service runs one batch item at a time on top of a store.
type Service struct {
	store         store
	authenticator authenticator
	logger        *slog.Logger
}

// New returns a Service. A nil logger discards logging, which keeps tests quiet.
func New(s store, auth authenticator, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{store: s, authenticator: auth, logger: logger}
}

// Authenticate resolves the fixed workspace and source of a plugin token. The
// identity of a batch always comes from here and never from the request payload.
func (s *Service) Authenticate(ctx context.Context, token string) (credentials.Credential, error) {
	return s.authenticator.Authenticate(ctx, token)
}

// process runs every batch position in input order and returns one result per
// position. It never aborts the batch: a rejected or failing item only affects its
// own slot. It stops starting new items once the request context is done and marks
// every remaining position retryable, never accepted.
func (s *Service) process(ctx context.Context, credential credentials.Credential, items []item) []result {
	results := make([]result, len(items))
	accepted := false
	for i := range items {
		// The budget is checked before any pre-decoded verdict: once the request
		// is over, an unstarted position is retryable even when its content is
		// permanently invalid, because the client never got a real decision.
		if ctx.Err() != nil {
			results[i] = stoppedResult()
			continue
		}
		if items[i].rejection != nil {
			results[i] = rejectedResult(items[i].rejection.code, items[i].rejection.message)
			continue
		}
		if items[i].event == nil {
			results[i] = retryableResult(ctx, transient(codeInternal, errMissingEvent))
			continue
		}
		results[i] = s.processItem(ctx, credential, items[i].event)
		if results[i].status == statusAccepted {
			accepted = true
		}
	}
	if accepted {
		// The source stamp is maintenance only: accepted items stay accepted even
		// when this single statement fails, and only one sanitized line is logged.
		if err := s.store.TouchSource(ctx, credential.WorkspaceID, credential.SourceID); err != nil {
			s.logger.Warn("source last_received_at update failed", "stage", "ingestion source", "err", database.SafeError("source last received", err))
		}
	}
	return results
}

// processItem runs the frozen per-item order inside one transaction:
// window check, project ownership, insert, re-checked window, conflict
// classification, minute increment, one more window check, commit.
func (s *Service) processItem(ctx context.Context, credential credentials.Credential, e *event) result {
	tx, err := s.store.BeginItem(ctx)
	if err != nil {
		return retryableResult(ctx, err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// The request context may already be canceled, so the rollback uses its own
		// short-lived budget: independent of the request, but still bounded so a
		// dead network cannot hold the connection forever.
		rollbackCtx, cancelRollback := context.WithTimeout(context.WithoutCancel(ctx), rollbackBudget)
		defer cancelRollback()
		if rollbackErr := tx.Rollback(rollbackCtx); rollbackErr != nil {
			// The driver message is never logged: only the sanitized class survives.
			s.logger.Warn("item rollback failed", "stage", "ingestion item", "err", database.SafeError("ingestion rollback", rollbackErr))
		}
	}()

	receivedAt, window, err := sampleWindow(ctx, tx, e)
	if err != nil {
		return retryableResult(ctx, err)
	}
	switch window {
	case verdictFuture:
		return rejectedResult(codeFutureEvent, "occurred_at is later than the server time")
	case verdictExpired:
		// Expiry is decided before any duplicate or conflict lookup.
		return result{status: statusExpired}
	}

	// An absent project is legitimate; an invalid, foreign or missing project is
	// rejected and never stored as NULL.
	if e.ProjectID.Valid {
		exists, err := tx.ProjectExists(ctx, credential.WorkspaceID, e.ProjectID)
		if err != nil {
			return retryableResult(ctx, err)
		}
		if !exists {
			return rejectedResult(codeProjectInvalid, "project is invalid for this workspace")
		}
	}

	inserted, err := tx.InsertEvent(ctx, storedEventFor(credential, e, receivedAt))
	if err != nil {
		return retryableResult(ctx, err)
	}
	// The insert may have waited on a lock, so the window is sampled again before
	// any classification: an event that aged out meanwhile is expired, not new.
	if expired, err := windowExpired(ctx, tx, e); err != nil {
		return retryableResult(ctx, err)
	} else if expired {
		return result{status: statusExpired}
	}

	if !inserted {
		// The old row is read by its own statement: under Read Committed the INSERT
		// snapshot cannot be assumed to contain a concurrently committed row.
		existing, found, err := tx.ExistingEvent(ctx, credential.WorkspaceID, credential.SourceID, e.SourceEventID)
		if err != nil {
			return retryableResult(ctx, err)
		}
		if expired, err := windowExpired(ctx, tx, e); err != nil {
			return retryableResult(ctx, err)
		} else if expired {
			return result{status: statusExpired}
		}
		if !found {
			// The identity vanished between the insert attempt and the read: the
			// item is neither duplicate nor accepted, and the client retries it.
			return retryableResult(ctx, transient(codeInternal, errors.New("ingestion: identity row disappeared")))
		}
		return compareContent(existing, e)
	}

	if err := tx.ApplyMinute(ctx, minuteInputFor(credential, e)); err != nil {
		return retryableResult(ctx, err)
	}
	// The minute increment may have waited on a bucket lock too, so the window is
	// sampled once more immediately before commit.
	if expired, err := windowExpired(ctx, tx, e); err != nil {
		return retryableResult(ctx, err)
	} else if expired {
		// The detail row and the minute increment roll back together.
		return result{status: statusExpired}
	}
	if err := tx.Commit(ctx); err != nil {
		// A commit error may mean the result is unknown: never report accepted and
		// never claim the row was rolled back. When the budget ran out, that is the
		// honest code; otherwise the failure is a general internal error.
		if ctx.Err() != nil {
			return retryableResult(ctx, ctx.Err())
		}
		return retryableResult(ctx, transient(codeInternal, err))
	}
	committed = true
	return result{status: statusAccepted}
}

// sampleWindow samples the database server time and applies the accepted window to
// the event.
func sampleWindow(ctx context.Context, tx itemTx, e *event) (time.Time, verdict, error) {
	now, err := tx.Now(ctx)
	if err != nil {
		return time.Time{}, verdictValid, err
	}
	return now, checkWindow(e.OccurredAt, now), nil
}

// windowExpired re-samples the server time and reports whether the event left the
// lower bound, so the caller can roll the item back before committing.
func windowExpired(ctx context.Context, tx itemTx, e *event) (bool, error) {
	_, window, err := sampleWindow(ctx, tx, e)
	if err != nil {
		return false, err
	}
	return window == verdictExpired, nil
}

// verdict is the outcome of the accepted-window test at one sampled server time.
type verdict uint8

const (
	verdictValid verdict = iota
	verdictExpired
	verdictFuture
)

// checkWindow applies [T-180*24h, T] with both ends inclusive. A time after T is a
// permanent FUTURE_EVENT, never expired and never truncated to T.
func checkWindow(occurredAt, now time.Time) verdict {
	if occurredAt.After(now) {
		return verdictFuture
	}
	if occurredAt.Before(now.Add(-retainedWindow)) {
		return verdictExpired
	}
	return verdictValid
}

// compareContent classifies an existing identity. Only a stored version this
// release can reproduce is comparable; an unknown version cannot be projected
// losslessly and is reported as permanently incompatible instead of guessed.
func compareContent(existing storedEvent, e *event) result {
	if existing.NormalizationVersion != normalizationVersion {
		return rejectedResult(codeNormalizationIncompatible, "no common normalization version can be formed")
	}
	if bytes.Equal(existing.PayloadHash, e.PayloadHash[:]) {
		return result{status: statusDuplicate}
	}
	return result{status: statusConflict}
}

// rejectedResult builds a permanent per-item refusal.
func rejectedResult(code, message string) result {
	return result{status: statusRejected, code: code, message: message}
}

// stoppedResult marks an item that never started because the request ended.
func stoppedResult() result {
	return result{status: statusRetryable, code: codeDeadlineExceeded, message: "processing stopped before this item finished"}
}

// retryableResult maps an item failure to its retryable code. The cause is never
// part of the returned message.
func retryableResult(ctx context.Context, err error) result {
	code := retryCode(ctx, err)
	return result{status: statusRetryable, code: code, message: retryMessage(code)}
}

// retryCode picks the documented retryable code for a failure. An exhausted
// request budget outranks every specific failure class: the client only learns
// that the item must be retried, so DEADLINE_EXCEEDED is the honest code even when
// the underlying statement failed for another reason.
func retryCode(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return codeDeadlineExceeded
	}
	var failure *transientError
	if errors.As(err, &failure) {
		return failure.code
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return codeDeadlineExceeded
	}
	return codeUnavailable
}

// retryMessage is the human readable text of a retryable code. It is not a branch
// basis and carries no SQL, stack, credential or event content.
func retryMessage(code string) string {
	switch code {
	case codeDeadlineExceeded:
		return "processing budget exhausted"
	case codeInternal:
		return "internal error"
	default:
		return "temporarily unavailable"
	}
}

// storedEventFor converts a decoded event into the detail row. received_at is the
// server time sampled in the accepting transaction and is never renewed by a
// retransmission.
func storedEventFor(credential credentials.Credential, e *event, receivedAt time.Time) storedEvent {
	return storedEvent{
		WorkspaceID:          credential.WorkspaceID,
		SourceID:             credential.SourceID,
		ProjectID:            e.ProjectID,
		SourceEventID:        e.SourceEventID,
		Provider:             e.Provider,
		Model:                e.Model,
		OccurredAt:           e.OccurredAt,
		ReceivedAt:           receivedAt,
		NormalizationVersion: normalizationVersion,
		PayloadHash:          e.PayloadHash[:],
		Usage:                e.Usage,
		Status:               e.Status,
		DurationMS:           e.DurationMS,
	}
}

// minuteInputFor converts a decoded event into the aggregation input. The bucket is
// owned by occurred_at, never by received_at.
func minuteInputFor(credential credentials.Credential, e *event) aggregation.MinuteInput {
	return aggregation.MinuteInput{
		WorkspaceID: credential.WorkspaceID,
		SourceID:    credential.SourceID,
		ProjectID:   e.ProjectID,
		Provider:    e.Provider,
		Model:       e.Model,
		OccurredAt:  e.OccurredAt,
		Level:       aggregation.LevelRequest,
		Input:       e.Usage.Input,
		Output:      e.Usage.Output,
		Total:       e.Usage.Total,
		CacheRead:   e.Usage.CacheRead,
		CacheWrite:  e.Usage.CacheWrite,
		Reasoning:   e.Usage.Reasoning,
	}
}

// normalizedText is the frozen v1 comparison text: one JSON array with exactly
// seven elements, no insignificant whitespace and no localization.
func (e *event) normalizedText() []byte {
	buffer := make([]byte, 0, 512)
	buffer = append(buffer, '[')
	buffer = appendJSONString(buffer, e.OccurredAt.UTC().Format(microLayout))
	buffer = append(buffer, ',')
	if e.ProjectID.Valid {
		buffer = appendJSONString(buffer, projectText(e.ProjectID))
	} else {
		buffer = append(buffer, "null"...)
	}
	buffer = append(buffer, ',')
	buffer = appendJSONString(buffer, e.Provider)
	buffer = append(buffer, ',')
	buffer = appendJSONString(buffer, e.Model)
	buffer = append(buffer, ',')
	buffer = appendUsage(buffer, e.Usage)
	buffer = append(buffer, ',')
	if e.Status != nil {
		buffer = appendJSONString(buffer, *e.Status)
	} else {
		buffer = append(buffer, "null"...)
	}
	buffer = append(buffer, ',')
	if e.DurationMS != nil {
		buffer = strconv.AppendInt(buffer, *e.DurationMS, 10)
	} else {
		buffer = append(buffer, "null"...)
	}
	buffer = append(buffer, ']')
	return buffer
}

// microLayout is the normalized occurred_at representation: UTC with exactly six
// fractional digits and a trailing Z.
const microLayout = "2006-01-02T15:04:05.000000Z"

// appendUsage writes the usage object in the frozen key order, with all six
// metrics always present.
func appendUsage(buffer []byte, u usage) []byte {
	buffer = append(buffer, `{"input_tokens":`...)
	buffer = appendMetric(buffer, u.Input)
	buffer = append(buffer, `,"output_tokens":`...)
	buffer = appendMetric(buffer, u.Output)
	buffer = append(buffer, `,"total_tokens":`...)
	buffer = appendMetric(buffer, u.Total)
	buffer = append(buffer, `,"cache_read_tokens":`...)
	buffer = appendMetric(buffer, u.CacheRead)
	buffer = append(buffer, `,"cache_write_tokens":`...)
	buffer = appendMetric(buffer, u.CacheWrite)
	buffer = append(buffer, `,"reasoning_tokens":`...)
	buffer = appendMetric(buffer, u.Reasoning)
	return append(buffer, '}')
}

// appendMetric writes one metric as value then quality.
func appendMetric(buffer []byte, metric aggregation.Metric) []byte {
	if metric.Value == nil || metric.Quality == aggregation.QualityMissing {
		return append(buffer, `{"value":null,"quality":"missing"}`...)
	}
	buffer = append(buffer, `{"value":`...)
	buffer = strconv.AppendInt(buffer, *metric.Value, 10)
	buffer = append(buffer, `,"quality":"`...)
	buffer = append(buffer, string(metric.Quality)...)
	return append(buffer, `"}`...)
}

// appendJSONString writes a JSON string with RFC 8259 minimal escaping: only the
// quote and the reverse solidus are escaped here, non-ASCII is emitted as its UTF-8
// bytes. Entry validation already rejects every control character that could
// otherwise need an escape.
func appendJSONString(buffer []byte, text string) []byte {
	buffer = append(buffer, '"')
	for i := 0; i < len(text); i++ {
		switch character := text[i]; {
		case character == '"':
			buffer = append(buffer, '\\', '"')
		case character == '\\':
			buffer = append(buffer, '\\', '\\')
		case character < 0x20:
			const hexDigits = "0123456789abcdef"
			buffer = append(buffer, '\\', 'u', '0', '0', hexDigits[character>>4], hexDigits[character&0x0f])
		default:
			buffer = append(buffer, character)
		}
	}
	return append(buffer, '"')
}

// projectText renders the stored project identity as the frozen lower case UUID.
func projectText(id credentials.ID) string {
	raw := id.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}
