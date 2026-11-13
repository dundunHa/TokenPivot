package ingestion

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"time"

	"github.com/dundunHa/TokenPivot/internal/aggregation"
	"github.com/dundunHa/TokenPivot/internal/credentials"
)

// NormalizationVersionV2 is the only persisted normalization version: the nine
// element array every stored event is compared under. A stored row carrying any
// other version was written by an older release and has to be converted by the
// one-off tools before it can be compared again.
const NormalizationVersionV2 = "tokenpivot-usage-event/v2"

// NormalizationInput is the minimal nine element content the persisted digest is
// computed over. It is the single input type shared by the request path (both the
// first digest and the re-hash after a project_key is resolved) and the offline
// PostgreSQL converter, so neither side can drift into a second algorithm. The six
// usage metrics keep their own value/quality pair; a nil value is the missing
// quality's value, never a real zero.
type NormalizationInput struct {
	// OccurredAt is rendered at UTC microsecond precision, never in a local zone.
	OccurredAt time.Time
	// ProjectID is invalid for an unattributed event and renders as null.
	ProjectID            credentials.ID
	Provider             string
	Model                string
	InputTokens          aggregation.Metric
	OutputTokens         aggregation.Metric
	TotalTokens          aggregation.Metric
	CacheReadTokens      aggregation.Metric
	CacheWriteTokens     aggregation.Metric
	ReasoningTokens      aggregation.Metric
	Status               *string
	DurationMS           *int64
	GenerationDurationMS *int64
	TTFTMS               *int64
}

// DigestV2 returns the SHA-256 digest of the nine element normalization text. It is
// the only digest implementation: the handler, the project_key re-hash and the
// offline converter all call it.
func DigestV2(input NormalizationInput) [sha256.Size]byte {
	return sha256.Sum256(normalizedTextV2(input))
}

// normalizationInput projects a decoded request onto the nine element content.
func (e *event) normalizationInput() NormalizationInput {
	return NormalizationInput{
		OccurredAt:           e.OccurredAt,
		ProjectID:            e.ProjectID,
		Provider:             e.Provider,
		Model:                e.Model,
		InputTokens:          e.Usage.Input,
		OutputTokens:         e.Usage.Output,
		TotalTokens:          e.Usage.Total,
		CacheReadTokens:      e.Usage.CacheRead,
		CacheWriteTokens:     e.Usage.CacheWrite,
		ReasoningTokens:      e.Usage.Reasoning,
		Status:               e.Status,
		DurationMS:           e.DurationMS,
		GenerationDurationMS: e.GenerationDurationMS,
		TTFTMS:               e.TTFTMS,
	}
}

// normalizedTextV2 renders the frozen nine element comparison text: a JSON array
// with occurred_at, project_id, provider, model, the six metric usage object,
// status, duration_ms and the two client observed timings. No insignificant
// whitespace, no localization, no floating point. Both timings are always present;
// an unknown timing is the null literal, so it can never be confused with a real
// zero.
func normalizedTextV2(input NormalizationInput) []byte {
	buffer := make([]byte, 0, 512)
	buffer = append(buffer, '[')
	buffer = appendJSONString(buffer, input.OccurredAt.UTC().Format(microLayout))
	buffer = append(buffer, ',')
	if input.ProjectID.Valid {
		buffer = appendJSONString(buffer, projectText(input.ProjectID))
	} else {
		buffer = append(buffer, "null"...)
	}
	buffer = append(buffer, ',')
	buffer = appendJSONString(buffer, input.Provider)
	buffer = append(buffer, ',')
	buffer = appendJSONString(buffer, input.Model)
	buffer = append(buffer, ',')
	buffer = appendUsage(buffer, input)
	buffer = append(buffer, ',')
	if input.Status != nil {
		buffer = appendJSONString(buffer, *input.Status)
	} else {
		buffer = append(buffer, "null"...)
	}
	buffer = append(buffer, ',')
	buffer = appendOptionalInteger(buffer, input.DurationMS)
	buffer = append(buffer, ',')
	buffer = appendOptionalInteger(buffer, input.GenerationDurationMS)
	buffer = append(buffer, ',')
	buffer = appendOptionalInteger(buffer, input.TTFTMS)
	buffer = append(buffer, ']')
	return buffer
}

// appendOptionalInteger writes a decimal integer or the null literal, so an
// unknown timing can never be confused with a real zero.
func appendOptionalInteger(buffer []byte, value *int64) []byte {
	if value == nil {
		return append(buffer, "null"...)
	}
	return strconv.AppendInt(buffer, *value, 10)
}

// microLayout is the normalized occurred_at representation: UTC with exactly six
// fractional digits and a trailing Z.
const microLayout = "2006-01-02T15:04:05.000000Z"

// appendUsage writes the usage object in the frozen key order, with all six
// metrics always present.
func appendUsage(buffer []byte, input NormalizationInput) []byte {
	buffer = append(buffer, `{"input_tokens":`...)
	buffer = appendMetric(buffer, input.InputTokens)
	buffer = append(buffer, `,"output_tokens":`...)
	buffer = appendMetric(buffer, input.OutputTokens)
	buffer = append(buffer, `,"total_tokens":`...)
	buffer = appendMetric(buffer, input.TotalTokens)
	buffer = append(buffer, `,"cache_read_tokens":`...)
	buffer = appendMetric(buffer, input.CacheReadTokens)
	buffer = append(buffer, `,"cache_write_tokens":`...)
	buffer = appendMetric(buffer, input.CacheWriteTokens)
	buffer = append(buffer, `,"reasoning_tokens":`...)
	buffer = appendMetric(buffer, input.ReasoningTokens)
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
