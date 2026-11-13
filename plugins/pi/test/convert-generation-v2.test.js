import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { copyFile, mkdtemp, readdir, readFile, writeFile } from 'node:fs/promises'
import { DatabaseSync } from 'node:sqlite'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { SqliteQueue } from '../lib/queue.js'

const SCRIPT = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'scripts', 'convert-generation-v2.js')

const tmp = () => mkdtemp(path.join(os.tmpdir(), 'tp-pi-convert-'))

/** 旧格式事件：没有两个计时键。 */
const legacyEvent = (n) => ({
  source_event_id: `pi:turn:${n}`,
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

const dbPathOf = (dir) => path.join(dir, 'queue.sqlite')

/** 用现行队列建一个真实 fixture，然后关闭，供脚本离线处理。 */
async function makeQueue(events) {
  const dir = await tmp()
  const queue = new SqliteQueue({ dir, now: () => 1763000000000 })
  queue.open()
  for (const event of events) {
    const result = queue.enqueue({ event, serverUrl: 'https://tokenpivot.example.com', fingerprint: 'fp' })
    assert.ok(result.state === 'queued' || result.state === 'overflow', result.state)
  }
  queue.close()
  return dir
}

/** 直接把某行改成终态，模拟 dead/overflow 现场。 */
async function setStatus(dir, id, status) {
  const db = new DatabaseSync(dbPathOf(dir))
  db.prepare('UPDATE events SET status = ? WHERE id = ?').run(status, id)
  db.close()
}

/** 读整库与目录现场，用来证明零变化。 */
async function scene(dir) {
  const listing = (await readdir(dir)).sort()
  const bytes = await readFile(dbPathOf(dir))
  return { listing, hex: bytes.toString('hex') }
}

/** 读出全部行，按 rowid 顺序。 */
function rows(dir) {
  const db = new DatabaseSync(dbPathOf(dir), { readOnly: true })
  try {
    return db.prepare('SELECT rowid, id, payload, status, outcome, code, created_at, updated_at FROM events ORDER BY rowid').all()
  } finally {
    db.close()
  }
}

function meta(dir) {
  const db = new DatabaseSync(dbPathOf(dir), { readOnly: true })
  try {
    return db.prepare('SELECT key, value FROM meta ORDER BY key').all()
  } finally {
    db.close()
  }
}

function userVersion(dir) {
  const db = new DatabaseSync(dbPathOf(dir), { readOnly: true })
  try {
    return Number(db.prepare('PRAGMA user_version').get().user_version)
  } finally {
    db.close()
  }
}

function run(dir, args = []) {
  const result = spawnSync(process.execPath, [SCRIPT, '--dir', dir, ...args], { encoding: 'utf8' })
  return { status: result.status, stdout: result.stdout.trim(), stderr: result.stderr.trim() }
}

const parse = (text) => JSON.parse(text)

test('check reports the pending rows that need the two keys and changes no byte', async () => {
  const dir = await makeQueue([legacyEvent(1), legacyEvent(2)])
  await setStatus(dir, 'pi:turn:2', 'dead')
  const before = await scene(dir)
  const result = run(dir)
  assert.equal(result.status, 0)
  const report = parse(result.stdout)
  assert.equal(report.mode, 'check')
  assert.equal(report.pending, 1)
  assert.equal(report.convertible, 1)
  assert.equal(report.converted, 0)
  assert.equal(report.state, 'needs_conversion')
  assert.deepEqual(await scene(dir), before)
})

test('apply only patches pending payloads and leaves every other column alone', async () => {
  const dir = await makeQueue([legacyEvent(1), legacyEvent(2), legacyEvent(3)])
  await setStatus(dir, 'pi:turn:2', 'dead')
  await setStatus(dir, 'pi:turn:3', 'overflow')
  const beforeRows = rows(dir)
  const beforeMeta = meta(dir)
  const beforeVersion = userVersion(dir)
  const beforeScene = await scene(dir)

  const result = run(dir, ['--apply'])
  assert.equal(result.status, 0)
  const report = parse(result.stdout)
  assert.equal(report.mode, 'apply')
  assert.equal(report.pending, 1)
  assert.equal(report.converted, 1)
  assert.equal(report.state, 'converted')
  assert.match(report.backup, /^queue\.sqlite\.bak-/)

  const afterRows = rows(dir)
  assert.deepEqual(afterRows.map((row) => row.rowid), beforeRows.map((row) => row.rowid))
  assert.deepEqual(afterRows.map((row) => row.id), beforeRows.map((row) => row.id))
  assert.deepEqual(afterRows.map((row) => row.status), beforeRows.map((row) => row.status))
  assert.deepEqual(afterRows.map((row) => [row.outcome, row.code, row.created_at, row.updated_at]),
    beforeRows.map((row) => [row.outcome, row.code, row.created_at, row.updated_at]))

  const patched = JSON.parse(afterRows[0].payload)
  assert.equal(patched.generation_duration_ms, null)
  assert.equal(patched.ttft_ms, null)
  const original = JSON.parse(beforeRows[0].payload)
  assert.deepEqual({ ...patched, generation_duration_ms: undefined, ttft_ms: undefined },
    { ...original, generation_duration_ms: undefined, ttft_ms: undefined })

  // dead/overflow 的 payload 一个字节都不动。
  assert.equal(afterRows[1].payload, beforeRows[1].payload)
  assert.equal(afterRows[2].payload, beforeRows[2].payload)
  assert.deepEqual(meta(dir), beforeMeta)
  assert.equal(userVersion(dir), beforeVersion)
  assert.equal(userVersion(dir), 1)

  const backup = await readFile(path.join(dir, report.backup))
  assert.equal(backup.toString('hex'), beforeScene.hex)
})

test('an already converted queue is never rewritten and gains no backup', async () => {
  const dir = await makeQueue([legacyEvent(1)])
  const first = run(dir, ['--apply'])
  assert.equal(parse(first.stdout).converted, 1)
  const afterFirst = await scene(dir)
  const second = run(dir, ['--apply'])
  assert.equal(second.status, 0)
  assert.equal(parse(second.stdout).converted, 0)
  assert.equal(parse(second.stdout).state, 'converted')
  assert.deepEqual(await scene(dir), afterFirst)
  const backups = afterFirst.listing.filter((name) => name.startsWith('queue.sqlite.bak-'))
  assert.equal(backups.length, 1)
})

test('an existing rollback journal stops the run before any writable open', async () => {
  const dir = await makeQueue([legacyEvent(1)])
  const journal = path.join(dir, 'queue.sqlite-journal')
  await writeFile(journal, Buffer.from('hot journal placeholder'))
  const before = await scene(dir)
  const journalBefore = await readFile(journal)
  for (const args of [[], ['--apply']]) {
    const result = run(dir, args)
    assert.equal(result.status, 1)
    assert.equal(parse(result.stderr).state, 'journal_present')
    assert.deepEqual(await scene(dir), before)
    assert.deepEqual(await readFile(journal), journalBefore)
    assert.deepEqual((await readdir(dir)).sort(), before.listing)
  }
})

test('a WAL side file is refused as an unsupported journal mode', async () => {
  const dir = await makeQueue([legacyEvent(1)])
  await writeFile(path.join(dir, 'queue.sqlite-wal'), Buffer.from('wal placeholder'))
  const result = run(dir, ['--apply'])
  assert.equal(result.status, 1)
  assert.equal(parse(result.stderr).state, 'unexpected_wal')
  assert.deepEqual(rows(dir).map((row) => row.status), ['pending'])
})

test('a database that is not in DELETE journal mode is handed to a human', async () => {
  const dir = await makeQueue([legacyEvent(1)])
  const db = new DatabaseSync(dbPathOf(dir))
  db.exec('PRAGMA journal_mode = WAL')
  db.close()
  const result = run(dir)
  assert.equal(result.status, 1)
  assert.ok(['unexpected_journal_mode', 'unexpected_wal'].includes(parse(result.stderr).state))
})

test('a foreign user_version or a missing table stops the run', async () => {
  const versionDir = await makeQueue([legacyEvent(1)])
  const versionDb = new DatabaseSync(dbPathOf(versionDir))
  versionDb.exec('PRAGMA user_version = 2')
  versionDb.close()
  const versionResult = run(versionDir, ['--apply'])
  assert.equal(versionResult.status, 1)
  assert.equal(parse(versionResult.stderr).state, 'legacy_state')

  const tableDir = await tmp()
  const tableDb = new DatabaseSync(dbPathOf(tableDir))
  tableDb.exec('PRAGMA journal_mode = DELETE')
  tableDb.exec('PRAGMA user_version = 1')
  tableDb.exec('CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)')
  tableDb.close()
  const tableResult = run(tableDir, ['--apply'])
  assert.equal(tableResult.status, 1)
  assert.match(parse(tableResult.stderr).error, /events/)
})

test('a missing database reports zero and creates no directory', async () => {
  const dir = path.join(await tmp(), 'absent')
  const result = run(dir, ['--apply'])
  assert.equal(result.status, 0)
  assert.equal(parse(result.stdout).state, 'no_queue')
  assert.equal(parse(result.stdout).converted, 0)
  await assert.rejects(readdir(dir), { code: 'ENOENT' })
})

test('a corrupt payload or an illegal timing stops the whole run without writing', async () => {
  const cases = [
    ['not json', '{not json'],
    ['not an object', '[1,2,3]'],
    ['identity mismatch', JSON.stringify({ ...legacyEvent(1), source_event_id: 'pi:turn:other' })],
  ]
  for (const [name, payload] of cases) {
    const dir = await makeQueue([legacyEvent(1), legacyEvent(2)])
    const db = new DatabaseSync(dbPathOf(dir))
    db.prepare('UPDATE events SET payload = ? WHERE id = ?').run(payload, 'pi:turn:2')
    db.close()
    const before = await scene(dir)
    const result = run(dir, ['--apply'])
    assert.equal(result.status, 1, name)
    assert.equal(parse(result.stderr).state, 'failed', name)
    assert.deepEqual(await scene(dir), before, name)
    assert.deepEqual((await readdir(dir)).sort(), before.listing, name)
  }
  for (const [name, value] of [
    ['negative', -1],
    ['fractional', 1.5],
    ['beyond the safe integer range', Number.MAX_SAFE_INTEGER + 2],
    ['string', '1800'],
    ['object', { value: 1800 }],
  ]) {
    const dir = await makeQueue([legacyEvent(1), legacyEvent(2)])
    const payload = JSON.stringify({ ...legacyEvent(2), ttft_ms: value })
    const db = new DatabaseSync(dbPathOf(dir))
    db.prepare('UPDATE events SET payload = ? WHERE id = ?').run(payload, 'pi:turn:2')
    db.close()
    const before = await scene(dir)
    const result = run(dir, ['--apply'])
    assert.equal(result.status, 1, name)
    assert.match(parse(result.stderr).error, /ttft_ms/, name)
    assert.deepEqual(await scene(dir), before, name)
  }
})

test('a valid existing timing is kept as is and never derived from duration', async () => {
  const dir = await makeQueue([legacyEvent(1)])
  const payload = JSON.stringify({ ...legacyEvent(1), generation_duration_ms: 1800, ttft_ms: 0 })
  const db = new DatabaseSync(dbPathOf(dir))
  db.prepare('UPDATE events SET payload = ? WHERE id = ?').run(payload, 'pi:turn:1')
  db.close()
  const result = run(dir, ['--apply'])
  assert.equal(result.status, 0)
  assert.equal(parse(result.stdout).converted, 0)
  const stored = JSON.parse(rows(dir)[0].payload)
  assert.equal(stored.generation_duration_ms, 1800)
  assert.equal(stored.ttft_ms, 0)
})

test('the backup file is a real copy that can be restored', async () => {
  const dir = await makeQueue([legacyEvent(1)])
  const original = await readFile(dbPathOf(dir))
  const result = run(dir, ['--apply'])
  const backup = path.join(dir, parse(result.stdout).backup)
  await copyFile(backup, path.join(dir, 'restored.sqlite'))
  assert.deepEqual(await readFile(path.join(dir, 'restored.sqlite')), original)
})

test('the help text and an unknown argument are handled without touching the database', async () => {
  const dir = await makeQueue([legacyEvent(1)])
  const before = await scene(dir)
  const help = spawnSync(process.execPath, [SCRIPT, '--help'], { encoding: 'utf8' })
  assert.equal(help.status, 0)
  assert.match(help.stdout, /--apply/)
  const bad = run(dir, ['--force'])
  assert.equal(bad.status, 1)
  assert.deepEqual(await scene(dir), before)
})

test('an empty rollback journal also stops the run before any writable open', async () => {
  const dir = await makeQueue([legacyEvent(1)])
  const journal = path.join(dir, 'queue.sqlite-journal')
  await writeFile(journal, Buffer.alloc(0))
  const before = await scene(dir)
  for (const args of [[], ['--apply']]) {
    const result = run(dir, args)
    assert.equal(result.status, 1)
    assert.equal(parse(result.stderr).state, 'journal_present')
    assert.deepEqual(await scene(dir), before)
    assert.equal((await readFile(journal)).length, 0)
  }
})

test('an event that is not a valid current event stops check and apply without writing', async () => {
  const base = legacyEvent(2)
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
    const dir = await makeQueue([legacyEvent(1), legacyEvent(2)])
    // 好坏混合：第一行合法，第二行结构损坏，整体必须中止。
    const id = typeof bad.source_event_id === 'string' ? bad.source_event_id : 'pi:turn:2'
    const db = new DatabaseSync(dbPathOf(dir))
    db.prepare('UPDATE events SET id = ?, payload = ? WHERE id = ?').run(id, JSON.stringify(bad), 'pi:turn:2')
    db.close()
    const before = await scene(dir)
    for (const args of [[], ['--apply']]) {
      const result = run(dir, args)
      assert.equal(result.status, 1, name)
      assert.equal(parse(result.stderr).state, 'failed', name)
      assert.deepEqual(await scene(dir), before, name)
      assert.deepEqual((await readdir(dir)).sort(), before.listing, name)
    }
  }
})

test('project_id and project_key stay legal as long as only one of them is set', async () => {
  const withKey = { ...legacyEvent(1), project_key: 'tokenpivot' }
  const withId = { ...legacyEvent(2), project_id: '11111111-1111-4111-8111-111111111111' }
  const withNeither = { ...legacyEvent(3), project_id: null }
  const dir = await makeQueue([withKey, withId, withNeither])
  const result = run(dir, ['--apply'])
  assert.equal(result.status, 0)
  assert.equal(parse(result.stdout).converted, 3)
  const converted = rows(dir).map((row) => JSON.parse(row.payload))
  assert.equal(converted[0].project_id, null)
  assert.equal(converted[0].project_key, 'tokenpivot')
  assert.equal(converted[1].project_id, '11111111-1111-4111-8111-111111111111')
  assert.equal(converted[1].project_key, undefined)
  assert.equal(converted[2].project_id, null)
  assert.equal(converted[2].project_key, undefined)
  for (const payload of converted) {
    assert.equal(payload.generation_duration_ms, null)
    assert.equal(payload.ttft_ms, null)
  }
})

test('a current event keeps estimated and zero values while gaining the two keys', async () => {
  const dir = await makeQueue([legacyEvent(1)])
  const payload = JSON.stringify({
    ...legacyEvent(1),
    duration_ms: 0,
    usage: { ...legacyEvent(1).usage, total_tokens: { value: 0, quality: 'estimated' } },
  })
  const db = new DatabaseSync(dbPathOf(dir))
  db.prepare('UPDATE events SET payload = ? WHERE id = ?').run(payload, 'pi:turn:1')
  db.close()
  const result = run(dir, ['--apply'])
  assert.equal(result.status, 0)
  assert.equal(parse(result.stdout).converted, 1)
  const stored = JSON.parse(rows(dir)[0].payload)
  assert.equal(stored.generation_duration_ms, null)
  assert.equal(stored.ttft_ms, null)
  assert.deepEqual(stored.usage.total_tokens, { value: 0, quality: 'estimated' })
  assert.equal(stored.duration_ms, 0)
})
