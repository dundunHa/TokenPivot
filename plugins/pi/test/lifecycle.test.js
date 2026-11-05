/**
 * 生命周期验收（计划 §3、§7、§9.2/§9.5/§9.6）：采集→入队→上报、跳过计数、
 * 压缩只计数、凭证轮换暂停与确认后补报、无新事件也能恢复、服务地址变化、
 * 绑定缺失暂停、响应丢失后同 ID 重传、退出限时且不清空未确认事件、隐私断言。
 */
import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { DatabaseSync } from 'node:sqlite'
import { Collector, createLogger } from '../lib/collector.js'
import { loadConfig, tokenFingerprint, normalizeServerUrl } from '../lib/config.js'
import { SqliteQueue, QUEUE_FILE } from '../lib/queue.js'

const TOKEN_A = 'tpk_token_a_0123456789'
const TOKEN_B = 'tpk_token_b_9876543210'
const PROJECT = '0199a0b0-0000-7000-8000-00000000000a'
const START = Date.parse('2026-11-05T00:00:00.000Z')
const SERVER = 'https://tokenpivot.example.com/api/v1/ingestion/events'

function workspace(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'tokenpivot-pi-life-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))
  return { root, stateDir: path.join(root, 'state'), configPath: path.join(root, 'config.json') }
}

function writeConfig(ws, over = {}) {
  const body = {
    serverUrl: 'https://tokenpivot.example.com',
    token: TOKEN_A,
    stateDir: ws.stateDir,
    projects: [{ path: '/work/project-a', projectId: PROJECT }],
    ...over,
  }
  fs.writeFileSync(ws.configPath, JSON.stringify(body))
  fs.chmodSync(ws.configPath, 0o600)
  return ws.configPath
}

function idleTimers() {
  return {
    setTimer: () => ({ unref() {} }),
    clearTimer: () => {},
    setInterval: () => ({ unref() {} }),
    clearInterval: () => {},
  }
}

function message(over = {}) {
  return {
    role: 'assistant',
    provider: 'anthropic',
    model: 'claude-x',
    usage: { input: 100, output: 20, cacheRead: 0, cacheWrite: 0, totalTokens: 120 },
    stopReason: 'stop',
    timestamp: START,
    durationMs: 1200,
    ...over,
  }
}

function turn(over = {}, entryId = 'entry-1') {
  return {
    type: 'turn_end',
    turnIndex: 0,
    message: message(over),
    messageEntryId: entryId,
    toolResults: [],
    entries: [],
    continue: false,
    context: {},
    outcome: 'completed',
  }
}

function context(sessionId = 'session-1', cwd = '/work/project-a/sub') {
  return { cwd, sessionManager: { getSessionId: () => sessionId } }
}

function makeCollector(ws, overrides = {}) {
  const logs = []
  const calls = []
  const collector = new Collector({
    env: { TOKENPIVOT_PI_CONFIG: ws.configPath },
    logger: { log: (code, data) => logs.push({ code, data }) },
    fetch: (url, init) => {
      calls.push({ url, init })
      return overrides.fetch ? overrides.fetch(url, init) : jsonResponse(200, acceptedFor(init))
    },
    configPollMs: 1e9,
    flushDelayMs: 0,
    requestTimeoutMs: 200,
    ...idleTimers(),
    ...overrides.collector,
  })
  return { collector, logs, calls }
}

function jsonResponse(status, body) {
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } })
}

function acceptedFor(init) {
  const body = JSON.parse(init.body)
  return {
    request_id: 'req',
    results: body.events.map((event, index) => ({ index, status: 'accepted', source_event_id: event.source_event_id })),
  }
}

function pendingIds(ws) {
  const queue = new SqliteQueue({ dir: ws.stateDir }).open()
  try {
    return queue.peek(1000).items.map((item) => item.id)
  } finally {
    queue.close()
  }
}

test('配置齐备：turn_end 采集、入队、上报，字段与映射口径一致', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  const { collector, calls } = makeCollector(ws)
  t.after(() => collector.stop())
  await collector.start()
  assert.equal(collector.status().state, 'ok')

  const result = await collector.handleTurnEnd(turn(), context())
  assert.equal(result.state, 'queued')
  assert.equal(collector.counters.queued, 1)

  await collector.runFlush()
  assert.equal(calls.length, 1)
  const body = JSON.parse(calls[0].init.body)
  assert.equal(body.events.length, 1)
  const event = body.events[0]
  assert.equal(event.source_event_id, 'pi:msg:session-1:entry-1')
  assert.equal(event.project_id, PROJECT)
  assert.equal(event.provider, 'anthropic')
  assert.equal(event.model, 'claude-x')
  assert.equal(event.occurred_at, '2026-11-05T00:00:01.200Z')
  assert.deepEqual(event.usage.input_tokens, { value: 100, quality: 'known' })
  assert.deepEqual(event.usage.total_tokens, { value: 120, quality: 'known' })
  assert.equal(event.status, null)
  assert.equal(event.duration_ms, 1200)
  assert.equal(calls[0].init.headers.authorization, `Bearer ${TOKEN_A}`)
  assert.deepEqual(collector.status().counts, { pending: 0, dead: 0, overflow: 0 })
  // 队列清空后绑定必须保留
  assert.equal(collector.queue.binding().serverUrl, SERVER)
})

test('跳过原因计数：非终态、缺耗时、非 assistant、压缩只计数', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  const { collector, calls } = makeCollector(ws)
  t.after(() => collector.stop())
  await collector.start()

  await collector.handleTurnEnd(turn({ stopReason: 'pending' }), context())
  await collector.handleTurnEnd(turn({ stopReason: 'deferred' }), context())
  const withoutDuration = message()
  delete withoutDuration.durationMs
  await collector.handleTurnEnd({ ...turn(), message: withoutDuration }, context())
  await collector.handleTurnEnd({ ...turn(), message: message({ role: 'user' }) }, context())
  collector.handleCompact()
  collector.handleCompact()

  assert.equal(collector.counters.skip_pending, 1)
  assert.equal(collector.counters.skip_deferred, 1)
  assert.equal(collector.counters.skip_no_duration, 1)
  assert.equal(collector.counters.skip_not_assistant, 1)
  assert.equal(collector.counters.compact_skipped, 2)
  assert.equal((await collector.runFlush()).state, 'idle')
  assert.equal(calls.length, 0)
  assert.equal(collector.queue.counts().pending, 0)
})

test('凭证轮换：新 token 下不采集也不发送旧事件；确认后立即补报', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  const first = makeCollector(ws)
  await first.collector.start()
  await first.collector.handleTurnEnd(turn(), context())
  assert.equal(first.collector.queue.counts().pending, 1)
  const frozenPayload = first.collector.queue.peek(10).items[0].payloadText
  await first.collector.stop()
  assert.deepEqual(pendingIds(ws), ['pi:msg:session-1:entry-1'])

  // 换 token（B）重启：绑定仍是 A → 暂停
  writeConfig(ws, { token: TOKEN_B })
  const second = makeCollector(ws)
  t.after(() => second.collector.stop())
  await second.collector.start()
  const queued = await second.collector.handleTurnEnd(turn(), context('session-2', '/work/project-a'))
  assert.deepEqual(queued, { state: 'paused', reason: 'token_changed' })
  assert.equal(second.collector.counters.paused_token_changed, 1)
  assert.equal(second.collector.queue.counts().pending, 1)
  assert.equal((await second.collector.runFlush()).state, 'paused')
  assert.equal(second.calls.length, 0)

  // 用户确认「新凭证仍属原 source」
  const confirmed = await second.collector.confirmSameSource()
  assert.equal(confirmed.ok, true)
  assert.equal(second.collector.queue.binding().fingerprint, tokenFingerprint(TOKEN_B))

  await second.collector.runFlush()
  assert.equal(second.calls.length, 1)
  assert.equal(second.calls[0].init.headers.authorization, `Bearer ${TOKEN_B}`)
  assert.equal(second.collector.queue.counts().pending, 0)
  // 配置/凭证变化不重算已入队事件：发出的字节与入队时冻结的 payload 完全一致。
  const sent = JSON.parse(second.calls[0].init.body).events[0]
  assert.deepEqual(sent, JSON.parse(frozenPayload))
  // 换 token 后新事件照常采集
  const next = await second.collector.handleTurnEnd(turn({}, 'entry-2'), context('session-2'))
  assert.equal(next.state, 'queued')

  // 旧进程（仍然持旧 token 的配置）在下一次操作检查时暂停，不会继续发旧队列。
  writeConfig(ws, { token: TOKEN_A })
  const stale = makeCollector(ws)
  t.after(() => stale.collector.stop())
  await stale.collector.start()
  const staleResult = await stale.collector.handleTurnEnd(turn({}, 'entry-3'), context('session-3'))
  assert.deepEqual(staleResult, { state: 'paused', reason: 'token_changed' })
  assert.equal((await stale.collector.runFlush()).state, 'paused')
  assert.equal(stale.calls.length, 0)
})

test('暂停后没有新事件也能恢复：配置回到已绑定凭证，配置检查唤醒 flush', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  const first = makeCollector(ws)
  await first.collector.start()
  await first.collector.handleTurnEnd(turn(), context())
  await first.collector.stop()

  writeConfig(ws, { token: TOKEN_B })
  const paused = makeCollector(ws)
  await paused.collector.start()
  assert.equal((await paused.collector.runFlush()).state, 'paused')
  assert.equal(paused.calls.length, 0)

  // 没有新 turn_end，只把配置改回原 token，然后做一次独立配置检查。
  writeConfig(ws, { token: TOKEN_A })
  const polled = await paused.collector.pollConfig()
  assert.equal(polled.state, 'ready')
  assert.equal(paused.collector.uploader.paused, false)
  t.after(() => paused.collector.stop())

  await paused.collector.runFlush()
  assert.equal(paused.calls.length, 1)
  assert.equal(paused.collector.queue.counts().pending, 0)
})

test('服务地址变化必须新 stateDir：不采集、不发送、绑定不被覆盖', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  const first = makeCollector(ws)
  await first.collector.start()
  await first.collector.handleTurnEnd(turn(), context())
  await first.collector.stop()

  writeConfig(ws, { serverUrl: 'https://other.example.com' })
  const other = makeCollector(ws)
  t.after(() => other.collector.stop())
  await other.collector.start()
  const result = await other.collector.handleTurnEnd(turn({}, 'entry-2'), context('session-2'))
  assert.deepEqual(result, { state: 'paused', reason: 'server_changed' })
  assert.equal((await other.collector.runFlush()).state, 'paused')
  assert.equal(other.calls.length, 0)
  assert.equal(other.collector.queue.binding().serverUrl, SERVER)
  assert.equal(other.collector.queue.counts().pending, 1)
})

test('绑定缺失但队列有事件：暂停并保留事件，不擅自重新绑定', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  const first = makeCollector(ws)
  await first.collector.start()
  await first.collector.handleTurnEnd(turn(), context())
  await first.collector.stop()

  const raw = new DatabaseSync(path.join(ws.stateDir, QUEUE_FILE), { timeout: 0 })
  raw.exec('DELETE FROM meta')
  raw.close()

  const second = makeCollector(ws)
  t.after(() => second.collector.stop())
  await second.collector.start()
  assert.equal((await second.collector.runFlush()).state, 'paused')
  assert.equal(second.collector.queue.binding(), null)
  assert.equal(second.calls.length, 0)
  assert.deepEqual(pendingIds(ws), ['pi:msg:session-1:entry-1'])
})

test('服务端已提交但响应丢失：重传同一 ID 与同一字节，返回 duplicate 且不重计', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  // 有状态桩：第一次真的接收并记录（提交），然后在返回响应前断开；
  // 第二次按「已存在」返回 duplicate。
  const committed = new Map()
  const bodies = []
  const seenStatuses = []
  let attempt = 0
  const { collector } = makeCollector(ws, {
    fetch: (_url, init) => {
      bodies.push(init.body)
      attempt += 1
      const body = JSON.parse(init.body)
      if (attempt === 1) {
        for (const event of body.events) {
          committed.set(event.source_event_id, (committed.get(event.source_event_id) ?? 0) + 1)
        }
        throw new Error('socket hang up after commit')
      }
      const results = body.events.map((event, index) => {
        const status = committed.has(event.source_event_id) ? 'duplicate' : 'accepted'
        seenStatuses.push(status)
        return { index, status }
      })
      return jsonResponse(200, { request_id: 'req', results })
    },
  })
  t.after(() => collector.stop())
  await collector.start()
  await collector.handleTurnEnd(turn(), context())

  const first = await collector.runFlush()
  assert.equal(first.state, 'backoff')
  assert.equal(collector.queue.counts().pending, 1)

  const second = await collector.runFlush()
  assert.equal(second.state, 'idle')
  assert.equal(collector.queue.counts().pending, 0)
  assert.equal(bodies.length, 2)
  assert.equal(bodies[0], bodies[1])
  // 服务端只提交一次；第二次是幂等 duplicate，没有新增事件。
  assert.deepEqual([...committed.entries()], [['pi:msg:session-1:entry-1', 1]])
  assert.deepEqual(seenStatuses, ['duplicate'])
})

test('断网补报：重启即发上一轮未确认事件；退出不清空；限时等待', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)

  // 第一轮：服务端一直挂起，退出时取消网络请求，事件保留。
  const hanging = makeCollector(ws, {
    fetch: (_url, init) => new Promise((_resolve, reject) => {
      init.signal.addEventListener('abort', () => reject(new Error('aborted')))
    }),
  })
  await hanging.collector.start()
  await hanging.collector.handleTurnEnd(turn(), context())
  const inFlight = hanging.collector.runFlush()
  const started = Date.now()
  await hanging.collector.stop({ timeoutMs: 2000 })
  await inFlight.catch(() => {})
  assert.equal(Date.now() - started < 2000, true)
  assert.deepEqual(pendingIds(ws), ['pi:msg:session-1:entry-1'])

  // 第二轮：启动即补报，无需新的 turn_end。
  const recovered = makeCollector(ws)
  t.after(() => recovered.collector.stop())
  await recovered.collector.start()
  await recovered.collector.runFlush()
  assert.equal(recovered.calls.length, 1)
  assert.deepEqual(recovered.collector.status().counts, { pending: 0, dead: 0, overflow: 0 })
})

test('隐私：真实采集事件分别落 pending/dead/overflow，DB 字节与日志都不含标记', async (t) => {
  const ws = workspace(t)
  // pending 上限 1：第二条走 overflow。
  writeConfig(ws, { maxQueueItems: 1 })
  const stream = { lines: [], write(chunk) { this.lines.push(chunk) } }
  const markerPrompt = 'PRIVATE-PROMPT-不要上传'
  const markerTitle = 'PRIVATE-SESSION-TITLE'
  const markerCwd = '/work/PRIVATE-CWD-MARKER/sub'
  const { collector, calls } = makeCollector(ws, {
    collector: { logger: createLogger(stream) },
    fetch: (_url, init) => {
      const body = JSON.parse(init.body)
      // 让已提交的事件变成死信，覆盖 dead 状态。
      return jsonResponse(200, {
        request_id: 'req',
        results: body.events.map((_event, index) => ({ index, status: 'conflict' })),
      })
    },
  })
  t.after(() => collector.stop())
  await collector.start()

  const secretTurn = (entryId) => {
    const event = turn({}, entryId)
    event.message.content = [{ type: 'text', text: markerPrompt }]
    event.entries = [{ type: 'session_info', name: markerTitle }]
    return event
  }

  // e1 → pending；e2 → overflow
  await collector.handleTurnEnd(secretTurn('entry-1'), context('session-1', markerCwd))
  await collector.handleTurnEnd(secretTurn('entry-2'), context('session-1', markerCwd))
  assert.deepEqual(collector.status().counts, { pending: 1, dead: 0, overflow: 1 })

  // 上报 e1：服务端 conflict → dead
  await collector.runFlush()
  assert.deepEqual(collector.status().counts, { pending: 0, dead: 1, overflow: 1 })
  assert.equal(calls.length, 1)

  // e3 → 再次 pending
  await collector.handleTurnEnd(secretTurn('entry-3'), context('session-1', markerCwd))
  assert.deepEqual(collector.status().counts, { pending: 1, dead: 1, overflow: 1 })

  // journal 只在写事务期间存在：这里故意开一个写事务把页面写进 journal，
  // 断言文件确实存在（否则下面的字节检查等于没做），再检查它的原始字节。
  const raw = new DatabaseSync(path.join(ws.stateDir, QUEUE_FILE), { timeout: 0 })
  raw.exec('BEGIN IMMEDIATE')
  raw.exec('UPDATE events SET updated_at = updated_at + 1')
  const journalPath = path.join(ws.stateDir, `${QUEUE_FILE}-journal`)
  assert.equal(fs.existsSync(journalPath), true, 'journal 必须存在，字节检查才有意义')
  const journalBytes = fs.readFileSync(journalPath)
  for (const needle of [TOKEN_A, markerPrompt, markerTitle, 'PRIVATE', markerCwd]) {
    assert.equal(journalBytes.includes(Buffer.from(needle, 'utf8')), false, `journal 不得包含 ${needle}`)
  }
  raw.exec('ROLLBACK')
  raw.close()
  await collector.stop()

  // 队列文件与 rollback journal 的原始字节
  for (const name of [QUEUE_FILE, `${QUEUE_FILE}-journal`]) {
    const file = path.join(ws.stateDir, name)
    if (!fs.existsSync(file)) continue
    const bytes = fs.readFileSync(file)
    for (const needle of [TOKEN_A, markerPrompt, markerTitle, 'PRIVATE', markerCwd]) {
      assert.equal(bytes.includes(Buffer.from(needle, 'utf8')), false, `${name} 不得包含 ${needle}`)
    }
  }

  // 真实 stderr 行
  const logText = stream.lines.join('')
  for (const needle of [TOKEN_A, tokenFingerprint(TOKEN_A), markerPrompt, markerTitle, 'PRIVATE', ws.stateDir]) {
    assert.equal(logText.includes(needle), false, `日志不得包含 ${needle}`)
  }

  // 三种状态都留下了记录，且 payload 键集合固定：不夹带 prompt、路径或会话标题。
  const db = new DatabaseSync(path.join(ws.stateDir, QUEUE_FILE), { timeout: 0 })
  const rows = db.prepare('SELECT status, payload FROM events ORDER BY id').all()
  db.close()
  assert.deepEqual(rows.map((row) => row.status), ['dead', 'overflow', 'pending'])
  assert.deepEqual(rows.map((row) => JSON.parse(row.payload).source_event_id), [
    'pi:msg:session-1:entry-1',
    'pi:msg:session-1:entry-2',
    'pi:msg:session-1:entry-3',
  ])
  for (const row of rows) {
    assert.deepEqual(Object.keys(JSON.parse(row.payload)).sort(), [
      'duration_ms', 'model', 'occurred_at', 'project_id', 'provider', 'source_event_id', 'status', 'usage',
    ])
  }
})

test('配置文件权限不足时不启用，且不擅自 chmod', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  fs.chmodSync(ws.configPath, 0o644)
  const { collector, calls } = makeCollector(ws)
  t.after(() => collector.stop())
  await collector.start()

  assert.equal(collector.status().state, 'paused')
  assert.equal(collector.status().reason, 'config_permissions')
  await collector.handleTurnEnd(turn(), context())
  assert.equal(collector.counters.skip_paused, 1)
  assert.equal(calls.length, 0)
  assert.equal(fs.statSync(ws.configPath).mode & 0o777, 0o644)
})

test('环境变量 token 优先于配置文件，且不要求配置文件里存凭证', (t) => {
  const ws = workspace(t)
  writeConfig(ws, { token: TOKEN_A })
  const fromEnv = loadConfig({ env: { TOKENPIVOT_PI_CONFIG: ws.configPath, TOKENPIVOT_PI_TOKEN: TOKEN_B } })
  assert.equal(fromEnv.status, 'ok')
  assert.equal(fromEnv.config.token, TOKEN_B)
  assert.equal(fromEnv.config.fingerprint, tokenFingerprint(TOKEN_B))

  const withoutToken = loadConfig({ env: { TOKENPIVOT_PI_CONFIG: ws.configPath, TOKENPIVOT_PI_TOKEN: '  ' } })
  assert.equal(withoutToken.status, 'ok')
  assert.equal(withoutToken.config.token, TOKEN_A)
})

test('服务地址校验收紧：仅 https（回环可 http）、拒绝 userinfo 与查询串、强制接口路径', (t) => {
  assert.equal(normalizeServerUrl('http://tokenpivot.example.com').error, 'server_url_scheme')
  assert.equal(normalizeServerUrl('http://127.0.0.1:8080').endpoint, 'http://127.0.0.1:8080/api/v1/ingestion/events')
  assert.equal(normalizeServerUrl('http://localhost:3000/base/').endpoint, 'http://localhost:3000/base/api/v1/ingestion/events')
  assert.equal(normalizeServerUrl('https://u:p@host.example.com').error, 'server_url_userinfo')
  assert.equal(normalizeServerUrl('https://host.example.com?tenant=1').error, 'server_url_query')
  assert.equal(
    normalizeServerUrl('https://host.example.com/api/v1/ingestion/events').endpoint,
    'https://host.example.com/api/v1/ingestion/events',
  )
  assert.equal(normalizeServerUrl('').error, 'server_url_missing')

  const ws = workspace(t)
  writeConfig(ws, { serverUrl: 'ftp://host.example.com' })
  assert.equal(loadConfig({ env: { TOKENPIVOT_PI_CONFIG: ws.configPath } }).reason, 'server_url_scheme')
})

test('持锁时入队按异步退避重试，耗尽后明确记为未采集（不同步阻塞 PI）', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  const { collector } = makeCollector(ws, {
    collector: {
      enqueueAttempts: 2,
      enqueueRetryMs: 5,
      // 这里要真的 await 异步退避，所以用真实定时器而不是 idleTimers。
      setTimer: (fn, ms) => setTimeout(fn, ms),
      clearTimer: (handle) => clearTimeout(handle),
    },
  })
  await collector.start()
  t.after(() => collector.stop())

  const raw = new DatabaseSync(path.join(ws.stateDir, QUEUE_FILE), { timeout: 0 })
  raw.exec('BEGIN IMMEDIATE')
  const started = Date.now()
  try {
    const result = await collector.handleTurnEnd(turn(), context())
    assert.equal(result.state, 'failed')
    assert.equal(result.reason, 'busy')
    // 异步退避，不是长同步等待：总耗时应远小于请求超时。
    assert.equal(Date.now() - started < 1000, true)
    assert.equal(collector.counters.queue_busy, 2)
    assert.equal(collector.counters.queue_failed, 1)
    assert.equal(collector.queue.counts().pending, 0)
  } finally {
    raw.exec('ROLLBACK')
    raw.close()
  }
})

test('响应确认前再对比绑定与配置：在途期间配置被改则保留事件，不落结果', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  let collector
  let firstAttempt = true
  const built = makeCollector(ws, {
    fetch: async (_url, init) => {
      if (firstAttempt) {
        firstAttempt = false
        // 请求在途时配置检查跑了一次，服务地址已被改成另一个地址。
        writeConfig(ws, { serverUrl: 'https://other.example.com' })
        await collector.pollConfig()
      }
      return jsonResponse(200, acceptedFor(init))
    },
  })
  collector = built.collector
  t.after(() => collector.stop())
  await collector.start()
  await collector.handleTurnEnd(turn(), context())

  const outcome = await collector.runFlush()
  assert.equal(outcome.state, 'paused')
  assert.equal(collector.uploader.pauseReason, 'server_changed')
  assert.equal(collector.queue.counts().pending, 1)
  assert.equal(built.logs.some((l) => l.code === 'upload_binding_changed'), true)

  // 配置改回原服务地址后恢复，重传同一 ID，服务端幂等。
  writeConfig(ws)
  await collector.pollConfig()
  assert.equal(collector.uploader.paused, false)
  await collector.runFlush()
  assert.equal(collector.queue.counts().pending, 0)
})

test('配置换 stateDir：停旧库、按新目录重开，旧事件留在旧目录', async (t) => {
  const ws = workspace(t)
  const dirA = ws.stateDir
  const dirB = path.join(ws.root, 'state-b')
  writeConfig(ws)
  const { collector, calls } = makeCollector(ws)
  t.after(() => collector.stop())
  await collector.start()
  await collector.handleTurnEnd(turn({}, 'entry-1'), context())
  assert.equal(collector.queueDir, dirA)

  // 同一个服务与凭证，只是换了队列目录：不用等 30 秒检查，采集时立刻换库。
  writeConfig(ws, { stateDir: dirB })
  const queued = await collector.handleTurnEnd(turn({}, 'entry-2'), context())
  assert.equal(queued.state, 'queued')
  assert.equal(collector.queueDir, dirB)
  assert.deepEqual(collector.status().counts, { pending: 1, dead: 0, overflow: 0 })

  // 之后的配置检查也得是同一个目录，不会把库再换回去。
  const polled = await collector.pollConfig()
  assert.equal(polled.state, 'ready')
  assert.equal(collector.queueDir, dirB)
  assert.equal(collector.status().counts.pending, 1)
  await collector.runFlush()
  assert.equal(calls.length, 1)
  assert.equal(JSON.parse(calls[0].init.body).events[0].source_event_id, 'pi:msg:session-1:entry-2')

  // 旧目录的事件没有被迁移或删除。
  assert.deepEqual(pendingIds({ stateDir: dirA }), ['pi:msg:session-1:entry-1'])
})

test('队列目录打不开时不要求重启：修正配置后下一次配置检查自动重开', async (t) => {
  const ws = workspace(t)
  const realDir = path.join(ws.root, 'real')
  const linkDir = path.join(ws.root, 'link')
  fs.mkdirSync(realDir)
  fs.symlinkSync(realDir, linkDir)
  writeConfig(ws, { stateDir: linkDir })
  const { collector } = makeCollector(ws)
  t.after(() => collector.stop())
  await collector.start()
  assert.equal(collector.blocked, 'state_dir_symlink')
  assert.deepEqual(await collector.handleTurnEnd(turn(), context()), { skipped: 'not_ready' })

  writeConfig(ws, { stateDir: realDir })
  const polled = await collector.pollConfig()
  assert.equal(polled.state, 'ready')
  assert.equal(collector.blocked, null)
  await collector.handleTurnEnd(turn(), context())
  assert.equal(collector.status().counts.pending, 1)
})

test('缺凭证不开始新采集：跳过并计数；补齐凭证后无需重启即恢复采集', async (t) => {
  const ws = workspace(t)
  writeConfig(ws, { token: '' })
  const { collector, calls } = makeCollector(ws)
  t.after(() => collector.stop())
  await collector.start()

  assert.deepEqual(await collector.handleTurnEnd(turn(), context()), { skipped: 'collect_disabled_no_token' })
  assert.equal(collector.counters.skip_collect_disabled_no_token, 1)
  assert.deepEqual(collector.status().counts, { pending: 0, dead: 0, overflow: 0 })
  assert.equal(calls.length, 0)

  // 已有队列保留；补齐凭证后由配置检查恢复采集。
  writeConfig(ws, { token: TOKEN_A })
  await collector.pollConfig()
  const queued = await collector.handleTurnEnd(turn(), context())
  assert.equal(queued.state, 'queued')
  await collector.runFlush()
  assert.equal(calls.length, 1)
  assert.equal(collector.queue.counts().pending, 0)
})

test('HTTP 401 暂停后，配置未改时轮询不会自行恢复发送', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  let calls = 0
  let fail = true
  const { collector } = makeCollector(ws, {
    fetch: (_url, init) => {
      calls += 1
      return fail
        ? jsonResponse(401, { error: { code: 'INVALID_ARGUMENT' } })
        : jsonResponse(200, acceptedFor(init))
    },
  })
  t.after(() => collector.stop())
  await collector.start()
  await collector.handleTurnEnd(turn(), context())
  assert.equal((await collector.runFlush()).state, 'paused')
  assert.equal(collector.uploader.pauseReason, 'http_401')
  assert.equal(calls, 1)

  // 配置一个字节都没改：连续几次配置检查都必须保持暂停。
  for (let round = 0; round < 3; round += 1) {
    assert.equal((await collector.pollConfig()).state, 'ready')
    assert.equal(collector.uploader.paused, true)
    assert.equal(collector.uploader.pauseReason, 'http_401')
  }
  assert.equal(calls, 1)
  assert.equal(collector.queue.counts().pending, 1)

  // 用户换凭证并确认同源后，才恢复发送。
  writeConfig(ws, { token: TOKEN_B })
  await collector.pollConfig()
  assert.equal(collector.uploader.paused, true)
  fail = false
  assert.equal((await collector.confirmSameSource()).ok, true)
  assert.equal((await collector.runFlush()).state, 'idle')
  assert.equal(calls, 2)
  assert.equal(collector.queue.counts().pending, 0)
})

test('上报在途时轮换凭证：该批保留，后续批次不再用旧凭证', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  let collector
  let first = true
  const auths = []
  const built = makeCollector(ws, {
    collector: { uploaderOptions: { batchSize: 1 } },
    fetch: async (_url, init) => {
      auths.push(init.headers.authorization)
      if (first) {
        first = false
        // 第一批在途时只改配置文件：没有新事件、也没轮到 30 秒配置检查。
        writeConfig(ws, { token: TOKEN_B })
      }
      return jsonResponse(200, acceptedFor(init))
    },
  })
  collector = built.collector
  t.after(() => collector.stop())
  await collector.start()
  await collector.handleTurnEnd(turn({}, 'entry-1'), context())
  await collector.handleTurnEnd(turn({}, 'entry-2'), context())

  const outcome = await collector.runFlush()
  assert.equal(outcome.state, 'paused')
  assert.equal(outcome.reason, 'token_changed')
  assert.equal(auths.length, 1)
  assert.equal(auths[0], `Bearer ${TOKEN_A}`)
  // 第一批没有落结果、第二批没有被发送：两批都还在库里。
  assert.equal(collector.queue.counts().pending, 2)
})

test('退出时等入队落地；限时到了仍未落地则明确计数未采集事件', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  const { collector } = makeCollector(ws, {
    collector: {
      enqueueAttempts: 5,
      enqueueRetryMs: 200,
      setTimer: (fn, ms) => setTimeout(fn, ms),
      clearTimer: (handle) => clearTimeout(handle),
    },
  })
  await collector.start()

  const raw = new DatabaseSync(path.join(ws.stateDir, QUEUE_FILE), { timeout: 0 })
  raw.exec('BEGIN IMMEDIATE')
  const pending = collector.handleTurnEnd(turn(), context())
  await new Promise((resolve) => setTimeout(resolve, 20))
  assert.equal(collector.counters.queue_busy, 1)
  assert.equal(collector.inflight.size, 1)

  const started = Date.now()
  const beforeStop = collector.inflight.size
  await collector.stop({ timeoutMs: 100 })
  assert.equal(Date.now() - started < 1000, true)
  // stop 返回当刻：在途入队仍然存在（已被明确计数），不是等它落地后才有状态。
  assert.equal(beforeStop, 1)
  assert.equal(collector.inflight.size, 1)
  assert.equal(collector.counters.enqueue_unflushed, 1)

  raw.exec('ROLLBACK')
  raw.close()
  const result = await pending
  assert.equal(result.state, 'failed')
  assert.equal(result.reason, 'stopped')
  assert.equal(collector.inflight.size, 0)
  assert.equal(collector.counters.enqueue_aborted, 1)
})

test('配置文件里存着 token 时，即使环境变量优先生效也要求 0600/0700', (t) => {
  const ws = workspace(t)
  writeConfig(ws, { token: TOKEN_A })
  fs.chmodSync(ws.configPath, 0o644)
  const withFileToken = loadConfig({ env: { TOKENPIVOT_PI_CONFIG: ws.configPath, TOKENPIVOT_PI_TOKEN: TOKEN_B } })
  assert.equal(withFileToken.status, 'paused')
  assert.equal(withFileToken.reason, 'config_permissions')

  // 文件里没有凭证时，宽松权限没有可泄露的东西，不因此暂停。
  writeConfig(ws, { token: '' })
  fs.chmodSync(ws.configPath, 0o644)
  const withoutFileToken = loadConfig({ env: { TOKENPIVOT_PI_CONFIG: ws.configPath, TOKENPIVOT_PI_TOKEN: TOKEN_B } })
  assert.equal(withoutFileToken.status, 'ok')
  assert.equal(withoutFileToken.config.token, TOKEN_B)
})

test('切目录时有上传在途：新事件暂停采集，不写进旧库；上传结束后才换库', async (t) => {
  const ws = workspace(t)
  const dirA = ws.stateDir
  const dirB = path.join(ws.root, 'state-b')
  writeConfig(ws)
  let release
  const gate = new Promise((resolve) => { release = resolve })
  const { collector } = makeCollector(ws, {
    fetch: async (_url, init) => {
      // 上传在途时配置换了目录
      writeConfig(ws, { stateDir: dirB })
      await gate
      return jsonResponse(200, acceptedFor(init))
    },
  })
  t.after(() => collector.stop())
  await collector.start()
  await collector.handleTurnEnd(turn({}, 'entry-1'), context())
  const flushing = collector.runFlush()
  await new Promise((resolve) => setTimeout(resolve, 20))

  // 换库还没完成：新的 turn_end 必须暂停采集，而不是写进旧目录
  assert.deepEqual(await collector.handleTurnEnd(turn({}, 'entry-2'), context()), { skipped: 'queue_moving' })
  assert.equal(collector.counters.skip_queue_moving, 1)
  assert.equal(collector.status().queueMoving, true)
  assert.deepEqual(pendingIds({ stateDir: dirA }), ['pi:msg:session-1:entry-1'])

  release()
  await flushing
  const polled = await collector.pollConfig()
  assert.equal(polled.state, 'ready')
  assert.equal(collector.queueDir, dirB)
  assert.equal(collector.status().queueMoving, false)
  assert.deepEqual(collector.status().counts, { pending: 0, dead: 0, overflow: 0 })

  // 换库之后的新事件落在新目录，旧目录不再变化
  assert.equal((await collector.handleTurnEnd(turn({}, 'entry-3'), context())).state, 'queued')
  assert.deepEqual(pendingIds({ stateDir: dirB }), ['pi:msg:session-1:entry-3'])
  assert.deepEqual(pendingIds({ stateDir: dirA }), [])
})

test('入队 BUSY 重试期间换目录：重试仍落原库，切库推迟到入队结束', async (t) => {
  const ws = workspace(t)
  const dirA = ws.stateDir
  const dirB = path.join(ws.root, 'state-b')
  writeConfig(ws)
  const { collector } = makeCollector(ws, {
    collector: {
      enqueueAttempts: 4,
      enqueueRetryMs: 30,
      setTimer: (fn, ms) => setTimeout(fn, ms),
      clearTimer: (handle) => clearTimeout(handle),
    },
  })
  t.after(() => collector.stop())
  await collector.start()

  const raw = new DatabaseSync(path.join(ws.stateDir, QUEUE_FILE), { timeout: 0 })
  raw.exec('BEGIN IMMEDIATE')
  const pending = collector.handleTurnEnd(turn({}, 'entry-1'), context())
  await new Promise((resolve) => setTimeout(resolve, 15))
  assert.equal(collector.counters.queue_busy >= 1, true)

  writeConfig(ws, { stateDir: dirB })
  const polled = await collector.pollConfig()
  assert.deepEqual(polled, { state: 'deferred', reason: 'queue_busy' })
  assert.equal(collector.queueDir, dirA)

  raw.exec('COMMIT')
  raw.close()
  await new Promise((resolve) => setTimeout(resolve, 50))
  assert.equal((await pending).state, 'queued')
  // 归属：重试落在开始时那个库
  assert.deepEqual(pendingIds({ stateDir: dirA }), ['pi:msg:session-1:entry-1'])
  assert.deepEqual(pendingIds({ stateDir: dirB }), [])

  const swapped = await collector.pollConfig()
  assert.equal(swapped.state, 'ready')
  assert.equal(collector.queueDir, dirB)
})

test('采集后、发送前换凭证：零发送并暂停', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  const { collector, calls } = makeCollector(ws)
  t.after(() => collector.stop())
  await collector.start()
  await collector.handleTurnEnd(turn(), context())
  assert.equal(collector.queue.counts().pending, 1)

  // 事件已按 A 入队；发送前配置换成 B
  writeConfig(ws, { token: TOKEN_B })
  const outcome = await collector.runFlush()
  assert.equal(outcome.state, 'paused')
  assert.equal(outcome.reason, 'token_changed')
  assert.equal(calls.length, 0)
  assert.equal(collector.queue.counts().pending, 1)
})

test('其他进程在途更新绑定：本进程响应前发现并暂停，不落结果', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  const { collector } = makeCollector(ws, {
    fetch: (_url, init) => {
      // 另一个进程把 bindingRevision 加一（同地址、同指纹）
      const other = new SqliteQueue({ dir: ws.stateDir }).open()
      other.writeBindingRaw({ serverUrl: SERVER, fingerprint: tokenFingerprint(TOKEN_A), revision: 2 })
      other.close()
      return jsonResponse(200, acceptedFor(init))
    },
  })
  t.after(() => collector.stop())
  await collector.start()
  await collector.handleTurnEnd(turn(), context())

  const outcome = await collector.runFlush()
  assert.equal(outcome.state, 'paused')
  assert.equal(outcome.reason, 'binding_changed')
  assert.equal(collector.queue.counts().pending, 1)
})

test('HTTP 暂停不被临时配置错误冲掉：恢复原配置后仍暂停、零新增请求', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  let calls = 0
  const { collector } = makeCollector(ws, {
    fetch: () => {
      calls += 1
      return jsonResponse(401, { error: { code: 'INVALID_ARGUMENT', message: 'bad token' } })
    },
  })
  t.after(() => collector.stop())
  await collector.start()
  await collector.handleTurnEnd(turn(), context())
  assert.equal((await collector.runFlush()).state, 'paused')
  assert.equal(collector.uploader.pauseReason, 'http_401')

  // 临时写坏配置：几轮检查都看到配置错误
  fs.writeFileSync(ws.configPath, '{ not json')
  for (let round = 0; round < 2; round += 1) {
    assert.equal((await collector.pollConfig()).state, 'error')
    assert.equal(collector.uploader.paused, true)
  }

  // 配置恢复成暂停时的同一地址+同一凭证：HTTP 暂停必须保留
  writeConfig(ws)
  assert.equal((await collector.pollConfig()).state, 'ready')
  assert.equal(collector.uploader.paused, true)
  assert.equal(collector.uploader.pauseReason, 'http_401')
  assert.equal(calls, 1)
  assert.equal(collector.queue.counts().pending, 1)
})

test('配置里的 flushDelayMs/requestTimeoutMs/maxQueueItems 生效，运行中修改也生效', async (t) => {
  const ws = workspace(t)
  writeConfig(ws, { flushDelayMs: 7000, requestTimeoutMs: 40, maxQueueItems: 2 })
  const { collector } = makeCollector(ws, {
    // 清掉测试默认的显式覆盖，让这三个参数跟随配置文件
    collector: { flushDelayMs: undefined, requestTimeoutMs: undefined },
    fetch: () => new Response(new ReadableStream({ start() {} }), { status: 200 }),
  })
  t.after(() => collector.stop())
  await collector.start()
  assert.equal(collector.flushDelayMs, 7000)
  assert.equal(collector.requestTimeoutMs, 40)
  assert.equal(collector.uploader.timeoutMs, 40)
  assert.equal(collector.queue.maxItems, 2)

  writeConfig(ws, { flushDelayMs: 3000, requestTimeoutMs: 25, maxQueueItems: 1 })
  await collector.pollConfig()
  assert.equal(collector.flushDelayMs, 3000)
  assert.equal(collector.requestTimeoutMs, 25)
  assert.equal(collector.uploader.timeoutMs, 25)
  assert.equal(collector.queue.maxItems, 1)

  // 超时按新值生效：挂起的响应体在 25ms 左右被打断
  await collector.handleTurnEnd(turn({}, 'entry-1'), context())
  const started = Date.now()
  const outcome = await collector.runFlush()
  assert.equal(outcome.state, 'backoff')
  assert.equal(Date.now() - started < 120, true)
  assert.equal(collector.queue.counts().pending, 1)
})

test('配置读坏时不换目录：不会把队列挪去默认 stateDir', async (t) => {
  const ws = workspace(t)
  writeConfig(ws)
  const { collector } = makeCollector(ws)
  t.after(() => collector.stop())
  await collector.start()
  const before = collector.queueDir
  assert.equal(before, ws.stateDir)

  fs.writeFileSync(ws.configPath, '{ not json')
  assert.equal((await collector.pollConfig()).state, 'error')
  assert.equal(collector.queueDir, before)
  assert.equal(collector.swapPending, false)
  // 恢复原配置后仍在原目录
  writeConfig(ws)
  assert.equal((await collector.pollConfig()).state, 'ready')
  assert.equal(collector.queueDir, before)
})

test('待切目录时配置读坏：保留旧队列零切换，配置恢复后正常切', async (t) => {
  const ws = workspace(t)
  const dirA = ws.stateDir
  const dirB = path.join(ws.root, 'state-b')
  writeConfig(ws)
  let release
  const gate = new Promise((resolve) => { release = resolve })
  const { collector } = makeCollector(ws, {
    fetch: async (_url, init) => {
      await gate
      return jsonResponse(200, acceptedFor(init))
    },
  })
  t.after(() => collector.stop())
  await collector.start()
  await collector.handleTurnEnd(turn({}, 'entry-1'), context())
  const flushing = collector.runFlush()
  await new Promise((resolve) => setTimeout(resolve, 20))

  // 切目录已排定（在途），新的采集被暂停
  writeConfig(ws, { stateDir: dirB })
  assert.deepEqual(await collector.handleTurnEnd(turn({}, 'entry-2'), context()), { skipped: 'queue_moving' })
  assert.equal(collector.swapPending, true)

  // 此时配置读坏：不能拿默认目录去换库
  fs.writeFileSync(ws.configPath, '{ not json')
  assert.deepEqual(await collector.handleTurnEnd(turn({}, 'entry-3'), context()), { skipped: 'error' })
  assert.equal(collector.queueDir, dirA)
  assert.equal(collector.swapPending, true)
  assert.deepEqual(pendingIds({ stateDir: dirA }), ['pi:msg:session-1:entry-1'])

  // 配置恢复（仍指向 B）：在途结束后正常换库
  writeConfig(ws, { stateDir: dirB })
  release()
  await flushing
  assert.equal((await collector.pollConfig()).state, 'ready')
  assert.equal(collector.queueDir, dirB)
  assert.equal(collector.swapPending, false)
})

test('切库未完成时新容量不穿透旧队列；换库后新队列用新容量', async (t) => {
  const ws = workspace(t)
  const dirA = ws.stateDir
  const dirB = path.join(ws.root, 'state-b')
  writeConfig(ws, { maxQueueItems: 10 })
  const { collector } = makeCollector(ws, {
    collector: {
      enqueueAttempts: 4,
      enqueueRetryMs: 30,
      setTimer: (fn, ms) => setTimeout(fn, ms),
      clearTimer: (handle) => clearTimeout(handle),
    },
  })
  t.after(() => collector.stop())
  await collector.start()
  assert.equal(collector.queue.maxItems, 10)

  const raw = new DatabaseSync(path.join(ws.stateDir, QUEUE_FILE), { timeout: 0 })
  raw.exec('BEGIN IMMEDIATE')
  const pending = collector.handleTurnEnd(turn({}, 'entry-1'), context())
  await new Promise((resolve) => setTimeout(resolve, 15))

  // 切到 B 并把上限调到 1：旧队列仍在用，容量不能被改
  writeConfig(ws, { stateDir: dirB, maxQueueItems: 1 })
  const polled = await collector.pollConfig()
  assert.deepEqual(polled, { state: 'deferred', reason: 'queue_busy' })
  assert.equal(collector.queueDir, dirA)
  assert.equal(collector.queue.maxItems, 10)

  raw.exec('COMMIT')
  raw.close()
  await new Promise((resolve) => setTimeout(resolve, 50))
  // 在途事件按旧队列的容量入队：仍是 pending，不是 overflow
  assert.equal((await pending).state, 'queued')
  assert.deepEqual(collector.status().counts, { pending: 1, dead: 0, overflow: 0 })

  const swapped = await collector.pollConfig()
  assert.equal(swapped.state, 'ready')
  assert.equal(collector.queueDir, dirB)
  assert.equal(collector.queue.maxItems, 1)
  assert.deepEqual(pendingIds({ stateDir: dirA }), ['pi:msg:session-1:entry-1'])

  // 新队列按新容量 1 生效：第二条转 overflow
  await collector.handleTurnEnd(turn({}, 'entry-2'), context())
  await collector.handleTurnEnd(turn({}, 'entry-3'), context())
  assert.deepEqual(collector.status().counts, { pending: 1, dead: 0, overflow: 1 })
})

test('待切目录期间不再拉旧队列的下一批；切换后新队列继续上报', async (t) => {
  const ws = workspace(t)
  const dirB = path.join(ws.root, 'state-b')
  writeConfig(ws)
  let release
  const gate = new Promise((resolve) => { release = resolve })
  const batches = []
  const { collector } = makeCollector(ws, {
    collector: { uploaderOptions: { batchSize: 1 } },
    fetch: async (_url, init) => {
      const body = JSON.parse(init.body)
      batches.push(body.events.map((event) => event.source_event_id))
      if (batches.length === 1) await gate
      return jsonResponse(200, acceptedFor(init))
    },
  })
  t.after(() => collector.stop())
  await collector.start()
  await collector.handleTurnEnd(turn({}, 'entry-1'), context())
  await collector.handleTurnEnd(turn({}, 'entry-2'), context())
  assert.deepEqual(collector.status().counts, { pending: 2, dead: 0, overflow: 0 })

  const flushing = collector.runFlush()
  await new Promise((resolve) => setTimeout(resolve, 20))
  assert.equal(batches.length, 1)

  writeConfig(ws, { stateDir: dirB })
  assert.deepEqual(await collector.pollConfig(), { state: 'deferred', reason: 'queue_busy' })
  assert.equal(collector.swapPending, true)

  release()
  const outcome = await flushing
  // 第二批没有被拉走：drain 在批次边界停手，旧队列还剩 1 条
  assert.equal(outcome.state, 'deferred')
  assert.equal(batches.length, 1)
  assert.deepEqual(collector.status().counts, { pending: 1, dead: 0, overflow: 0 })

  // 在途结束后配置检查完成切换，新队列继续上报
  assert.equal((await collector.pollConfig()).state, 'ready')
  assert.equal(collector.queueDir, dirB)
  await collector.handleTurnEnd(turn({}, 'entry-3'), context())
  await collector.runFlush()
  assert.equal(batches.length, 2)
  assert.deepEqual(batches[1], ['pi:msg:session-1:entry-3'])
  assert.deepEqual(pendingIds({ stateDir: dirB }), [])
})

test('目录 A→B→A 往返不清掉 A 的 HTTP 暂停：同身份不再发请求', async (t) => {
  const ws = workspace(t)
  const dirA = ws.stateDir
  const dirB = path.join(ws.root, 'state-b')
  writeConfig(ws)
  let release
  const gate = new Promise((resolve) => { release = resolve })
  let calls = 0
  const { collector } = makeCollector(ws, {
    collector: { uploaderOptions: { batchSize: 1 } },
    fetch: async (_url, init) => {
      calls += 1
      if (calls === 1) {
        await gate
        return jsonResponse(401, { error: { code: 'INVALID_ARGUMENT', message: 'expired' } })
      }
      return jsonResponse(200, acceptedFor(init))
    },
  })
  t.after(() => collector.stop())
  await collector.start()
  await collector.handleTurnEnd(turn({}, 'entry-1'), context())
  const flushing = collector.runFlush()
  await new Promise((resolve) => setTimeout(resolve, 20))

  // 在途时切 B，再切回 A
  writeConfig(ws, { stateDir: dirB })
  assert.deepEqual(await collector.pollConfig(), { state: 'deferred', reason: 'queue_busy' })
  writeConfig(ws)
  release()
  await flushing
  assert.equal(collector.uploader.paused, true)
  assert.equal(collector.uploader.pauseReason, 'http_401')

  // 目录回到 A：只取消待切换，保留 HTTP 暂停，不重开队列、不再发请求
  assert.equal((await collector.pollConfig()).state, 'ready')
  assert.equal(collector.queueDir, dirA)
  assert.equal(collector.swapPending, false)
  assert.equal(collector.uploader.paused, true)
  assert.equal(collector.uploader.pauseReason, 'http_401')
  assert.equal(calls, 1)
  assert.equal(collector.queue.counts().pending, 1)

  // 只有凭证身份真的变了（换 B 并确认同源）才解除
  writeConfig(ws, { token: TOKEN_B })
  await collector.pollConfig()
  assert.equal(collector.uploader.pauseReason, 'token_changed')
  assert.equal((await collector.confirmSameSource()).ok, true)
  assert.equal(collector.uploader.paused, false)
  await collector.runFlush()
  assert.equal(calls, 2)
  assert.equal(collector.queue.counts().pending, 0)
})
