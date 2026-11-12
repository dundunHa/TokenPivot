/**
 * Collector ⇄ `agent/generation-timing` association (T-061 D).
 *
 * The host commits the final `session/event` and then publishes the timing notice on the same
 * stack; the collector stages the event synchronously and settles it in one microtask. These
 * tests pin that contract with a real Collector (real FileQueue, real mapper) and drive the
 * microtask by hand so the ordering is explicit.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, readFile } from 'node:fs/promises'
import os from 'node:os'
import path from 'node:path'
import { Collector } from '../lib/collector.js'

const tmp = () => mkdtemp(path.join(os.tmpdir(), 'tp-dsh-col-'))

function logs() {
  const lines = []
  return { lines, warn: (m) => lines.push(String(m)) }
}

/** A collector whose microtask queue is drained by the test, not by the runtime. */
async function collectorWithMicrotasks(options = {}) {
  const tasks = []
  const dir = await tmp()
  const c = new Collector({
    stateDir: dir,
    endpoint: null,
    getToken: () => undefined,
    logger: logs(),
    setTimer: () => 1,
    clearTimer: () => {},
    microtask: (fn) => { tasks.push(fn) },
    ...options,
  })
  await c.start()
  const drain = async () => {
    while (tasks.length > 0) {
      const batch = tasks.splice(0)
      for (const fn of batch) fn()
      // the microtask body is async: let its awaits (queue load, persist) finish
      await new Promise((resolve) => setImmediate(resolve))
    }
  }
  return { collector: c, tasks, drain, dir }
}

const agentOf = (session) => ({ session })

const assistantMessage = (id, seq, time, usage) => ({
  type: 'assistant/message',
  seq,
  time,
  data: {
    message: { role: 'assistant', id, source: { provider: 'p', model: 'm' } },
    ...(usage === undefined ? {} : { usage }),
  },
})

const USAGE = { inputTokens: 10, outputTokens: 100, totalTokens: 110 }
const NOW = Date.UTC(2026, 10, 12, 12, 0, 0)

test('notice on the same stack attaches timing to the staged event (normal request)', async () => {
  const { collector, drain } = await collectorWithMicrotasks()
  const session = { id: 's-1', header: { id: 's-1' } }
  const done = collector.handle(session, assistantMessage('m-1', 5, NOW, USAGE))
  collector.receive({ agent: agentOf(session), notice: { seq: 5, timing: { requestStartedAt: 0, firstOutputAt: 8000, lastOutputAt: 10000 } } })
  await drain()
  await done
  const [event] = collector.queue.peek(1)
  assert.equal(event.generation_duration_ms, 2000)
  assert.equal(event.ttft_ms, 8000)
  assert.deepEqual(event.usage.output_tokens, { value: 100, quality: 'known' })
  assert.equal(event.source_event_id, 'dsh:msg:m-1')
  await collector.stop()
})

test('compaction summary takes its own timing', async () => {
  const { collector, drain } = await collectorWithMicrotasks()
  const session = { id: 's-1', header: { id: 's-1' } }
  const done = collector.handle(session, {
    type: 'compaction/summary',
    seq: 21,
    time: NOW,
    data: { compactionId: 'c-1', provider: 'p', model: 'm' },
  })
  collector.receive({ agent: agentOf(session), notice: { seq: 21, timing: { requestStartedAt: 100, firstOutputAt: 500, lastOutputAt: 1500 } } })
  await drain()
  await done
  const [event] = collector.queue.peek(1)
  assert.equal(event.source_event_id, 'dsh:compaction:s-1:c-1')
  assert.equal(event.ttft_ms, 400)
  assert.equal(event.generation_duration_ms, 1000)
  // no usage: still reported, counts unknown rather than zero
  assert.equal(event.usage.input_tokens.quality, 'missing')
  assert.equal(event.usage.output_tokens.value, null)
  await collector.stop()
})

test('retry: only the final successful seq carries timing; the failed attempt reports nothing new', async () => {
  const { collector, drain } = await collectorWithMicrotasks()
  const session = { id: 's-1', header: { id: 's-1' } }
  // the failed attempt never produced a committed message event, only the retry did
  const done = collector.handle(session, assistantMessage('m-final', 9, NOW, USAGE))
  collector.receive({ agent: agentOf(session), notice: { seq: 9, timing: { requestStartedAt: 50_000, firstOutputAt: 51_000, lastOutputAt: 53_000 } } })
  await drain()
  await done
  const items = collector.queue.peek(10)
  assert.equal(items.length, 1)
  // timing belongs to the retry only: TTFT excludes the failed attempt's wait
  assert.equal(items[0].ttft_ms, 1000)
  assert.equal(items[0].generation_duration_ms, 2000)
  await collector.stop()
})

test('two agents publish interleaved and identically-numbered seqs without crossing', async () => {
  const { collector, drain } = await collectorWithMicrotasks()
  const a = { id: 'shared-id', header: { id: 'shared-id' } }
  const b = { id: 'shared-id', header: { id: 'shared-id' } } // same string id, different object
  const doneA = collector.handle(a, assistantMessage('m-a', 3, NOW, USAGE))
  const doneB = collector.handle(b, assistantMessage('m-b', 3, NOW, USAGE))
  // out-of-order notices, and the same seq number for both
  collector.receive({ agent: agentOf(b), notice: { seq: 3, timing: { requestStartedAt: 0, firstOutputAt: 2000, lastOutputAt: 4000 } } })
  collector.receive({ agent: agentOf(a), notice: { seq: 3, timing: { requestStartedAt: 0, firstOutputAt: 500, lastOutputAt: 1500 } } })
  await drain()
  await Promise.all([doneA, doneB])
  const byId = new Map(collector.queue.peek(10).map((e) => [e.source_event_id, e]))
  assert.equal(byId.get('dsh:msg:m-a').ttft_ms, 500)
  assert.equal(byId.get('dsh:msg:m-a').generation_duration_ms, 1000)
  assert.equal(byId.get('dsh:msg:m-b').ttft_ms, 2000)
  assert.equal(byId.get('dsh:msg:m-b').generation_duration_ms, 2000)
  await collector.stop()
})

test('a duplicate notice for an already-matched seq does not overwrite it; one event queued', async () => {
  const { collector, drain } = await collectorWithMicrotasks()
  const session = { id: 's-1', header: { id: 's-1' } }
  const done = collector.handle(session, assistantMessage('m-1', 4, NOW, USAGE))
  collector.receive({ agent: agentOf(session), notice: { seq: 4, timing: { requestStartedAt: 0, firstOutputAt: 100, lastOutputAt: 200 } } })
  collector.receive({ agent: agentOf(session), notice: { seq: 4, timing: { requestStartedAt: 0, firstOutputAt: 700, lastOutputAt: 1200 } } })
  await drain()
  await done
  const items = collector.queue.peek(10)
  assert.equal(items.length, 1)
  assert.equal(items[0].ttft_ms, 100) // the first match is consumed; a duplicate is ignored
  await collector.stop()
})

test('old host (no notice at all): reports the original v1 shape, no extra keys', async () => {
  const { collector, drain, tasks } = await collectorWithMicrotasks()
  const session = { id: 's-1', header: { id: 's-1' } }
  const done = collector.handle(session, assistantMessage('m-1', 5, NOW, USAGE))
  assert.equal(tasks.length, 1) // settling is scheduled at once, never a wait for a notice
  await drain()
  await done
  const [event] = collector.queue.peek(1)
  assert.ok(!('generation_duration_ms' in event) && !('ttft_ms' in event))
  assert.deepEqual(event.usage.input_tokens, { value: 10, quality: 'known' })
  await collector.stop()
})

test('late notice after the microtask is ignored and never attaches to a later request', async () => {
  const { collector, drain } = await collectorWithMicrotasks()
  const session = { id: 's-1', header: { id: 's-1' } }
  const first = collector.handle(session, assistantMessage('m-1', 5, NOW, USAGE))
  await drain()
  await first
  collector.receive({ agent: agentOf(session), notice: { seq: 5, timing: { requestStartedAt: 0, firstOutputAt: 8000, lastOutputAt: 10000 } } })
  const second = collector.handle(session, assistantMessage('m-2', 6, NOW, USAGE))
  await drain()
  await second
  const [a, b] = collector.queue.peek(10)
  assert.ok(!('ttft_ms' in a))
  assert.ok(!('ttft_ms' in b))
  assert.equal(collector.queue.size, 2)
  await collector.stop()
})

test('notice for an unknown seq, a foreign session or a malformed payload is ignored', async () => {
  const { collector, drain } = await collectorWithMicrotasks()
  const session = { id: 's-1', header: { id: 's-1' } }
  const other = { id: 's-1', header: { id: 's-1' } }
  const done = collector.handle(session, assistantMessage('m-1', 5, NOW, USAGE))
  collector.receive({ agent: agentOf(other), notice: { seq: 5, timing: { requestStartedAt: 0, firstOutputAt: 8000, lastOutputAt: 10000 } } })
  collector.receive({ agent: agentOf(session), notice: { seq: 99, timing: { requestStartedAt: 0, firstOutputAt: 8000, lastOutputAt: 10000 } } })
  collector.receive({ agent: agentOf(session), notice: { seq: '5', timing: {} } })
  collector.receive({ agent: agentOf(session) })
  collector.receive(undefined)
  collector.receive({ agent: null, notice: { seq: 5 } })
  await drain()
  await done
  const [event] = collector.queue.peek(1)
  assert.ok(!('ttft_ms' in event))
  assert.equal(collector.queue.size, 1)
  await collector.stop()
})

test('event without seq or without timing still reports; timing unknown stays omitted', async () => {
  const { collector, drain } = await collectorWithMicrotasks()
  const session = { id: 's-1', header: { id: 's-1' } }
  const noSeq = assistantMessage('m-1', undefined, NOW, USAGE)
  const doneA = collector.handle(session, noSeq)
  collector.receive({ agent: agentOf(session), notice: { seq: undefined, timing: { requestStartedAt: 0, firstOutputAt: 1, lastOutputAt: 2 } } })
  const doneB = collector.handle(session, assistantMessage('m-2', 6, NOW, USAGE))
  // notice with an unusable timing: mapped to no fields, event still reported
  collector.receive({ agent: agentOf(session), notice: { seq: 6, timing: { requestStartedAt: 'x' } } })
  await drain()
  await Promise.all([doneA, doneB])
  const items = collector.queue.peek(10)
  assert.equal(items.length, 2)
  assert.ok(items.every((e) => !('ttft_ms' in e) && !('generation_duration_ms' in e)))
  await collector.stop()
})

test('a request with timing but no usage keeps TTFT through queue → upload body', async () => {
  const sent = []
  const { collector, drain } = await collectorWithMicrotasks({
    endpoint: 'http://127.0.0.1:9/api/v1/ingestion/events',
    getToken: () => 'tpk_x',
    fetch: async (url, init) => {
      sent.push(JSON.parse(init.body))
      return new Response(JSON.stringify({ request_id: 'r', results: [{ index: 0, status: 'accepted' }] }), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      })
    },
  })
  const session = { id: 's-1', header: { id: 's-1' } }
  const done = collector.handle(session, assistantMessage('m-1', 5, NOW, undefined))
  collector.receive({ agent: agentOf(session), notice: { seq: 5, timing: { requestStartedAt: 0, firstOutputAt: 8000, lastOutputAt: 10000 } } })
  await drain()
  await done
  assert.equal((await collector.flush()).state, 'idle')
  const [event] = sent[0].events
  assert.equal(event.ttft_ms, 8000)
  assert.equal(event.generation_duration_ms, 2000)
  assert.equal(event.usage.output_tokens.value, null)
  assert.equal(event.usage.output_tokens.quality, 'missing')
  await collector.stop()
})

test('stop() writes a batch that was received before the stop, even if its microtask never ran', async () => {
  const { collector, drain, dir } = await collectorWithMicrotasks()
  const session = { id: 's-1', header: { id: 's-1' } }
  const done = collector.handle(session, assistantMessage('m-1', 5, NOW, USAGE))
  await collector.stop() // the host unloads before the staged microtask runs
  await done // settled, not left hanging
  const saved = JSON.parse(await readFile(path.join(dir, 'queue.json'), 'utf8'))
  assert.deepEqual(saved.items.map((e) => e.source_event_id), ['dsh:msg:m-1'])
  // after the stop both listeners are closed: a late event is ignored, even once the leftover
  // microtask runs
  await collector.handle(session, assistantMessage('m-2', 6, NOW, USAGE))
  await drain()
  assert.equal(collector.queue.size, 1)
})

test('handle() never rejects, even for an event that breaks the mapper', async () => {
  const { collector, drain } = await collectorWithMicrotasks()
  const poisoned = assistantMessage('m-1', 5, NOW, USAGE)
  Object.defineProperty(poisoned.data, 'message', { get() { throw new Error('boom') } })
  const done = collector.handle({ id: 's-1', header: { id: 's-1' } }, poisoned)
  await drain()
  await done
  assert.equal(collector.queue.size, 0)
  await collector.stop()
})

test('notice arriving before the Session event has nothing to attach to: reported without timing', async () => {
  const { collector, drain } = await collectorWithMicrotasks()
  const session = { id: 's-1', header: { id: 's-1' } }
  // The documented order is event-then-notice; a notice that comes first cannot be retro-fitted.
  collector.receive({ agent: agentOf(session), notice: { seq: 4, timing: { requestStartedAt: 0, firstOutputAt: 8000, lastOutputAt: 10000 } } })
  const done = collector.handle(session, assistantMessage('m-1', 4, NOW, USAGE))
  await drain()
  await done
  const [event] = collector.queue.peek(1)
  assert.ok(!('ttft_ms' in event) && !('generation_duration_ms' in event))
  assert.equal(collector.queue.size, 1)
  await collector.stop()
})

/**
 * A collector whose queue load genuinely has not finished: `start()` is called but its promise is
 * still pending, so `drain()` really awaits. This is the shape a slow `queue.json` read produces.
 */
async function loadingCollector() {
  const tasks = []
  const dir = await tmp()
  let releaseLoad
  const gate = new Promise((resolve) => { releaseLoad = resolve })
  const collector = new Collector({
    stateDir: dir, endpoint: null, getToken: () => undefined, logger: logs(),
    setTimer: () => 1, clearTimer: () => {},
    microtask: (fn) => { tasks.push(fn) },
  })
  const realLoad = collector.queue.load.bind(collector.queue)
  collector.queue.load = () => gate.then(() => realLoad())
  const started = collector.start() // deliberately not awaited: still loading
  return { collector, tasks, releaseLoad, started, dir }
}

test('delayed queue load: the microtask cutoff keeps the same-stack notice and ignores a later one', async () => {
  const { collector, tasks, releaseLoad, started } = await loadingCollector()
  const session = { id: 's-1', header: { id: 's-1' } }
  const done = collector.handle(session, assistantMessage('m-1', 5, NOW, USAGE))
  // published on the same stack as the Session event: must be kept
  collector.receive({ agent: agentOf(session), notice: { seq: 5, timing: { requestStartedAt: 0, firstOutputAt: 800, lastOutputAt: 1800 } } })
  assert.equal(tasks.length, 1)
  tasks.shift()() // the single microtask starts; its drain is now awaiting the pending load
  // after the microtask has started, a late notice must not reach this batch
  collector.receive({ agent: agentOf(session), notice: { seq: 5, timing: { requestStartedAt: 0, firstOutputAt: 9000, lastOutputAt: 12000 } } })
  releaseLoad()
  await started
  await done
  const [event] = collector.queue.peek(1)
  assert.equal(event.ttft_ms, 800)
  assert.equal(event.generation_duration_ms, 1000)
  await collector.stop()
})

test('delayed queue load: an event with no same-stack notice reports without timing', async () => {
  const { collector, tasks, releaseLoad, started } = await loadingCollector()
  const session = { id: 's-1', header: { id: 's-1' } }
  const done = collector.handle(session, assistantMessage('m-1', 5, NOW, USAGE))
  assert.equal(tasks.length, 1)
  tasks.shift()()
  collector.receive({ agent: agentOf(session), notice: { seq: 5, timing: { requestStartedAt: 0, firstOutputAt: 800, lastOutputAt: 1800 } } })
  releaseLoad()
  await started
  await done
  const [event] = collector.queue.peek(1)
  assert.ok(!('ttft_ms' in event) && !('generation_duration_ms' in event))
  await collector.stop()
})

test('notice arriving while a persist is in flight cannot attach to another event of the batch', async () => {
  const { collector, tasks } = await collectorWithMicrotasks()
  const session = { id: 's-1', header: { id: 's-1' } }
  const first = collector.handle(session, assistantMessage('m-a', 5, NOW, USAGE))
  const second = collector.handle(session, assistantMessage('m-b', 6, NOW, USAGE))
  // both notices arrive on the same stack as their Session events
  collector.receive({ agent: agentOf(session), notice: { seq: 5, timing: { requestStartedAt: 0, firstOutputAt: 500, lastOutputAt: 1500 } } })
  // hold the first entry's write so the batch is mid-flight when the late notice shows up
  const realPersist = collector.queue.persist.bind(collector.queue)
  let release
  const gate = new Promise((resolve) => { release = resolve })
  let calls = 0
  collector.queue.persist = () => (++calls === 1 ? gate.then(() => realPersist()) : realPersist())
  assert.equal(tasks.length, 1)
  tasks.shift()()
  collector.receive({ agent: agentOf(session), notice: { seq: 6, timing: { requestStartedAt: 0, firstOutputAt: 9000, lastOutputAt: 12000 } } })
  release()
  await Promise.all([first, second])
  const byId = new Map(collector.queue.peek(10).map((e) => [e.source_event_id, e]))
  assert.equal(byId.get('dsh:msg:m-a').ttft_ms, 500)
  assert.ok(!('ttft_ms' in byId.get('dsh:msg:m-b')), 'the late notice must not attach')
  assert.ok(!('generation_duration_ms' in byId.get('dsh:msg:m-b')))
  await collector.stop()
})

test('stop() during a blocked write keeps both events of the batch (the old drain settled the second unqueued)', async () => {
  const { collector, tasks, dir } = await collectorWithMicrotasks()
  const session = { id: 's-1', header: { id: 's-1' } }
  const first = collector.handle(session, assistantMessage('m-a', 5, NOW, USAGE))
  collector.receive({ agent: agentOf(session), notice: { seq: 5, timing: { requestStartedAt: 0, firstOutputAt: 8000, lastOutputAt: 10000 } } })
  const second = collector.handle(session, assistantMessage('m-b', 6, NOW, USAGE))
  collector.receive({ agent: agentOf(session), notice: { seq: 6, timing: { requestStartedAt: 0, firstOutputAt: 2000, lastOutputAt: 4000 } } })
  // one batch, one write: hold it so the stop arrives while the batch is mid-write
  const realPersist = collector.queue.persist.bind(collector.queue)
  let release
  const gate = new Promise((resolve) => { release = resolve })
  collector.queue.persist = () => gate.then(() => realPersist())
  assert.equal(tasks.length, 1)
  tasks.shift()() // the batch starts; its write is now blocked
  let stopped = false
  const stopping = collector.stop().then(() => { stopped = true })
  await new Promise((resolve) => setImmediate(resolve))
  assert.equal(stopped, false, 'stop waits for the accepted batch instead of dropping it')
  release()
  await stopping
  await Promise.all([first, second])
  assert.deepEqual(collector.queue.peek(10).map((e) => e.source_event_id), ['dsh:msg:m-a', 'dsh:msg:m-b'])
  // both are on disk, so a reloaded instance can still 补报 them
  const saved = JSON.parse(await readFile(path.join(dir, 'queue.json'), 'utf8'))
  assert.deepEqual(saved.items.map((e) => e.source_event_id), ['dsh:msg:m-a', 'dsh:msg:m-b'])
  const reloaded = new Collector({
    stateDir: dir, endpoint: null, getToken: () => undefined, logger: logs(),
    setTimer: () => 1, clearTimer: () => {},
  })
  await reloaded.start()
  const byId = new Map(reloaded.queue.peek(10).map((e) => [e.source_event_id, e]))
  assert.equal(reloaded.queue.size, 2)
  assert.equal(byId.get('dsh:msg:m-a').ttft_ms, 8000)
  assert.equal(byId.get('dsh:msg:m-b').ttft_ms, 2000)
  await reloaded.stop()
})

test('stop() while the queue file is still loading writes the staged event before resolving', async () => {
  const { collector, tasks, releaseLoad, started, dir } = await loadingCollector()
  const session = { id: 's-1', header: { id: 's-1' } }
  const done = collector.handle(session, assistantMessage('m-1', 5, NOW, USAGE))
  collector.receive({ agent: agentOf(session), notice: { seq: 5, timing: { requestStartedAt: 0, firstOutputAt: 2000, lastOutputAt: 4000 } } })
  assert.equal(tasks.length, 1)
  let stopped = false
  const stopping = collector.stop().then(() => { stopped = true })
  await new Promise((resolve) => setImmediate(resolve))
  assert.equal(stopped, false, 'stop waits for the load and the write')
  releaseLoad()
  await stopping
  await started
  await done
  const [event] = collector.queue.peek(1)
  assert.equal(event.source_event_id, 'dsh:msg:m-1')
  assert.equal(event.ttft_ms, 2000)
  assert.equal(event.generation_duration_ms, 2000)
  const reloaded = new Collector({
    stateDir: dir, endpoint: null, getToken: () => undefined, logger: logs(),
    setTimer: () => 1, clearTimer: () => {},
  })
  await reloaded.start()
  assert.deepEqual(reloaded.queue.peek(10).map((e) => e.source_event_id), ['dsh:msg:m-1'])
  await reloaded.stop()
})

test('partially broken usage with valid timing keeps the known output in the real upload body', async () => {
  const sent = []
  const { collector, drain } = await collectorWithMicrotasks({
    endpoint: 'http://127.0.0.1:9/api/v1/ingestion/events',
    getToken: () => 'tpk_x',
    fetch: async (url, init) => {
      sent.push(JSON.parse(init.body))
      return new Response(JSON.stringify({ request_id: 'r', results: [{ index: 0, status: 'accepted' }, { index: 1, status: 'accepted' }] }), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      })
    },
  })
  const session = { id: 's-1', header: { id: 's-1' } }
  // normal request: inputTokens is missing, but outputTokens 100 is a real provider count
  const doneA = collector.handle(session, assistantMessage('m-1', 5, NOW, { outputTokens: 100 }))
  collector.receive({ agent: agentOf(session), notice: { seq: 5, timing: { requestStartedAt: 0, firstOutputAt: 8000, lastOutputAt: 10000 } } })
  // compaction: a malformed optional cache value must not cost the output either
  const doneB = collector.handle(session, {
    type: 'compaction/summary',
    seq: 21,
    time: NOW,
    data: { compactionId: 'c-1', provider: 'p', model: 'm', usage: { inputTokens: 10, outputTokens: 100, cacheReadTokens: 'x', totalTokens: 110 } },
  })
  collector.receive({ agent: agentOf(session), notice: { seq: 21, timing: { requestStartedAt: 0, firstOutputAt: 2000, lastOutputAt: 4000 } } })
  await drain()
  await Promise.all([doneA, doneB])
  assert.equal((await collector.flush()).state, 'idle')
  const byId = new Map(sent.flatMap((body) => body.events).map((e) => [e.source_event_id, e]))
  const normal = byId.get('dsh:msg:m-1')
  assert.equal(normal.ttft_ms, 8000)
  assert.equal(normal.generation_duration_ms, 2000)
  assert.deepEqual(normal.usage.output_tokens, { value: 100, quality: 'known' })
  assert.deepEqual(normal.usage.input_tokens, { value: null, quality: 'missing' }) // unknown, never 0
  const summary = byId.get('dsh:compaction:s-1:c-1')
  assert.equal(summary.ttft_ms, 2000)
  assert.equal(summary.generation_duration_ms, 2000)
  assert.deepEqual(summary.usage.output_tokens, { value: 100, quality: 'known' })
  assert.deepEqual(summary.usage.input_tokens, { value: null, quality: 'missing' })
  assert.deepEqual(summary.usage.cache_read_tokens, { value: null, quality: 'missing' })
  await collector.stop()
})

test('a confirmed reasoning count survives an unusable output in the real upload body', async () => {
  const sent = []
  const { collector, drain } = await collectorWithMicrotasks({
    endpoint: 'http://127.0.0.1:9/api/v1/ingestion/events',
    getToken: () => 'tpk_x',
    fetch: async (url, init) => {
      sent.push(JSON.parse(init.body))
      return new Response(JSON.stringify({ request_id: 'r', results: [{ index: 0, status: 'accepted' }] }), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      })
    },
  })
  const session = { id: 's-1', header: { id: 's-1' } }
  // input and output are unusable, but the reasoning count is a real provider observation
  const done = collector.handle(session, assistantMessage('m-r', 7, NOW, { inputTokens: -1, outputTokens: -1, reasoningTokens: 30 }))
  collector.receive({ agent: agentOf(session), notice: { seq: 7, timing: { requestStartedAt: 0, firstOutputAt: 8000, lastOutputAt: 10000 } } })
  await drain()
  await done
  assert.equal((await collector.flush()).state, 'idle')
  const events = sent.flatMap((body) => body.events)
  assert.equal(events.length, 1)
  const [event] = events
  assert.equal(event.source_event_id, 'dsh:msg:m-r')
  assert.equal(event.ttft_ms, 8000)
  assert.equal(event.generation_duration_ms, 2000)
  assert.deepEqual(event.usage.reasoning_tokens, { value: 30, quality: 'known' })
  assert.deepEqual(event.usage.output_tokens, { value: null, quality: 'missing' })
  assert.deepEqual(event.usage.input_tokens, { value: null, quality: 'missing' })
  await collector.stop()
})
