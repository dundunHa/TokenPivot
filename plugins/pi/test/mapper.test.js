/**
 * 映射验收（计划 §9.1 与 §4）：三分支口径、三态质量、缓存与 reasoning 不重复计算、
 * 缺完成时间/非终态跳过、会话命名空间与路径边界匹配。
 */
import test from 'node:test'
import assert from 'node:assert/strict'
import { mapTurnEnd, mapUsage, resolveProject } from '../lib/mapper.js'

const PROJECT_A = '0199a0b0-0000-7000-8000-00000000000a'
const PROJECT_B = '0199a0b0-0000-7000-8000-00000000000b'
const START = Date.parse('2026-11-05T00:00:00.000Z')

function message(over = {}) {
  return {
    role: 'assistant',
    provider: 'anthropic',
    model: 'claude-requested',
    stopReason: 'stop',
    timestamp: START,
    durationMs: 1200,
    usage: { input: 100, output: 20, cacheRead: 0, cacheWrite: 0, totalTokens: 120 },
    ...over,
  }
}

function map(over = {}, extra = {}) {
  return mapTurnEnd({
    sessionId: 'session-1',
    messageEntryId: 'entry-1',
    message: message(over),
    ...extra,
  })
}

test('口径 A：total = input + output + cacheRead + cacheWrite，服务端 input 含缓存', () => {
  const { event } = map({
    usage: { input: 100, output: 20, cacheRead: 30, cacheWrite: 10, totalTokens: 160 },
  })
  assert.deepEqual(event.usage.input_tokens, { value: 140, quality: 'known' })
  assert.deepEqual(event.usage.total_tokens, { value: 160, quality: 'known' })
  assert.deepEqual(event.usage.cache_read_tokens, { value: 30, quality: 'known' })
  assert.deepEqual(event.usage.cache_write_tokens, { value: 10, quality: 'known' })
})

test('口径 B：total = input + output 且缓存是输入子集，input 原样', () => {
  const { event } = map({
    usage: { input: 160, output: 20, cacheRead: 30, cacheWrite: 10, totalTokens: 180 },
  })
  assert.deepEqual(event.usage.input_tokens, { value: 160, quality: 'known' })
  assert.deepEqual(event.usage.total_tokens, { value: 180, quality: 'known' })
})

test('口径 C：缓存全为已知零，两式等价，input 精确', () => {
  const { event } = map()
  assert.deepEqual(event.usage.input_tokens, { value: 100, quality: 'known' })
  assert.deepEqual(event.usage.total_tokens, { value: 120, quality: 'known' })
})

test('口径未知：保留来源数值但降级 estimated，不把未知缓存当已知零', () => {
  const { event } = map({
    usage: { input: 160, output: 20, cacheRead: 30, cacheWrite: 10, totalTokens: 999 },
  })
  assert.deepEqual(event.usage.input_tokens, { value: 160, quality: 'estimated' })
  assert.deepEqual(event.usage.total_tokens, { value: 999, quality: 'estimated' })
})

test('缓存未报告 + total 有效：input 与 total 都是 estimated，不假设缓存为零', () => {
  const { event } = map({ usage: { input: 100, output: 20, totalTokens: 120 } })
  assert.deepEqual(event.usage.input_tokens, { value: 100, quality: 'estimated' })
  assert.deepEqual(event.usage.total_tokens, { value: 120, quality: 'estimated' })
  assert.deepEqual(event.usage.cache_read_tokens, { value: null, quality: 'missing' })
  assert.deepEqual(event.usage.cache_write_tokens, { value: null, quality: 'missing' })
})

test('缓存未报告 + total 缺失：input estimated，total 保持 missing（服务端不得推导出 known）', () => {
  const { event } = map({ usage: { input: 100, output: 20 } })
  assert.deepEqual(event.usage.input_tokens, { value: 100, quality: 'estimated' })
  assert.deepEqual(event.usage.total_tokens, { value: null, quality: 'missing' })
})

test('缓存已知为零 + total 缺失：input 仍精确，total 留给服务端按两 known 推导', () => {
  const { event } = map({ usage: { input: 100, output: 20, cacheRead: 0, cacheWrite: 0 } })
  assert.deepEqual(event.usage.input_tokens, { value: 100, quality: 'known' })
  assert.deepEqual(event.usage.total_tokens, { value: null, quality: 'missing' })
})

test('cacheWrite1h 是 cacheWrite 子集，不再相加', () => {
  const { event } = map({
    usage: { input: 100, output: 20, cacheRead: 0, cacheWrite: 50, cacheWrite1h: 40, totalTokens: 170 },
  })
  assert.deepEqual(event.usage.input_tokens, { value: 150, quality: 'known' })
  assert.deepEqual(event.usage.cache_write_tokens, { value: 50, quality: 'known' })
  assert.deepEqual(event.usage.total_tokens, { value: 170, quality: 'known' })
})

test('reasoning 是 output 子集，不加到 output 或 total', () => {
  const { event } = map({
    usage: { input: 100, output: 20, cacheRead: 0, cacheWrite: 0, totalTokens: 120, reasoning: 15 },
  })
  assert.deepEqual(event.usage.output_tokens, { value: 20, quality: 'known' })
  assert.deepEqual(event.usage.reasoning_tokens, { value: 15, quality: 'known' })
  assert.deepEqual(event.usage.total_tokens, { value: 120, quality: 'known' })
})

test('reasoning 大于 output：如实报缺失，不钳制、不加到 output', () => {
  const mapped = map({
    usage: { input: 100, output: 20, cacheRead: 0, cacheWrite: 0, totalTokens: 120, reasoning: 40 },
  })
  assert.deepEqual(mapped.event.usage.reasoning_tokens, { value: null, quality: 'missing' })
  assert.deepEqual(mapped.event.usage.output_tokens, { value: 20, quality: 'known' })
  assert.equal(mapped.invalid, 1)
})

test('缺失指标不补 0；非法值单独计数并按缺失表达', () => {
  const clean = map({ usage: { input: 100, output: 20 } })
  assert.deepEqual(clean.event.usage.reasoning_tokens, { value: null, quality: 'missing' })

  const dirty = map({ usage: { input: -1, output: 20, cacheRead: 0, cacheWrite: 0 } })
  assert.deepEqual(dirty.event.usage.input_tokens, { value: null, quality: 'missing' })
  assert.equal(dirty.invalid, 1)
})

test('total 保留来源数值，不与独立数值取最大钳制', () => {
  const low = mapUsage({ input: 100, output: 20, cacheRead: 0, cacheWrite: 0, totalTokens: 5 })
  assert.deepEqual(low.usage.total_tokens, { value: 5, quality: 'estimated' })
  assert.deepEqual(low.usage.input_tokens, { value: 100, quality: 'known' })
})

test('全部指标缺失：跳过并计数，不构造零用量事件', () => {
  assert.deepEqual(mapUsage({}), { skip: 'usage_missing' })
  assert.deepEqual(map({ usage: undefined }), { skip: 'usage_missing' })
})

test('pending / deferred 不是终态：跳过', () => {
  assert.deepEqual(map({ stopReason: 'pending' }), { skip: 'pending' })
  assert.deepEqual(map({ stopReason: 'deferred' }), { skip: 'deferred' })
})

test('缺 durationMs 时跳过，不用开始时间或采集时间替代', () => {
  const messageWithout = message()
  delete messageWithout.durationMs
  const mapped = mapTurnEnd({
    sessionId: 's', messageEntryId: 'e', message: messageWithout,
  })
  assert.deepEqual(mapped, { skip: 'no_duration' })
})

test('error 与 aborted：事实齐全时可采，aborted 不宣称成功', () => {
  const errored = map({ stopReason: 'error' })
  assert.equal(errored.event.status, 'error')
  assert.equal(errored.event.duration_ms, 1200)

  const aborted = map({ stopReason: 'aborted' })
  assert.equal(aborted.event.status, null)
  assert.equal(aborted.event.duration_ms, 1200)
})

test('非 assistant 或事实不全的消息跳过', () => {
  assert.deepEqual(map({ role: 'user' }), { skip: 'not_assistant' })
  assert.deepEqual(map({ provider: '' }), { skip: 'no_provider' })
  assert.deepEqual(map({ model: undefined }), { skip: 'no_model' })
  assert.deepEqual(map({ model: '   ' }), { skip: 'no_model' })
})

test('occurred_at = timestamp + durationMs，duration_ms 是来源耗时', () => {
  const { event } = map({ timestamp: START, durationMs: 1500 })
  assert.equal(event.occurred_at, '2026-11-05T00:00:01.500Z')
  assert.equal(event.duration_ms, 1500)
})

test('model 优先取 responseModel，无效时回落 message.model', () => {
  assert.equal(map({ model: 'requested-model', responseModel: 'served-model' }).event.model, 'served-model')
  assert.equal(map({ model: 'requested-model' }).event.model, 'requested-model')
  assert.equal(map({ model: 'requested-model', responseModel: '  ' }).event.model, 'requested-model')
})

test('source_event_id 稳定，并带会话命名空间：两会话同 entryId 不合并', () => {
  const first = map()
  const again = map()
  assert.equal(first.event.source_event_id, 'pi:msg:session-1:entry-1')
  assert.deepEqual(first.event, again.event)

  const otherSession = mapTurnEnd({
    sessionId: 'session-2',
    messageEntryId: 'entry-1',
    message: message(),
  })
  assert.notEqual(otherSession.event.source_event_id, first.event.source_event_id)
  assert.equal(otherSession.event.source_event_id, 'pi:msg:session-2:entry-1')
})

test('缺 sessionId 或 entryId 时跳过（ID 不完整就不上报）', () => {
  assert.deepEqual(
    mapTurnEnd({ sessionId: undefined, messageEntryId: 'e', message: message() }),
    { skip: 'invalid_id' },
  )
  assert.deepEqual(
    mapTurnEnd({ sessionId: 's', messageEntryId: '', message: message() }),
    { skip: 'invalid_id' },
  )
})

test('项目按路径边界最长匹配，未匹配为 null', () => {
  const projects = [
    { path: '/work/foo', projectId: PROJECT_A },
    { path: '/work/foo/bar', projectId: PROJECT_B },
  ]
  assert.equal(resolveProject('/work/foo', projects), PROJECT_A)
  assert.equal(resolveProject('/work/foo/bar', projects), PROJECT_B)
  assert.equal(resolveProject('/work/foo/bar/baz', projects), PROJECT_B)
  // 裸字符串前缀会把 bar2 当 bar 的子目录，这里必须不匹配。
  assert.equal(resolveProject('/work/foo/bar2', projects), PROJECT_A)
  assert.equal(resolveProject('/elsewhere', projects), null)
  assert.equal(resolveProject('/work/foo', [{ path: '/work/foo', projectId: 'not-a-uuid' }]), null)
  assert.equal(map({}, { cwd: '/work/foo/bar/x', projects }).event.project_id, PROJECT_B)
})
