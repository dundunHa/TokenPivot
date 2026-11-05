/**
 * 上报验收（计划 §6、§9.4）：逐项混合结果、FUTURE_EVENT、503/429 与 Retry-After、
 * 413 拆批与单条死信、暂停与恢复、畸形 200、未知状态、重定向不外泄凭证、超时、
 * 批量上限与 1 MiB 预算、日志不含凭证。
 */
import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { SqliteQueue } from '../lib/queue.js'
import { Uploader, parseRetryAfter } from '../lib/uploader.js'

const SERVER = 'https://tokenpivot.example.com/api/v1/ingestion/events'
const TOKEN = 'tpk_secret_used_only_in_header'
const FP = 'f'.repeat(64)

function setup(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tokenpivot-pi-upload-'))
  const queue = new SqliteQueue({ dir }).open()
  t.after(() => {
    queue.close()
    fs.rmSync(dir, { recursive: true, force: true })
  })
  return { queue, dir }
}

function seed(queue, count, extra = {}) {
  for (let i = 0; i < count; i += 1) {
    queue.enqueue({
      event: {
        source_event_id: `e${i}`,
        provider: 'anthropic',
        model: 'claude-x',
        occurred_at: '2026-11-05T00:00:01.200Z',
        usage: { input_tokens: { value: 100, quality: 'known' } },
        status: null,
        duration_ms: 1200,
        ...extra,
      },
      serverUrl: SERVER,
      fingerprint: FP,
    })
  }
}

function jsonResponse(status, body, headers = {}) {
  return new Response(body === undefined ? null : JSON.stringify(body), { status, headers })
}

function resultsOf(...statuses) {
  return {
    request_id: 'req-1',
    results: statuses.map((entry, index) => (
      typeof entry === 'string' ? { index, status: entry } : { index, status: entry.status, error: { code: entry.code } }
    )),
  }
}

function makeUploader(queue, fetchImpl, overrides = {}) {
  const logs = []
  const calls = []
  const uploader = new Uploader({
    queue,
    getEndpoint: () => SERVER,
    getToken: () => TOKEN,
    fetch: (url, init) => {
      calls.push({ url, init, body: init.body })
      return fetchImpl(url, init)
    },
    logger: { log: (code, data) => logs.push({ code, data }) },
    random: () => 0.5,
    timeoutMs: 200,
    ...overrides,
  })
  return { uploader, logs, calls }
}

test('逐项混合结果：accepted/duplicate 出队，conflict 死信，retryable 保留退避', async (t) => {
  const { queue } = setup(t)
  seed(queue, 4)
  let round = 0
  const { uploader, logs, calls } = makeUploader(queue, () => {
    round += 1
    return jsonResponse(200, resultsOf('accepted', 'duplicate', { status: 'conflict', code: 'USAGE_UNSUPPORTED' }, { status: 'retryable', code: 'UNAVAILABLE' }))
  })

  const outcome = await uploader.flush()
  assert.equal(round, 1)
  assert.equal(calls[0].body.startsWith('{"events":['), true)
  assert.equal(outcome.state, 'backoff')
  assert.equal(outcome.delayMs > 0, true)
  assert.deepEqual(queue.counts(), { pending: 1, dead: 1, overflow: 0 })
  assert.deepEqual(queue.peek(10).items.map((i) => i.id), ['e3'])
  assert.equal(logs.some((l) => l.code === 'event_dead' && l.data.count === 1), true)
  assert.equal(logs.some((l) => l.code === 'upload_accepted' && l.data.count === 3), true)
})

test('FUTURE_EVENT 是永久 rejected 里的例外：保留并退避，不写死信', async (t) => {
  const { queue } = setup(t)
  seed(queue, 1)
  const { uploader } = makeUploader(queue, () => jsonResponse(200, resultsOf({ status: 'rejected', code: 'FUTURE_EVENT' })))

  const outcome = await uploader.flush()
  assert.equal(outcome.state, 'backoff')
  assert.deepEqual(queue.counts(), { pending: 1, dead: 0, overflow: 0 })
})

test('rejected 没有稳定错误码时不替服务端判死刑：保留重试', async (t) => {
  const { queue } = setup(t)
  seed(queue, 1)
  const { uploader } = makeUploader(queue, () => jsonResponse(200, resultsOf({ status: 'rejected' })))
  assert.equal((await uploader.flush()).state, 'backoff')
  assert.equal(queue.counts().pending, 1)
})

test('503 与 429 遵守 Retry-After 的秒数与 HTTP-date，事件保留', async (t) => {
  const { queue } = setup(t)
  seed(queue, 1)
  const now = Date.now()
  const { uploader } = makeUploader(queue, () => jsonResponse(503, { error: { code: 'UNAVAILABLE' } }, { 'retry-after': '2' }), { now: () => now })

  const first = await uploader.flush()
  assert.equal(first.state, 'backoff')
  assert.equal(first.delayMs >= 2000, true)
  assert.equal(queue.counts().pending, 1)

  const dateBased = makeUploader(queue, () => jsonResponse(429, null, { 'retry-after': new Date(now + 4000).toUTCString() }), { now: () => now })
  const second = await dateBased.uploader.flush()
  assert.equal(second.state, 'backoff')
  assert.equal(second.delayMs >= 3000, true)

  assert.equal(parseRetryAfter('7'), 7000)
  assert.equal(parseRetryAfter('not-a-date'), undefined)
  assert.equal(parseRetryAfter(undefined), undefined)
})

test('413 多条对半拆批，直到找到超限的单条', async (t) => {
  const { queue } = setup(t)
  seed(queue, 4)
  const sizes = []
  const { uploader } = makeUploader(queue, (_url, init) => {
    const body = JSON.parse(init.body)
    sizes.push(body.events.length)
    if (sizes.length === 1) return jsonResponse(413, 'too large')
    return jsonResponse(200, resultsOf(...body.events.map(() => 'accepted')))
  })

  await uploader.flush()
  assert.deepEqual(sizes, [4, 2, 2])
  assert.equal(queue.counts().pending, 0)
})

test('413 单条仍超限：写死信后出队', async (t) => {
  const { queue } = setup(t)
  seed(queue, 1)
  const { uploader, logs } = makeUploader(queue, () => jsonResponse(413, 'too large'))
  await uploader.flush()
  assert.deepEqual(queue.counts(), { pending: 0, dead: 1, overflow: 0 })
  assert.equal(logs.some((l) => l.code === 'event_dead' && l.data.reason === 'payload_too_large'), true)
})

test('400/401/403/415 暂停自动发送并保留队列；resume 后无新事件也能继续', async (t) => {
  for (const status of [400, 401, 403, 415]) {
    const { queue } = setup(t)
    seed(queue, 2)
    let calls = 0
    const { uploader } = makeUploader(queue, () => {
      calls += 1
      return calls === 1 ? jsonResponse(status, { error: { code: 'INVALID_ARGUMENT' } }) : jsonResponse(200, resultsOf('accepted', 'accepted'))
    })

    const paused = await uploader.flush()
    assert.equal(paused.state, 'paused')
    assert.equal(uploader.pauseReason, `http_${status}`)
    assert.equal(queue.counts().pending, 2)
    // 暂停期间即使再有新的 flush 请求也不发
    assert.equal((await uploader.flush()).state, 'paused')
    assert.equal(calls, 1)

    uploader.resume()
    const resumed = await uploader.flush()
    assert.equal(resumed.state, 'idle')
    assert.equal(queue.counts().pending, 0)
  }
})

test('畸形 200：results 数量不符、index 重复、未知状态都保留并退避', async (t) => {
  const malformed = [
    { request_id: 'r', results: [] },
    { request_id: 'r', results: [{ index: 0, status: 'accepted' }, { index: 2, status: 'accepted' }] },
    { request_id: 'r', results: [{ index: 0, status: 'accepted' }, { index: 0, status: 'accepted' }] },
    { request_id: 'r', results: [{ index: 0, status: 'made-up-status' }] },
    'not json',
  ]
  for (const body of malformed) {
    const { queue } = setup(t)
    seed(queue, 2)
    const { uploader, logs } = makeUploader(queue, async () => {
      if (body === 'not json') return new Response('<<<', { status: 200 })
      return jsonResponse(200, body)
    })
    const outcome = await uploader.flush()
    assert.equal(queue.counts().pending, 2)
    assert.equal(outcome.state, 'backoff')
    assert.equal(logs.some((l) => l.code === 'upload_malformed'), true)
  }
})

test('3xx 不跟随重定向：保留事件、只发一次请求、Authorization 不随 Location 外泄', async (t) => {
  const { queue } = setup(t)
  seed(queue, 1)
  const { uploader, calls, logs } = makeUploader(
    queue,
    () => new Response(null, { status: 302, headers: { location: 'https://evil.example.com/steal' } }),
  )

  const outcome = await uploader.flush()
  assert.equal(outcome.state, 'backoff')
  assert.equal(calls.length, 1)
  assert.equal(calls[0].init.redirect, 'manual')
  assert.equal(calls[0].init.headers.authorization, `Bearer ${TOKEN}`)
  assert.equal(queue.counts().pending, 1)
  assert.equal(logs.some((l) => l.code === 'upload_redirect_refused'), true)
})

test('网络错误与超时：保留事件并退避，不宣称成功', async (t) => {
  const { queue } = setup(t)
  seed(queue, 1)

  const network = makeUploader(queue, () => {
    throw new Error('ECONNRESET')
  })
  const first = await network.uploader.flush()
  assert.equal(first.state, 'backoff')
  assert.equal(queue.counts().pending, 1)
  assert.equal(network.logs.some((l) => l.code === 'upload_failed' && l.data.reason === 'network'), true)

  const hanging = makeUploader(queue, (_url, init) => new Promise((_resolve, reject) => {
    init.signal.addEventListener('abort', () => reject(new Error('aborted')))
  }), { timeoutMs: 30 })
  const second = await hanging.uploader.flush()
  assert.equal(second.state, 'backoff')
  assert.equal(hanging.logs.some((l) => l.code === 'upload_failed' && l.data.reason === 'timeout'), true)
  assert.equal(queue.counts().pending, 1)
})

test('批量上限 100 条；单条超出 1 MiB 预算时单独成批', async (t) => {
  const { queue } = setup(t)
  seed(queue, 150)
  const sizes = []
  const { uploader } = makeUploader(queue, (_url, init) => {
    const body = JSON.parse(init.body)
    sizes.push(body.events.length)
    return jsonResponse(200, resultsOf(...body.events.map(() => 'accepted')))
  })
  await uploader.flush()
  assert.deepEqual(sizes, [100, 50])

  const big = setup(t)
  big.queue.enqueue({
    event: { source_event_id: 'big', provider: 'p', model: 'm', occurred_at: '2026-11-05T00:00:00.000Z', usage: {}, pad: 'x'.repeat(1024 * 1024 + 10) },
    serverUrl: SERVER,
    fingerprint: FP,
  })
  seed(big.queue, 1)
  const bigSizes = []
  const second = makeUploader(big.queue, (_url, init) => {
    const body = JSON.parse(init.body)
    bigSizes.push(body.events.map((e) => e.source_event_id))
    return jsonResponse(200, resultsOf(...body.events.map(() => 'accepted')))
  })
  await second.uploader.flush()
  assert.deepEqual(bigSizes, [['big'], ['e0']])
})

test('没有凭证时不发送；有凭证时 Authorization 只在请求头，正文与日志都不含 token', async (t) => {
  const { queue } = setup(t)
  seed(queue, 1)

  let called = 0
  const disabled = new Uploader({
    queue,
    getEndpoint: () => SERVER,
    getToken: () => undefined,
    fetch: () => {
      called += 1
      return jsonResponse(200, resultsOf('accepted'))
    },
    logger: { log: () => {} },
  })
  assert.equal((await disabled.flush()).state, 'disabled')
  assert.equal(called, 0)

  const { uploader, logs, calls } = makeUploader(queue, () => jsonResponse(200, resultsOf('accepted')))
  await uploader.flush()
  assert.equal(calls[0].init.headers.authorization, `Bearer ${TOKEN}`)
  assert.equal(calls[0].body.includes(TOKEN), false)
  assert.equal(JSON.stringify(logs).includes(TOKEN), false)
})
