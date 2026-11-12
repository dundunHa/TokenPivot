import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  mapSessionEvent, mapUsage, mapGenerationTiming, resolveProject, formatTime, requestDuration, MAX_DURATION_MS,
} from '../lib/mapper.js'

const PID = '018f3c5e-2b3c-7d4e-9f0a-1b2c3d4e5f60'
const session = { id: 's-1', header: { id: 's-1', cwd: '/work/app/sub' } }
const msg = (usage, extra = {}) => ({
  type: 'assistant/message',
  seq: 5,
  time: Date.UTC(2026, 9, 28, 12, 0, 0, 123),
  data: {
    turn: 1,
    step: 1,
    message: {
      role: 'assistant',
      id: '7a1c9a52-0b7e-4e0b-9a3f-2f7c1b1d0e11',
      content: [{ type: 'text', text: 'SECRET PROMPT ANSWER' }],
      source: { kind: 'model', provider: 'deepseek-official', model: 'deepseek-chat' },
    },
    stream: [],
    usage,
    ...extra,
  },
})

test('assistant/message maps to a UsageEvent; DSH input excludes cache so it is added back', () => {
  const e = mapSessionEvent(session, msg({ inputTokens: 100, outputTokens: 20, cacheReadTokens: 300, cacheWriteTokens: 5, totalTokens: 425 }), {
    projects: [{ path: '/work/app', projectId: PID.toUpperCase() }],
  })
  assert.deepEqual(e, {
    source_event_id: 'dsh:msg:7a1c9a52-0b7e-4e0b-9a3f-2f7c1b1d0e11',
    project_id: PID,
    provider: 'deepseek-official',
    model: 'deepseek-chat',
    occurred_at: '2026-10-28T12:00:00.123Z',
    usage: {
      input_tokens: { value: 405, quality: 'known' },
      output_tokens: { value: 20, quality: 'known' },
      total_tokens: { value: 425, quality: 'known' },
      cache_read_tokens: { value: 300, quality: 'known' },
      cache_write_tokens: { value: 5, quality: 'known' },
      reasoning_tokens: { value: null, quality: 'missing' },
    },
    status: null,
    duration_ms: null,
  })
})

test('no content, cwd or path leaks into the event', () => {
  const e = mapSessionEvent(session, msg({ inputTokens: 1, outputTokens: 1 }))
  const text = JSON.stringify(e)
  assert.ok(!text.includes('SECRET'))
  assert.ok(!text.includes('/work'))
  assert.equal(e.project_id, null)
})

test('absent cache fields are missing, not zero', () => {
  const u = mapUsage({ inputTokens: 10, outputTokens: 2, totalTokens: 12 })
  assert.deepEqual(u.cache_read_tokens, { value: null, quality: 'missing' })
  assert.deepEqual(u.cache_write_tokens, { value: null, quality: 'missing' })
  assert.deepEqual(u.input_tokens, { value: 10, quality: 'known' })
  assert.deepEqual(u.total_tokens, { value: 12, quality: 'known' })
})

test('real zero stays known zero', () => {
  const u = mapUsage({ inputTokens: 0, outputTokens: 0, cacheReadTokens: 0, totalTokens: 0 })
  assert.deepEqual(u.cache_read_tokens, { value: 0, quality: 'known' })
  assert.deepEqual(u.total_tokens, { value: 0, quality: 'known' })
})

test('DSH total that matches neither reading: input and total estimated (server will not derive a known total)', () => {
  const u = mapUsage({ inputTokens: 10, outputTokens: 2, totalTokens: 99 })
  assert.deepEqual(u.input_tokens, { value: 10, quality: 'estimated' })
  assert.deepEqual(u.total_tokens, { value: 99, quality: 'estimated' })
})

test('route whose inputTokens already includes cache (totalTokens == input + output)', () => {
  const u = mapUsage({ inputTokens: 1000, outputTokens: 10, cacheReadTokens: 900, totalTokens: 1010 })
  assert.deepEqual(u.input_tokens, { value: 1000, quality: 'known' })
  assert.deepEqual(u.total_tokens, { value: 1010, quality: 'known' })
})

test('cache present but no totalTokens (openrouter sample): input estimated, total left for server', () => {
  const u = mapUsage({ inputTokens: 325555, outputTokens: 193, cacheReadTokens: 325504 })
  assert.deepEqual(u.input_tokens, { value: 325555, quality: 'estimated' })
  assert.deepEqual(u.total_tokens, { value: null, quality: 'missing' })
  // cache larger than inputTokens rules out the inclusive reading
  assert.equal(mapUsage({ inputTokens: 5, outputTokens: 1, cacheReadTokens: 50 }).input_tokens.value, 55)
})

test('no cache and no totalTokens: input known, total left for server to derive', () => {
  const u = mapUsage({ inputTokens: 7, outputTokens: 3 })
  assert.deepEqual(u.input_tokens, { value: 7, quality: 'known' })
  assert.deepEqual(u.total_tokens, { value: null, quality: 'missing' })
})

test('reasoning: reported when present and consistent, missing otherwise', () => {
  assert.deepEqual(mapUsage({ inputTokens: 1, outputTokens: 10, reasoningTokens: 4 }).reasoning_tokens, { value: 4, quality: 'known' })
  assert.deepEqual(mapUsage({ inputTokens: 1, outputTokens: 10, reasoningTokens: 11 }).reasoning_tokens, { value: null, quality: 'missing' })
})

test('invalid or absent usage skips the event', () => {
  assert.equal(mapUsage(undefined), null)
  assert.equal(mapUsage({ inputTokens: -1, outputTokens: 1 }), null)
  assert.equal(mapUsage({ inputTokens: 1.5, outputTokens: 1 }), null)
  assert.equal(mapUsage({ inputTokens: 1, outputTokens: 1, cacheReadTokens: 'x' }), null)
  assert.equal(mapSessionEvent(session, msg(undefined)), null)
})

test('compaction/summary uses session-scoped id', () => {
  const e = mapSessionEvent(session, {
    type: 'compaction/summary',
    time: Date.UTC(2026, 9, 28),
    data: { compactionId: 'c-9', provider: 'nebius', model: 'm', usage: { inputTokens: 50, outputTokens: 5, totalTokens: 55 } },
  })
  assert.equal(e.source_event_id, 'dsh:compaction:s-1:c-9')
  assert.equal(e.provider, 'nebius')
  assert.equal(e.usage.total_tokens.value, 55)
})

test('other event types and non-assistant messages are ignored', () => {
  assert.equal(mapSessionEvent(session, { type: 'assistant/attempt', time: 1, data: { stream: [] } }), null)
  assert.equal(mapSessionEvent(session, { type: 'session/title-llm-request', time: 1, data: {} }), null)
  const user = msg({ inputTokens: 1, outputTokens: 1 })
  user.data.message = { ...user.data.message, role: 'user' }
  assert.equal(mapSessionEvent(session, user), null)
})

test('invalid provider/model skips the event', () => {
  const bad = msg({ inputTokens: 1, outputTokens: 1 })
  bad.data.message = { ...bad.data.message, source: { kind: 'model', provider: ' padded', model: 'm' } }
  assert.equal(mapSessionEvent(session, bad), null)
  bad.data.message = { ...bad.data.message, source: { kind: 'model', provider: 'p', model: 'x'.repeat(129) } }
  assert.equal(mapSessionEvent(session, bad), null)
  for (const provider of ['p\u0085', 'a\u0090b', '\u3000p', 'p\u00a0']) {
    bad.data.message = { ...bad.data.message, source: { kind: 'model', provider, model: 'm' } }
    assert.equal(mapSessionEvent(session, bad), null, JSON.stringify(provider))
  }
  bad.data.message = { ...bad.data.message, source: { kind: 'model', provider: '路由 a', model: 'm' } }
  assert.notEqual(mapSessionEvent(session, bad), null)
})

test('resolveProject: longest prefix on directory boundary, invalid ids ignored', () => {
  const projects = [
    { path: '/work', projectId: '00000000-0000-0000-0000-000000000001' },
    { path: '/work/app/', projectId: '00000000-0000-0000-0000-000000000002' },
    { path: '/work/app/sub', projectId: 'not-a-uuid' },
  ]
  assert.equal(resolveProject('/work/app/sub/x', projects), '00000000-0000-0000-0000-000000000002')
  assert.equal(resolveProject('/work/apple', projects), '00000000-0000-0000-0000-000000000001')
  assert.equal(resolveProject('/other', projects), null)
  assert.equal(resolveProject(undefined, projects), null)
})

test('formatTime emits RFC 3339 UTC with ≤ 6 fractional digits', () => {
  assert.match(formatTime(Date.UTC(2026, 0, 1, 0, 0, 0, 5)), /^2026-01-01T00:00:00\.005Z$/)
  assert.equal(formatTime(NaN), null)
})

// ---- generation timing (T-061 D) ----

const TIMED = {
  type: 'assistant/message',
  seq: 9,
  time: Date.UTC(2026, 9, 28, 12, 0, 10),
  data: {
    message: {
      role: 'assistant',
      id: '7a1c9a52-0b7e-4e0b-9a3f-2f7c1b1d0e12',
      source: { kind: 'model', provider: 'p', model: 'm' },
    },
    usage: { inputTokens: 100, outputTokens: 100, totalTokens: 200 },
  },
}
const timingOf = (start, first, last) => ({ requestStartedAt: start, firstOutputAt: first, lastOutputAt: last })

test('2000/8000 ms timing maps to decode duration and TTFT; no speed is uploaded', () => {
  const e = mapSessionEvent(session, TIMED, {}, timingOf(0, 8000, 10000))
  assert.equal(e.generation_duration_ms, 2000)
  assert.equal(e.ttft_ms, 8000)
  assert.ok(!Object.keys(e).some((k) => /speed|tps|rate/.test(k)))
  assert.equal(e.occurred_at, '2026-10-28T12:00:10.000Z') // unchanged by timing
  assert.deepEqual(e.usage.output_tokens, { value: 100, quality: 'known' })
})

test('single chunk: zero decode duration is a real observation, TTFT still computed', () => {
  const e = mapSessionEvent(session, TIMED, {}, timingOf(300, 1200, 1200))
  assert.equal(e.generation_duration_ms, 0)
  assert.equal(e.ttft_ms, 900)
})

test('same millisecond first output: TTFT 0 is kept, not dropped', () => {
  const e = mapSessionEvent(session, TIMED, {}, timingOf(5000, 5000, 6000))
  assert.equal(e.ttft_ms, 0)
  assert.equal(e.generation_duration_ms, 1000)
})

test('clock going backwards: each invalid difference is null; both invalid omits both keys', () => {
  // last before first → duration null, TTFT still valid
  const partial = mapSessionEvent(session, TIMED, {}, timingOf(0, 8000, 7000))
  assert.equal(partial.generation_duration_ms, null)
  assert.equal(partial.ttft_ms, 8000)
  // first before start → TTFT null, duration valid (no abs(), no zero)
  const other = mapSessionEvent(session, TIMED, {}, timingOf(9000, 8000, 10000))
  assert.equal(other.generation_duration_ms, 2000)
  assert.equal(other.ttft_ms, null)
  // both broken → the event is reported exactly as before: neither key present
  const none = mapSessionEvent(session, TIMED, {}, timingOf(9000, 8000, 7000))
  assert.deepEqual(Object.keys(none).sort(), ['duration_ms', 'model', 'occurred_at', 'project_id', 'provider', 'source_event_id', 'status', 'usage'])
  assert.ok(!('generation_duration_ms' in none) && !('ttft_ms' in none))
})

test('missing boundaries: null value, absent field, non-integer and out-of-range are unknown', () => {
  assert.deepEqual(mapGenerationTiming(timingOf(0, null, 1000)), {})
  // last boundary unknown: duration null, TTFT is independent and still valid
  assert.deepEqual(mapGenerationTiming(timingOf(0, 1000, null)), { generation_duration_ms: null, ttft_ms: 1000 })
  assert.deepEqual(mapGenerationTiming(timingOf(0, 1000, undefined)), { generation_duration_ms: null, ttft_ms: 1000 })
  assert.deepEqual(mapGenerationTiming({ requestStartedAt: 0 }), {})
  assert.deepEqual(mapGenerationTiming(undefined), {})
  assert.deepEqual(mapGenerationTiming(null), {})
  assert.deepEqual(mapGenerationTiming('nope'), {})
  // negative start → TTFT unknown, duration unaffected
  assert.deepEqual(mapGenerationTiming(timingOf(-1, 1000, 2000)), { generation_duration_ms: 1000, ttft_ms: null })
  // fractional and beyond 2^53-1 boundaries are not observations
  assert.deepEqual(mapGenerationTiming(timingOf(0.5, 1000, 2000)), { generation_duration_ms: 1000, ttft_ms: null })
  assert.deepEqual(mapGenerationTiming(timingOf(0, 1000.5, 2000)), {})
  assert.deepEqual(mapGenerationTiming({ requestStartedAt: 0, firstOutputAt: 2 ** 53, lastOutputAt: 2 ** 53 }), {})
  assert.deepEqual(mapGenerationTiming({ requestStartedAt: 0, firstOutputAt: 0, lastOutputAt: 2 ** 53 }), { generation_duration_ms: null, ttft_ms: 0 })
  assert.deepEqual(mapGenerationTiming({ requestStartedAt: 0, firstOutputAt: 9007199254740991, lastOutputAt: 9007199254740991 }), { generation_duration_ms: 0, ttft_ms: 9007199254740991 })
})

test('old host: no timing argument keeps the exact previous event shape (v1)', () => {
  const e = mapSessionEvent(session, msg({ inputTokens: 100, outputTokens: 20, totalTokens: 120 }))
  assert.deepEqual(e, {
    source_event_id: 'dsh:msg:7a1c9a52-0b7e-4e0b-9a3f-2f7c1b1d0e11',
    project_id: null,
    provider: 'deepseek-official',
    model: 'deepseek-chat',
    occurred_at: '2026-10-28T12:00:00.123Z',
    usage: {
      input_tokens: { value: 100, quality: 'known' },
      output_tokens: { value: 20, quality: 'known' },
      total_tokens: { value: 120, quality: 'known' },
      cache_read_tokens: { value: null, quality: 'missing' },
      cache_write_tokens: { value: null, quality: 'missing' },
      reasoning_tokens: { value: null, quality: 'missing' },
    },
    status: null,
    duration_ms: null,
  })
})

test('timing without any usage still reports, with every metric missing (TTFT survives)', () => {
  const event = { ...TIMED, data: { message: TIMED.data.message } }
  const e = mapSessionEvent(session, event, {}, timingOf(0, 8000, 10000))
  assert.equal(e.ttft_ms, 8000)
  assert.equal(e.generation_duration_ms, 2000)
  assert.deepEqual(e.usage, {
    input_tokens: { value: null, quality: 'missing' },
    output_tokens: { value: null, quality: 'missing' },
    total_tokens: { value: null, quality: 'missing' },
    cache_read_tokens: { value: null, quality: 'missing' },
    cache_write_tokens: { value: null, quality: 'missing' },
    reasoning_tokens: { value: null, quality: 'missing' },
  })
})

test('invalid usage plus no usable timing is still skipped', () => {
  const event = { ...TIMED, data: { message: TIMED.data.message, usage: { inputTokens: -1, outputTokens: 1 } } }
  assert.equal(mapSessionEvent(session, event, {}, timingOf(9000, 8000, 7000)), null)
  assert.equal(mapSessionEvent(session, event), null)
})

test('compaction summary without usage keeps its own TTFT', () => {
  const e = mapSessionEvent(session, {
    type: 'compaction/summary',
    seq: 12,
    time: Date.UTC(2026, 9, 28),
    data: { compactionId: 'c-9', provider: 'nebius', model: 'm' },
  }, {}, timingOf(100, 500, 1500))
  assert.equal(e.source_event_id, 'dsh:compaction:s-1:c-9')
  assert.equal(e.ttft_ms, 400)
  assert.equal(e.generation_duration_ms, 1000)
  assert.equal(e.usage.total_tokens.quality, 'missing')
})

test('usage is preserved verbatim when timing is also present', () => {
  const e = mapSessionEvent(session, TIMED, {}, timingOf(0, 8000, 10000))
  assert.deepEqual(e.usage, mapUsage(TIMED.data.usage))
})

// ---- partial usage recovery on the timing path (T-061 D fix) ----

const timedPartial = (usage) => ({ ...TIMED, data: { message: TIMED.data.message, usage } })

test('partial usage: a missing input count no longer discards the known output', () => {
  const e = mapSessionEvent(session, timedPartial({ outputTokens: 100 }), {}, timingOf(0, 8000, 10000))
  assert.deepEqual(e.usage.output_tokens, { value: 100, quality: 'known' })
  assert.deepEqual(e.usage.input_tokens, { value: null, quality: 'missing' }) // unknown, never 0
  assert.deepEqual(e.usage.total_tokens, { value: null, quality: 'missing' })
  assert.equal(e.ttft_ms, 8000)
  assert.equal(e.generation_duration_ms, 2000)
  // only the two documented shapes ever leave the mapper
  for (const metric of Object.values(e.usage)) {
    assert.ok(metric.quality === 'known' ? Number.isSafeInteger(metric.value) : metric.quality === 'missing' && metric.value === null)
  }
})

test('partial usage: a malformed optional cache value cannot assert a known input', () => {
  const e = mapSessionEvent(session, timedPartial({ inputTokens: 10, outputTokens: 100, cacheReadTokens: 'x', totalTokens: 110 }), {}, timingOf(0, 8000, 10000))
  assert.deepEqual(e.usage.output_tokens, { value: 100, quality: 'known' })
  // the cache contribution is unknown, so treating it as 0 would claim input=10 known
  assert.deepEqual(e.usage.input_tokens, { value: null, quality: 'missing' })
  assert.deepEqual(e.usage.cache_read_tokens, { value: null, quality: 'missing' })
  // the host's own total is an independent count and is kept as reported
  assert.deepEqual(e.usage.total_tokens, { value: 110, quality: 'known' })
})

test('partial usage: an inconsistent total never survives next to two known counts', () => {
  const e = mapSessionEvent(session, timedPartial({ inputTokens: 10, outputTokens: 100, totalTokens: 999, reasoningTokens: 'x' }), {}, timingOf(0, 8000, 10000))
  // no cache: the input reading is exact on its own, and the contradicting total goes missing
  assert.deepEqual(e.usage.input_tokens, { value: 10, quality: 'known' })
  assert.deepEqual(e.usage.output_tokens, { value: 100, quality: 'known' })
  assert.deepEqual(e.usage.total_tokens, { value: null, quality: 'missing' })
  assert.deepEqual(e.usage.cache_write_tokens, { value: null, quality: 'missing' })
  // a cache-bearing input cannot be read from a contradicting total, so it stays unknown
  const cached = mapSessionEvent(session, timedPartial({ inputTokens: 10, outputTokens: 100, cacheReadTokens: 20, reasoningTokens: 'x' }), {}, timingOf(0, 8000, 10000))
  assert.deepEqual(cached.usage.input_tokens, { value: null, quality: 'missing' })
  assert.deepEqual(cached.usage.output_tokens, { value: 100, quality: 'known' })
  assert.deepEqual(cached.usage.cache_read_tokens, { value: 20, quality: 'known' })
})

test('partial usage: reasoning is kept only when it is a confirmed subset of the known output', () => {
  const kept = mapSessionEvent(session, timedPartial({ outputTokens: 100, reasoningTokens: 40 }), {}, timingOf(0, 8000, 10000))
  assert.deepEqual(kept.usage.reasoning_tokens, { value: 40, quality: 'known' })
  assert.deepEqual(kept.usage.output_tokens, { value: 100, quality: 'known' })
  // an unusable input breaks mapUsage; reasoning larger than the output must still be dropped
  const dropped = mapSessionEvent(session, timedPartial({ inputTokens: -1, outputTokens: 100, reasoningTokens: 140 }), {}, timingOf(0, 8000, 10000))
  assert.deepEqual(dropped.usage.reasoning_tokens, { value: null, quality: 'missing' })
  assert.deepEqual(dropped.usage.output_tokens, { value: 100, quality: 'known' })
  assert.deepEqual(dropped.usage.input_tokens, { value: null, quality: 'missing' })
})

test('partial usage: a cache-free input is confirmed without any output', () => {
  // inclusive and exclusive coincide when nothing was cached, so input 10 stands on its own
  const e = mapSessionEvent(session, timedPartial({ inputTokens: 10, outputTokens: -1, reasoningTokens: 5 }), {}, timingOf(0, 8000, 10000))
  assert.equal(e.ttft_ms, 8000)
  assert.equal(e.generation_duration_ms, 2000)
  assert.deepEqual(e.usage.input_tokens, { value: 10, quality: 'known' })
  assert.deepEqual(e.usage.output_tokens, { value: null, quality: 'missing' })
  assert.deepEqual(e.usage.total_tokens, { value: null, quality: 'missing' })
  assert.deepEqual(e.usage.reasoning_tokens, { value: null, quality: 'missing' }) // no output to bound it
})

test('partial usage: a cache-bearing input still needs the output to pick a reading', () => {
  const e = mapSessionEvent(session, timedPartial({ inputTokens: 10, outputTokens: -1, cacheReadTokens: 300 }), {}, timingOf(0, 8000, 10000))
  assert.deepEqual(e.usage.input_tokens, { value: null, quality: 'missing' })
  assert.deepEqual(e.usage.cache_read_tokens, { value: 300, quality: 'known' })
  assert.deepEqual(e.usage.output_tokens, { value: null, quality: 'missing' })
})

test('partial usage: nothing confirmed at all is the same all-missing shape, with timing kept', () => {
  const e = mapSessionEvent(session, timedPartial({ inputTokens: -1, outputTokens: -1, reasoningTokens: 5 }), {}, timingOf(0, 8000, 10000))
  assert.equal(e.ttft_ms, 8000)
  assert.deepEqual(e.usage, {
    input_tokens: { value: null, quality: 'missing' },
    output_tokens: { value: null, quality: 'missing' },
    total_tokens: { value: null, quality: 'missing' },
    cache_read_tokens: { value: null, quality: 'missing' },
    cache_write_tokens: { value: null, quality: 'missing' },
    reasoning_tokens: { value: null, quality: 'missing' },
  })
})

test('compaction summary confirms a cache-free input without a usable output', () => {
  const e = mapSessionEvent(session, {
    type: 'compaction/summary',
    seq: 13,
    time: Date.UTC(2026, 9, 28),
    data: { compactionId: 'c-10', provider: 'nebius', model: 'm', usage: { inputTokens: 10, outputTokens: -1 } },
  }, {}, timingOf(0, 2000, 4000))
  assert.equal(e.source_event_id, 'dsh:compaction:s-1:c-10')
  assert.equal(e.ttft_ms, 2000)
  assert.equal(e.generation_duration_ms, 2000)
  assert.deepEqual(e.usage.input_tokens, { value: 10, quality: 'known' })
  assert.deepEqual(e.usage.output_tokens, { value: null, quality: 'missing' })
})

test('the same broken usage without a usable timing is still skipped: recovery is timing-only', () => {
  const usage = { inputTokens: 10, outputTokens: 100, cacheReadTokens: 'x' }
  assert.equal(mapSessionEvent(session, timedPartial(usage)), null) // old host, v1 path
  assert.equal(mapSessionEvent(session, timedPartial(usage), {}, timingOf(9000, 8000, 7000)), null) // no usable timing
  assert.notEqual(mapSessionEvent(session, timedPartial(usage), {}, timingOf(0, 8000, 10000)), null)
})

test('compaction summary recovers its known output from a partially broken usage', () => {
  const e = mapSessionEvent(session, {
    type: 'compaction/summary',
    seq: 12,
    time: Date.UTC(2026, 9, 28),
    data: { compactionId: 'c-9', provider: 'nebius', model: 'm', usage: { outputTokens: 100, inputTokens: 'x' } },
  }, {}, timingOf(0, 2000, 4000))
  assert.equal(e.source_event_id, 'dsh:compaction:s-1:c-9')
  assert.equal(e.ttft_ms, 2000)
  assert.equal(e.generation_duration_ms, 2000)
  assert.deepEqual(e.usage.output_tokens, { value: 100, quality: 'known' })
  assert.deepEqual(e.usage.input_tokens, { value: null, quality: 'missing' })
})

test('a cache-bearing known total still pins the input reading exactly as mapUsage does', () => {
  // Same fields mapUsage accepts: the recovery path must not disagree with it.
  const usage = { inputTokens: 1000, outputTokens: 10, cacheReadTokens: 900, totalTokens: 1910 }
  assert.deepEqual(mapSessionEvent(session, timedPartial(usage), {}, timingOf(0, 8000, 10000)).usage, mapUsage(usage))
})

test('partial usage: a malformed total still leaves the no-cache input exact, total left to the server', () => {
  const e = mapSessionEvent(session, timedPartial({ inputTokens: 10, outputTokens: 100, totalTokens: 1.5 }), {}, timingOf(0, 8000, 10000))
  // no cache reported: both readings coincide, so the input is exact and the server derives the total
  assert.deepEqual(e.usage.input_tokens, { value: 10, quality: 'known' })
  assert.deepEqual(e.usage.output_tokens, { value: 100, quality: 'known' })
  assert.deepEqual(e.usage.total_tokens, { value: null, quality: 'missing' })
})

test('requestDuration: step start to completion, unknown when absent or implausible', () => {
  assert.equal(requestDuration(1000, 3500), 2500)
  assert.equal(requestDuration(1000, 1000), 0) // a real zero is not unknown
  assert.equal(requestDuration(undefined, 3500), null)
  assert.equal(requestDuration(Number.NaN, 3500), null)
  assert.equal(requestDuration(5000, 4000), null) // clock went backwards
  assert.equal(requestDuration(0, MAX_DURATION_MS), MAX_DURATION_MS)
  assert.equal(requestDuration(0, MAX_DURATION_MS + 1), null)
  assert.equal(requestDuration(0.4, 10.6), 10)
})

test('duration_ms comes from options.startedAt for assistant/message only', () => {
  const event = msg({ inputTokens: 1, outputTokens: 2 })
  const startedAt = event.time - 1800
  assert.equal(mapSessionEvent(session, event, { startedAt }).duration_ms, 1800)
  assert.equal(mapSessionEvent(session, event, {}).duration_ms, null)
  const compaction = {
    type: 'compaction/summary',
    time: event.time,
    data: { compactionId: 'c-1', provider: 'p', model: 'm', usage: { inputTokens: 1, outputTokens: 1 } },
  }
  assert.equal(mapSessionEvent(session, compaction, { startedAt }).duration_ms, null)
})
