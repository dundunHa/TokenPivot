/**
 * 队列验收（计划 §5、§9.3）：SQLite 短事务、不可变事件、绑定与凭证轮换、
 * 并发写、双 uploader 只删确认 ID、SIGSTOP 持锁仍 BUSY、提交前后 SIGKILL 恢复、
 * 损坏与旧格式保留、死信/overflow 原子化、无明文 token。
 *
 * 需要 Node >= 22.19 的内置 node:sqlite。测试只用临时目录，不连真实服务。
 */
import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { DatabaseSync } from 'node:sqlite'
import { SqliteQueue, QueueError, QUEUE_FILE } from '../lib/queue.js'

const QUEUE_URL = new URL('../lib/queue.js', import.meta.url).href
const SERVER_A = 'https://tokenpivot.example.com/api/v1/ingestion/events'
const SERVER_B = 'https://other.example.com/api/v1/ingestion/events'
const TOKEN = 'tpk_do_not_leak_7d13f9'
const FP_A = 'a'.repeat(64)
const FP_B = 'b'.repeat(64)
const FP_C = 'c'.repeat(64)

function tempDir(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tokenpivot-pi-queue-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  return dir
}

function openQueue(t, options = {}) {
  const dir = options.dir ?? tempDir(t)
  const queue = new SqliteQueue({ dir, maxItems: options.maxItems ?? 10000 })
  queue.open()
  t.after(() => queue.close())
  return { queue, dir }
}

function event(id, extra = {}) {
  return {
    source_event_id: id,
    provider: 'anthropic',
    model: 'claude-x',
    occurred_at: '2026-11-05T00:00:01.200Z',
    usage: { input_tokens: { value: 100, quality: 'known' } },
    status: null,
    duration_ms: 1200,
    ...extra,
  }
}

/** 启动一个子进程并等待它输出指定标记行。 */
function spawnAndWait(script, args, marker) {
  const child = spawn(process.execPath, ['--input-type=module', '-e', script, ...args], {
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  const ready = new Promise((resolve, reject) => {
    let buffer = ''
    let errors = ''
    child.stderr.on('data', (chunk) => { errors += chunk.toString() })
    const timer = setTimeout(() => reject(new Error(`child did not reach ${marker}`)), 15000)
    child.stdout.on('data', (chunk) => {
      buffer += chunk.toString()
      if (buffer.includes(marker)) {
        clearTimeout(timer)
        resolve()
      }
    })
    child.on('error', (error) => {
      clearTimeout(timer)
      reject(error)
    })
    child.on('exit', (code) => {
      if (!buffer.includes(marker)) {
        clearTimeout(timer)
        reject(new Error(`child exited early (code ${code}): ${errors}`))
      }
    })
  })
  return { child, ready }
}

function waitExit(child) {
  return new Promise((resolve) => {
    if (child.exitCode !== null) {
      resolve(child.exitCode)
      return
    }
    child.on('exit', (code, signal) => resolve(code ?? signal))
  })
}

test('事件写入后不可变：同 ID 同内容 duplicate，不同内容 conflict', (t) => {
  const { queue } = openQueue(t)
  assert.deepEqual(queue.enqueue({ event: event('e1'), serverUrl: SERVER_A, fingerprint: FP_A }), { state: 'queued' })
  assert.deepEqual(queue.enqueue({ event: event('e1'), serverUrl: SERVER_A, fingerprint: FP_A }), { state: 'duplicate' })

  const changed = event('e1', { model: 'claude-y' })
  assert.deepEqual(queue.enqueue({ event: changed, serverUrl: SERVER_A, fingerprint: FP_A }), { state: 'conflict' })

  const rows = queue.peek(10).items
  assert.equal(rows.length, 1)
  assert.equal(JSON.parse(rows[0].payloadText).model, 'claude-x')
})

test('空库首次有效配置在事务内绑定，revision 从 1 开始', (t) => {
  const { queue } = openQueue(t)
  assert.equal(queue.binding(), null)
  queue.enqueue({ event: event('e1'), serverUrl: SERVER_A, fingerprint: FP_A })
  assert.deepEqual(queue.binding(), { serverUrl: SERVER_A, fingerprint: FP_A, revision: 1 })
})

test('绑定缺失但库里已有事件：暂停而不是重新绑定', (t) => {
  const { queue, dir } = openQueue(t)
  const raw = new DatabaseSync(path.join(dir, QUEUE_FILE), { timeout: 0 })
  t.after(() => raw.close())
  raw.prepare(
    "INSERT INTO events (id, payload, status, created_at, updated_at) VALUES ('legacy', '{}', 'pending', 0, 0)",
  ).run()

  const result = queue.enqueue({ event: event('e2'), serverUrl: SERVER_A, fingerprint: FP_A })
  assert.deepEqual(result, { state: 'paused', reason: 'binding_missing' })
  assert.equal(queue.binding(), null)
})

test('服务地址变化必须新 stateDir：暂停且不覆盖绑定', (t) => {
  const { queue } = openQueue(t)
  queue.enqueue({ event: event('e1'), serverUrl: SERVER_A, fingerprint: FP_A })
  const result = queue.enqueue({ event: event('e2'), serverUrl: SERVER_B, fingerprint: FP_A })
  assert.deepEqual(result, { state: 'paused', reason: 'server_changed' })
  assert.equal(queue.binding().serverUrl, SERVER_A)
  assert.equal(queue.counts().pending, 1)
})

test('token 指纹变化暂停；确认后 A→B 生效，B→C 仍需再次确认（CAS）', (t) => {
  const { queue } = openQueue(t)
  queue.enqueue({ event: event('e1'), serverUrl: SERVER_A, fingerprint: FP_A })

  assert.deepEqual(
    queue.enqueue({ event: event('e2'), serverUrl: SERVER_A, fingerprint: FP_B }),
    { state: 'paused', reason: 'token_changed' },
  )
  assert.equal(queue.binding().fingerprint, FP_A)

  const stale = queue.confirmSameSource({
    serverUrl: SERVER_A, observedFingerprint: FP_A, observedRevision: 9, newFingerprint: FP_B,
  })
  assert.deepEqual(stale, { ok: false, reason: 'stale_binding' })

  const confirmed = queue.confirmSameSource({
    serverUrl: SERVER_A, observedFingerprint: FP_A, observedRevision: 1, newFingerprint: FP_B,
  })
  assert.deepEqual(confirmed, { ok: true, revision: 2 })
  assert.deepEqual(queue.binding(), { serverUrl: SERVER_A, fingerprint: FP_B, revision: 2 })
  assert.deepEqual(
    queue.enqueue({ event: event('e2'), serverUrl: SERVER_A, fingerprint: FP_B }),
    { state: 'queued' },
  )

  assert.deepEqual(
    queue.enqueue({ event: event('e3'), serverUrl: SERVER_A, fingerprint: FP_C }),
    { state: 'paused', reason: 'token_changed' },
  )
  // 用上一次观察到的 B/rev2 再确认一次，C 才被接受。
  assert.deepEqual(
    queue.confirmSameSource({ serverUrl: SERVER_A, observedFingerprint: FP_B, observedRevision: 2, newFingerprint: FP_C }),
    { ok: true, revision: 3 },
  )
  assert.deepEqual(
    queue.enqueue({ event: event('e3'), serverUrl: SERVER_A, fingerprint: FP_C }),
    { state: 'queued' },
  )
})

test('并发条件更新只一方成功（同一旧指纹 + revision 第二次必失败）', (t) => {
  const { queue } = openQueue(t)
  queue.enqueue({ event: event('e1'), serverUrl: SERVER_A, fingerprint: FP_A })
  const first = queue.confirmSameSource({
    serverUrl: SERVER_A, observedFingerprint: FP_A, observedRevision: 1, newFingerprint: FP_B,
  })
  const second = queue.confirmSameSource({
    serverUrl: SERVER_A, observedFingerprint: FP_A, observedRevision: 1, newFingerprint: FP_C,
  })
  assert.equal(first.ok, true)
  assert.deepEqual(second, { ok: false, reason: 'stale_binding' })
  assert.equal(queue.binding().fingerprint, FP_B)
})

test('服务地址不匹配时确认被拒绝，不能借确认覆盖绑定', (t) => {
  const { queue } = openQueue(t)
  queue.enqueue({ event: event('e1'), serverUrl: SERVER_A, fingerprint: FP_A })
  assert.deepEqual(
    queue.confirmSameSource({ serverUrl: SERVER_B, observedFingerprint: FP_A, observedRevision: 1, newFingerprint: FP_B }),
    { ok: false, reason: 'server_changed' },
  )
  assert.equal(queue.binding().serverUrl, SERVER_A)
})

test('首次拿到凭证：空指纹绑定不算轮换，非空指纹不得被静默覆盖', (t) => {
  const { queue } = openQueue(t)
  queue.enqueue({ event: event('e1'), serverUrl: SERVER_A, fingerprint: null })
  assert.deepEqual(queue.binding(), { serverUrl: SERVER_A, fingerprint: null, revision: 1 })
  assert.deepEqual(
    queue.bindFirstCredential({ serverUrl: SERVER_A, fingerprint: FP_A }),
    { ok: true, revision: 2 },
  )
  assert.deepEqual(
    queue.bindFirstCredential({ serverUrl: SERVER_A, fingerprint: FP_B }),
    { ok: false, reason: 'stale_binding' },
  )
  assert.equal(queue.binding().fingerprint, FP_A)
})

test('capacity 只限制 pending，超额转 overflow 并计数，不静默删除', (t) => {
  const { queue } = openQueue(t, { maxItems: 2 })
  assert.equal(queue.enqueue({ event: event('a'), serverUrl: SERVER_A, fingerprint: FP_A }).state, 'queued')
  assert.equal(queue.enqueue({ event: event('b'), serverUrl: SERVER_A, fingerprint: FP_A }).state, 'queued')
  assert.equal(queue.enqueue({ event: event('c'), serverUrl: SERVER_A, fingerprint: FP_A }).state, 'overflow')
  assert.equal(queue.enqueue({ event: event('d'), serverUrl: SERVER_A, fingerprint: FP_A }).state, 'overflow')
  assert.deepEqual(queue.counts(), { pending: 2, dead: 0, overflow: 2 })
  assert.deepEqual(queue.peek(10).items.map((i) => i.id), ['a', 'b'])
})

test('死信与确认出队原子生效：只删已确认 ID，死信保留原记录', (t) => {
  const { queue } = openQueue(t)
  for (let i = 0; i < 6; i += 1) {
    queue.enqueue({ event: event(`e${i}`), serverUrl: SERVER_A, fingerprint: FP_A })
  }
  const applied = queue.applyOutcome({
    remove: ['e0', 'e1'],
    dead: [{ id: 'e2', outcome: 'conflict', code: 'USAGE_UNSUPPORTED' }],
  })
  assert.deepEqual(applied, { removed: 2, dead: 1 })
  assert.deepEqual(queue.counts(), { pending: 3, dead: 1, overflow: 0 })
  assert.deepEqual(queue.peek(10).items.map((i) => i.id), ['e3', 'e4', 'e5'])
  // 再次应用同一条死信不会改变状态
  assert.deepEqual(queue.applyOutcome({ dead: [{ id: 'e2', outcome: 'conflict', code: null }] }), { removed: 0, dead: 1 })
  assert.equal(queue.counts().dead, 1)
})

test('两写者并发入队不丢事件（两个进程 + barrier 同步起跑）', async (t) => {
  const dir = tempDir(t)
  const go = path.join(dir, 'go')
  const script = `
import fs from 'node:fs'
import { SqliteQueue } from ${JSON.stringify(QUEUE_URL)}
const [dir, prefix, go] = process.argv.slice(1)
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
let queue = null
for (let attempt = 0; attempt < 100; attempt += 1) {
  try {
    queue = new SqliteQueue({ dir })
    queue.open()
    break
  } catch (error) {
    queue = null
    if (attempt === 99) { process.stdout.write('open-failed\\n'); process.exit(5) }
    await sleep(5)
  }
}
process.stdout.write('ready\\n')
for (let spin = 0; !fs.existsSync(go); spin += 1) {
  if (spin > 5000) { process.stdout.write('barrier-timeout\\n'); process.exit(3) }
  await sleep(2)
}
const event = (id) => ({
  source_event_id: id,
  provider: 'anthropic',
  model: 'claude-x',
  occurred_at: '2026-11-05T00:00:01.200Z',
  usage: { input_tokens: { value: 100, quality: 'known' } },
  status: null,
  duration_ms: 1200,
})
for (let i = 0; i < 20;) {
  const result = queue.enqueue({ event: event(prefix + i), serverUrl: ${JSON.stringify(SERVER_A)}, fingerprint: ${JSON.stringify(FP_A)} })
  if (result.busy) { await sleep(2); continue }
  if (result.state === 'paused' || result.state === 'failed') {
    process.stdout.write('failed ' + result.state + '\\n')
    process.exit(4)
  }
  i += 1
}
queue.close()
process.stdout.write('done 20\\n')
`
  // 先把 schema 建好：两个进程同时首次 open 会在 PRAGMA journal_mode 上抢写锁。
  new SqliteQueue({ dir }).open().close()

  const a = spawnAndWait(script, [dir, 'a', go], 'ready')
  const b = spawnAndWait(script, [dir, 'b', go], 'ready')
  let outA = ''
  let outB = ''
  a.child.stdout.on('data', (chunk) => { outA += chunk })
  b.child.stdout.on('data', (chunk) => { outB += chunk })

  // barrier：两个进程都打开数据库并停在起跑线上，再一起放行。
  await Promise.all([a.ready, b.ready])
  const exits = Promise.all([once(a.child, 'close'), once(b.child, 'close')])
  fs.writeFileSync(go, 'go')
  const codes = await exits
  assert.deepEqual(codes.map((entry) => entry[0]), [0, 0])
  assert.match(outA, /done 20/)
  assert.match(outB, /done 20/)

  const queue = new SqliteQueue({ dir }).open()
  t.after(() => queue.close())
  assert.deepEqual(queue.counts(), { pending: 40, dead: 0, overflow: 0 })
  const ids = queue.peek(100).items.map((item) => item.id)
  assert.equal(ids.filter((id) => id.startsWith('a')).length, 20)
  assert.equal(ids.filter((id) => id.startsWith('b')).length, 20)
})

test('双 uploader：各自按快照删自己的确认 ID，不删对方未确认的事件', (t) => {
  const { queue } = openQueue(t)
  for (let i = 0; i < 10; i += 1) {
    queue.enqueue({ event: event(`e${i}`), serverUrl: SERVER_A, fingerprint: FP_A })
  }
  const uploaderA = queue.peek(100).items.map((i) => i.id)
  const uploaderB = queue.peek(100).items.map((i) => i.id)

  queue.applyOutcome({ remove: uploaderA.slice(0, 5) })
  queue.applyOutcome({ remove: uploaderB.slice(5, 8) })
  // B 再删一次已被 A 删掉的 ID：静默忽略，不报错。
  queue.applyOutcome({ remove: uploaderA.slice(0, 2) })

  // A 删 e0–e4，B 删 e5–e7 中它自己确认的 e5–e7，剩下 e8、e9 仍待上报。
  assert.deepEqual(queue.peek(10).items.map((i) => i.id), ['e8', 'e9'])
})

test('入队与出队交错：另一个连接的确认不误删新入队事件', (t) => {
  const dir = tempDir(t)
  const writer = new SqliteQueue({ dir }).open()
  const drainer = new SqliteQueue({ dir }).open()
  t.after(() => {
    writer.close()
    drainer.close()
  })

  for (let i = 0; i < 10; i += 1) {
    writer.enqueue({ event: event(`e${i}`), serverUrl: SERVER_A, fingerprint: FP_A })
    if (i % 2 === 0) drainer.applyOutcome({ remove: [`e${i}`] })
  }
  assert.deepEqual(drainer.peek(100).items.map((i) => i.id), ['e1', 'e3', 'e5', 'e7', 'e9'])
  assert.equal(writer.counts().pending, 5)
})

test('journal_mode=DELETE、synchronous=FULL、目录 0700、数据库 0600', (t) => {
  const { queue, dir } = openQueue(t)
  assert.equal(queue.db.prepare('PRAGMA journal_mode').get().journal_mode, 'delete')
  assert.equal(queue.db.prepare('PRAGMA synchronous').get().synchronous, 2)
  assert.equal(fs.statSync(dir).mode & 0o777, 0o700)
  assert.equal(fs.statSync(path.join(dir, QUEUE_FILE)).mode & 0o777, 0o600)
})

test('持锁进程被 SIGSTOP 后仍然 BUSY：没有按锁龄抢锁，异步退避交给调用方', async (t) => {
  const { queue, dir } = openQueue(t)
  const holdMs = Number(process.env.TOKENPIVOT_PI_SLOW_HOLD_MS ?? 1500)

  const child = spawnAndWait(
    `import { DatabaseSync } from 'node:sqlite'
     const db = new DatabaseSync(process.argv[1], { timeout: 0 })
     db.exec('BEGIN IMMEDIATE')
     db.prepare("INSERT INTO events (id, payload, status, created_at, updated_at) VALUES ('locked', '{}', 'pending', 0, 0)").run()
     console.log('locked')
     setInterval(() => {}, 1000)`,
    [path.join(dir, QUEUE_FILE)],
    'locked',
  )
  await child.ready

  child.child.kill('SIGSTOP')
  try {
    const first = queue.enqueue({ event: event('x1'), serverUrl: SERVER_A, fingerprint: FP_A })
    assert.equal(first.state, 'busy')
    await new Promise((resolve) => setTimeout(resolve, holdMs))
    const second = queue.enqueue({ event: event('x2'), serverUrl: SERVER_A, fingerprint: FP_A })
    assert.equal(second.state, 'busy')
    assert.equal(queue.counts().pending, 0)
  } finally {
    child.child.kill('SIGCONT')
    child.child.kill('SIGKILL')
    await waitExit(child.child)
  }
})

test('提交后 SIGKILL：已提交事件保留；未提交事务回滚且完整性 ok', async (t) => {
  const dir = tempDir(t)

  const committed = spawnAndWait(
    `import { SqliteQueue } from ${JSON.stringify(QUEUE_URL)}
     const queue = new SqliteQueue({ dir: process.argv[1] }).open()
     for (let i = 0; i < 5; i += 1) {
       const r = queue.enqueue({ event: { source_event_id: 'k' + i, n: i }, serverUrl: ${JSON.stringify(SERVER_A)}, fingerprint: ${JSON.stringify(FP_A)} })
       if (r.state !== 'queued') { console.error('bad', r); process.exit(2) }
     }
     console.log('done')
     setInterval(() => {}, 1000)`,
    [dir],
    'done',
  )
  await committed.ready
  committed.child.kill('SIGKILL')
  await waitExit(committed.child)

  const queue = new SqliteQueue({ dir }).open()
  t.after(() => queue.close())
  assert.equal(queue.counts().pending, 5)
  assert.deepEqual(queue.peek(10).items.map((i) => i.id), ['k0', 'k1', 'k2', 'k3', 'k4'])

  const uncommitted = spawnAndWait(
    `import { DatabaseSync } from 'node:sqlite'
     const db = new DatabaseSync(process.argv[1], { timeout: 0 })
     db.exec('BEGIN IMMEDIATE')
     db.prepare("INSERT INTO events (id, payload, status, created_at, updated_at) VALUES ('ghost', '{}', 'pending', 0, 0)").run()
     console.log('uncommitted')
     setInterval(() => {}, 1000)`,
    [path.join(dir, QUEUE_FILE)],
    'uncommitted',
  )
  await uncommitted.ready
  uncommitted.child.kill('SIGKILL')
  await waitExit(uncommitted.child)

  const reopened = new SqliteQueue({ dir }).open()
  t.after(() => reopened.close())
  assert.deepEqual(reopened.peek(100).items.map((i) => i.id), ['k0', 'k1', 'k2', 'k3', 'k4'])
  assert.equal(reopened.db.prepare('PRAGMA integrity_check').get().integrity_check, 'ok')
})

test('数据库损坏：保留原文件并暂停，不覆盖重建', (t) => {
  const dir = tempDir(t)
  const file = path.join(dir, QUEUE_FILE)
  const garbage = Buffer.from('this is not a sqlite database at all'.repeat(40))
  fs.writeFileSync(file, garbage)
  const queue = new SqliteQueue({ dir })
  assert.throws(() => queue.open(), (error) => error instanceof QueueError && error.code === 'queue_corrupt')
  assert.deepEqual(fs.readFileSync(file), garbage)
})

test('发现非空旧格式状态：暂停并报告需要显式迁移', (t) => {
  const dir = tempDir(t)
  fs.writeFileSync(path.join(dir, 'queue.json'), JSON.stringify({ format: 1, items: [{ source_event_id: 'old' }] }))
  const queue = new SqliteQueue({ dir })
  assert.throws(() => queue.open(), (error) => error instanceof QueueError && error.code === 'legacy_state')
})

test('软链 stateDir 与数据库：拒绝打开，不跟随后改别人文件的权限', (t) => {
  const base = tempDir(t)
  const real = path.join(base, 'real')
  const link = path.join(base, 'link')
  fs.mkdirSync(real)
  fs.symlinkSync(real, link)
  const queue = new SqliteQueue({ dir: link })
  assert.throws(() => queue.open(), (error) => error instanceof QueueError && error.code === 'state_dir_symlink')
})

test('队列里没有明文 token（pending/dead/overflow 三态都检查 DB 与 journal 字节）', (t) => {
  const { queue, dir } = openQueue(t, { maxItems: 1 })
  // e1 → pending；e2 → overflow；e1 转 dead；e3 再次 pending
  queue.enqueue({ event: event('e1'), serverUrl: SERVER_A, fingerprint: FP_A })
  queue.enqueue({ event: event('e2'), serverUrl: SERVER_A, fingerprint: FP_A })
  queue.applyOutcome({ dead: [{ id: 'e1', outcome: 'conflict', code: 'USAGE_UNSUPPORTED' }] })
  queue.enqueue({ event: event('e3'), serverUrl: SERVER_A, fingerprint: FP_A })
  assert.deepEqual(queue.counts(), { pending: 1, dead: 1, overflow: 1 })
  queue.close()

  for (const name of [QUEUE_FILE, `${QUEUE_FILE}-journal`]) {
    const file = path.join(dir, name)
    if (!fs.existsSync(file)) continue
    const bytes = fs.readFileSync(file)
    assert.equal(bytes.includes(Buffer.from(TOKEN, 'utf8')), false, `${name} 不得包含明文 token`)
    assert.equal(bytes.includes(Buffer.from('PRIVATE', 'utf8')), false, `${name} 不得包含标记`)
  }

  const binding = new SqliteQueue({ dir })
  binding.open()
  const stored = binding.binding()
  const rows = binding.peek(10)
  binding.close()
  assert.match(stored.fingerprint, /^[a-f0-9]{64}$/)
  assert.deepEqual(rows.items.map((item) => item.id), ['e3'])
  // payload 只含事件字段，不夹带 token 或额外键
  assert.deepEqual(Object.keys(JSON.parse(rows.items[0].payloadText)).sort(), [
    'duration_ms', 'model', 'occurred_at', 'provider', 'source_event_id', 'status', 'usage',
  ])
})
