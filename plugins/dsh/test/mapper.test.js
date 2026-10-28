import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mapSessionEvent, mapUsage, resolveProject, formatTime } from '../lib/mapper.js'

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
