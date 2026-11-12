/**
 * DSH session event → TokenPivot UsageEvent (docs/openapi/openapi.yaml, POST /api/v1/ingestion/events).
 *
 * Only two DSH event types carry final per-request usage (T-043 research,
 * docs/research/2026-10-28-dsh-platform-capabilities.md):
 *   - assistant/message   data.message.id (UUID, stable across forked sessions), data.message.source.{provider,model}, data.usage
 *   - compaction/summary  data.compactionId, data.provider, data.model, data.usage
 * Nothing else leaves this module: no prompt/response text, no cwd/path, no session title. The only
 * project information is a mapped project_id or, with automatic detection, the repository's directory name.
 */

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
// OpenAPI provider/model/source_event_id: no C0/C1 control chars; provider/model also no leading or
// trailing Unicode whitespace (the exact set in docs/openapi/openapi.yaml, incl. U+0085 that trim() keeps).
const CONTROL_RE = /[\u0000-\u001f\u007f-\u009f]/
const WS = '\\u0009-\\u000d\\u0020\\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000'
const EDGE_WS_RE = new RegExp(`^[${WS}]|[${WS}]$`)
const encoder = new TextEncoder()

const byteLength = (s) => encoder.encode(s).length

/** provider/model: 1–128 UTF-8 bytes, no leading/trailing whitespace, no control chars. */
function validName(value) {
  return typeof value === 'string' && value.length > 0 && !EDGE_WS_RE.test(value)
    && !CONTROL_RE.test(value) && byteLength(value) <= 128
}

function validEventId(value) {
  return typeof value === 'string' && value.length > 0 && !CONTROL_RE.test(value) && byteLength(value) <= 256
}

const isCount = (v) => Number.isSafeInteger(v) && v >= 0
const known = (value) => ({ value, quality: 'known' })
const MISSING = Object.freeze({ value: null, quality: 'missing' })

/** An event whose request finished without usable usage: all six metrics missing, never zero. */
const missingUsage = () => ({
  input_tokens: MISSING,
  output_tokens: MISSING,
  total_tokens: MISSING,
  cache_read_tokens: MISSING,
  cache_write_tokens: MISSING,
  reasoning_tokens: MISSING,
})

/**
 * One generation boundary → a non-negative safe integer, or null. A missing, negative,
 * fractional or out-of-range boundary is unknown, not zero.
 */
const boundary = (v) => (Number.isSafeInteger(v) && v >= 0 ? v : null)

/** A difference that is still a non-negative safe integer, else null. */
function difference(later, earlier) {
  const value = later - earlier
  return Number.isSafeInteger(value) && value >= 0 ? value : null
}

/**
 * Host `GenerationTiming` → the two optional ingestion fields.
 *
 * Both are independent observations: `generation_duration_ms = lastOutputAt-firstOutputAt`
 * and `ttft_ms = firstOutputAt-requestStartedAt`. A missing boundary, a clock going backwards
 * and a non-safe difference are unknown (null); `0` is a real observation. When both are
 * unknown the fields are omitted entirely, so a host without timing keeps sending the v1
 * shape; when at least one is valid both keys appear and the other is null. This never derives
 * or uploads a speed — the server does that from final known output tokens.
 * @param {{requestStartedAt?: number, firstOutputAt?: number|null, lastOutputAt?: number|null}|null|undefined} timing
 * @returns {{generation_duration_ms?: number|null, ttft_ms?: number|null}}
 */
export function mapGenerationTiming(timing) {
  if (!timing || typeof timing !== 'object') return {}
  const startedAt = boundary(timing.requestStartedAt)
  const firstAt = boundary(timing.firstOutputAt)
  const lastAt = boundary(timing.lastOutputAt)
  const duration = firstAt === null || lastAt === null ? null : difference(lastAt, firstAt)
  const ttft = startedAt === null || firstAt === null ? null : difference(firstAt, startedAt)
  if (duration === null && ttft === null) return {}
  return { generation_duration_ms: duration, ttft_ms: ttft }
}

/**
 * DSH TokenUsage → TokenPivot usage (TokenPivot input_tokens INCLUDES cache reads/writes).
 *
 * Whether DSH's inputTokens includes cache depends on the provider route (T-043 survey,
 * docs/research/2026-10-28-dsh-platform-capabilities.md): most routes exclude it
 * (totalTokens == input + output + cacheRead + cacheWrite), an openrouter route includes it and
 * sends no totalTokens. DSH's own totalTokens decides which; without a usable totalTokens the input
 * is reported `estimated` and total is left for the server to derive (it then becomes estimated too).
 * Returns null when no usable usage exists (event is skipped, never reported as zero).
 */
export function mapUsage(u) {
  if (!u || typeof u !== 'object') return null
  if (!isCount(u.inputTokens) || !isCount(u.outputTokens)) return null
  const has = (k) => u[k] !== undefined && u[k] !== null
  for (const k of ['cacheReadTokens', 'cacheWriteTokens', 'reasoningTokens', 'totalTokens']) {
    if (has(k) && !isCount(u[k])) return null
  }
  const cache = (has('cacheReadTokens') ? u.cacheReadTokens : 0) + (has('cacheWriteTokens') ? u.cacheWriteTokens : 0)
  const output = u.outputTokens
  const exclusive = u.inputTokens + cache
  const inclusive = u.inputTokens
  const dshTotal = has('totalTokens') ? u.totalTokens : undefined

  let input
  let total = MISSING // missing → server derives input + output with the matching quality
  if (dshTotal === exclusive + output || (cache === 0 && dshTotal === undefined)) {
    input = known(exclusive) // no cache: both readings coincide, input is exact
    if (dshTotal !== undefined) total = known(dshTotal)
  } else if (dshTotal === inclusive + output && cache <= inclusive) {
    input = known(inclusive)
    total = known(dshTotal)
  } else {
    // Cannot tell which reading applies. A cache larger than inputTokens rules out "inclusive".
    input = { value: cache <= inclusive ? inclusive : exclusive, quality: 'estimated' }
    if (dshTotal !== undefined) total = { value: dshTotal, quality: 'estimated' }
  }
  if (!Number.isSafeInteger(input.value + output)) return null
  // reasoning is a subset of output; an inconsistent value is reported missing, not clamped.
  const reasoningOk = has('reasoningTokens') && u.reasoningTokens <= output
  return {
    input_tokens: input,
    output_tokens: known(output),
    total_tokens: total,
    cache_read_tokens: has('cacheReadTokens') ? known(u.cacheReadTokens) : MISSING,
    cache_write_tokens: has('cacheWriteTokens') ? known(u.cacheWriteTokens) : MISSING,
    reasoning_tokens: reasoningOk ? known(u.reasoningTokens) : MISSING,
  }
}

/**
 * Partial recovery for a final event that carries a valid timing but whose usage `mapUsage`
 * rejected (one required or optional count was unusable).
 *
 * Only counts that are independently confirmed survive; anything unconfirmed stays `missing`,
 * never `0`, so a known output (and the speed the server derives from it) is not thrown away
 * together with one unrelated broken field. The input reading reuses `mapUsage`'s rule: the cache
 * sum counts only when every present cache field is itself usable, and an absent cache field still
 * means "none reported". A cache-free input needs no other field (both readings coincide), while a
 * cache-bearing one is only confirmed when the total pins down which reading applies. When that
 * reading cannot be pinned down (unusable cache, ambiguous inclusive/exclusive, unsafe sum, a
 * total that disagrees) the field is missing instead of an asserted shape the server would reject.
 * @returns {object|null} usage, or null when no count can be confirmed
 */
export function recoverUsage(u) {
  if (!u || typeof u !== 'object') return null
  const has = (k) => u[k] !== undefined && u[k] !== null
  const count = (k) => (isCount(u[k]) ? u[k] : null)
  const inputTokens = count('inputTokens')
  const output = count('outputTokens')
  const cacheRead = count('cacheReadTokens')
  const cacheWrite = count('cacheWriteTokens')
  const dshTotal = count('totalTokens')
  // A present but unusable cache field hides an unknown part of the input; assuming 0 there would
  // assert a known input that can be too small.
  const cacheUsable = !(has('cacheReadTokens') && cacheRead === null)
    && !(has('cacheWriteTokens') && cacheWrite === null)
  const cache = cacheUsable ? (cacheRead ?? 0) + (cacheWrite ?? 0) : null
  const fits = (...parts) => Number.isSafeInteger(parts.reduce((sum, part) => sum + part, 0))

  let input = null
  if (inputTokens !== null && cache !== null) {
    const inclusive = inputTokens
    const exclusive = inclusive + cache
    if (cache === 0) {
      // No cache reported: inclusive and exclusive coincide, so neither the output nor a total can
      // change the reading — this input is confirmed on its own.
      input = exclusive
    } else if (output !== null && Number.isSafeInteger(exclusive)) {
      if (dshTotal !== null && fits(exclusive, output) && dshTotal === exclusive + output) {
        input = exclusive // DSH's total says the input excludes the cache
      } else if (dshTotal !== null && fits(inclusive, output) && dshTotal === inclusive + output && cache <= inclusive) {
        input = inclusive // DSH's total says the input already includes the cache
      }
    }
  }

  let total = dshTotal
  // The host's own total stands on its own when the input reading stayed unconfirmed; only a total
  // that contradicts two known counts would be rejected, so that one is reported as unknown.
  if (input !== null && output !== null && total !== null && total !== input + output) total = null
  // Without a total the server derives input+output from two known counts; a sum outside the safe
  // range would make it reject the event, so the input gives way and the output survives.
  if (input !== null && output !== null && total === null && !fits(input, output)) input = null

  const reasoning = count('reasoningTokens')
  const usage = {
    input_tokens: input === null ? MISSING : known(input),
    output_tokens: output === null ? MISSING : known(output),
    total_tokens: total === null ? MISSING : known(total),
    cache_read_tokens: cacheRead === null ? MISSING : known(cacheRead),
    cache_write_tokens: cacheWrite === null ? MISSING : known(cacheWrite),
    // reasoning is a subset of output, but the server only applies that check when both are known
    // (internal/ingestion/handler.go normalizeUsage). A confirmed reasoning count therefore stands
    // on its own when the output is unknown; only a known output can rule it out.
    reasoning_tokens: reasoning !== null && (output === null || reasoning <= output) ? known(reasoning) : MISSING,
  }
  return Object.values(usage).some((m) => m.quality === 'known') ? usage : null
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

/** A single model call longer than this is treated as a bad measurement and reported as unknown. */
export const MAX_DURATION_MS = 3_600_000

/**
 * Request duration: from the step's `step/start` (or the last failed attempt) to the commit of the
 * final `assistant/message`. DSH does not record a duration itself; this is the model-call latency
 * (request preparation, time to first token and generation) measured on DSH's own event clock.
 * Unknown (null) when the start was not seen or the value is implausible; never a guess.
 * @param {number|undefined} startedAt ms epoch of the step start
 * @param {number} completedAt ms epoch of the final message
 */
export function requestDuration(startedAt, completedAt) {
  if (!Number.isFinite(startedAt) || !Number.isFinite(completedAt)) return null
  const ms = Math.round(completedAt - startedAt)
  return ms >= 0 && ms <= MAX_DURATION_MS ? ms : null
}

/**
 * @param {{ id?: string, header?: { id?: string, cwd?: string } }} session
 * @param {{ type: string, time: number, data: any }} event
 * @param {{ cwd?: string, projects?: {path: string, projectId: string}[], detectProject?: (cwd: string|undefined) => string|null, startedAt?: number }} options
 * @param {object} [timing] the host's `GenerationTiming` for this final event, when it published one
 * @returns {object|null} UsageEvent, or null when the event carries no reportable usage or timing
 */
export function mapSessionEvent(session, event, options = {}, timing) {
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
  const generation = mapGenerationTiming(timing)
  const timed = Object.keys(generation).length > 0
  // A request with a published timing is still worth reporting without usage: the count stays
  // unknown (missing), never zero, so TTFT survives on its own. A usage that only broke on one
  // field is partially recovered (the known output keeps its speed sample); without timing the
  // event is skipped exactly as before.
  if (!usage && !timed) return null
  const occurredAt = formatTime(event.time)
  if (!occurredAt) return null

  // An explicit directory → project_id mapping wins; otherwise the repository name is sent as project_key
  // (the server finds or creates that project). Never both: the server rejects an event that carries both.
  const cwd = options.cwd ?? session?.header?.cwd
  const projectId = resolveProject(cwd, options.projects)
  const projectKey = projectId === null ? options.detectProject?.(cwd) ?? null : null

  return {
    source_event_id: sourceEventId,
    project_id: projectId,
    ...(projectKey ? { project_key: projectKey } : {}),
    provider,
    model,
    occurred_at: occurredAt,
    usage: usage ?? (timed ? recoverUsage(data.usage) : null) ?? missingUsage(),
    ...generation,
    // DSH records no request status for successful calls. Only assistant/message has a measurable
    // model call (compaction has no step/start of its own), so compaction stays unknown.
    status: null,
    duration_ms: event.type === 'assistant/message' ? requestDuration(options.startedAt, event.time) : null,
  }
}
