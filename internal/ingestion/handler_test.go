package ingestion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dundunHa/TokenPivot/internal/aggregation"
	"github.com/dundunHa/TokenPivot/internal/credentials"
	"github.com/jackc/pgx/v5"
)

// fixedNow is the frozen time base of every unit test: 2026-10-27T14:00:00Z, the
// same instant the documented boundary examples use.
var fixedNow = time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC)

// syntheticWorkspace and syntheticSource are the identity a successful
// authentication returns.
var (
	syntheticWorkspace = syntheticID(0x11)
	syntheticSource    = syntheticID(0x22)
)

// syntheticID builds a deterministic UUID shaped identity.
func syntheticID(fill byte) credentials.ID {
	var id credentials.ID
	for i := range id.Bytes {
		id.Bytes[i] = fill
	}
	id.Bytes[15] = fill + 1
	id.Valid = true
	return id
}

// fakeClock hands out a scripted sequence of server times. The last entry repeats,
// so a test only scripts the samples it cares about.
type fakeClock struct {
	times []time.Time
	mu    sync.Mutex
	call  int
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.times) == 0 {
		return fixedNow
	}
	index := c.call
	if index >= len(c.times) {
		index = len(c.times) - 1
	}
	c.call++
	return c.times[index]
}

// fakeEvent is one stored detail row as the fake store keeps it: exactly the two
// comparison columns the service reads back.
type fakeEvent struct {
	version string
	hash    []byte
}

// fakeStore is an in-memory stand-in that mirrors only the SQL semantics the
// service depends on, plus call counters so the order of operations can be proven.
type fakeStore struct {
	mu       sync.Mutex
	clock    fakeClock
	projects map[[16]byte]struct{}
	events   map[string]fakeEvent // source_event_id -> row
	calls    []string
	touched  int
	// failure injects one error per named step.
	failure map[string]error
	// onStep runs after a step has been recorded, so a test can cancel a context
	// or move the clock at one exact point of the frozen per-item order.
	onStep func(step string)
	// rollbackFailure is returned by Rollback, modelling a connection that could
	// not discard the transaction.
	rollbackFailure error
	// commitWaitsForContext makes Commit block until the context ends and then
	// return its error, modelling a commit that hits the request deadline.
	commitWaitsForContext bool
	// insertResult overrides the insert outcome when set: -1 keeps the mapped
	// behavior, 0 reports "identity exists", 1 reports "inserted".
	insertResult int
	// vanish makes ExistingEvent report no row even though the insert reported a
	// conflict.
	vanish bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		projects:     map[[16]byte]struct{}{},
		events:       map[string]fakeEvent{},
		failure:      map[string]error{},
		insertResult: -1,
	}
}

func (f *fakeStore) record(step string) {
	f.mu.Lock()
	f.calls = append(f.calls, step)
	hook := f.onStep
	f.mu.Unlock()
	if hook != nil {
		hook(step)
	}
}

func (f *fakeStore) called(step string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, entry := range f.calls {
		if entry == step {
			count++
		}
	}
	return count
}

func (f *fakeStore) sequence() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ",")
}

func (f *fakeStore) BeginItem(ctx context.Context) (itemTx, error) {
	f.record("begin")
	if err := f.failure["begin"]; err != nil {
		return nil, err
	}
	return &fakeTx{store: f}, nil
}

func (f *fakeStore) TouchSource(ctx context.Context, workspaceID, sourceID credentials.ID) error {
	f.record("touch")
	f.mu.Lock()
	f.touched++
	f.mu.Unlock()
	return f.failure["touch"]
}

// fakeTx is one scripted item transaction.
type fakeTx struct {
	store *fakeStore
	done  bool
}

func (t *fakeTx) failure(step string) error { return t.store.failure[step] }

func (t *fakeTx) Now(ctx context.Context) (time.Time, error) {
	t.store.record("now")
	if err := t.failure("now"); err != nil {
		return time.Time{}, err
	}
	return t.store.clock.now(), nil
}

func (t *fakeTx) ProjectExists(ctx context.Context, workspaceID, projectID credentials.ID) (bool, error) {
	t.store.record("project")
	if err := t.failure("project"); err != nil {
		return false, err
	}
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	_, ok := t.store.projects[projectID.Bytes]
	return ok, nil
}

func (t *fakeTx) InsertEvent(ctx context.Context, event storedEvent) (bool, error) {
	t.store.record("insert")
	if err := t.failure("insert"); err != nil {
		return false, err
	}
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	if t.store.insertResult == 0 {
		return false, nil
	}
	if t.store.insertResult == 1 {
		t.store.events[event.SourceEventID] = fakeEvent{version: event.NormalizationVersion, hash: event.PayloadHash}
		return true, nil
	}
	if _, exists := t.store.events[event.SourceEventID]; exists {
		return false, nil
	}
	t.store.events[event.SourceEventID] = fakeEvent{version: event.NormalizationVersion, hash: event.PayloadHash}
	return true, nil
}

func (t *fakeTx) ExistingEvent(ctx context.Context, workspaceID, sourceID credentials.ID, sourceEventID string) (storedEvent, bool, error) {
	t.store.record("existing")
	if err := t.failure("existing"); err != nil {
		return storedEvent{}, false, err
	}
	if t.store.vanish {
		return storedEvent{}, false, nil
	}
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	row, ok := t.store.events[sourceEventID]
	if !ok {
		return storedEvent{}, false, nil
	}
	return storedEvent{NormalizationVersion: row.version, PayloadHash: row.hash}, true, nil
}

func (t *fakeTx) ApplyMinute(ctx context.Context, input aggregation.MinuteInput) error {
	t.store.record("minute")
	if err := t.failure("minute"); err != nil {
		return err
	}
	if input.Level != aggregation.LevelRequest {
		return errors.New("synthetic: not request level")
	}
	return nil
}

func (t *fakeTx) Commit(ctx context.Context) error {
	t.store.record("commit")
	if t.store.commitWaitsForContext {
		<-ctx.Done()
		t.done = true
		return ctx.Err()
	}
	t.done = true
	return t.failure("commit")
}

func (t *fakeTx) Rollback(ctx context.Context) error {
	t.store.record("rollback")
	t.done = true
	// A dead context models a rollback that could not run at all, so a regression
	// back to the request context is visible instead of silent.
	if err := ctx.Err(); err != nil {
		return err
	}
	return t.store.rollbackFailure
}

// fakeAuthenticator resolves one fixed identity for the accepted token. Any other
// token is unauthenticated; a scripted error is a dependency failure.
type fakeAuthenticator struct {
	token   string
	failure error
	calls   int
	// deadline is the budget the handler handed to authentication.
	deadline time.Time
}

func (a *fakeAuthenticator) Authenticate(ctx context.Context, token string) (credentials.Credential, error) {
	a.calls++
	a.deadline, _ = ctx.Deadline()
	if a.failure != nil {
		return credentials.Credential{}, a.failure
	}
	if token != a.token {
		return credentials.Credential{}, credentials.ErrUnauthenticated
	}
	return credentials.Credential{
		ID:          syntheticID(0x33),
		WorkspaceID: syntheticWorkspace,
		SourceID:    syntheticSource,
	}, nil
}

// testEnvironment bundles a handler with its fake dependencies.
type testEnvironment struct {
	store   *fakeStore
	auth    *fakeAuthenticator
	logs    *bytes.Buffer
	handler http.Handler
}

// newTestEnvironment builds the handler behind the real server mux.
func newTestEnvironment(t *testing.T) *testEnvironment {
	t.Helper()
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := newFakeStore()
	auth := &fakeAuthenticator{token: "tpk_synthetic"}
	service := New(store, auth, logger)
	mux := http.NewServeMux()
	NewHandler(service).Register(mux)
	return &testEnvironment{store: store, auth: auth, logs: logs, handler: mux}
}

// encodedBatch renders the given events as the request body.
func encodedBatch(t *testing.T, events ...string) string {
	t.Helper()
	return `{"events":[` + strings.Join(events, ",") + `]}`
}

// validEventJSON is one minimal valid event with the given identity and time.
func validEventJSON(sourceEventID string, occurredAt time.Time) string {
	return fmt.Sprintf(`{"source_event_id":%s,"provider":"example-provider","model":"example-model","occurred_at":%q,"usage":{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"}}}`,
		strconv.Quote(sourceEventID), occurredAt.UTC().Format(microLayout))
}

// send posts one batch and returns the recorder.
func (e *testEnvironment) send(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return e.sendWith(t, body, func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+e.auth.token)
	})
}

// sendWith posts one batch after the caller adjusted the request.
func (e *testEnvironment) sendWith(t *testing.T, body string, adjust func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, ingestionPath, strings.NewReader(body))
	adjust(request)
	recorder := httptest.NewRecorder()
	e.handler.ServeHTTP(recorder, request)
	return recorder
}

// batchErrorBody is the decoded whole-batch error.
type batchErrorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
		Details   []struct {
			Field  string `json:"field"`
			Reason string `json:"reason"`
		} `json:"details"`
	} `json:"error"`
}

// decodeBatchError decodes and validates a whole-batch error envelope.
func decodeBatchError(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int, wantCode string) batchErrorBody {
	t.Helper()
	if recorder.Code != wantStatus {
		t.Fatalf("status=%d want=%d body=%s", recorder.Code, wantStatus, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type=%q", got)
	}
	var body batchErrorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Code != wantCode {
		t.Fatalf("code=%q want=%q", body.Error.Code, wantCode)
	}
	if body.Error.RequestID == "" {
		t.Fatal("missing request_id")
	}
	if body.Error.Message == "" {
		t.Fatal("missing message")
	}
	return body
}

// itemResults is the decoded per-item response.
type itemResults struct {
	RequestID string `json:"request_id"`
	Results   []struct {
		Index  int    `json:"index"`
		Status string `json:"status"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"results"`
}

// decodeResults decodes the HTTP 200 body and checks the index contract.
func decodeResults(t *testing.T, recorder *httptest.ResponseRecorder, wantItems int) itemResults {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 body=%s", recorder.Code, recorder.Body.String())
	}
	var body itemResults
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.RequestID == "" {
		t.Fatal("missing request_id")
	}
	if len(body.Results) != wantItems {
		t.Fatalf("results=%d want=%d", len(body.Results), wantItems)
	}
	for i, result := range body.Results {
		if result.Index != i {
			t.Fatalf("result %d has index=%d", i, result.Index)
		}
		switch result.Status {
		case statusAccepted, statusDuplicate, statusConflict, statusExpired:
			if result.Error != nil {
				t.Fatalf("result %d: %s must not carry an error", i, result.Status)
			}
		case statusRejected, statusRetryable:
			if result.Error == nil || result.Error.Code == "" || result.Error.Message == "" {
				t.Fatalf("result %d: %s must carry a code and a message", i, result.Status)
			}
		default:
			t.Fatalf("result %d has unknown status %q", i, result.Status)
		}
	}
	return body
}

// statuses returns the per-item status list.
func (b itemResults) statuses() []string {
	list := make([]string, len(b.Results))
	for i, result := range b.Results {
		list[i] = result.Status
	}
	return list
}

// codes returns the per-item error codes, empty where none is set.
func (b itemResults) codes() []string {
	list := make([]string, len(b.Results))
	for i, result := range b.Results {
		if result.Error != nil {
			list[i] = result.Error.Code
		}
	}
	return list
}

// requireStatuses asserts the full per-item status list.
func requireStatuses(t *testing.T, body itemResults, want ...string) {
	t.Helper()
	got := body.statuses()
	if len(got) != len(want) {
		t.Fatalf("statuses=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("statuses=%v want=%v", got, want)
		}
	}
}

// requireCodes asserts the full per-item error code list.
func requireCodes(t *testing.T, body itemResults, want ...string) {
	t.Helper()
	got := body.codes()
	if len(got) != len(want) {
		t.Fatalf("codes=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("codes=%v want=%v", got, want)
		}
	}
}

func TestBatchRejections(t *testing.T) {
	tooMany := make([]string, maxBatchEvents+1)
	for i := range tooMany {
		tooMany[i] = validEventJSON("id-"+strconv.Itoa(i), fixedNow)
	}
	for _, tt := range []struct {
		name   string
		body   string
		adjust func(*http.Request)
		status int
		code   string
		field  string
		reason string
	}{
		{
			name: "wrong media type",
			body: encodedBatch(t, validEventJSON("a", fixedNow)),
			adjust: func(r *http.Request) {
				r.Header.Set("Content-Type", "text/plain")
				r.Header.Set("Authorization", "Bearer tpk_synthetic")
			},
			status: http.StatusUnsupportedMediaType, code: "UNSUPPORTED_MEDIA_TYPE",
		},
		{
			name:   "missing bearer",
			body:   encodedBatch(t, validEventJSON("a", fixedNow)),
			adjust: func(r *http.Request) { r.Header.Set("Content-Type", "application/json") },
			status: http.StatusUnauthorized, code: "UNAUTHENTICATED",
		},
		{
			name: "cookie only",
			body: encodedBatch(t, validEventJSON("a", fixedNow)),
			adjust: func(r *http.Request) {
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Cookie", "tokenpivot_session=tps_synthetic")
			},
			status: http.StatusUnauthorized, code: "UNAUTHENTICATED",
		},
		{
			name: "wrong scheme",
			body: encodedBatch(t, validEventJSON("a", fixedNow)),
			adjust: func(r *http.Request) {
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Basic tpk_synthetic")
			},
			status: http.StatusUnauthorized, code: "UNAUTHENTICATED",
		},
		{
			name: "two authorization values",
			body: encodedBatch(t, validEventJSON("a", fixedNow)),
			adjust: func(r *http.Request) {
				r.Header.Set("Content-Type", "application/json")
				r.Header.Add("Authorization", "Bearer tpk_synthetic")
				r.Header.Add("Authorization", "Bearer tpk_synthetic")
			},
			status: http.StatusUnauthorized, code: "UNAUTHENTICATED",
		},
		{
			name: "unknown token",
			body: encodedBatch(t, validEventJSON("a", fixedNow)),
			adjust: func(r *http.Request) {
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer tpk_other")
			},
			status: http.StatusUnauthorized, code: "UNAUTHENTICATED",
		},
		{
			name: "body too large",
			body: encodedBatch(t, validEventJSON(strings.Repeat("x", maxBodyBytes), fixedNow)),
			adjust: func(r *http.Request) {
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer tpk_synthetic")
			},
			status: http.StatusRequestEntityTooLarge, code: "BODY_TOO_LARGE",
		},
		{
			name:   "shell boundary is not valid UTF-8",
			body:   encodedBatch(t, `{"source_event_id":"a","provider":"\xff","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{}}`),
			status: http.StatusBadRequest, code: "INVALID_ARGUMENT",
		},
		{
			name:   "duplicate shell key",
			body:   `{"events":[` + validEventJSON("a", fixedNow) + `],"events":[]}`,
			status: http.StatusBadRequest, code: "INVALID_ARGUMENT",
		},
		{
			name:   "duplicate nested key",
			body:   `{"events":[{"source_event_id":"a","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{"input_tokens":{"value":1,"quality":"known"},"input_tokens":{"value":2,"quality":"known"}}}]}`,
			status: http.StatusBadRequest, code: "INVALID_ARGUMENT",
		},
		{
			name:   "unknown shell field",
			body:   `{"events":[` + validEventJSON("a", fixedNow) + `],"workspace_id":"0199a0b0-0000-7000-8000-00000000000a"}`,
			status: http.StatusBadRequest, code: "INVALID_ARGUMENT",
			field: "/workspace_id", reason: "unknown_field",
		},
		{
			name:   "empty events",
			body:   `{"events":[]}`,
			status: http.StatusBadRequest, code: "INVALID_ARGUMENT",
			field: "/events", reason: "empty_events",
		},
		{
			name:   "too many events",
			body:   encodedBatch(t, tooMany...),
			status: http.StatusBadRequest, code: "INVALID_ARGUMENT",
			field: "/events", reason: "too_many_events",
		},
		{
			name:   "trailing content",
			body:   encodedBatch(t, validEventJSON("a", fixedNow)) + "{}",
			status: http.StatusBadRequest, code: "INVALID_ARGUMENT",
		},
		{
			name:   "not JSON at all",
			body:   `{`,
			status: http.StatusBadRequest, code: "INVALID_ARGUMENT",
			field: "/", reason: "body_not_json",
		},
		{
			name:   "not an object",
			body:   `[]`,
			status: http.StatusBadRequest, code: "INVALID_ARGUMENT",
			field: "/", reason: "body_not_object",
		},
		{
			name:   "events not an array",
			body:   `{"events":{}}`,
			status: http.StatusBadRequest, code: "INVALID_ARGUMENT",
			field: "/events", reason: "not_an_array",
		},
		{
			name:   "missing events",
			body:   `{}`,
			status: http.StatusBadRequest, code: "INVALID_ARGUMENT",
			field: "/events", reason: "missing_events",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			environment := newTestEnvironment(t)
			var recorder *httptest.ResponseRecorder
			if tt.adjust != nil {
				recorder = environment.sendWith(t, tt.body, tt.adjust)
			} else {
				recorder = environment.send(t, tt.body)
			}
			body := decodeBatchError(t, recorder, tt.status, tt.code)
			if tt.field != "" {
				if len(body.Error.Details) != 1 || body.Error.Details[0].Field != tt.field || body.Error.Details[0].Reason != tt.reason {
					t.Fatalf("details=%+v want field=%q reason=%q", body.Error.Details, tt.field, tt.reason)
				}
			}
			// A whole-batch rejection never starts an item transaction and never
			// touches the source stamp.
			if sequence := environment.store.sequence(); sequence != "" {
				t.Fatalf("store calls=%q want none", sequence)
			}
			if strings.Contains(environment.logs.String(), "tpk_synthetic") || strings.Contains(environment.logs.String(), "tps_synthetic") {
				t.Fatal("logs leaked a credential")
			}
		})
	}
}

func TestAuthenticationDependencyFailureIsBatchUnavailable(t *testing.T) {
	environment := newTestEnvironment(t)
	environment.auth.failure = errors.New("synthetic: connection refused")
	recorder := environment.send(t, encodedBatch(t, validEventJSON("a", fixedNow)))
	decodeBatchError(t, recorder, http.StatusServiceUnavailable, "UNAVAILABLE")
	if sequence := environment.store.sequence(); sequence != "" {
		t.Fatalf("store calls=%q want none", sequence)
	}
	if strings.Contains(environment.logs.String(), "connection refused") {
		t.Fatal("logs leaked the driver message")
	}
	if !strings.Contains(environment.logs.String(), "ingestion authenticate") {
		t.Fatal("missing sanitized stage log")
	}
}

func TestAcceptedThenDuplicateKeepsInputOrder(t *testing.T) {
	environment := newTestEnvironment(t)
	event := validEventJSON("req-dup", fixedNow)
	// The same identity twice with different key order and a different offset
	// spelling: the second must be duplicate, not a second accepted.
	reordered := `{"usage":{"output_tokens":{"quality":"known","value":20},"input_tokens":{"quality":"known","value":100}},"occurred_at":"2026-10-27T07:00:00-07:00","model":"example-model","provider":"example-provider","source_event_id":"req-dup"}`
	recorder := environment.send(t, encodedBatch(t, event, reordered, validEventJSON("req-other", fixedNow)))
	body := decodeResults(t, recorder, 3)
	requireStatuses(t, body, statusAccepted, statusDuplicate, statusAccepted)
	if environment.store.touched != 1 {
		t.Fatalf("source touches=%d want=1", environment.store.touched)
	}
	if commits := environment.store.called("commit"); commits != 2 {
		t.Fatalf("commits=%d want=2", commits)
	}
}

func TestPerItemRejectionsDoNotAffectNeighbours(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		code string
		// decidedInTransaction marks a case whose verdict needs the database clock,
		// so it opens its own item transaction before being refused.
		decidedInTransaction bool
	}{
		{
			name: "unknown event field",
			body: `{"source_event_id":"bad-unknown","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{},"workspace_id":"0199a0b0-0000-7000-8000-00000000000a"}`,
			code: codeInvalidArgument,
		},
		{
			name: "unknown usage field",
			body: `{"source_event_id":"bad-usage-unknown","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{"billed_tokens":{"value":1,"quality":"known"}}}`,
			code: codeInvalidArgument,
		},
		{
			name: "numeric literal is not an integer",
			body: `{"source_event_id":"bad-total","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{"total_tokens":{"value":1.0,"quality":"known"}}}`,
			code: codeInvalidArgument,
		},
		{
			name: "exponent duration",
			body: `{"source_event_id":"bad-duration","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{},"duration_ms":1e3}`,
			code: codeInvalidArgument,
		},
		{
			name: "seven fractional digits",
			body: `{"source_event_id":"bad-time","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00.1234567Z","usage":{}}`,
			code: codeInvalidArgument,
		},
		{
			name: "leap second",
			body: `{"source_event_id":"bad-leap","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:60Z","usage":{}}`,
			code: codeInvalidArgument,
		},
		{
			name: "missing offset",
			body: `{"source_event_id":"bad-offset","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00","usage":{}}`,
			code: codeInvalidArgument,
		},
		{
			name: "impossible date",
			body: `{"source_event_id":"bad-date","provider":"p","model":"m","occurred_at":"2026-02-30T14:00:00Z","usage":{}}`,
			code: codeInvalidArgument,
		},
		{
			name: "offset out of range",
			body: `{"source_event_id":"bad-range","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00+24:00","usage":{}}`,
			code: codeInvalidArgument,
		},
		{
			name: "unknown quality",
			body: `{"source_event_id":"bad-quality","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{"input_tokens":{"value":1,"quality":"exact"}}}`,
			code: codeInvalidArgument,
		},
		{
			name: "known without value",
			body: `{"source_event_id":"bad-known","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{"input_tokens":{"value":null,"quality":"known"}}}`,
			code: codeInvalidArgument,
		},
		{
			name: "surrounding NBSP in provider",
			body: `{"source_event_id":"bad-nbsp","provider":"\u00a0example","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{}}`,
			code: codeInvalidArgument,
		},
		{
			name: "surrounding ideographic space in model",
			body: `{"source_event_id":"bad-ideographic","provider":"p","model":"m\u3000","occurred_at":"2026-10-27T14:00:00Z","usage":{}}`,
			code: codeInvalidArgument,
		},
		{
			name: "control character in status",
			body: `{"source_event_id":"bad-status","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{},"status":"a\u0001"}`,
			code: codeInvalidArgument,
		},
		{
			name: "source_event_id over 256 bytes",
			body: `{"source_event_id":"` + strings.Repeat("汉", 100) + `","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{}}`,
			code: codeInvalidArgument,
		},
		{
			name: "malformed project",
			body: `{"source_event_id":"bad-project","project_id":"not-a-uuid","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{}}`,
			code: codeInvalidArgument,
		},
		{
			name: "known child exceeds known parent",
			body: `{"source_event_id":"bad-subset","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{"input_tokens":{"value":50,"quality":"known"},"cache_read_tokens":{"value":60,"quality":"known"}}}`,
			code: codeUsageUnsupported,
		},
		{
			name: "known total equation fails",
			body: `{"source_event_id":"bad-total-equation","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"},"total_tokens":{"value":130,"quality":"known"}}}`,
			code: codeUsageUnsupported,
		},
		{
			name: "derived total overflows",
			body: `{"source_event_id":"bad-overflow","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{"input_tokens":{"value":9007199254740991,"quality":"known"},"output_tokens":{"value":9007199254740991,"quality":"known"}}}`,
			code: codeUsageOverflow,
		},
		{
			name: "metric value over the safe range",
			body: `{"source_event_id":"bad-metric-overflow","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{"input_tokens":{"value":9007199254740992,"quality":"known"}}}`,
			code: codeUsageOverflow,
		},
		{
			name: "future event",
			body: `{"source_event_id":"bad-future","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00.000001Z","usage":{}}`,
			code: codeFutureEvent, decidedInTransaction: true,
		},
		{
			name: "expired event",
			body: `{"source_event_id":"bad-expired","provider":"p","model":"m","occurred_at":"2026-04-30T13:59:59.999999Z","usage":{}}`,
			code: "", decidedInTransaction: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			environment := newTestEnvironment(t)
			body := decodeResults(t, environment.send(t, encodedBatch(t,
				validEventJSON("good-first", fixedNow),
				tt.body,
				validEventJSON("good-last", fixedNow),
			)), 3)
			want := statusRejected
			if tt.code == "" {
				want = statusExpired
			}
			requireStatuses(t, body, statusAccepted, want, statusAccepted)
			if tt.code != "" {
				requireCodes(t, body, "", tt.code, "")
			}
			// A position refused by decoding never opens a transaction; the two
			// valid neighbours always do.
			wantBegins := 2
			if tt.decidedInTransaction {
				wantBegins = 3
			}
			if begins := environment.store.called("begin"); begins != wantBegins {
				t.Fatalf("begins=%d want=%d", begins, wantBegins)
			}
		})
	}
}

func TestExpiryPrecedesAnyIdentityLookup(t *testing.T) {
	environment := newTestEnvironment(t)
	// Seed the identity exactly as an earlier accepted batch would have left it.
	environment.store.events["known-identity"] = fakeEvent{
		version: normalizationVersion,
		hash:    make([]byte, sha256.Size),
	}
	body := decodeResults(t, environment.send(t, encodedBatch(t,
		`{"source_event_id":"known-identity","provider":"p","model":"m","occurred_at":"2026-04-30T13:59:59.999999Z","usage":{}}`,
	)), 1)
	requireStatuses(t, body, statusExpired)
	if lookups := environment.store.called("existing"); lookups != 0 {
		t.Fatalf("identity lookups=%d want=0 before an expired decision", lookups)
	}
	if inserts := environment.store.called("insert"); inserts != 0 {
		t.Fatalf("inserts=%d want=0", inserts)
	}
}

func TestWindowIsResampledAfterInsertAndAfterMinute(t *testing.T) {
	// The event sits exactly on the lower bound at T0 and is one microsecond late
	// at T1, which is the documented example 7: it must be rolled back, not
	// committed, and it must not reach the source stamp.
	for _, tt := range []struct {
		name  string
		clock []time.Time
		want  string
	}{
		{name: "left window after insert", clock: []time.Time{fixedNow, fixedNow.Add(time.Microsecond)}, want: statusExpired},
		{name: "left window after minute", clock: []time.Time{fixedNow, fixedNow, fixedNow.Add(time.Microsecond)}, want: statusExpired},
		{name: "still valid", clock: []time.Time{fixedNow}, want: statusAccepted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			environment := newTestEnvironment(t)
			environment.store.clock.times = tt.clock
			body := decodeResults(t, environment.send(t, encodedBatch(t,
				`{"source_event_id":"boundary-lower","provider":"p","model":"m","occurred_at":"2026-04-30T14:00:00.000000Z","usage":{}}`,
			)), 1)
			requireStatuses(t, body, tt.want)
			if tt.want != statusAccepted {
				if commits := environment.store.called("commit"); commits != 0 {
					t.Fatalf("commits=%d want=0 for an expired item", commits)
				}
				if environment.store.touched != 0 {
					t.Fatalf("source touches=%d want=0", environment.store.touched)
				}
			}
		})
	}
}

func TestWindowBoundariesAreInclusive(t *testing.T) {
	for _, tt := range []struct {
		name       string
		occurredAt string
		want       string
		wantCode   string
	}{
		{name: "exactly at the lower bound", occurredAt: "2026-04-30T14:00:00.000000Z", want: statusAccepted},
		{name: "one microsecond under", occurredAt: "2026-04-30T13:59:59.999999Z", want: statusExpired},
		{name: "exactly now", occurredAt: "2026-10-27T14:00:00.000000Z", want: statusAccepted},
		{name: "one microsecond later", occurredAt: "2026-10-27T14:00:00.000001Z", want: statusRejected, wantCode: codeFutureEvent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			environment := newTestEnvironment(t)
			body := decodeResults(t, environment.send(t, encodedBatch(t,
				`{"source_event_id":"edge","provider":"p","model":"m","occurred_at":"`+tt.occurredAt+`","usage":{}}`,
			)), 1)
			requireStatuses(t, body, tt.want)
			if tt.wantCode != "" {
				requireCodes(t, body, tt.wantCode)
			}
		})
	}
}

func TestCommitAndDependencyFailuresAreRetryable(t *testing.T) {
	for _, tt := range []struct {
		name string
		step string
		// wantCode is the documented retryable code for the failing step.
		wantCode string
		// insertResult overrides the insert outcome: -1 means a new row, 0 means
		// the identity already exists.
		insertResult int
	}{
		{name: "begin fails", step: "begin", wantCode: codeUnavailable, insertResult: -1},
		{name: "server time fails", step: "now", wantCode: codeUnavailable, insertResult: -1},
		{name: "project lookup fails", step: "project", wantCode: codeUnavailable, insertResult: -1},
		{name: "insert fails", step: "insert", wantCode: codeUnavailable, insertResult: -1},
		{name: "conflict read fails", step: "existing", wantCode: codeUnavailable, insertResult: 0},
		{name: "minute increment fails", step: "minute", wantCode: codeUnavailable, insertResult: -1},
		{name: "commit fails", step: "commit", wantCode: codeInternal, insertResult: -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			environment := newTestEnvironment(t)
			environment.store.failure[tt.step] = errors.New("synthetic: driver detail")
			environment.store.projects[syntheticID(0x44).Bytes] = struct{}{}
			environment.store.insertResult = tt.insertResult
			body := decodeResults(t, environment.send(t, encodedBatch(t,
				`{"source_event_id":"failing","project_id":"44444444-4444-4444-4444-444444444445","provider":"p","model":"m","occurred_at":"2026-10-27T13:00:00Z","usage":{}}`,
			)), 1)
			requireStatuses(t, body, statusRetryable)
			requireCodes(t, body, tt.wantCode)
			if body.Results[0].Error.Message == "" || strings.Contains(body.Results[0].Error.Message, "driver detail") {
				t.Fatalf("unsafe message: %q", body.Results[0].Error.Message)
			}
			if strings.Contains(environment.logs.String(), "driver detail") {
				t.Fatal("logs leaked the driver message")
			}
			if environment.store.touched != 0 {
				t.Fatalf("source touches=%d want=0", environment.store.touched)
			}
		})
	}
}

func TestVanishedIdentityIsRetryableNotAccepted(t *testing.T) {
	environment := newTestEnvironment(t)
	environment.store.insertResult = 0
	environment.store.vanish = true
	body := decodeResults(t, environment.send(t, encodedBatch(t,
		`{"source_event_id":"vanished","provider":"p","model":"m","occurred_at":"2026-10-27T13:00:00Z","usage":{}}`,
	)), 1)
	requireStatuses(t, body, statusRetryable)
	requireCodes(t, body, codeInternal)
	if inserts := environment.store.called("insert"); inserts != 1 {
		t.Fatalf("inserts=%d want=1 (no blind reinsert loop)", inserts)
	}
}

func TestConflictAndCrossVersionComparison(t *testing.T) {
	t.Run("same identity different content", func(t *testing.T) {
		environment := newTestEnvironment(t)
		first := validEventJSON("req-conflict", fixedNow)
		second := `{"source_event_id":"req-conflict","provider":"example-provider","model":"example-model","occurred_at":"2026-10-27T14:00:00Z","usage":{"input_tokens":{"value":101,"quality":"known"},"output_tokens":{"value":20,"quality":"known"}}}`
		body := decodeResults(t, environment.send(t, encodedBatch(t, first, second)), 2)
		requireStatuses(t, body, statusAccepted, statusConflict)
	})
	t.Run("stored version cannot be projected", func(t *testing.T) {
		environment := newTestEnvironment(t)
		environment.store.events["req-version"] = fakeEvent{version: "tokenpivot-usage-event/v2", hash: make([]byte, sha256.Size)}
		body := decodeResults(t, environment.send(t, encodedBatch(t,
			validEventJSON("req-version", fixedNow),
		)), 1)
		requireStatuses(t, body, statusRejected)
		requireCodes(t, body, codeNormalizationIncompatible)
	})
}

func TestProjectRules(t *testing.T) {
	projectID := syntheticID(0x44)
	t.Run("unknown project is rejected and not stored as null", func(t *testing.T) {
		environment := newTestEnvironment(t)
		body := decodeResults(t, environment.send(t, encodedBatch(t,
			`{"source_event_id":"req-project","project_id":"44444444-4444-4444-4444-444444444445","provider":"p","model":"m","occurred_at":"2026-10-27T13:00:00Z","usage":{}}`,
		)), 1)
		requireStatuses(t, body, statusRejected)
		requireCodes(t, body, codeProjectInvalid)
		if inserts := environment.store.called("insert"); inserts != 0 {
			t.Fatalf("inserts=%d want=0", inserts)
		}
	})
	t.Run("archived project is still accepted", func(t *testing.T) {
		environment := newTestEnvironment(t)
		// The store only answers ownership; an archived row still belongs here.
		environment.store.projects[projectID.Bytes] = struct{}{}
		body := decodeResults(t, environment.send(t, encodedBatch(t,
			`{"source_event_id":"req-archived","project_id":"44444444-4444-4444-4444-444444444445","provider":"p","model":"m","occurred_at":"2026-10-27T13:00:00Z","usage":{}}`,
		)), 1)
		requireStatuses(t, body, statusAccepted)
	})
	t.Run("absent project stays null", func(t *testing.T) {
		environment := newTestEnvironment(t)
		body := decodeResults(t, environment.send(t, encodedBatch(t,
			`{"source_event_id":"req-null-project","provider":"p","model":"m","occurred_at":"2026-10-27T13:00:00Z","usage":{}}`,
		)), 1)
		requireStatuses(t, body, statusAccepted)
		if lookups := environment.store.called("project"); lookups != 0 {
			t.Fatalf("project lookups=%d want=0 for an unattributed event", lookups)
		}
	})
}

func TestCancellationStopsRemainingItemsWithoutAccepted(t *testing.T) {
	environment := newTestEnvironment(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	items := []item{
		{event: mustDecodeEvent(t, validEventJSON("req-a", fixedNow))},
		{event: mustDecodeEvent(t, validEventJSON("req-b", fixedNow))},
	}
	results := New(environment.store, environment.auth, nil).process(ctx, credentials.Credential{
		WorkspaceID: syntheticWorkspace,
		SourceID:    syntheticSource,
	}, items)
	if len(results) != 2 {
		t.Fatalf("results=%d want=2", len(results))
	}
	for i, result := range results {
		if result.status != statusRetryable || result.code != codeDeadlineExceeded {
			t.Fatalf("result %d=%+v want retryable/DEADLINE_EXCEEDED", i, result)
		}
	}
	if sequence := environment.store.sequence(); sequence != "touch" && sequence != "" {
		t.Fatalf("store calls=%q want no item transaction", sequence)
	}
}

func TestSourceStampOnlyMovesOnAcceptedAndFailureIsNotFatal(t *testing.T) {
	t.Run("duplicate only leaves the stamp alone", func(t *testing.T) {
		environment := newTestEnvironment(t)
		environment.store.events["req-dup"] = fakeEvent{version: normalizationVersion, hash: hashOf(t, validEventJSON("req-dup", fixedNow))}
		body := decodeResults(t, environment.send(t, encodedBatch(t, validEventJSON("req-dup", fixedNow))), 1)
		requireStatuses(t, body, statusDuplicate)
		if environment.store.touched != 0 {
			t.Fatalf("source touches=%d want=0", environment.store.touched)
		}
	})
	t.Run("stamp failure keeps the accepted result", func(t *testing.T) {
		environment := newTestEnvironment(t)
		environment.store.failure["touch"] = errors.New("synthetic: update failed")
		body := decodeResults(t, environment.send(t, encodedBatch(t, validEventJSON("req-accepted", fixedNow))), 1)
		requireStatuses(t, body, statusAccepted)
		if environment.store.touched != 1 {
			t.Fatalf("source touches=%d want=1", environment.store.touched)
		}
		if !strings.Contains(environment.logs.String(), "source last_received_at update failed") {
			t.Fatal("missing sanitized stamp failure log")
		}
	})
}

// mustDecodeEvent decodes one event through the production path.
func mustDecodeEvent(t *testing.T, eventJSON string) *event {
	t.Helper()
	items, detail, ok := decodeBatch([]byte(encodedBatch(t, eventJSON)))
	if !ok {
		t.Fatalf("decode failed: %+v", detail)
	}
	if len(items) != 1 || items[0].event == nil {
		t.Fatalf("decode produced no event: %+v", items[0].rejection)
	}
	return items[0].event
}

// hashOf returns the payload hash of one event JSON.
func hashOf(t *testing.T, eventJSON string) []byte {
	t.Helper()
	decoded := mustDecodeEvent(t, eventJSON)
	return decoded.PayloadHash[:]
}

func TestNormalizationVectors(t *testing.T) {
	for _, tt := range []struct {
		name  string
		event string
		text  string
		hash  string
	}{
		{
			name:  "vector A: offset equivalence with derived total",
			event: `{"source_event_id":"req-a1","provider":"example-provider","model":"example-model","occurred_at":"2026-10-27T14:00:00Z","usage":{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"}},"status":"success","duration_ms":1250}`,
			text:  `["2026-10-27T14:00:00.000000Z",null,"example-provider","example-model",{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"},"total_tokens":{"value":120,"quality":"known"},"cache_read_tokens":{"value":null,"quality":"missing"},"cache_write_tokens":{"value":null,"quality":"missing"},"reasoning_tokens":{"value":null,"quality":"missing"}},"success",1250]`,
			hash:  "66311d0f0854f50184e40db98ea8bc0f086c470b1dd0817eb512fdb17cbdc121",
		},
		{
			name:  "vector B: field order and UUID case equivalence",
			event: `{"source_event_id":"req-b1","duration_ms":null,"status":"success","usage":{"reasoning_tokens":{"quality":"known","value":50},"cache_read_tokens":{"value":400,"quality":"known"},"output_tokens":{"value":250,"quality":"estimated"},"input_tokens":{"value":1000,"quality":"known"}},"occurred_at":"2026-10-26T23:30:00.123456Z","model":"example-model","provider":"example-provider","project_id":"0199A0B0-0000-7000-8000-00000000000A"}`,
			text:  `["2026-10-26T23:30:00.123456Z","0199a0b0-0000-7000-8000-00000000000a","example-provider","example-model",{"input_tokens":{"value":1000,"quality":"known"},"output_tokens":{"value":250,"quality":"estimated"},"total_tokens":{"value":1250,"quality":"estimated"},"cache_read_tokens":{"value":400,"quality":"known"},"cache_write_tokens":{"value":null,"quality":"missing"},"reasoning_tokens":{"value":50,"quality":"known"}},"success",null]`,
			hash:  "538702154d0bc75b274ecb4e70bfc7ec6241a2029d9c07dff6b68e0242ba3e3e",
		},
		{
			name:  "vector C1: real zero",
			event: `{"source_event_id":"req-c1","provider":"example-provider","model":"example-model","occurred_at":"2026-10-27T00:00:00Z","usage":{"input_tokens":{"value":0,"quality":"known"}},"duration_ms":0}`,
			text:  `["2026-10-27T00:00:00.000000Z",null,"example-provider","example-model",{"input_tokens":{"value":0,"quality":"known"},"output_tokens":{"value":null,"quality":"missing"},"total_tokens":{"value":null,"quality":"missing"},"cache_read_tokens":{"value":null,"quality":"missing"},"cache_write_tokens":{"value":null,"quality":"missing"},"reasoning_tokens":{"value":null,"quality":"missing"}},null,0]`,
			hash:  "fc31a256fba11fc95019164f220e4b47f6673d924c1135c0a57b3117ac841c89",
		},
		{
			name:  "vector C2: all metrics missing",
			event: `{"source_event_id":"req-c2","provider":"example-provider","model":"example-model","occurred_at":"2026-10-27T00:00:00Z","usage":{},"duration_ms":0}`,
			text:  `["2026-10-27T00:00:00.000000Z",null,"example-provider","example-model",{"input_tokens":{"value":null,"quality":"missing"},"output_tokens":{"value":null,"quality":"missing"},"total_tokens":{"value":null,"quality":"missing"},"cache_read_tokens":{"value":null,"quality":"missing"},"cache_write_tokens":{"value":null,"quality":"missing"},"reasoning_tokens":{"value":null,"quality":"missing"}},null,0]`,
			hash:  "48a0d3828e2b02476e0fb61998b81019821041d605f9f180aea49015a86ce816",
		},
		{
			name:  "vector D1: cache read known",
			event: `{"source_event_id":"req-d1","provider":"example-provider","model":"example-model","occurred_at":"2026-10-27T12:00:00.500000Z","usage":{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"},"cache_read_tokens":{"value":60,"quality":"known"}},"status":"success","duration_ms":900}`,
			text:  `["2026-10-27T12:00:00.500000Z",null,"example-provider","example-model",{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"},"total_tokens":{"value":120,"quality":"known"},"cache_read_tokens":{"value":60,"quality":"known"},"cache_write_tokens":{"value":null,"quality":"missing"},"reasoning_tokens":{"value":null,"quality":"missing"}},"success",900]`,
			hash:  "033249310b0dcc7b79b675f99fce10e320761e5ba84120a873ddaf9fab1229cf",
		},
		{
			name:  "vector D2: cache read estimated",
			event: `{"source_event_id":"req-d2","provider":"example-provider","model":"example-model","occurred_at":"2026-10-27T12:00:00.500000Z","usage":{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"},"cache_read_tokens":{"value":60,"quality":"estimated"}},"status":"success","duration_ms":900}`,
			text:  `["2026-10-27T12:00:00.500000Z",null,"example-provider","example-model",{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"},"total_tokens":{"value":120,"quality":"known"},"cache_read_tokens":{"value":60,"quality":"estimated"},"cache_write_tokens":{"value":null,"quality":"missing"},"reasoning_tokens":{"value":null,"quality":"missing"}},"success",900]`,
			hash:  "b488e38a6331fcb575c37af32bfbadcbbde4b6d7f037cb81ca4e5d3e5a6a61d4",
		},
		{
			name:  "vector E: explicit missing total matches omission",
			event: `{"source_event_id":"req-e2","provider":"example-provider","model":"example-model","occurred_at":"2026-10-27T09:15:00Z","usage":{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"},"total_tokens":{"value":null,"quality":"missing"}},"status":"success","duration_ms":640}`,
			text:  `["2026-10-27T09:15:00.000000Z",null,"example-provider","example-model",{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"},"total_tokens":{"value":120,"quality":"known"},"cache_read_tokens":{"value":null,"quality":"missing"},"cache_write_tokens":{"value":null,"quality":"missing"},"reasoning_tokens":{"value":null,"quality":"missing"}},"success",640]`,
			hash:  "2936ec89e70daa4df0d027f1944d9c887bb2b4a23e866799d0c2c51109b566b8",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			decoded := mustDecodeEvent(t, tt.event)
			if text := string(decoded.normalizedText()); text != tt.text {
				t.Fatalf("normalized text mismatch\ngot:  %s\nwant: %s", text, tt.text)
			}
			digest := sha256.Sum256([]byte(tt.text))
			if wantDigest, _ := hex.DecodeString(tt.hash); !bytes.Equal(digest[:], wantDigest) {
				t.Fatalf("vector hash=%x want=%s", digest, tt.hash)
			}
			if got := hex.EncodeToString(decoded.PayloadHash[:]); got != tt.hash {
				t.Fatalf("payload hash=%s want=%s", got, tt.hash)
			}
		})
	}
}

func TestNormalizationEquivalences(t *testing.T) {
	// Different offsets and both total spellings must produce one digest, while a
	// real zero, an estimated zero and a missing value must differ.
	offsetZ := mustDecodeEvent(t, `{"source_event_id":"a","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00Z","usage":{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"}}}`)
	offsetLocal := mustDecodeEvent(t, `{"source_event_id":"a","provider":"p","model":"m","occurred_at":"2026-10-27T07:00:00-07:00","usage":{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"}}}`)
	micro := mustDecodeEvent(t, `{"source_event_id":"a","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00.000000Z","usage":{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"}}}`)
	lowerT := mustDecodeEvent(t, `{"source_event_id":"a","provider":"p","model":"m","occurred_at":"2026-10-27T14:00:00z","usage":{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"}}}`)
	for name, decoded := range map[string]*event{"offset": offsetLocal, "micro seconds": micro, "lower case z": lowerT} {
		if decoded.PayloadHash != offsetZ.PayloadHash {
			t.Fatalf("%s spelling produced a different digest", name)
		}
	}
	zeroKnown := mustDecodeEvent(t, `{"source_event_id":"a","provider":"p","model":"m","occurred_at":"2026-10-27T00:00:00Z","usage":{"input_tokens":{"value":0,"quality":"known"}}}`)
	zeroEstimated := mustDecodeEvent(t, `{"source_event_id":"a","provider":"p","model":"m","occurred_at":"2026-10-27T00:00:00Z","usage":{"input_tokens":{"value":0,"quality":"estimated"}}}`)
	missingInput := mustDecodeEvent(t, `{"source_event_id":"a","provider":"p","model":"m","occurred_at":"2026-10-27T00:00:00Z","usage":{}}`)
	seen := map[string]string{}
	for name, decoded := range map[string]*event{"zero known": zeroKnown, "zero estimated": zeroEstimated, "missing": missingInput} {
		digest := hex.EncodeToString(decoded.PayloadHash[:])
		if previous, duplicate := seen[digest]; duplicate {
			t.Fatalf("%s and %s share a digest", previous, name)
		}
		seen[digest] = name
	}
}

func TestUsageNormalizationRules(t *testing.T) {
	for _, tt := range []struct {
		name  string
		usage string
		code  string
		check func(*testing.T, event)
	}{
		{
			name:  "total only is preserved without reverse derivation",
			usage: `{"total_tokens":{"value":500,"quality":"known"}}`,
			check: func(t *testing.T, decoded event) {
				if decoded.Usage.Total.Value == nil || *decoded.Usage.Total.Value != 500 || decoded.Usage.Total.Quality != aggregation.QualityKnown {
					t.Fatalf("total=%+v", decoded.Usage.Total)
				}
				if decoded.Usage.Input.Value != nil || decoded.Usage.Output.Value != nil {
					t.Fatal("input or output was reverse derived")
				}
			},
		},
		{
			name:  "mixed quality derives an estimated total",
			usage: `{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":250,"quality":"estimated"}}`,
			check: func(t *testing.T, decoded event) {
				if decoded.Usage.Total.Value == nil || *decoded.Usage.Total.Value != 350 || decoded.Usage.Total.Quality != aggregation.QualityEstimated {
					t.Fatalf("total=%+v", decoded.Usage.Total)
				}
			},
		},
		{
			name:  "explicit agreeing known total is kept",
			usage: `{"input_tokens":{"value":100,"quality":"known"},"output_tokens":{"value":20,"quality":"known"},"total_tokens":{"value":120,"quality":"known"}}`,
			check: func(t *testing.T, decoded event) {
				if decoded.Usage.Total.Value == nil || *decoded.Usage.Total.Value != 120 {
					t.Fatalf("total=%+v", decoded.Usage.Total)
				}
			},
		},
		{
			name:  "estimated parent may be smaller than a known child",
			usage: `{"input_tokens":{"value":50,"quality":"estimated"},"cache_read_tokens":{"value":60,"quality":"known"}}`,
			check: func(t *testing.T, decoded event) {},
		},
		{
			name:  "estimated zero is legal",
			usage: `{"input_tokens":{"value":0,"quality":"estimated"}}`,
			check: func(t *testing.T, decoded event) {},
		},
		{
			name:  "known cache write exceeds known input",
			usage: `{"input_tokens":{"value":1,"quality":"known"},"cache_write_tokens":{"value":2,"quality":"known"}}`,
			code:  codeUsageUnsupported,
		},
		{
			name:  "known reasoning exceeds known output",
			usage: `{"output_tokens":{"value":1,"quality":"known"},"reasoning_tokens":{"value":2,"quality":"known"}}`,
			code:  codeUsageUnsupported,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			items, _, ok := decodeBatch([]byte(`{"events":[{"source_event_id":"req","provider":"p","model":"m","occurred_at":"2026-10-27T13:00:00Z","usage":` + tt.usage + `}]}`))
			if !ok {
				t.Fatal("batch decode failed")
			}
			if tt.code != "" {
				if items[0].rejection == nil || items[0].rejection.code != tt.code {
					t.Fatalf("rejection=%+v want code=%s", items[0].rejection, tt.code)
				}
				return
			}
			if items[0].rejection != nil {
				t.Fatalf("unexpected rejection: %+v", items[0].rejection)
			}
			tt.check(t, *items[0].event)
		})
	}
}

func TestDigitLexemeAndIntegerRange(t *testing.T) {
	for _, tt := range []struct {
		number string
		want   bool
	}{
		{"0", true},
		{"12", true},
		{"9007199254740991", true},
		{"-1", false},
		{"-0", false},
		{"+1", false},
		{"1.0", false},
		{"1e2", false},
		{"01", false},
		{"", false},
	} {
		if got := decimalIntegerLexeme(tt.number); got != tt.want {
			t.Fatalf("decimalIntegerLexeme(%q)=%t want=%t", tt.number, got, tt.want)
		}
	}
	if value, ok := parseInteger("9007199254740992"); ok {
		t.Fatalf("value=%d accepted above 2^53-1", value)
	}
}

func TestJSONStringEscaping(t *testing.T) {
	for _, tt := range []struct {
		text string
		want string
	}{
		{"plain", `"plain"`},
		{`quote"inside`, `"quote\"inside"`},
		{`back\slash`, `"back\\slash"`},
		{"hz汉", `"hz汉"`},
		{"tab\tinside", `"tab\u0009inside"`},
		{"/kept", `"/kept"`},
	} {
		got := string(appendJSONString(nil, tt.text))
		if got != tt.want {
			t.Fatalf("escaping %q=%s want=%s", tt.text, got, tt.want)
		}
		var roundTrip string
		if err := json.Unmarshal([]byte(got), &roundTrip); err != nil || roundTrip != tt.text {
			t.Fatalf("escaping %q is not a JSON string: %v", tt.text, err)
		}
	}
}

func TestRouteRegistrationAndFallback(t *testing.T) {
	t.Run("no handler registered without a service", func(t *testing.T) {
		mux := http.NewServeMux()
		NewHandler(nil).Register(mux)
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, ingestionPath, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status=%d want=404", recorder.Code)
		}
	})
	t.Run("wrong method is not served", func(t *testing.T) {
		environment := newTestEnvironment(t)
		recorder := environment.sendWith(t, `{}`, func(r *http.Request) {
			r.Method = http.MethodGet
			r.Header.Set("Authorization", "Bearer tpk_synthetic")
		})
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status=%d want=405", recorder.Code)
		}
	})
}

func TestDeadlineExhaustionFillsRemainingItems(t *testing.T) {
	environment := newTestEnvironment(t)
	// A dead context models the 8 second budget being used up between items: the
	// first position is decided, the rest are retryable and never accepted.
	received := encodedBatch(t,
		validEventJSON("req-dl-01", fixedNow),
		validEventJSON("req-dl-02", fixedNow),
		validEventJSON("req-dl-03", fixedNow),
	)
	items, detail, ok := decodeBatch([]byte(received))
	if !ok {
		t.Fatalf("decode failed: %+v", detail)
	}
	// Process one item with a live context, then cancel and continue.
	ctx, cancel := context.WithCancel(context.Background())
	service := New(environment.store, environment.auth, nil)
	first := service.process(ctx, credentials.Credential{WorkspaceID: syntheticWorkspace, SourceID: syntheticSource}, items[:1])
	cancel()
	rest := service.process(ctx, credentials.Credential{WorkspaceID: syntheticWorkspace, SourceID: syntheticSource}, items[1:])
	if first[0].status != statusAccepted {
		t.Fatalf("first=%+v want accepted", first[0])
	}
	for i, result := range rest {
		if result.status != statusRetryable || result.code != codeDeadlineExceeded {
			t.Fatalf("remaining %d=%+v want retryable/DEADLINE_EXCEEDED", i, result)
		}
	}
	if commits := environment.store.called("commit"); commits != 1 {
		t.Fatalf("commits=%d want=1", commits)
	}
}

func TestDeadlineOutranksPreDecodedRejection(t *testing.T) {
	environment := newTestEnvironment(t)
	service := New(environment.store, environment.auth, nil)
	items := []item{
		{event: mustDecodeEvent(t, validEventJSON("req-order-1", fixedNow))},
		// A permanently invalid position after the cancellation point: without the
		// budget check first it would be reported as rejected forever.
		{rejection: invalidItem("synthetic: not a JSON object")},
		{event: mustDecodeEvent(t, validEventJSON("req-order-3", fixedNow))},
	}
	ctx, cancel := context.WithCancel(context.Background())
	environment.store.onStep = func(step string) {
		if step == "commit" {
			cancel()
		}
	}
	results := service.process(ctx, credentials.Credential{WorkspaceID: syntheticWorkspace, SourceID: syntheticSource}, items)
	if results[0].status != statusAccepted {
		t.Fatalf("first=%+v want accepted", results[0])
	}
	for i := 1; i < len(results); i++ {
		if results[i].status != statusRetryable || results[i].code != codeDeadlineExceeded {
			t.Fatalf("result %d=%+v want retryable/DEADLINE_EXCEEDED", i, results[i])
		}
	}
}

func TestCommitDeadlineIsDeadlineNotInternal(t *testing.T) {
	t.Run("commit blocks until the budget ends", func(t *testing.T) {
		environment := newTestEnvironment(t)
		environment.store.commitWaitsForContext = true
		service := New(environment.store, environment.auth, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		items := []item{{event: mustDecodeEvent(t, validEventJSON("req-commit-deadline", fixedNow))}}
		results := service.process(ctx, credentials.Credential{WorkspaceID: syntheticWorkspace, SourceID: syntheticSource}, items)
		if len(results) != 1 {
			t.Fatalf("results=%d want=1", len(results))
		}
		if results[0].status != statusRetryable || results[0].code != codeDeadlineExceeded {
			t.Fatalf("result=%+v want retryable/DEADLINE_EXCEEDED", results[0])
		}
		if commits := environment.store.called("commit"); commits != 1 {
			t.Fatalf("commits=%d want=1", commits)
		}
		if rollbacks := environment.store.called("rollback"); rollbacks != 1 {
			t.Fatalf("rollbacks=%d want=1", rollbacks)
		}
	})

	t.Run("commit fails while the budget is already gone", func(t *testing.T) {
		environment := newTestEnvironment(t)
		environment.store.failure["commit"] = errors.New("synthetic: driver detail")
		service := New(environment.store, environment.auth, nil)
		ctx, cancel := context.WithCancel(context.Background())
		environment.store.onStep = func(step string) {
			if step == "commit" {
				cancel()
			}
		}
		items := []item{{event: mustDecodeEvent(t, validEventJSON("req-commit-canceled", fixedNow))}}
		results := service.process(ctx, credentials.Credential{WorkspaceID: syntheticWorkspace, SourceID: syntheticSource}, items)
		if results[0].status != statusRetryable || results[0].code != codeDeadlineExceeded {
			t.Fatalf("result=%+v want retryable/DEADLINE_EXCEEDED", results[0])
		}
		if strings.Contains(environment.logs.String(), "driver detail") {
			t.Fatal("logs leaked the driver message")
		}
	})
}

func TestRollbackFailureIsLoggedWithoutDriverDetail(t *testing.T) {
	const marker = "synthetic-sensitive-marker"
	environment := newTestEnvironment(t)
	environment.store.failure["insert"] = errors.New("synthetic: driver detail")
	environment.store.rollbackFailure = errors.New("synthetic-rollback-failure: connection lost " + marker)
	body := decodeResults(t, environment.send(t, encodedBatch(t,
		validEventJSON("req-rollback", fixedNow),
	)), 1)
	requireStatuses(t, body, statusRetryable)
	requireCodes(t, body, codeUnavailable)
	if rollbacks := environment.store.called("rollback"); rollbacks != 1 {
		t.Fatalf("rollbacks=%d want=1", rollbacks)
	}
	logs := environment.logs.String()
	if strings.Contains(logs, marker) {
		t.Fatalf("rollback log leaked the driver detail: %s", logs)
	}
	if !strings.Contains(logs, "ingestion rollback") {
		t.Fatalf("missing the sanitized rollback stage log: %s", logs)
	}
}

func TestRollbackUsesItsOwnBoundedContext(t *testing.T) {
	// A canceled request still rolls the item back, with a fresh budget that is
	// independent of the request but still bounded.
	environment := newTestEnvironment(t)
	environment.store.failure["insert"] = errors.New("synthetic: driver detail")
	ctx, cancel := context.WithCancel(context.Background())
	environment.store.onStep = func(step string) {
		if step == "insert" {
			cancel()
		}
	}
	recorder := &contextRecordingStore{inner: environment.store}
	results := New(recorder, environment.auth, nil).process(ctx, credentials.Credential{
		WorkspaceID: syntheticWorkspace, SourceID: syntheticSource,
	}, []item{{event: mustDecodeEvent(t, validEventJSON("req-rollback-ctx", fixedNow))}})
	if len(results) != 1 || results[0].status != statusRetryable {
		t.Fatalf("results=%+v want one retryable result", results)
	}
	if recorder.rollbackErr != nil {
		t.Fatalf("rollback context error=%v want a live dedicated context", recorder.rollbackErr)
	}
	if !recorder.rollbackDeadlinePresent {
		t.Fatal("rollback context had no deadline")
	}
	if remaining := time.Until(recorder.rollbackDeadline); remaining > rollbackBudget {
		t.Fatalf("rollback deadline is %s away, want at most %s", remaining, rollbackBudget)
	}
}

// contextRecordingStore wraps a store and records the context the service hands to
// Rollback, so the dedicated rollback budget can be observed.
type contextRecordingStore struct {
	inner                   *fakeStore
	rollbackErr             error
	rollbackDeadline        time.Time
	rollbackDeadlinePresent bool
}

func (s *contextRecordingStore) BeginItem(ctx context.Context) (itemTx, error) {
	tx, err := s.inner.BeginItem(ctx)
	if err != nil {
		return nil, err
	}
	return &contextRecordingTx{inner: tx.(*fakeTx), recorder: s}, nil
}

func (s *contextRecordingStore) TouchSource(ctx context.Context, workspaceID, sourceID credentials.ID) error {
	return s.inner.TouchSource(ctx, workspaceID, sourceID)
}

// contextRecordingTx records the rollback context of one item transaction.
type contextRecordingTx struct {
	inner    *fakeTx
	recorder *contextRecordingStore
}

func (t *contextRecordingTx) Now(ctx context.Context) (time.Time, error) {
	return t.inner.Now(ctx)
}

func (t *contextRecordingTx) ProjectExists(ctx context.Context, workspaceID, projectID credentials.ID) (bool, error) {
	return t.inner.ProjectExists(ctx, workspaceID, projectID)
}

func (t *contextRecordingTx) InsertEvent(ctx context.Context, event storedEvent) (bool, error) {
	return t.inner.InsertEvent(ctx, event)
}

func (t *contextRecordingTx) ExistingEvent(ctx context.Context, workspaceID, sourceID credentials.ID, sourceEventID string) (storedEvent, bool, error) {
	return t.inner.ExistingEvent(ctx, workspaceID, sourceID, sourceEventID)
}

func (t *contextRecordingTx) ApplyMinute(ctx context.Context, input aggregation.MinuteInput) error {
	return t.inner.ApplyMinute(ctx, input)
}

func (t *contextRecordingTx) Commit(ctx context.Context) error { return t.inner.Commit(ctx) }

func (t *contextRecordingTx) Rollback(ctx context.Context) error {
	t.recorder.rollbackErr = ctx.Err()
	t.recorder.rollbackDeadline, t.recorder.rollbackDeadlinePresent = ctx.Deadline()
	return t.inner.Rollback(ctx)
}

func TestHandlerBudgetStartsAtEntry(t *testing.T) {
	environment := newTestEnvironment(t)
	request := httptest.NewRequest(http.MethodPost, ingestionPath, strings.NewReader(encodedBatch(t, validEventJSON("req-budget", fixedNow))))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+environment.auth.token)
	entered := time.Now()
	recorder := httptest.NewRecorder()
	environment.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	// Authentication runs under the same deadline, and that deadline is the
	// handler entry plus the budget: it is not restarted after the body read.
	window := environment.auth.deadline.Sub(entered)
	if window < processingBudget || window > processingBudget+time.Second {
		t.Fatalf("authentication deadline is %s after entry, want about the %s budget", window, processingBudget)
	}
}

// deadlineReader fails the body read the way a connection-level read deadline does.
type deadlineReader struct{}

func (deadlineReader) Read([]byte) (int, error) { return 0, os.ErrDeadlineExceeded }

func TestBodyReadDeadlineIsRetryableNotInvalidArgument(t *testing.T) {
	environment := newTestEnvironment(t)
	request := httptest.NewRequest(http.MethodPost, ingestionPath, deadlineReader{})
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+environment.auth.token)
	recorder := httptest.NewRecorder()
	environment.handler.ServeHTTP(recorder, request)
	decodeBatchError(t, recorder, http.StatusServiceUnavailable, "UNAVAILABLE")
	if sequence := environment.store.sequence(); sequence != "" {
		t.Fatalf("store calls=%q want none", sequence)
	}
}

func TestPostgresTransactionErrorsAreSanitized(t *testing.T) {
	const marker = "synthetic-sensitive-marker"
	failing := failingTx{err: errors.New("driver detail " + marker)}
	tx := &postgresTx{tx: failing}

	commitErr := tx.Commit(context.Background())
	if commitErr == nil {
		t.Fatal("commit error was dropped")
	}
	rollbackErr := tx.Rollback(context.Background())
	if rollbackErr == nil {
		t.Fatal("rollback error was dropped")
	}
	for _, err := range []error{commitErr, rollbackErr} {
		if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "driver detail") {
			t.Fatalf("sanitized error leaked driver detail: %q", err)
		}
	}
}

// failingTx satisfies pgx.Tx through the embedded interface and only implements the
// two lifecycle calls this test exercises.
type failingTx struct {
	pgx.Tx
	err error
}

func (t failingTx) Commit(context.Context) error   { return t.err }
func (t failingTx) Rollback(context.Context) error { return t.err }
