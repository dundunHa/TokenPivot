import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtemp, readFile, readdir, writeFile, mkdir } from 'node:fs/promises'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const SCRIPT = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'scripts', 'convert-generation-v2.js')

const tmp = () => mkdtemp(path.join(os.tmpdir(), 'tp-dsh-convert-'))

/** 旧格式事件：没有两个计时键。 */
const legacyEvent = (n) => ({
  source_event_id: `dsh:msg:${n}`,
  project_id: null,
  provider: 'p',
  model: 'm',
  occurred_at: '2026-11-13T10:00:00.000Z',
  usage: {
    input_tokens: { value: n, quality: 'known' },
    output_tokens: { value: 1, quality: 'known' },
    total_tokens: { value: n + 1, quality: 'known' },
    cache_read_tokens: { value: null, quality: 'missing' },
    cache_write_tokens: { value: null, quality: 'missing' },
    reasoning_tokens: { value: null, quality: 'missing' },
  },
  status: null,
  duration_ms: null,
})

/** 与 FileQueue.persist 相同的序列化形态，便于逐字节比对。 */
const snapshot = (items) => JSON.stringify({ format: 1, items })

async function writeQueue(dir, doc) {
  await mkdir(dir, { recursive: true })
  const file = path.join(dir, 'queue.json')
  await writeFile(file, typeof doc === 'string' ? doc : JSON.stringify(doc), { mode: 0o600 })
  return file
}

function run(dir, args = []) {
  const result = spawnSync(process.execPath, [SCRIPT, '--dir', dir, ...args], { encoding: 'utf8' })
  return { status: result.status, stdout: result.stdout.trim(), stderr: result.stderr.trim() }
}

const parse = (text) => JSON.parse(text)

test('check reports how many pending events need the two keys and writes nothing', async () => {
  const dir = await tmp()
  const file = await writeQueue(dir, { format: 1, items: [legacyEvent(1), legacyEvent(2)] })
  const before = await readFile(file, 'utf8')
  const result = run(dir)
  assert.equal(result.status, 0)
  const report = parse(result.stdout)
  assert.equal(report.mode, 'check')
  assert.equal(report.items, 2)
  assert.equal(report.convertible, 2)
  assert.equal(report.converted, 0)
  assert.equal(report.state, 'needs_conversion')
  assert.equal(await readFile(file, 'utf8'), before)
  assert.deepEqual(await readdir(dir), ['queue.json'])
})

test('apply fills exactly the two missing keys and backs the file up first', async () => {
  const dir = await tmp()
  const items = [legacyEvent(1), { ...legacyEvent(2), generation_duration_ms: 1800, ttft_ms: 0 }]
  const file = await writeQueue(dir, { format: 1, items })
  const before = await readFile(file, 'utf8')
  const result = run(dir, ['--apply'])
  assert.equal(result.status, 0)
  const report = parse(result.stdout)
  assert.equal(report.mode, 'apply')
  assert.equal(report.converted, 1)
  assert.equal(report.state, 'converted')
  const after = parse(await readFile(file, 'utf8'))
  assert.equal(after.format, 1)
  assert.deepEqual(after.items.map((item) => item.source_event_id), items.map((item) => item.source_event_id))
  assert.equal(after.items[0].generation_duration_ms, null)
  assert.equal(after.items[0].ttft_ms, null)
  // 已合法存在的值原样保留，不从 duration 推导。
  assert.equal(after.items[1].generation_duration_ms, 1800)
  assert.equal(after.items[1].ttft_ms, 0)
  assert.deepEqual({ ...after.items[0], generation_duration_ms: undefined, ttft_ms: undefined },
    { ...items[0], generation_duration_ms: undefined, ttft_ms: undefined })
  assert.deepEqual({ ...after.items[1] }, { ...items[1] })
  const backupName = report.backup
  assert.match(backupName, /^queue\.json\.bak-/)
  assert.equal(await readFile(path.join(dir, backupName), 'utf8'), before)
  const leftovers = (await readdir(dir)).filter((name) => name.endsWith('.tmp'))
  assert.deepEqual(leftovers, [])
})

test('a second run converts nothing and touches neither the queue nor the backup', async () => {
  const dir = await tmp()
  await writeQueue(dir, { format: 1, items: [legacyEvent(1)] })
  assert.equal(run(dir, ['--apply']).status, 0)
  const afterFirst = await readFile(path.join(dir, 'queue.json'), 'utf8')
  const listing = (await readdir(dir)).sort()
  const second = run(dir, ['--apply'])
  assert.equal(second.status, 0)
  assert.equal(parse(second.stdout).converted, 0)
  assert.equal(parse(second.stdout).state, 'converted')
  assert.equal(await readFile(path.join(dir, 'queue.json'), 'utf8'), afterFirst)
  assert.deepEqual((await readdir(dir)).sort(), listing)
  const third = run(dir)
  assert.equal(parse(third.stdout).state, 'converted')
  assert.equal(parse(third.stdout).convertible, 0)
})

test('a queue with nothing to convert is never rewritten', async () => {
  const dir = await tmp()
  const items = [{ ...legacyEvent(1), generation_duration_ms: null, ttft_ms: null }]
  const file = await writeQueue(dir, { format: 1, items })
  // 故意写成带缩进的形态：没有待补键时不重新序列化，格式化差异必须原样保留。
  const pretty = JSON.stringify({ format: 1, items }, null, 2)
  await writeFile(file, pretty, { mode: 0o600 })
  const result = run(dir, ['--apply'])
  assert.equal(result.status, 0)
  assert.equal(parse(result.stdout).converted, 0)
  assert.equal(await readFile(file, 'utf8'), pretty)
  assert.deepEqual(await readdir(dir), ['queue.json'])
})

test('a missing queue file reports zero and creates nothing', async () => {
  const dir = path.join(await tmp(), 'absent')
  const result = run(dir, ['--apply'])
  assert.equal(result.status, 0)
  assert.equal(parse(result.stdout).state, 'no_queue')
  assert.equal(parse(result.stdout).converted, 0)
  await assert.rejects(readdir(dir), { code: 'ENOENT' })
})

test('a dead letter file is never read or modified', async () => {
  const dir = await tmp()
  await writeQueue(dir, { format: 1, items: [legacyEvent(1)] })
  const deadLetter = path.join(dir, 'dead-letter.jsonl')
  const lines = '{"at":"2026-11-13T10:00:00.000Z","source_event_id":"dsh:msg:9","outcome":"rejected","code":"INVALID_ARGUMENT"}\n'
  await writeFile(deadLetter, lines, { mode: 0o600 })
  assert.equal(run(dir, ['--apply']).status, 0)
  assert.equal(await readFile(deadLetter, 'utf8'), lines)
})

test('corrupt input stops the whole run without writing', async () => {
  for (const [name, content] of [
    ['not json', '{"format":1,'],
    ['not an object', '[]'],
    ['wrong format', JSON.stringify({ format: 2, items: [legacyEvent(1)] })],
    ['items not an array', JSON.stringify({ format: 1, items: {} })],
    ['missing event id', JSON.stringify({ format: 1, items: [{ provider: 'p' }] })],
    ['duplicate event id', JSON.stringify({ format: 1, items: [legacyEvent(1), legacyEvent(1)] })],
  ]) {
    const dir = await tmp()
    const file = await writeQueue(dir, content)
    const result = run(dir, ['--apply'])
    assert.equal(result.status, 1, name)
    assert.equal(parse(result.stderr).state, 'failed', name)
    assert.equal(await readFile(file, 'utf8'), content, name)
    assert.deepEqual(await readdir(dir), ['queue.json'], name)
  }
})

test('an illegal existing timing stops the whole run without partial writes', async () => {
  const cases = [
    ['negative', -1],
    ['fractional', 1.5],
    ['beyond the safe integer range', Number.MAX_SAFE_INTEGER + 2],
    ['string', '1800'],
    ['object', { value: 1800 }],
  ]
  for (const [name, value] of cases) {
    const dir = await tmp()
    const items = [legacyEvent(1), { ...legacyEvent(2), ttft_ms: value }]
    const content = snapshot(items)
    const file = await writeQueue(dir, content)
    const result = run(dir, ['--apply'])
    assert.equal(result.status, 1, name)
    assert.match(parse(result.stderr).error, /ttft_ms/, name)
    assert.equal(await readFile(file, 'utf8'), content, name)
    assert.deepEqual(await readdir(dir), ['queue.json'], name)
  }
})

test('the help text and an unknown argument are handled without touching the queue', async () => {
  const dir = await tmp()
  const file = await writeQueue(dir, { format: 1, items: [legacyEvent(1)] })
  const help = spawnSync(process.execPath, [SCRIPT, '--help'], { encoding: 'utf8' })
  assert.equal(help.status, 0)
  assert.match(help.stdout, /--apply/)
  const bad = run(dir, ['--force'])
  assert.equal(bad.status, 1)
  assert.equal(await readFile(file, 'utf8'), snapshot([legacyEvent(1)]))
})

test('an event that is not a valid current event stops check and apply without writing', async () => {
  const base = legacyEvent(1)
  const cases = [
    ['missing provider', { ...base, provider: undefined }],
    ['empty model', { ...base, model: '' }],
    ['missing occurred_at', { ...base, occurred_at: undefined }],
    ['occurred_at without an offset', { ...base, occurred_at: '2026-11-13T10:00:00' }],
    ['missing usage', { ...base, usage: undefined }],
    ['usage is not an object', { ...base, usage: [] }],
    ['usage carries an unknown metric', { ...base, usage: { ...base.usage, prompt_tokens: { value: 1, quality: 'known' } } }],
    ['known metric without a value', { ...base, usage: { ...base.usage, input_tokens: { value: null, quality: 'known' } } }],
    ['metric without a quality', { ...base, usage: { ...base.usage, input_tokens: { value: 1 } } }],
    ['missing metric with a value', { ...base, usage: { ...base.usage, output_tokens: { value: 2, quality: 'missing' } } }],
    ['metric with an unknown quality', { ...base, usage: { ...base.usage, total_tokens: { value: 3, quality: 'guessed' } } }],
    ['estimated metric with a string value', { ...base, usage: { ...base.usage, total_tokens: { value: '3', quality: 'estimated' } } }],
    ['negative duration', { ...base, duration_ms: -5 }],
    ['invalid project_id', { ...base, project_id: 'not-a-uuid' }],
    ['a top-level key the server does not know', { ...base, enqueued_at: 1763000000000 }],
    ['source_event_id longer than 256 bytes', { ...base, source_event_id: 'i'.repeat(257) }],
    ['provider longer than 128 bytes', { ...base, provider: 'p'.repeat(129) }],
    ['provider with surrounding whitespace', { ...base, provider: ' p' }],
    ['model with a control character', { ...base, model: 'm\u0001' }],
    ['project_key with surrounding whitespace', { ...base, project_key: ' k' }],
    ['project_key longer than 128 bytes', { ...base, project_key: 'k'.repeat(129) }],
    ['status longer than 64 bytes', { ...base, status: 's'.repeat(65) }],
    ['occurred_at on a day the month does not have', { ...base, occurred_at: '2026-02-30T10:00:00.000Z' }],
    ['occurred_at with an offset hour beyond 23', { ...base, occurred_at: '2026-11-13T10:00:00.000+24:00' }],
    ['both a project_id and a project_key', {
      ...base,
      project_id: '11111111-1111-4111-8111-111111111111',
      project_key: 'tokenpivot',
    }],
  ]
  for (const [name, bad] of cases) {
    const dir = await tmp()
    // 好坏混合：第一条合法，第二条结构损坏，整体必须中止。
    const content = snapshot([legacyEvent(2), bad])
    const file = await writeQueue(dir, content)
    for (const args of [[], ['--apply']]) {
      const result = run(dir, args)
      assert.equal(result.status, 1, name)
      assert.equal(parse(result.stderr).state, 'failed', name)
      assert.equal(await readFile(file, 'utf8'), content, name)
      assert.deepEqual(await readdir(dir), ['queue.json'], name)
    }
  }
})

test('project_id and project_key stay legal as long as only one of them is set', async () => {
  const dir = await tmp()
  const withKey = { ...legacyEvent(1), project_key: 'tokenpivot' }
  const withId = { ...legacyEvent(2), project_id: '11111111-1111-4111-8111-111111111111' }
  const withNeither = { ...legacyEvent(3), project_id: null }
  const file = await writeQueue(dir, { format: 1, items: [withKey, withId, withNeither] })
  const result = run(dir, ['--apply'])
  assert.equal(result.status, 0)
  assert.equal(parse(result.stdout).converted, 3)
  const items = parse(await readFile(file, 'utf8')).items
  assert.equal(items[0].project_id, null)
  assert.equal(items[0].project_key, 'tokenpivot')
  assert.equal(items[1].project_id, '11111111-1111-4111-8111-111111111111')
  assert.equal(items[1].project_key, undefined)
  assert.equal(items[2].project_id, null)
  assert.equal(items[2].project_key, undefined)
  for (const item of items) {
    assert.equal(item.generation_duration_ms, null)
    assert.equal(item.ttft_ms, null)
  }
})

test('a current event keeps estimated and zero values while gaining the two keys', async () => {
  const dir = await tmp()
  const event = {
    ...legacyEvent(1),
    duration_ms: 0,
    usage: { ...legacyEvent(1).usage, total_tokens: { value: 0, quality: 'estimated' } },
  }
  const file = await writeQueue(dir, { format: 1, items: [event] })
  const result = run(dir, ['--apply'])
  assert.equal(result.status, 0)
  assert.equal(parse(result.stdout).converted, 1)
  const after = parse(await readFile(file, 'utf8')).items[0]
  assert.equal(after.generation_duration_ms, null)
  assert.equal(after.ttft_ms, null)
  assert.deepEqual(after.usage.total_tokens, { value: 0, quality: 'estimated' })
  assert.equal(after.duration_ms, 0)
})
