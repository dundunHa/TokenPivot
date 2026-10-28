import { test } from 'node:test'
import assert from 'node:assert/strict'
import http from 'node:http'
import { mkdtemp, readFile, readdir } from 'node:fs/promises'
import os from 'node:os'
import path from 'node:path'
import { FileQueue } from '../lib/queue.js'
import { Uploader, parseRetryAfter } from '../lib/uploader.js'
import { Collector, ingestionEndpoint } from '../lib/collector.js'

const TOKEN = 'tpk_test_secret_value_do_not_log'

const tmp = () => mkdtemp(path.join(os.tmpdir(), 'tp-dsh-'))
const ev = (n) => ({
  source_event_id: `dsh:msg:${n}`,
  project_id: null,
  provider: 'p',
  model: 'm',
  occurred_at: '2026-10-28T12:00:00.000Z',
  usage: { input_tokens: { value: n, quality: 'known' }, output_tokens: { value: 1, quality: 'known' } },
  status: null,
  duration_ms: null,
})

/** Mock ingestion server. `handler(body, req, call)` returns {status, json?, headers?} or 'drop'. */
async function server(handler) {
  const calls = []
  const srv = http.createServer((req, res) => {
    let raw = ''
    req.on('data', (c) => { raw += c })
    req.on('end', () => {
      const body = JSON.parse(raw)
      calls.push({ body, auth: req.headers.authorization, path: req.url })
      const out = handler(body, req, calls.length)
      if (out === 'drop') return req.socket.destroy()
      res.writeHead(out.status, { 'content-type': 'application/json', ...(out.headers ?? {}) })
      res.end(JSON.stringify(out.json ?? {}))
    })
  })
  await new Promise((r) => srv.listen(0, '127.0.0.1', r))
  const url = `http://127.0.0.1:${srv.address().port}`
  return { url, endpoint: ingestionEndpoint(url), calls, close: () => new Promise((r) => srv.close(r)) }
}

const allAs = (status) => (body) => ({ status: 200, json: { request_id: 'r', results: body.events.map((_, index) => ({ index, status })) } })

function logs() {
  const lines = []
  return { lines, warn: (m) => lines.push(String(m)) }
}

async function uploader(endpoint, items, opts = {}) {
  const dir = await tmp()
  const queue = new FileQueue({ dir })
  await queue.load()
  for (const i of items) queue.enqueue(i)
  await queue.persist()
  const logger = logs()
  const up = new Uploader({ queue, endpoint, getToken: () => TOKEN, logger, random: () => 0, ...opts })
  return { dir, queue, up, logger }
}

test('ingestionEndpoint: https or loopback http only', () => {
  assert.equal(ingestionEndpoint('https://tp.example.com/'), 'https://tp.example.com/api/v1/ingestion/events')
  assert.equal(ingestionEndpoint('https://tp.example.com/base'), 'https://tp.example.com/base/api/v1/ingestion/events')
  assert.equal(ingestionEndpoint('http://tp.example.com'), null)
  assert.equal(ingestionEndpoint('http://127.0.0.1:8080'), 'http://127.0.0.1:8080/api/v1/ingestion/events')
  assert.equal(ingestionEndpoint(''), null)
  assert.equal(ingestionEndpoint('not a url'), null)
})

test('parseRetryAfter handles seconds and HTTP date', () => {
  assert.equal(parseRetryAfter('7'), 7000)
  assert.equal(parseRetryAfter(new Date(10_000).toUTCString(), 4_000), 6000)
  assert.equal(parseRetryAfter('soon'), undefined)
  assert.equal(parseRetryAfter(null), undefined)
})

test('accepted and duplicate dequeue; bearer token sent; body is {events}', async () => {
  const s = await server((body) => ({ status: 200, json: { request_id: 'r', results: [{ index: 0, status: 'accepted' }, { index: 1, status: 'duplicate' }] } }))
  const { queue, up } = await uploader(s.endpoint, [ev(1), ev(2)])
  assert.deepEqual(await up.flush(), { state: 'idle' })
  assert.equal(queue.size, 0)
  assert.equal(s.calls[0].auth, `Bearer ${TOKEN}`)
  assert.equal(s.calls[0].path, '/api/v1/ingestion/events')
  assert.deepEqual(Object.keys(s.calls[0].body), ['events'])
  await s.close()
})

test('terminal results dequeue into dead letter without usage payload', async () => {
  const s = await server(() => ({ status: 200, json: { request_id: 'r', results: [
    { index: 0, status: 'conflict' },
    { index: 1, status: 'rejected', error: { code: 'PROJECT_INVALID', message: 'x' } },
    { index: 2, status: 'expired' },
  ] } }))
  const { dir, queue, up } = await uploader(s.endpoint, [ev(1), ev(2), ev(3)])
  assert.equal((await up.flush()).state, 'idle')
  assert.equal(queue.size, 0)
  const dead = (await readFile(path.join(dir, 'dead-letter.jsonl'), 'utf8')).trim().split('\n').map(JSON.parse)
  assert.deepEqual(dead.map((d) => [d.source_event_id, d.outcome, d.code]), [
    ['dsh:msg:1', 'conflict', null], ['dsh:msg:2', 'rejected', 'PROJECT_INVALID'], ['dsh:msg:3', 'expired', null],
  ])
  assert.ok(!JSON.stringify(dead).includes('input_tokens'))
  await s.close()
})

test('retryable items and missing results stay queued and back off', async () => {
  const s = await server(() => ({ status: 200, json: { request_id: 'r', results: [
    { index: 0, status: 'accepted' }, { index: 1, status: 'retryable', error: { code: 'UNAVAILABLE' } },
  ] } }))
  const { queue, up } = await uploader(s.endpoint, [ev(1), ev(2), ev(3)])
  const out = await up.flush()
  assert.equal(out.state, 'backoff')
  assert.deepEqual(queue.peek(10).map((e) => e.source_event_id), ['dsh:msg:2', 'dsh:msg:3'])
  await s.close()
})

test('503 with Retry-After: kept, delay honours header, retry resends identical events', async () => {
  let n = 0
  const s = await server((body) => (++n === 1 ? { status: 503, headers: { 'retry-after': '30' } } : allAs('accepted')(body)))
  const { queue, up } = await uploader(s.endpoint, [ev(1), ev(2)])
  const out = await up.flush()
  assert.equal(out.state, 'backoff')
  assert.ok(out.delayMs >= 30000)
  assert.equal(queue.size, 2)
  assert.equal((await up.flush()).state, 'idle')
  assert.deepEqual(s.calls[0].body, s.calls[1].body)
  await s.close()
})

test('backoff grows exponentially and is capped', async () => {
  const { up } = await uploader('http://127.0.0.1:1/x', [], { baseDelayMs: 1000, maxDelayMs: 8000 })
  const delays = [1, 2, 3, 4, 5].map(() => up.backoff().delayMs)
  assert.deepEqual(delays, [500, 1000, 2000, 4000, 4000])
})

test('network drop: events kept, replayed later with the same source_event_id (断网补报)', async () => {
  let n = 0
  const s = await server((body) => (++n === 1 ? 'drop' : allAs('accepted')(body)))
  const { queue, up } = await uploader(s.endpoint, [ev(1)])
  assert.equal((await up.flush()).state, 'backoff')
  assert.equal(queue.size, 1)
  assert.equal((await up.flush()).state, 'idle')
  assert.equal(s.calls.length, 2)
  assert.equal(s.calls[1].body.events[0].source_event_id, 'dsh:msg:1')
  await s.close()
})

test('server down entirely: connection refused keeps events', async () => {
  const { queue, up } = await uploader('http://127.0.0.1:9/api/v1/ingestion/events', [ev(1)], { timeoutMs: 2000 })
  assert.equal((await up.flush()).state, 'backoff')
  assert.equal(queue.size, 1)
})

test('401 pauses uploads until resume()', async () => {
  let status = 401
  const s = await server((body) => (status === 401 ? { status: 401 } : allAs('accepted')(body)))
  const { queue, up, logger } = await uploader(s.endpoint, [ev(1)])
  assert.equal((await up.flush()).state, 'paused')
  assert.equal((await up.flush()).state, 'paused')
  assert.equal(s.calls.length, 1)
  status = 200
  up.resume()
  assert.equal((await up.flush()).state, 'idle')
  assert.equal(queue.size, 0)
  assert.ok(logger.lines.every((l) => !l.includes(TOKEN)))
  await s.close()
})

test('413 halves the batch; a lone item that still fails is dead-lettered', async () => {
  const s = await server((body) => {
    if (body.events.some((e) => e.source_event_id === 'dsh:msg:3')) return { status: 413 }
    return allAs('accepted')(body)
  })
  const { dir, queue, up } = await uploader(s.endpoint, [1, 2, 3, 4].map(ev))
  assert.equal((await up.flush()).state, 'idle')
  assert.equal(queue.size, 0)
  const sizes = s.calls.map((c) => c.body.events.length)
  assert.deepEqual(sizes.slice(0, 2), [4, 2])
  const dead = await readFile(path.join(dir, 'dead-letter.jsonl'), 'utf8')
  assert.match(dead, /dsh:msg:3.*http_413/)
  assert.ok(!dead.includes('dsh:msg:4'))
  await s.close()
})

test('batches are capped at 100 events', async () => {
  const s = await server(allAs('accepted'))
  const { queue, up } = await uploader(s.endpoint, Array.from({ length: 250 }, (_, i) => ev(i)))
  assert.equal((await up.flush()).state, 'idle')
  assert.equal(queue.size, 0)
  assert.deepEqual(s.calls.map((c) => c.body.events.length), [100, 100, 50])
  await s.close()
})

test('no endpoint or no token: nothing sent, events kept', async () => {
  const { queue, up } = await uploader(null, [ev(1)])
  assert.equal((await up.flush()).state, 'disabled')
  const b = await uploader('http://127.0.0.1:9/x', [ev(1)], { getToken: () => undefined })
  assert.equal((await b.up.flush()).state, 'disabled')
  assert.equal(queue.size, 1)
})

test('queue survives restart, dedupes ids, drops oldest beyond cap; file has no token', async () => {
  const dir = await tmp()
  const q1 = new FileQueue({ dir, maxItems: 3, logger: logs() })
  await q1.load()
  assert.equal(q1.enqueue(ev(1)), true)
  assert.equal(q1.enqueue(ev(1)), false)
  for (const n of [2, 3, 4]) q1.enqueue(ev(n))
  await q1.persist()
  const q2 = new FileQueue({ dir })
  await q2.load()
  assert.deepEqual(q2.peek(10).map((e) => e.source_event_id), ['dsh:msg:2', 'dsh:msg:3', 'dsh:msg:4'])
  const files = await readdir(dir)
  assert.deepEqual(files, ['queue.json'])
  assert.ok(!(await readFile(path.join(dir, 'queue.json'), 'utf8')).includes('tpk_'))
})

test('corrupt queue file is moved aside, not fatal', async () => {
  const dir = await tmp()
  const { writeFile } = await import('node:fs/promises')
  await writeFile(path.join(dir, 'queue.json'), '{not json')
  const q = new FileQueue({ dir, logger: logs() })
  await q.load()
  assert.equal(q.size, 0)
  assert.ok((await readdir(dir)).some((f) => f.startsWith('queue.json.corrupt-')))
})

test('collector: session events → queue → upload; restart replays leftovers', async () => {
  let up = false
  const s = await server((body) => (up ? allAs('accepted')(body) : 'drop'))
  const dir = await tmp()
  const timers = []
  const make = () => new Collector({
    stateDir: dir,
    endpoint: s.endpoint,
    getToken: () => TOKEN,
    resolveCwd: (session) => session.header.cwd,
    projects: [{ path: '/w', projectId: '00000000-0000-0000-0000-0000000000aa' }],
    logger: logs(),
    setTimer: (fn, ms) => { timers.push({ fn, ms }); return timers.length },
    clearTimer: () => {},
  })
  const c1 = make()
  await c1.start()
  const session = { id: 's', header: { id: 's', cwd: '/w/x' } }
  await c1.handle(session, { type: 'assistant/message', time: Date.now(), data: {
    message: { role: 'assistant', id: 'm-1', source: { provider: 'p', model: 'm' } },
    usage: { inputTokens: 3, outputTokens: 1, totalTokens: 4 },
  } })
  await c1.handle(session, { type: 'tool/result', time: Date.now(), data: {} })
  assert.equal(c1.queue.size, 1)
  assert.equal(timers.at(-1).ms, 2000)
  assert.equal((await c1.flush()).state, 'backoff') // offline
  await c1.stop()

  up = true
  const c2 = make()
  await c2.start()
  assert.equal(c2.queue.size, 1)
  assert.equal(timers.at(-1).ms, 0) // leftovers scheduled immediately on start
  assert.equal((await c2.flush()).state, 'idle')
  assert.equal(c2.queue.size, 0)
  const sent = s.calls.at(-1).body.events[0]
  assert.equal(sent.source_event_id, 'dsh:msg:m-1')
  assert.equal(sent.project_id, '00000000-0000-0000-0000-0000000000aa')
  assert.deepEqual(s.calls[0].body, s.calls.at(-1).body)
  await c2.stop()
  await s.close()
})
