// Package requestdetails serves the read-only request detail list and the exact
// 60 second token rate from the raw usage_events rows.
package requestdetails

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
)

// Limits of the detail list and the fixed rate window.
const (
	DefaultLimit  = 50
	MaxLimit      = 100
	DefaultRange  = time.Hour
	RateWindow    = 60 * time.Second
	RetentionDays = 180
	cursorVersion = 1
	unattributed  = "unattributed"
)

// ErrInvalidArgument reports a rejected parameter, range or cursor. It carries
// no request value.
var ErrInvalidArgument = errors.New("requestdetails: invalid argument")

// Filter selects events. A dimension with no values is unfiltered; values of one
// dimension are OR-ed and dimensions are AND-ed.
type Filter struct {
	Providers    []string
	Models       []string
	Projects     []pgtype.UUID
	Unattributed bool
}

// EventsParams is one keyset page request. Cursor fields are valid only when
// HasCursor is set. Limit is the page size; the store returns up to Limit+1 rows.
type EventsParams struct {
	WorkspaceID pgtype.UUID
	Start, End  time.Time
	Filter      Filter
	HasCursor   bool
	CursorTime  time.Time
	CursorID    pgtype.UUID
	Limit       int
}

// RateParams selects the rate window. A nil AsOf makes the store use the
// database statement start time; only tests set it.
type RateParams struct {
	WorkspaceID pgtype.UUID
	AsOf        *time.Time
	Filter      Filter
}

// Token is one token field of a stored event.
type TokenRow struct {
	Value   *int64
	Quality string
}

// EventRow is one stored event joined with its project and source.
type EventRow struct {
	ID, SourceID          pgtype.UUID
	OccurredAt            time.Time
	ReceivedAt            time.Time
	Provider, Model       string
	ProjectID             pgtype.UUID
	ProjectKey            *string
	ProjectName           *string
	Platform, InstanceKey string
	Input, Output, Total  TokenRow
	Status                *string
	DurationMS            *int64
}

// RateRow is the window aggregate. Sum is an exact decimal string, nil when no
// known or estimated total exists.
type RateRow struct {
	AsOf                     time.Time
	Sum                      *string
	Events, Known, Estimated int64
	Missing                  int64
	LastReceived             *time.Time
}

// Store is the parameterised read surface over usage_events.
type Store interface {
	CountProjects(ctx context.Context, workspaceID pgtype.UUID, ids []pgtype.UUID) (int, error)
	Events(ctx context.Context, params EventsParams) ([]EventRow, error)
	Rate(ctx context.Context, params RateParams) (RateRow, error)
}

// Service validates queries and shapes results.
type Service struct {
	store Store
	now   func() time.Time
}

// New returns a Service on the given store.
func New(store Store) *Service { return &Service{store: store, now: time.Now} }

func (s *Service) sample() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// Token is the response form of one token field.
type Token struct {
	Value   *int64 `json:"value"`
	Quality string `json:"quality"`
}

// ProjectRef is the project of an event.
type ProjectRef struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

// SourceRef is the source of an event.
type SourceRef struct {
	ID          string `json:"id"`
	Platform    string `json:"platform"`
	InstanceKey string `json:"instance_key"`
}

// Event is one request detail.
type Event struct {
	ID           string      `json:"id"`
	OccurredAt   string      `json:"occurred_at"`
	ReceivedAt   string      `json:"received_at"`
	Provider     string      `json:"provider"`
	Model        string      `json:"model"`
	Project      *ProjectRef `json:"project"`
	Source       SourceRef   `json:"source"`
	InputTokens  Token       `json:"input_tokens"`
	OutputTokens Token       `json:"output_tokens"`
	TotalTokens  Token       `json:"total_tokens"`
	Status       *string     `json:"status"`
	DurationMS   *int64      `json:"duration_ms"`
}

// EventsResponse is the body of the detail list.
type EventsResponse struct {
	Items           []Event `json:"items"`
	NextCursor      *string `json:"next_cursor"`
	HasMore         bool    `json:"has_more"`
	Start           string  `json:"start"`
	End             string  `json:"end"`
	AsOf            string  `json:"as_of"`
	RetentionCutoff string  `json:"retention_cutoff"`
}

// RateTotal is the total token aggregate of the window.
type RateTotal struct {
	Value          *string `json:"value"`
	EventCount     int64   `json:"event_count"`
	KnownCount     int64   `json:"known_count"`
	EstimatedCount int64   `json:"estimated_count"`
	MissingCount   int64   `json:"missing_count"`
}

// RateResponse is the body of the rate endpoint.
type RateResponse struct {
	Start              string    `json:"start"`
	End                string    `json:"end"`
	AsOf               string    `json:"as_of"`
	WindowSeconds      int       `json:"window_seconds"`
	Unit               string    `json:"unit"`
	TotalTokens        RateTotal `json:"total_tokens"`
	WindowLastReceived *string   `json:"window_last_received_at"`
}

type cursorPayload struct {
	Version    int    `json:"v"`
	OccurredAt int64  `json:"t"`
	ID         string `json:"id"`
	Start      int64  `json:"s"`
	End        int64  `json:"e"`
}

var (
	eventsKeys = map[string]bool{"start": true, "end": true, "limit": true, "cursor": true, "provider": true, "model": true, "project": true}
	rateKeys   = map[string]bool{"provider": true, "model": true, "project": true}
)

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Events returns one keyset page, newest first.
func (s *Service) Events(ctx context.Context, workspaceID pgtype.UUID, values url.Values) (*EventsResponse, error) {
	if err := validateValues(values, eventsKeys); err != nil {
		return nil, err
	}
	now := s.sample()
	limit := DefaultLimit
	if raw, err := single(values, "limit"); err != nil {
		return nil, err
	} else if _, present := values["limit"]; present {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 1 || n > MaxLimit || strconv.Itoa(n) != raw {
			return nil, ErrInvalidArgument
		}
		limit = n
	}
	startRaw, err := single(values, "start")
	if err != nil {
		return nil, err
	}
	endRaw, err := single(values, "end")
	if err != nil {
		return nil, err
	}
	_, hasStart := values["start"]
	_, hasEnd := values["end"]
	if hasStart != hasEnd {
		return nil, ErrInvalidArgument
	}
	var start, end time.Time
	if hasStart {
		if start, err = parseTime(startRaw); err != nil {
			return nil, err
		}
		if end, err = parseTime(endRaw); err != nil {
			return nil, err
		}
		if !start.Before(end) {
			return nil, ErrInvalidArgument
		}
	}
	params := EventsParams{WorkspaceID: workspaceID, Limit: limit}
	cursorRaw, err := single(values, "cursor")
	if err != nil {
		return nil, err
	}
	if _, present := values["cursor"]; present {
		cursor, err := decodeCursor(cursorRaw)
		if err != nil {
			return nil, err
		}
		cs, ce := time.UnixMicro(cursor.Start).UTC(), time.UnixMicro(cursor.End).UTC()
		if hasStart && (!start.Equal(cs) || !end.Equal(ce)) {
			return nil, ErrInvalidArgument
		}
		start, end = cs, ce
		var id pgtype.UUID
		if id.Scan(cursor.ID) != nil || !start.Before(end) {
			return nil, ErrInvalidArgument
		}
		at := time.UnixMicro(cursor.OccurredAt).UTC()
		if at.Before(start) || !at.Before(end) {
			return nil, ErrInvalidArgument
		}
		params.HasCursor, params.CursorTime, params.CursorID = true, at, id
	} else if !hasStart {
		end, start = now, now.Add(-DefaultRange)
	}
	params.Start, params.End = start, end
	filter, err := s.parseFilter(ctx, workspaceID, values)
	if err != nil {
		return nil, err
	}
	params.Filter = filter
	rows, err := s.store.Events(ctx, params)
	if err != nil {
		return nil, err
	}
	response := &EventsResponse{
		Items: make([]Event, 0, len(rows)),
		Start: formatTime(start), End: formatTime(end), AsOf: formatTime(now),
		RetentionCutoff: formatTime(now.Add(-RetentionDays * 24 * time.Hour)),
	}
	if len(rows) > limit {
		rows, response.HasMore = rows[:limit], true
	}
	for _, row := range rows {
		response.Items = append(response.Items, newEvent(row))
	}
	if response.HasMore {
		last := rows[len(rows)-1]
		next, err := encodeCursor(cursorPayload{Version: cursorVersion, OccurredAt: last.OccurredAt.UnixMicro(), ID: last.ID.String(), Start: start.UnixMicro(), End: end.UnixMicro()})
		if err != nil {
			return nil, err
		}
		response.NextCursor = &next
	}
	return response, nil
}

// Rate returns the exact tokens/min of the rolling [as_of-60s, as_of) window.
func (s *Service) Rate(ctx context.Context, workspaceID pgtype.UUID, values url.Values) (*RateResponse, error) {
	return s.rateAt(ctx, workspaceID, values, nil)
}

// RateAt is Rate with a fixed as_of; the HTTP layer never exposes it.
func (s *Service) RateAt(ctx context.Context, workspaceID pgtype.UUID, values url.Values, asOf time.Time) (*RateResponse, error) {
	asOf = asOf.UTC().Truncate(time.Microsecond)
	return s.rateAt(ctx, workspaceID, values, &asOf)
}

func (s *Service) rateAt(ctx context.Context, workspaceID pgtype.UUID, values url.Values, asOf *time.Time) (*RateResponse, error) {
	if err := validateValues(values, rateKeys); err != nil {
		return nil, err
	}
	filter, err := s.parseFilter(ctx, workspaceID, values)
	if err != nil {
		return nil, err
	}
	row, err := s.store.Rate(ctx, RateParams{WorkspaceID: workspaceID, AsOf: asOf, Filter: filter})
	if err != nil {
		return nil, err
	}
	end := row.AsOf.UTC()
	response := &RateResponse{
		Start: formatTime(end.Add(-RateWindow)), End: formatTime(end), AsOf: formatTime(end),
		WindowSeconds: int(RateWindow / time.Second), Unit: "tokens/min",
		TotalTokens: RateTotal{Value: row.Sum, EventCount: row.Events, KnownCount: row.Known, EstimatedCount: row.Estimated, MissingCount: row.Missing},
	}
	if row.LastReceived != nil {
		text := formatTime(*row.LastReceived)
		response.WindowLastReceived = &text
	}
	return response, nil
}

func newEvent(row EventRow) Event {
	event := Event{
		ID: row.ID.String(), OccurredAt: formatTime(row.OccurredAt), ReceivedAt: formatTime(row.ReceivedAt),
		Provider: row.Provider, Model: row.Model,
		Source:      SourceRef{ID: row.SourceID.String(), Platform: row.Platform, InstanceKey: row.InstanceKey},
		InputTokens: Token(row.Input), OutputTokens: Token(row.Output), TotalTokens: Token(row.Total),
		Status: row.Status, DurationMS: row.DurationMS,
	}
	if row.ProjectID.Valid && row.ProjectKey != nil && row.ProjectName != nil {
		event.Project = &ProjectRef{ID: row.ProjectID.String(), Key: *row.ProjectKey, Name: *row.ProjectName}
	}
	return event
}

// parseFilter reads the repeated filter parameters and proves every project UUID
// belongs to the workspace before any event query runs.
func (s *Service) parseFilter(ctx context.Context, workspaceID pgtype.UUID, values url.Values) (Filter, error) {
	var filter Filter
	for _, name := range []string{"provider", "model"} {
		seen := map[string]bool{}
		var list []string
		for _, value := range values[name] {
			if value == "" {
				return Filter{}, ErrInvalidArgument
			}
			if !seen[value] {
				seen[value] = true
				list = append(list, value)
			}
		}
		if name == "provider" {
			filter.Providers = list
		} else {
			filter.Models = list
		}
	}
	seen := map[pgtype.UUID]bool{}
	for _, value := range values["project"] {
		if value == unattributed {
			filter.Unattributed = true
			continue
		}
		var id pgtype.UUID
		if err := id.Scan(value); err != nil {
			return Filter{}, ErrInvalidArgument
		}
		if !seen[id] {
			seen[id] = true
			filter.Projects = append(filter.Projects, id)
		}
	}
	if len(filter.Projects) > 0 {
		count, err := s.store.CountProjects(ctx, workspaceID, filter.Projects)
		if err != nil {
			return Filter{}, err
		}
		if count != len(filter.Projects) {
			return Filter{}, ErrInvalidArgument
		}
	}
	return filter, nil
}

func validateValues(values url.Values, allowed map[string]bool) error {
	for name, list := range values {
		if !allowed[name] || !cleanText(name) {
			return ErrInvalidArgument
		}
		for _, value := range list {
			if !cleanText(value) {
				return ErrInvalidArgument
			}
		}
	}
	return nil
}

func cleanText(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func single(values url.Values, name string) (string, error) {
	list := values[name]
	switch len(list) {
	case 0:
		return "", nil
	case 1:
		return list[0], nil
	}
	return "", ErrInvalidArgument
}

// parseTime accepts RFC 3339 with an offset and at most six fractional digits.
func parseTime(raw string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || t.Nanosecond()%1000 != 0 {
		return time.Time{}, ErrInvalidArgument
	}
	return t.UTC(), nil
}

func encodeCursor(payload cursorPayload) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeCursor(raw string) (cursorPayload, error) {
	var payload cursorPayload
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(data, &payload) != nil || payload.Version != cursorVersion {
		return cursorPayload{}, ErrInvalidArgument
	}
	return payload, nil
}
