// Package usage answers the read-only aggregate queries over the per-minute
// usage statistics: totals with a trend, and grouped rankings. It validates and
// normalises every parameter, computes the one set of time bucket boundaries the
// SQL then consumes, and maps stored decimal text onto the response shapes. It
// never imports the HTTP layer and never reads usage events.
package usage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dundunHa/TokenPivot/internal/aggregation"
	"github.com/jackc/pgx/v5/pgtype"
)

// Confirmed first-version limits (T-055 plan section 1).
const (
	// MaxPoints is the largest number of trend buckets one request may return.
	MaxPoints = 1000
	// DefaultLimit and MaxLimit bound one ranking page.
	DefaultLimit = 10
	MaxLimit     = 50
	// MaxFilterValues is the largest number of distinct values per filter dimension.
	MaxFilterValues = 50

	// maxTimezoneBytes bounds the IANA name before it reaches the tz loader.
	maxTimezoneBytes = 64
	// maxProbeSteps stops a boundary search that would otherwise never end.
	maxProbeSteps = 5000
	cursorVersion = 1
)

// Granularity is the trend bucket size.
type Granularity string

const (
	Minute Granularity = "minute"
	Hour   Granularity = "hour"
	Day    Granularity = "day"
)

// GroupBy is the ranking dimension. The value selects a fixed SQL expression;
// request text is never spliced into SQL.
type GroupBy string

const (
	GroupProject  GroupBy = "project"
	GroupProvider GroupBy = "provider"
	GroupModel    GroupBy = "model"
)

// ErrInvalidArgument reports a rejected parameter, range or cursor. It carries no
// request value.
var ErrInvalidArgument = errors.New("usage: invalid argument")

// OutsideRetentionError reports a start before the current retention cutoff.
type OutsideRetentionError struct{ Cutoff time.Time }

func (e *OutsideRetentionError) Error() string { return "usage: start is before the retention cutoff" }

// TooManyPointsError reports a trend with more than MaxPoints buckets.
// Suggested is the finest coarser granularity that fits, or nil when only a
// shorter range helps.
type TooManyPointsError struct {
	MaxPoints int
	Suggested *Granularity
}

func (e *TooManyPointsError) Error() string { return "usage: time range has too many points" }

// Metric order shared by RawStats and the SQL column order.
const metricCount = 6

// RawMetric is one metric group as PostgreSQL printed it: canonical decimal text.
type RawMetric struct{ Sum, Known, Estimated, Missing string }

// RawStats is the unconverted aggregate of one bucket or group. Metrics are in
// the order input, output, total, cache read, cache write, reasoning.
type RawStats struct {
	RequestCount string
	Metrics      [metricCount]RawMetric
}

// Filter is the normalised multi-select. A nil or empty slice means "no filter"
// for that dimension; Unassigned adds project_id IS NULL to the project filter.
type Filter struct {
	Providers  []string
	Models     []string
	Projects   []pgtype.UUID
	Unassigned bool
}

// Bucket is one precomputed trend bucket. Start and End are UTC instants and End
// is the true next boundary.
type Bucket struct {
	Start   time.Time
	End     time.Time
	Partial bool
}

// AggregateParams is the store input of the trend statement. Buckets are the only
// bucketing definition: the store never derives its own.
type AggregateParams struct {
	WorkspaceID pgtype.UUID
	Start, End  time.Time
	Filter      Filter
	Buckets     []Bucket
}

// RawPoint is one non-empty bucket; Index addresses AggregateParams.Buckets.
type RawPoint struct {
	Index int
	Stats RawStats
}

// AggregateRows is the trend result. Overall is nil when no row matched.
type AggregateRows struct {
	Overall *RawStats
	Points  []RawPoint
}

// RankingsParams is the store input of the ranking statement.
type RankingsParams struct {
	WorkspaceID pgtype.UUID
	Start, End  time.Time
	Filter      Filter
	GroupBy     GroupBy
	Offset      int
	Limit       int
}

// RawGroup is one ranked group; Key2 is only set for model groups (the model
// name, with Key1 the provider).
type RawGroup struct {
	Key1, Key2 *string
	Stats      RawStats
}

// RankingsRows is the ranking result: the overall aggregate of every group, the
// requested page, and the aggregate of all groups after the page (nil if none).
type RankingsRows struct {
	Overall RawStats
	Groups  []RawGroup
	Other   *RawStats
}

// Store is the parameterised read surface over usage_minute_stats.
type Store interface {
	Aggregate(ctx context.Context, params AggregateParams) (AggregateRows, error)
	Rankings(ctx context.Context, params RankingsParams) (RankingsRows, error)
}

// MetricStats is one token metric. Value is nil when nothing known or estimated
// exists; every number is a canonical decimal string.
type MetricStats struct {
	Value          *string `json:"value"`
	KnownCount     string  `json:"known_count"`
	EstimatedCount string  `json:"estimated_count"`
	MissingCount   string  `json:"missing_count"`
}

// Stats is the response aggregate of one bucket, group or total.
type Stats struct {
	RequestCount     string      `json:"request_count"`
	InputTokens      MetricStats `json:"input_tokens"`
	OutputTokens     MetricStats `json:"output_tokens"`
	TotalTokens      MetricStats `json:"total_tokens"`
	CacheReadTokens  MetricStats `json:"cache_read_tokens"`
	CacheWriteTokens MetricStats `json:"cache_write_tokens"`
	ReasoningTokens  MetricStats `json:"reasoning_tokens"`
}

// AggregateRange echoes the effective query range.
type AggregateRange struct {
	Start           string      `json:"start"`
	End             string      `json:"end"`
	AsOf            string      `json:"as_of"`
	RetentionCutoff string      `json:"retention_cutoff"`
	Timezone        string      `json:"timezone"`
	Granularity     Granularity `json:"granularity"`
}

// Point is one non-empty trend bucket.
type Point struct {
	BucketStart string `json:"bucket_start"`
	BucketEnd   string `json:"bucket_end"`
	Partial     bool   `json:"partial"`
	Stats       Stats  `json:"stats"`
}

// AggregateResponse is the body of the aggregate endpoint.
type AggregateResponse struct {
	Range  AggregateRange `json:"range"`
	Totals Stats          `json:"totals"`
	Points []Point        `json:"points"`
}

// RankingsRange echoes the effective query range of a ranking.
type RankingsRange struct {
	Start           string `json:"start"`
	End             string `json:"end"`
	AsOf            string `json:"as_of"`
	RetentionCutoff string `json:"retention_cutoff"`
}

// Group is one ranked group. Key holds project_id, provider, or provider+model.
type Group struct {
	Key   map[string]*string `json:"key"`
	Share *float64           `json:"share"`
	Stats Stats              `json:"stats"`
}

// Other aggregates every group after the current page.
type Other struct {
	Share *float64 `json:"share"`
	Stats Stats    `json:"stats"`
}

// RankingsResponse is the body of the rankings endpoint.
type RankingsResponse struct {
	Range      RankingsRange `json:"range"`
	GroupBy    GroupBy       `json:"group_by"`
	Overall    Stats         `json:"overall"`
	Groups     []Group       `json:"groups"`
	Other      *Other        `json:"other"`
	NextCursor *string       `json:"next_cursor"`
}

// Service validates queries and shapes results. It holds no request state.
type Service struct {
	store     Store
	now       func() time.Time
	maxPoints int
}

// New returns a Service on the given store.
func New(store Store) *Service {
	return &Service{store: store, now: time.Now, maxPoints: MaxPoints}
}

var (
	aggregateKeys = map[string]bool{"start": true, "end": true, "timezone": true, "granularity": true, "provider": true, "model": true, "project": true}
	rankingsKeys  = map[string]bool{"start": true, "end": true, "group_by": true, "limit": true, "cursor": true, "provider": true, "model": true, "project": true}
)

// query is the normalised request shared by both endpoints.
type query struct {
	start    time.Time
	end      time.Time // zero when omitted
	endGiven bool
	filter   Filter
}

// Aggregate returns the filtered totals and trend. The caller has already proved
// workspace read access; every statement is limited to workspaceID.
func (s *Service) Aggregate(ctx context.Context, workspaceID pgtype.UUID, values url.Values) (*AggregateResponse, error) {
	if err := validateValues(values, aggregateKeys); err != nil {
		return nil, err
	}
	now := s.sample()
	q, err := parseQuery(values)
	if err != nil {
		return nil, err
	}
	tz, err := single(values, "timezone")
	if err != nil || tz == "" {
		return nil, ErrInvalidArgument
	}
	loc, err := loadLocation(tz)
	if err != nil {
		return nil, err
	}
	granularity, err := parseGranularity(values)
	if err != nil {
		return nil, err
	}
	end := q.end
	if !q.endGiven {
		end = now
	}
	cutoff, err := checkRange(q, end, now)
	if err != nil {
		return nil, err
	}
	buckets, tooMany, err := enumerateBuckets(q.start, end, granularity, loc, s.maxPoints, nil)
	if err != nil {
		return nil, fmt.Errorf("usage buckets: %w", err)
	}
	if tooMany {
		suggested, err := s.suggest(q.start, end, granularity, loc)
		if err != nil {
			return nil, fmt.Errorf("usage suggestion: %w", err)
		}
		return nil, &TooManyPointsError{MaxPoints: s.maxPoints, Suggested: suggested}
	}
	rows, err := s.store.Aggregate(ctx, AggregateParams{WorkspaceID: workspaceID, Start: q.start, End: end, Filter: q.filter, Buckets: buckets})
	if err != nil {
		return nil, err
	}
	response := &AggregateResponse{
		Range: AggregateRange{
			Start: formatTime(q.start), End: formatTime(end), AsOf: formatTime(effectiveAsOf(q, end, now)),
			RetentionCutoff: formatTime(cutoff), Timezone: tz, Granularity: granularity,
		},
		Points: make([]Point, 0, len(rows.Points)),
	}
	if rows.Overall == nil {
		response.Totals = emptyStats()
	} else if response.Totals, err = convertStats(*rows.Overall); err != nil {
		return nil, err
	}
	for _, point := range rows.Points {
		if point.Index < 0 || point.Index >= len(buckets) {
			return nil, errors.New("usage: bucket index out of range")
		}
		stats, err := convertStats(point.Stats)
		if err != nil {
			return nil, err
		}
		bucket := buckets[point.Index]
		response.Points = append(response.Points, Point{BucketStart: formatTime(bucket.Start), BucketEnd: formatTime(bucket.End), Partial: bucket.Partial, Stats: stats})
	}
	sort.SliceStable(response.Points, func(i, j int) bool { return response.Points[i].BucketStart < response.Points[j].BucketStart })
	return response, nil
}

// effectiveAsOf is the sampled statistics cut-off: end itself when it was omitted,
// otherwise the sampling instant.
func effectiveAsOf(q query, end, now time.Time) time.Time {
	if !q.endGiven {
		return end
	}
	return now
}

// Rankings returns one page of grouped totals plus the overall aggregate and the
// aggregate of every group after the page.
func (s *Service) Rankings(ctx context.Context, workspaceID pgtype.UUID, values url.Values) (*RankingsResponse, error) {
	if err := validateValues(values, rankingsKeys); err != nil {
		return nil, err
	}
	now := s.sample()
	q, err := parseQuery(values)
	if err != nil {
		return nil, err
	}
	groupBy, err := parseGroupBy(values)
	if err != nil {
		return nil, err
	}
	limit, err := parseLimit(values)
	if err != nil {
		return nil, err
	}
	end, asOf, offset := q.end, now, 0
	if !q.endGiven {
		end = now
	}
	raw, err := single(values, "cursor")
	if err != nil {
		return nil, err
	}
	if _, present := values["cursor"]; present {
		if raw == "" {
			return nil, ErrInvalidArgument
		}
		cursor, err := decodeCursor(raw)
		if err != nil {
			return nil, err
		}
		if cursor.Digest != queryDigest(workspaceID, q, groupBy, limit) || cursor.Offset <= 0 || cursor.Offset%limit != 0 {
			return nil, ErrInvalidArgument
		}
		asOf = time.UnixMicro(cursor.AsOf).UTC()
		if !q.endGiven {
			end = asOf
		}
		// The original statistics cut-off must itself be a valid, past instant.
		if asOf.After(now) || (q.endGiven && asOf.Before(q.end)) {
			return nil, ErrInvalidArgument
		}
		offset = cursor.Offset
	}
	cutoff, err := checkRange(q, end, now)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.Rankings(ctx, RankingsParams{WorkspaceID: workspaceID, Start: q.start, End: end, Filter: q.filter, GroupBy: groupBy, Offset: offset, Limit: limit})
	if err != nil {
		return nil, err
	}
	overall, err := convertStats(rows.Overall)
	if err != nil {
		return nil, err
	}
	response := &RankingsResponse{
		Range:   RankingsRange{Start: formatTime(q.start), End: formatTime(end), AsOf: formatTime(asOf), RetentionCutoff: formatTime(cutoff)},
		GroupBy: groupBy,
		Overall: overall,
		Groups:  make([]Group, 0, len(rows.Groups)),
	}
	for _, group := range rows.Groups {
		stats, err := convertStats(group.Stats)
		if err != nil {
			return nil, err
		}
		response.Groups = append(response.Groups, Group{Key: groupKey(groupBy, group), Share: share(stats.TotalTokens.Value, overall.TotalTokens.Value), Stats: stats})
	}
	if rows.Other != nil {
		stats, err := convertStats(*rows.Other)
		if err != nil {
			return nil, err
		}
		response.Other = &Other{Share: share(stats.TotalTokens.Value, overall.TotalTokens.Value), Stats: stats}
		next, err := encodeCursor(cursorPayload{Version: cursorVersion, Digest: queryDigest(workspaceID, q, groupBy, limit), AsOf: asOf.UnixMicro(), Offset: offset + limit})
		if err != nil {
			return nil, err
		}
		response.NextCursor = &next
	}
	return response, nil
}

// sample reads the clock once per request, at the PostgreSQL timestamp resolution.
func (s *Service) sample() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// checkRange applies the shared range rules and returns the retention cutoff of
// the sampled instant. It runs before any statement.
func checkRange(q query, end, now time.Time) (time.Time, error) {
	if !q.start.Before(end) || end.After(now) {
		return time.Time{}, ErrInvalidArgument
	}
	cutoff := aggregation.RetentionCutoff(now)
	if q.start.Before(cutoff) {
		return time.Time{}, &OutsideRetentionError{Cutoff: cutoff}
	}
	return cutoff, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// validateValues rejects unknown names and any name or value that is not valid
// UTF-8 or contains NUL, which PostgreSQL text cannot hold.
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

// single returns the only value of a parameter, or "" when absent. A repeated
// parameter is an error.
func single(values url.Values, name string) (string, error) {
	list := values[name]
	switch len(list) {
	case 0:
		return "", nil
	case 1:
		return list[0], nil
	default:
		return "", ErrInvalidArgument
	}
}

func parseQuery(values url.Values) (query, error) {
	var q query
	startText, err := single(values, "start")
	if err != nil || startText == "" {
		return q, ErrInvalidArgument
	}
	if q.start, err = parseMinute(startText); err != nil {
		return q, err
	}
	if _, present := values["end"]; present {
		endText, err := single(values, "end")
		if err != nil || endText == "" {
			return q, ErrInvalidArgument
		}
		if q.end, err = parseMinute(endText); err != nil {
			return q, err
		}
		q.endGiven = true
	}
	q.filter, err = parseFilter(values)
	return q, err
}

// parseMinute parses an RFC 3339 instant with an offset and requires whole
// minutes; it never rounds.
func parseMinute(value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, ErrInvalidArgument
	}
	t = t.UTC()
	if t.Second() != 0 || t.Nanosecond() != 0 {
		return time.Time{}, ErrInvalidArgument
	}
	return t, nil
}

func parseGranularity(values url.Values) (Granularity, error) {
	text, err := single(values, "granularity")
	if err != nil {
		return "", err
	}
	switch g := Granularity(text); g {
	case Minute, Hour, Day:
		return g, nil
	}
	return "", ErrInvalidArgument
}

func parseGroupBy(values url.Values) (GroupBy, error) {
	text, err := single(values, "group_by")
	if err != nil {
		return "", err
	}
	switch g := GroupBy(text); g {
	case GroupProject, GroupProvider, GroupModel:
		return g, nil
	}
	return "", ErrInvalidArgument
}

func parseLimit(values url.Values) (int, error) {
	if _, present := values["limit"]; !present {
		return DefaultLimit, nil
	}
	text, err := single(values, "limit")
	if err != nil || text == "" || len(text) > 3 {
		return 0, ErrInvalidArgument
	}
	for _, c := range text {
		if c < '0' || c > '9' {
			return 0, ErrInvalidArgument
		}
	}
	limit, err := strconv.Atoi(text)
	if err != nil || limit < 1 || limit > MaxLimit {
		return 0, ErrInvalidArgument
	}
	return limit, nil
}

// loadLocation accepts IANA names only. Local and the empty name would read the
// server zone, and fixed offsets are not tz names.
func loadLocation(name string) (*time.Location, error) {
	if name == "" || name == "Local" || len(name) > maxTimezoneBytes {
		return nil, ErrInvalidArgument
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, ErrInvalidArgument
	}
	return loc, nil
}

// parseFilter collects the repeated filters: empty values are rejected, values
// are de-duplicated, and each dimension keeps at most MaxFilterValues.
func parseFilter(values url.Values) (Filter, error) {
	var f Filter
	var err error
	if f.Providers, err = distinctText(values["provider"]); err != nil {
		return f, err
	}
	if f.Models, err = distinctText(values["model"]); err != nil {
		return f, err
	}
	projects, err := distinctText(values["project"])
	if err != nil {
		return f, err
	}
	for _, p := range projects {
		if p == "unassigned" {
			f.Unassigned = true
			continue
		}
		id, ok := parseUUID(p)
		if !ok {
			return f, ErrInvalidArgument
		}
		f.Projects = append(f.Projects, id)
	}
	sort.Slice(f.Projects, func(i, j int) bool { return f.Projects[i].String() < f.Projects[j].String() })
	return f, nil
}

func distinctText(list []string) ([]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(list))
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v == "" {
			return nil, ErrInvalidArgument
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
			if len(out) > MaxFilterValues {
				return nil, ErrInvalidArgument
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// parseUUID accepts only the canonical 8-4-4-4-12 hexadecimal form.
func parseUUID(value string) (pgtype.UUID, bool) {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return pgtype.UUID{}, false
	}
	raw, err := hex.DecodeString(value[0:8] + value[9:13] + value[14:18] + value[19:23] + value[24:])
	if err != nil || len(raw) != 16 {
		return pgtype.UUID{}, false
	}
	id := pgtype.UUID{Valid: true}
	copy(id.Bytes[:], raw)
	return id, true
}

// ---- time buckets -------------------------------------------------------

// bucketKey identifies the bucket an instant belongs to. Hour includes the UTC
// offset so a repeated local hour is two buckets; day is the local date alone.
type bucketKey struct{ year, month, day, hour, offset int }

func keyOf(t time.Time, g Granularity, loc *time.Location) bucketKey {
	local := t.In(loc)
	year, month, day := local.Date()
	if g == Day {
		return bucketKey{year: year, month: int(month), day: day}
	}
	_, offset := local.Zone()
	return bucketKey{year: year, month: int(month), day: day, hour: local.Hour(), offset: offset}
}

// edge walks from t, which lies in bucket k, in direction dir (-1 or +1) over UTC
// minutes. Backwards it returns the first minute of the bucket; forwards it returns
// the first minute after it, the true next boundary. Day buckets are walked by the
// hour first and refined by the minute, so the cost stays bounded. probes counts
// the keys evaluated.
func edge(t time.Time, k bucketKey, dir int, g Granularity, loc *time.Location, probes *int) (time.Time, error) {
	steps := []time.Duration{time.Minute}
	if g == Day {
		steps = []time.Duration{time.Hour, time.Minute}
	}
	for _, step := range steps {
		for n := 0; ; n++ {
			if n > maxProbeSteps {
				return time.Time{}, errors.New("usage: bucket boundary not found")
			}
			next := t.Add(time.Duration(dir) * step)
			if probes != nil {
				*probes++
			}
			if keyOf(next, g, loc) != k {
				break
			}
			t = next
		}
	}
	if dir > 0 {
		return t.Add(time.Minute), nil
	}
	return t, nil
}

// enumerateBuckets lists the buckets intersecting [start,end). It stops as soon as
// it would produce more than max buckets and reports tooMany, without walking the
// rest of the range. A bucket is partial when [start,end) cuts it.
func enumerateBuckets(start, end time.Time, g Granularity, loc *time.Location, max int, probes *int) ([]Bucket, bool, error) {
	out := make([]Bucket, 0, 16)
	if g == Minute {
		for t := start; t.Before(end); t = t.Add(time.Minute) {
			if len(out) == max {
				return nil, true, nil
			}
			next := t.Add(time.Minute)
			out = append(out, Bucket{Start: t, End: next, Partial: next.After(end)})
		}
		return out, false, nil
	}
	cur, err := edge(start, keyOf(start, g, loc), -1, g, loc, probes)
	if err != nil {
		return nil, false, err
	}
	for cur.Before(end) {
		if len(out) == max {
			return nil, true, nil
		}
		next, err := edge(cur, keyOf(cur, g, loc), 1, g, loc, probes)
		if err != nil {
			return nil, false, err
		}
		out = append(out, Bucket{Start: cur, End: next, Partial: cur.Before(start) || next.After(end)})
		cur = next
	}
	return out, false, nil
}

// suggest returns the finest granularity coarser than g (hour, then day) whose
// bucket count fits, or nil when none does.
func (s *Service) suggest(start, end time.Time, g Granularity, loc *time.Location) (*Granularity, error) {
	for _, candidate := range []Granularity{Hour, Day} {
		if rank(candidate) <= rank(g) {
			continue
		}
		_, tooMany, err := enumerateBuckets(start, end, candidate, loc, s.maxPoints, nil)
		if err != nil {
			return nil, err
		}
		if !tooMany {
			c := candidate
			return &c, nil
		}
	}
	return nil, nil
}

func rank(g Granularity) int {
	switch g {
	case Minute:
		return 0
	case Hour:
		return 1
	}
	return 2
}

// ---- cursor -------------------------------------------------------------

// cursorPayload is the opaque cursor. It locates a page and carries the original
// statistics cut-off; it is not an authorisation credential.
type cursorPayload struct {
	Version int    `json:"v"`
	Digest  string `json:"q"`
	AsOf    int64  `json:"a"`
	Offset  int    `json:"o"`
}

func encodeCursor(p cursorPayload) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("usage cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeCursor(text string) (cursorPayload, error) {
	var p cursorPayload
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil {
		return p, ErrInvalidArgument
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil || dec.More() || p.Version != cursorVersion || len(p.Digest) != sha256.Size*2 || p.Offset < 0 || p.Offset > 1<<30 {
		return cursorPayload{}, ErrInvalidArgument
	}
	return p, nil
}

// queryDigest hashes the normalised ranking query, workspace included. Whether the
// end was supplied is part of it, so a cursor from an open-ended request cannot be
// replayed with an explicit end.
func queryDigest(workspaceID pgtype.UUID, q query, groupBy GroupBy, limit int) string {
	projects := make([]string, len(q.filter.Projects))
	for i, p := range q.filter.Projects {
		projects[i] = p.String()
	}
	var end *int64
	if q.endGiven {
		v := q.end.UnixMicro()
		end = &v
	}
	raw, _ := json.Marshal(struct {
		Workspace  string
		Start      int64
		End        *int64
		GroupBy    GroupBy
		Limit      int
		Providers  []string
		Models     []string
		Projects   []string
		Unassigned bool
	}{workspaceID.String(), q.start.UnixMicro(), end, groupBy, limit, q.filter.Providers, q.filter.Models, projects, q.filter.Unassigned})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ---- metrics ------------------------------------------------------------

func emptyStats() Stats {
	empty := MetricStats{KnownCount: "0", EstimatedCount: "0", MissingCount: "0"}
	return Stats{RequestCount: "0", InputTokens: empty, OutputTokens: empty, TotalTokens: empty, CacheReadTokens: empty, CacheWriteTokens: empty, ReasoningTokens: empty}
}

// convertStats validates the stored decimal text and maps it to the response. A
// metric with neither known nor estimated values has no value, even though its
// stored sum is 0.
func convertStats(raw RawStats) (Stats, error) {
	if !canonicalDecimal(raw.RequestCount) {
		return Stats{}, errors.New("usage: invalid stored count")
	}
	metrics := [metricCount]MetricStats{}
	for i, m := range raw.Metrics {
		for _, v := range []string{m.Sum, m.Known, m.Estimated, m.Missing} {
			if !canonicalDecimal(v) {
				return Stats{}, errors.New("usage: invalid stored count")
			}
		}
		metrics[i] = MetricStats{KnownCount: m.Known, EstimatedCount: m.Estimated, MissingCount: m.Missing}
		if m.Known != "0" || m.Estimated != "0" {
			sum := m.Sum
			metrics[i].Value = &sum
		}
	}
	return Stats{
		RequestCount: raw.RequestCount,
		InputTokens:  metrics[0], OutputTokens: metrics[1], TotalTokens: metrics[2],
		CacheReadTokens: metrics[3], CacheWriteTokens: metrics[4], ReasoningTokens: metrics[5],
	}, nil
}

// canonicalDecimal reports whether value is 0 or a digit string without leading
// zeros, sign, point or exponent.
func canonicalDecimal(value string) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

// share is value divided by total as a float, computed from the exact integers.
// It is nil when either side is unavailable or the total is zero.
func share(value, total *string) *float64 {
	if value == nil || total == nil {
		return nil
	}
	v, okV := new(big.Rat).SetString(*value)
	t, okT := new(big.Rat).SetString(*total)
	if !okV || !okT || t.Sign() == 0 {
		return nil
	}
	f, _ := new(big.Rat).Quo(v, t).Float64()
	return &f
}

func groupKey(groupBy GroupBy, g RawGroup) map[string]*string {
	switch groupBy {
	case GroupProject:
		return map[string]*string{"project_id": g.Key1}
	case GroupProvider:
		return map[string]*string{"provider": g.Key1}
	}
	return map[string]*string{"provider": g.Key1, "model": g.Key2}
}
