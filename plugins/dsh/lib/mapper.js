/**
 * DSH session event → TokenPivot UsageEvent (docs/openapi/openapi.yaml, POST /api/v1/ingestion/events).
 *
 * Only two DSH event types carry final per-request usage (T-043 research,
 * docs/research/2026-10-28-dsh-platform-capabilities.md):
 *   - assistant/message   data.message.id (UUID, stable across forked sessions), data.message.source.{provider,model}, data.usage
 *   - compaction/summary  data.compactionId, data.provider, data.model, data.usage
 * Nothing else leaves this module: no prompt/response text, no cwd/path, no session title.
 */

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
const CONTROL_RE = /[\u0000-\u001f\u007f]/
const encoder = new TextEncoder()

const byteLength = (s) => encoder.encode(s).length

/** provider/model: 1–128 UTF-8 bytes, no leading/trailing whitespace, no control chars. */
function validName(value) {
  return typeof value === 'string' && value.length > 0 && value === value.trim()
    && !CONTROL_RE.test(value) && byteLength(value) <= 128
}

function validEventId(value) {
  return typeof value === 'string' && value.length > 0 && !CONTROL_RE.test(value) && byteLength(value) <= 256
}

const isCount = (v) => Number.isSafeInteger(v) && v >= 0
const known = (value) => ({ value, quality: 'known' })
const MISSING = Object.freeze({ value: null, quality: 'missing' })

/**
 * DSH TokenUsage → TokenPivot usage.
 * DSH inputTokens EXCLUDES cache reads/writes (local survey: totalTokens == input+output+cacheRead+cacheWrite
 * in 23468/23760 messages, the rest have no cache fields), while TokenPivot's input_tokens includes them.
 * Returns null when no usable usage exists (event is skipped, never reported as zero).
 */
export function mapUsage(u) {
  if (!u || typeof u !== 'object') return null
  if (!isCount(u.inputTokens) || !isCount(u.outputTokens)) return null
  const has = (k) => u[k] !== undefined && u[k] !== null
  for (const k of ['cacheReadTokens', 'cacheWriteTokens', 'reasoningTokens', 'totalTokens']) {
    if (has(k) && !isCount(u[k])) return null
  }
  const cacheRead = has('cacheReadTokens') ? u.cacheReadTokens : 0
  const cacheWrite = has('cacheWriteTokens') ? u.cacheWriteTokens : 0
  const input = u.inputTokens + cacheRead + cacheWrite
  const output = u.outputTokens
  const total = input + output
  if (!Number.isSafeInteger(total)) return null
  // DSH's own total must agree with our reconstruction; if it does not, we cannot tell which side is
  // wrong, so total is reported missing rather than guessed.
  const totalAgrees = !has('totalTokens') || u.totalTokens === total
  // reasoning is a subset of output; an inconsistent value is reported missing, not clamped.
  const reasoningOk = has('reasoningTokens') && u.reasoningTokens <= output
  return {
    input_tokens: known(input),
    output_tokens: known(output),
    total_tokens: totalAgrees ? known(total) : MISSING,
    cache_read_tokens: has('cacheReadTokens') ? known(u.cacheReadTokens) : MISSING,
    cache_write_tokens: has('cacheWriteTokens') ? known(u.cacheWriteTokens) : MISSING,
    reasoning_tokens: reasoningOk ? known(u.reasoningTokens) : MISSING,
  }
}

/** RFC 3339 UTC with millisecond precision (≤ 6 fractional digits as required). */
export function formatTime(ms) {
  if (!Number.isFinite(ms)) return null
  const d = new Date(ms)
  return Number.isNaN(d.getTime()) ? null : d.toISOString()
}

function normalizeDir(p) {
  if (typeof p !== 'string' || p.length === 0) return null
  let s = p.replace(/\\/g, '/')
  while (s.length > 1 && s.endsWith('/')) s = s.slice(0, -1)
  return s
}

/**
 * Local directory → project_id. Longest matching directory wins; a directory matches itself and
 * anything beneath it. Unmatched → null (未归属). The path is used only here and never uploaded.
 * @param {string|undefined} cwd
 * @param {{path: string, projectId: string}[]} mappings
 */
export function resolveProject(cwd, mappings) {
  const dir = normalizeDir(cwd)
  if (!dir || !Array.isArray(mappings)) return null
  let best = null
  let bestLen = -1
  for (const m of mappings) {
    const base = normalizeDir(m?.path)
    if (!base || typeof m.projectId !== 'string' || !UUID_RE.test(m.projectId)) continue
    const hit = dir === base || dir.startsWith(base === '/' ? '/' : `${base}/`)
    if (hit && base.length > bestLen) {
      best = m.projectId.toLowerCase()
      bestLen = base.length
    }
  }
  return best
}

/**
 * @param {{ id?: string, header?: { id?: string, cwd?: string } }} session
 * @param {{ type: string, time: number, data: any }} event
 * @param {{ cwd?: string, projects?: {path: string, projectId: string}[] }} options
 * @returns {object|null} UsageEvent, or null when the event carries no reportable usage
 */
export function mapSessionEvent(session, event, options = {}) {
  if (!event || typeof event !== 'object') return null
  const data = event.data
  if (!data || typeof data !== 'object') return null

  let sourceEventId
  let provider
  let model
  if (event.type === 'assistant/message') {
    const message = data.message
    if (!message || message.role !== 'assistant' || typeof message.id !== 'string') return null
    sourceEventId = `dsh:msg:${message.id}`
    provider = message.source?.provider
    model = message.source?.model
  } else if (event.type === 'compaction/summary') {
    const sessionId = session?.id ?? session?.header?.id
    if (typeof sessionId !== 'string' || typeof data.compactionId !== 'string') return null
    // compactionId is scoped to one session; prefix it with the session id so two sessions never collide.
    sourceEventId = `dsh:compaction:${sessionId}:${data.compactionId}`
    provider = data.provider
    model = data.model
  } else {
    return null
  }

  if (!validEventId(sourceEventId) || !validName(provider) || !validName(model)) return null
  const usage = mapUsage(data.usage)
  if (!usage) return null
  const occurredAt = formatTime(event.time)
  if (!occurredAt) return null

  return {
    source_event_id: sourceEventId,
    project_id: resolveProject(options.cwd ?? session?.header?.cwd, options.projects),
    provider,
    model,
    occurred_at: occurredAt,
    usage,
    // DSH records neither a request status nor a request duration for successful calls.
    status: null,
    duration_ms: null,
  }
}
