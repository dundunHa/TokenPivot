package ingestion

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dundunHa/TokenPivot/internal/aggregation"
	"github.com/dundunHa/TokenPivot/internal/credentials"
	"github.com/dundunHa/TokenPivot/internal/database"
)

// ingestionPath is the single frozen ingestion endpoint.
const ingestionPath = "/api/v1/ingestion/events"

// Handler serves the synchronous batch ingestion endpoint. It owns the batch-wide
// gates (media type, credential, body size, unambiguous JSON, shell fields, item
// count) and hands the decoded positions to the service, which runs the per-item
// transactions.
type Handler struct {
	Service *Service
}

// NewHandler returns the HTTP adapter for a service. The route is only registered
// when the server receives this value, so a missing service never fails open.
func NewHandler(service *Service) *Handler {
	return &Handler{Service: service}
}

// Register binds the ingestion route on mux. Both the method and the exact path are
// pinned, so other methods and unknown paths keep their previous behavior.
func (h *Handler) Register(mux *http.ServeMux) {
	if h == nil || h.Service == nil {
		return
	}
	mux.Handle("POST "+ingestionPath, h)
}

// batchResponse is the HTTP 200 body: an opaque request id and one result per input
// position.
type batchResponse struct {
	RequestID string               `json:"request_id"`
	Results   []itemResultResponse `json:"results"`
}

// itemResultResponse is one per-item result. error is present exactly for rejected
// and retryable.
type itemResultResponse struct {
	Index  int                `json:"index"`
	Status string             `json:"status"`
	Error  *itemErrorResponse `json:"error,omitempty"`
}

// itemErrorResponse is the per-item error body. The message never carries SQL,
// credentials or event content.
type itemErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// batchErrorResponse is the whole-batch error envelope.
type batchErrorResponse struct {
	Error batchError `json:"error"`
}

// batchError carries the stable batch code, a human readable message, the request
// id for correlation and optional safe field details.
type batchError struct {
	Code      string             `json:"code"`
	Message   string             `json:"message"`
	RequestID string             `json:"request_id"`
	Details   []batchErrorDetail `json:"details,omitempty"`
}

// batchErrorDetail names one safe field location by JSON pointer and a short
// reason. It never contains a SQL fragment or a stack.
type batchErrorDetail struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// ServeHTTP runs one batch. Any failure before per-item processing rejects the
// whole batch with no write; afterwards the response is always HTTP 200 with one
// result per input position.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := newRequestID()
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeBatchError(w, http.StatusUnsupportedMediaType, requestID, "UNSUPPORTED_MEDIA_TYPE", "content type must be application/json", nil)
		return
	}
	token, ok := bearerToken(r)
	if !ok {
		// The plugin endpoint never falls back to a browser cookie.
		writeBatchError(w, http.StatusUnauthorized, requestID, "UNAUTHENTICATED", "bearer credential required", nil)
		return
	}
	credential, err := h.Service.Authenticate(r.Context(), token)
	if err != nil {
		if errors.Is(err, credentials.ErrUnauthenticated) {
			writeBatchError(w, http.StatusUnauthorized, requestID, "UNAUTHENTICATED", "credential is invalid or revoked", nil)
			return
		}
		h.Service.logger.ErrorContext(r.Context(), "ingestion authentication failed", "stage", "ingestion authenticate", "request_id", requestID, "err", database.SafeError("ingestion authenticate", err))
		writeBatchError(w, http.StatusServiceUnavailable, requestID, "UNAVAILABLE", "service is unavailable", nil)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeBatchError(w, http.StatusRequestEntityTooLarge, requestID, "BODY_TOO_LARGE", "request body exceeds 1 MiB", nil)
			return
		}
		writeBatchError(w, http.StatusBadRequest, requestID, "INVALID_ARGUMENT", "request body could not be read", nil)
		return
	}
	items, detail, ok := decodeBatch(body)
	if !ok {
		writeBatchError(w, http.StatusBadRequest, requestID, "INVALID_ARGUMENT", "request body is not an acceptable ingestion batch", []batchErrorDetail{detail})
		return
	}

	// The 8 second budget starts at the handler entry, so body reading and
	// decoding count against it. An exhausted budget leaves every unstarted item
	// retryable; it never produces accepted.
	ctx, cancel := context.WithTimeout(r.Context(), processingBudget-time.Since(started))
	defer cancel()
	results := h.Service.process(ctx, credential, items)

	response := batchResponse{RequestID: requestID, Results: make([]itemResultResponse, len(results))}
	for i, outcome := range results {
		response.Results[i] = itemResultResponse{Index: i, Status: outcome.status}
		if outcome.code != "" {
			response.Results[i].Error = &itemErrorResponse{Code: outcome.code, Message: outcome.message}
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// bearerToken extracts exactly one Bearer credential. A second Authorization value,
// another scheme or an empty token is rejected; a valid cookie is ignored.
func bearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, found := strings.Cut(strings.TrimSpace(values[0]), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

// isJSONContentType accepts application/json with optional parameters such as a
// charset.
func isJSONContentType(value string) bool {
	mediaType, _, _ := strings.Cut(value, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "application/json")
}

// readBody reads the whole request body, refusing anything over 1 MiB without
// buffering the excess, and requires valid UTF-8.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(body) {
		return nil, errors.New("ingestion: body is not valid UTF-8")
	}
	return body, nil
}

// newRequestID returns an opaque correlation id. It is not a business identity and
// grants nothing.
func newRequestID() string {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		// A crypto/rand failure is unrecoverable; a constant still yields a usable
		// correlation value without claiming uniqueness.
		return "request-id-unavailable"
	}
	return hex.EncodeToString(buffer[:])
}

// writeJSON writes one JSON body. A write failure means the peer disconnected and
// nothing about the request is logged.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeBatchError writes the shared whole-batch error envelope.
func writeBatchError(w http.ResponseWriter, status int, requestID, code, message string, details []batchErrorDetail) {
	writeJSON(w, status, batchErrorResponse{Error: batchError{Code: code, Message: message, RequestID: requestID, Details: details}})
}

// jsonKind is the decoded JSON value kind.
type jsonKind uint8

const (
	jsonNull jsonKind = iota
	jsonBool
	jsonString
	jsonNumber
	jsonArray
	jsonObject
)

// jsonNode is one decoded JSON value. Numbers keep their original lexeme so the
// decimal integer form can be enforced, and objects keep their pairs in order so
// duplicates can be refused.
type jsonNode struct {
	kind    jsonKind
	text    string
	number  json.Number
	boolean bool
	array   []*jsonNode
	object  []jsonMember
}

// jsonMember is one decoded object entry.
type jsonMember struct {
	key   string
	value *jsonNode
}

// errBatchJSON reports a body that is not one unambiguous JSON document.
var errBatchJSON = errors.New("ingestion: body is not one unambiguous JSON document")

// decodeBatch parses and validates the whole batch. A false result is a whole-batch
// rejection and carries the safe field location that caused it.
func decodeBatch(body []byte) ([]item, batchErrorDetail, bool) {
	root, err := parseJSON(body)
	if err != nil {
		return nil, batchErrorDetail{Field: "/", Reason: "body_not_json"}, false
	}
	if root.kind != jsonObject {
		return nil, batchErrorDetail{Field: "/", Reason: "body_not_object"}, false
	}
	events, detail, ok := shellEvents(root)
	if !ok {
		return nil, detail, false
	}
	items := make([]item, len(events))
	for i, node := range events {
		event, rejection := decodeEvent(node)
		items[i] = item{event: event, rejection: rejection}
	}
	return items, batchErrorDetail{}, true
}

// shellEvents returns the events array after enforcing the shell shape: only
// events, exactly one array with 1 to 100 entries.
func shellEvents(root *jsonNode) ([]*jsonNode, batchErrorDetail, bool) {
	var events *jsonNode
	for _, member := range root.object {
		if member.key != "events" {
			return nil, batchErrorDetail{Field: "/" + member.key, Reason: "unknown_field"}, false
		}
		events = member.value
	}
	if events == nil {
		return nil, batchErrorDetail{Field: "/events", Reason: "missing_events"}, false
	}
	if events.kind != jsonArray {
		return nil, batchErrorDetail{Field: "/events", Reason: "not_an_array"}, false
	}
	switch {
	case len(events.array) == 0:
		return nil, batchErrorDetail{Field: "/events", Reason: "empty_events"}, false
	case len(events.array) > maxBatchEvents:
		return nil, batchErrorDetail{Field: "/events", Reason: "too_many_events"}, false
	}
	return events.array, batchErrorDetail{}, true
}

// parseJSON decodes one JSON document. Duplicated keys at any level, trailing
// content and any other ambiguity reject the whole batch.
func parseJSON(body []byte) (*jsonNode, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	node, err := decodeValue(decoder)
	if err != nil {
		return nil, errBatchJSON
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errBatchJSON
	}
	return node, nil
}

// decodeValue decodes one value recursively, rejecting object keys that repeat.
func decodeValue(decoder *json.Decoder) (*jsonNode, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, errBatchJSON
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			node := &jsonNode{kind: jsonObject}
			seen := make(map[string]struct{}, 8)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, errBatchJSON
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errBatchJSON
				}
				if _, duplicate := seen[key]; duplicate {
					return nil, errBatchJSON
				}
				seen[key] = struct{}{}
				member, err := decodeValue(decoder)
				if err != nil {
					return nil, err
				}
				node.object = append(node.object, jsonMember{key: key, value: member})
			}
			if _, err := decoder.Token(); err != nil {
				return nil, errBatchJSON
			}
			return node, nil
		case '[':
			node := &jsonNode{kind: jsonArray}
			for decoder.More() {
				element, err := decodeValue(decoder)
				if err != nil {
					return nil, err
				}
				node.array = append(node.array, element)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, errBatchJSON
			}
			return node, nil
		default:
			return nil, errBatchJSON
		}
	case string:
		return &jsonNode{kind: jsonString, text: value}, nil
	case json.Number:
		return &jsonNode{kind: jsonNumber, number: value}, nil
	case bool:
		return &jsonNode{kind: jsonBool, boolean: value}, nil
	case nil:
		return &jsonNode{kind: jsonNull}, nil
	default:
		return nil, errBatchJSON
	}
}

// decodeEvent validates one event position. A rejection is permanent and only
// affects this position; it is decided without touching the database.
func decodeEvent(node *jsonNode) (*event, *rejection) {
	if node.kind != jsonObject {
		return nil, invalidItem("event must be a JSON object")
	}
	var (
		decoded        event
		occurredText   string
		haveSourceID   bool
		haveProvider   bool
		haveModel      bool
		haveOccurredAt bool
		haveUsage      bool
	)
	for _, member := range node.object {
		switch member.key {
		case "source_event_id":
			if member.value.kind != jsonString {
				return nil, invalidItem("source_event_id must be a string")
			}
			if !validText(member.value.text, 1, 256) {
				return nil, invalidItem("source_event_id must be 1-256 UTF-8 bytes without control characters")
			}
			decoded.SourceEventID = member.value.text
			haveSourceID = true
		case "project_id":
			switch member.value.kind {
			case jsonNull:
				decoded.ProjectID = credentials.ID{}
			case jsonString:
				id, ok := parseProjectID(member.value.text)
				if !ok {
					return nil, invalidItem("project_id must be a hyphenated UUID")
				}
				decoded.ProjectID = id
			default:
				return nil, invalidItem("project_id must be a UUID string or null")
			}
		case "provider":
			if member.value.kind != jsonString {
				return nil, invalidItem("provider must be a string")
			}
			if !validLabel(member.value.text) {
				return nil, invalidItem("provider must be 1-128 UTF-8 bytes without control characters or surrounding whitespace")
			}
			decoded.Provider = member.value.text
			haveProvider = true
		case "model":
			if member.value.kind != jsonString {
				return nil, invalidItem("model must be a string")
			}
			if !validLabel(member.value.text) {
				return nil, invalidItem("model must be 1-128 UTF-8 bytes without control characters or surrounding whitespace")
			}
			decoded.Model = member.value.text
			haveModel = true
		case "occurred_at":
			if member.value.kind != jsonString {
				return nil, invalidItem("occurred_at must be an RFC 3339 string")
			}
			occurredText = member.value.text
			haveOccurredAt = true
		case "usage":
			if member.value.kind != jsonObject {
				return nil, invalidItem("usage must be a JSON object")
			}
			parsed, rejection := decodeUsage(member.value)
			if rejection != nil {
				return nil, rejection
			}
			decoded.Usage = parsed
			haveUsage = true
		case "status":
			switch member.value.kind {
			case jsonNull:
				decoded.Status = nil
			case jsonString:
				if !validText(member.value.text, 1, 64) {
					return nil, invalidItem("status must be 1-64 UTF-8 bytes without control characters")
				}
				status := member.value.text
				decoded.Status = &status
			default:
				return nil, invalidItem("status must be a string or null")
			}
		case "duration_ms":
			switch member.value.kind {
			case jsonNull:
				decoded.DurationMS = nil
			case jsonNumber:
				value, ok := parseInteger(member.value.number)
				if !ok {
					return nil, invalidItem("duration_ms must be a decimal integer within the safe range")
				}
				duration := value
				decoded.DurationMS = &duration
			default:
				return nil, invalidItem("duration_ms must be a decimal integer or null")
			}
		default:
			// An unknown event field rejects only this position.
			return nil, invalidItem("event contains an unknown field")
		}
	}
	if !haveSourceID || !haveProvider || !haveModel || !haveOccurredAt || !haveUsage {
		return nil, invalidItem("event is missing a required field")
	}
	occurredAt, ok := parseOccurredAt(occurredText)
	if !ok {
		return nil, invalidItem("occurred_at must be RFC 3339 with an offset and at most 6 fractional digits")
	}
	decoded.OccurredAt = occurredAt
	decoded.PayloadHash = sha256.Sum256(decoded.normalizedText())
	return &decoded, nil
}

// decodeUsage decodes the six optional metrics, then applies the five cross-field
// normalization rules.
func decodeUsage(node *jsonNode) (usage, *rejection) {
	parsed := map[string]aggregation.Metric{}
	for _, member := range node.object {
		switch member.key {
		case "input_tokens", "output_tokens", "total_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens":
			metric, rejection := decodeMetric(member.value)
			if rejection != nil {
				return usage{}, rejection
			}
			parsed[member.key] = metric
		default:
			return usage{}, invalidItem("usage contains an unknown field")
		}
	}
	// An omitted metric and an explicit missing value are the same thing here.
	pick := func(name string) aggregation.Metric {
		if metric, ok := parsed[name]; ok {
			return metric
		}
		return missingMetric()
	}
	return normalizeUsage(usage{
		Input:      pick("input_tokens"),
		Output:     pick("output_tokens"),
		Total:      pick("total_tokens"),
		CacheRead:  pick("cache_read_tokens"),
		CacheWrite: pick("cache_write_tokens"),
		Reasoning:  pick("reasoning_tokens"),
	})
}

// decodeMetric decodes one metric object, which must be exactly one of the three
// documented shapes.
func decodeMetric(node *jsonNode) (aggregation.Metric, *rejection) {
	if node.kind != jsonObject {
		return aggregation.Metric{}, invalidItem("metric must be a JSON object")
	}
	var (
		haveValue   bool
		haveQuality bool
		value       *int64
		quality     aggregation.Quality
	)
	for _, member := range node.object {
		switch member.key {
		case "value":
			switch member.value.kind {
			case jsonNull:
				haveValue = true
			case jsonNumber:
				parsedValue, rejection := parseMetricValue(member.value.number)
				if rejection != nil {
					return aggregation.Metric{}, rejection
				}
				value = &parsedValue
				haveValue = true
			default:
				return aggregation.Metric{}, invalidItem("metric value must be a decimal integer or null")
			}
		case "quality":
			if member.value.kind != jsonString {
				return aggregation.Metric{}, invalidItem("metric quality must be a string")
			}
			switch aggregation.Quality(member.value.text) {
			case aggregation.QualityKnown, aggregation.QualityEstimated, aggregation.QualityMissing:
				quality = aggregation.Quality(member.value.text)
				haveQuality = true
			default:
				return aggregation.Metric{}, invalidItem("metric quality must be known, estimated or missing")
			}
		default:
			return aggregation.Metric{}, invalidItem("metric contains an unknown field")
		}
	}
	if !haveValue || !haveQuality {
		// Both keys are mandatory: a metric is only one of the three shapes.
		return aggregation.Metric{}, invalidItem("metric must carry value and quality")
	}
	switch quality {
	case aggregation.QualityMissing:
		if value != nil {
			return aggregation.Metric{}, invalidItem("a missing metric must not carry a value")
		}
		return aggregation.Metric{Quality: aggregation.QualityMissing}, nil
	default:
		if value == nil {
			return aggregation.Metric{}, invalidItem("a known or estimated metric must carry a value")
		}
		return aggregation.Metric{Value: value, Quality: quality}, nil
	}
}

// parseMetricValue enforces the decimal integer lexeme and the safe integer range.
// A value above 2^53-1 is a permanent USAGE_OVERFLOW, never truncated.
func parseMetricValue(number json.Number) (int64, *rejection) {
	if !decimalIntegerLexeme(number.String()) {
		return 0, invalidItem("metric value must be written as a decimal integer")
	}
	value, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil || value > maxSafeInteger {
		return 0, rejectedItem(codeUsageOverflow, "metric value exceeds the safe integer range")
	}
	return value, nil
}

// parseInteger enforces the decimal integer lexeme for a non-metric field and
// rejects values outside the safe range as an invalid argument.
func parseInteger(number json.Number) (int64, bool) {
	if !decimalIntegerLexeme(number.String()) {
		return 0, false
	}
	value, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil || value > maxSafeInteger {
		return 0, false
	}
	return value, true
}

// decimalIntegerLexeme reports whether a JSON number is written as a plain
// non-negative decimal integer: no fraction, no exponent, no plus sign, no leading
// zero and no negative zero.
func decimalIntegerLexeme(text string) bool {
	return decimalIntegerPattern.MatchString(text)
}

var (
	// decimalIntegerPattern is the accepted number lexeme. The empty alternative is
	// not used; 0 is the only value starting with a zero.
	decimalIntegerPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	// occurredAtPattern is the accepted time shape. It is written in RE2 syntax
	// because Go's regexp has no \u escapes.
	occurredAtPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt][0-9]{2}:[0-9]{2}:[0-5][0-9](\.[0-9]{1,6})?(?:[Zz]|[+-][0-9]{2}:[0-9]{2})$`)
	// projectIDPattern is a hyphenated UUID, either case.
	projectIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// occurredAtLayouts are the two equivalent spellings time.Parse accepts after the
// lower case t and z markers are raised to upper case.
const (
	occurredAtLayout = "2006-01-02T15:04:05.999999Z07:00"
	occurredAtSecond = "2006-01-02T15:04:05Z07:00"
)

// parseOccurredAt parses the frozen time shape and returns it at UTC microsecond
// precision. Sub-microsecond digits, leap seconds, missing offsets, non-calendar
// dates and out-of-range offsets are all rejected without rounding or truncation.
func parseOccurredAt(text string) (time.Time, bool) {
	if !occurredAtPattern.MatchString(text) {
		return time.Time{}, false
	}
	// The pattern admits only digits and the T/t/Z/z/./:+/- characters, so upper
	// casing changes only the two case-insensitive markers RFC 3339 allows.
	normalized := strings.ToUpper(text)
	// time.Parse accepts an offset beyond the RFC 3339 range (+24:00), so the hour
	// and minute fields of an explicit offset are checked here.
	if offset := normalized[len(normalized)-6:]; normalized[len(normalized)-1] != 'Z' {
		hours, _ := strconv.Atoi(offset[1:3])
		minutes, _ := strconv.Atoi(offset[4:6])
		if hours > 23 || minutes > 59 {
			return time.Time{}, false
		}
	}
	layout := occurredAtSecond
	if strings.Contains(normalized, ".") {
		layout = occurredAtLayout
	}
	parsed, err := time.Parse(layout, normalized)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

// parseProjectID parses a hyphenated UUID into its identity form.
func parseProjectID(text string) (credentials.ID, bool) {
	if !projectIDPattern.MatchString(text) {
		return credentials.ID{}, false
	}
	compact := strings.ReplaceAll(text, "-", "")
	raw, err := hex.DecodeString(compact)
	if err != nil || len(raw) != 16 {
		return credentials.ID{}, false
	}
	var id credentials.ID
	copy(id.Bytes[:], raw)
	id.Valid = true
	return id, true
}

// controlRune reports the characters every text field refuses: NUL, the C0 range,
// DEL and the C1 range.
func controlRune(character rune) bool {
	return character <= 0x1f || (character >= 0x7f && character <= 0x9f)
}

// validText enforces the byte length range and the rejected character set of an
// opaque text field. Lengths are UTF-8 bytes, not code points.
func validText(text string, minimum, maximum int) bool {
	if len(text) < minimum || len(text) > maximum {
		return false
	}
	for _, character := range text {
		if controlRune(character) {
			return false
		}
	}
	return true
}

// surroundingSpace is the Unicode White_Space set used for the first and last
// character. Go's \s and [[:space:]] miss U+00A0 and U+3000, so the set is spelled
// out. The C0 members are already refused by the control character rule.
func surroundingSpace(character rune) bool {
	switch character {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x85, 0xa0, 0x1680,
		0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200a,
		0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	default:
		return false
	}
}

// validLabel enforces the provider and model rules: 1-128 UTF-8 bytes, no control
// characters and no surrounding White_Space. Case and inner bytes are preserved.
func validLabel(text string) bool {
	if !validText(text, 1, 128) {
		return false
	}
	runes := []rune(text)
	return !surroundingSpace(runes[0]) && !surroundingSpace(runes[len(runes)-1])
}

// missingMetric is the explicit missing shape used for an omitted metric.
func missingMetric() aggregation.Metric {
	return aggregation.Metric{Quality: aggregation.QualityMissing}
}

// invalidItem builds a permanent INVALID_ARGUMENT rejection.
func invalidItem(message string) *rejection {
	return &rejection{code: codeInvalidArgument, message: message}
}

// rejectedItem builds a permanent rejection with an explicit code.
func rejectedItem(code, message string) *rejection {
	return &rejection{code: code, message: message}
}

// normalizeUsage applies the five cross-field metric rules that no JSON schema can
// express: subset sizes, total derivation, the known total equation, overflow and
// the all-missing case.
func normalizeUsage(u usage) (usage, *rejection) {
	if u.Input.Value != nil && u.CacheRead.Value != nil && knownQuality(u.Input) && knownQuality(u.CacheRead) && *u.CacheRead.Value > *u.Input.Value {
		return usage{}, rejectedItem(codeUsageUnsupported, "known cache_read_tokens exceeds known input_tokens")
	}
	if u.Input.Value != nil && u.CacheWrite.Value != nil && knownQuality(u.Input) && knownQuality(u.CacheWrite) && *u.CacheWrite.Value > *u.Input.Value {
		return usage{}, rejectedItem(codeUsageUnsupported, "known cache_write_tokens exceeds known input_tokens")
	}
	if u.Output.Value != nil && u.Reasoning.Value != nil && knownQuality(u.Output) && knownQuality(u.Reasoning) && *u.Reasoning.Value > *u.Output.Value {
		return usage{}, rejectedItem(codeUsageUnsupported, "known reasoning_tokens exceeds known output_tokens")
	}

	// total is "not provided" when it is omitted, null or explicitly missing; all
	// three spellings take the same derivation path.
	if u.Total.Value == nil {
		if u.Input.Value != nil && u.Output.Value != nil {
			sum := *u.Input.Value + *u.Output.Value
			if *u.Input.Value > maxSafeInteger-*u.Output.Value || sum > maxSafeInteger {
				return usage{}, rejectedItem(codeUsageOverflow, "derived total_tokens exceeds the safe integer range")
			}
			derived := sum
			quality := aggregation.QualityEstimated
			if knownQuality(u.Input) && knownQuality(u.Output) {
				quality = aggregation.QualityKnown
			}
			u.Total = aggregation.Metric{Value: &derived, Quality: quality}
		}
		return u, nil
	}

	// A provided total is kept as is; only three known values must add up.
	if u.Input.Value != nil && u.Output.Value != nil && knownQuality(u.Input) && knownQuality(u.Output) && knownQuality(u.Total) {
		if *u.Total.Value != *u.Input.Value+*u.Output.Value {
			return usage{}, rejectedItem(codeUsageUnsupported, "known total_tokens does not equal known input_tokens plus output_tokens")
		}
	}
	return u, nil
}

// knownQuality reports an exact known value.
func knownQuality(metric aggregation.Metric) bool {
	return metric.Quality == aggregation.QualityKnown
}
