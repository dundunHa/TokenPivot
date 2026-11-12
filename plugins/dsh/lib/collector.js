/**
 * Wiring between DSH host events and the queue/uploader, kept free of DSH imports so it can be
 * tested with plain node:test.
 */
import os from 'node:os'
import path from 'node:path'
import { mapSessionEvent } from './mapper.js'
import { FileQueue } from './queue.js'
import { Uploader } from './uploader.js'

export const INGESTION_PATH = '/api/v1/ingestion/events'

/** https only, except loopback for local development. Returns null if unusable. */
export function ingestionEndpoint(serverUrl) {
  if (typeof serverUrl !== 'string' || serverUrl.trim() === '') return null
  let url
  try {
    url = new URL(serverUrl.trim())
  } catch {
    return null
  }
  // Credentials in the URL would make fetch throw on every attempt (endless back-off) and keep a
  // secret in memory outside the token field.
  if (url.username || url.password) return null
  const loopback = ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname)
  if (url.protocol !== 'https:' && !(url.protocol === 'http:' && loopback)) return null
  url.pathname = url.pathname.replace(/\/+$/, '') + INGESTION_PATH
  url.search = ''
  url.hash = ''
  return url.toString()
}

export function expandHome(dir) {
  if (typeof dir !== 'string' || dir === '') return path.join(os.homedir(), '.tokenpivot', 'dsh')
  if (dir === '~') return os.homedir()
  if (dir.startsWith('~/')) return path.join(os.homedir(), dir.slice(2))
  return dir
}

/** Step starts waiting for their message; bounded so a cancelled step cannot leak memory. */
const MAX_PENDING_STEPS = 512

export class Collector {
  /**
   * @param {{
   *   stateDir: string, endpoint: string|null, getToken: () => string|undefined,
   *   projects?: {path: string, projectId: string}[], detectProject?: (cwd: string|undefined) => string|null, resolveCwd?: (session) => string|undefined,
   *   maxQueueItems?: number, flushDelayMs?: number, logger?: any, fetch?: Function,
   *   setTimer?: (fn: Function, ms: number) => any, clearTimer?: (handle: any) => void,
   *   microtask?: (fn: Function) => void, uploaderOptions?: object,
   * }} options
   */
  constructor(options) {
    this.options = options
    this.logger = options.logger
    this.queue = new FileQueue({ dir: options.stateDir, maxItems: options.maxQueueItems, logger: options.logger })
    this.uploader = new Uploader({
      queue: this.queue,
      endpoint: options.endpoint,
      getToken: options.getToken,
      fetch: options.fetch,
      logger: options.logger,
      ...options.uploaderOptions,
    })
    this.setTimer = options.setTimer ?? ((fn, ms) => setTimeout(fn, ms))
    this.clearTimer = options.clearTimer ?? ((h) => clearTimeout(h))
    this.microtask = options.microtask ?? ((fn) => queueMicrotask(fn))
    this.flushDelayMs = options.flushDelayMs ?? 2000
    this.timer = null
    this.timerDue = Infinity
    this.stopped = false
    this.ready = null
    /** Final events waiting for their timing notice, in arrival order. */
    this.waiting = []
    /** session object → Map(final event seq → waiting entry); identity, never a string id. */
    this.pendingBySession = new WeakMap()
    /** Batches being mapped and written; `stop()` waits for every one of them. */
    this.batches = new Set()
    this.flushScheduled = false
    /** session id + turn + step → ms epoch when that model call started (in memory only). */
    this.steps = new Map()
  }

  stepKey(session, data) {
    const id = session?.id ?? session?.header?.id
    if (typeof id !== 'string' || !Number.isInteger(data?.turn) || !Number.isInteger(data?.step)) return null
    return `${id}:${data.turn}:${data.step}`
  }

  /** Remember when a model call started, or restarted after a failed attempt; evict the oldest beyond the cap. */
  noteStep(key, time) {
    if (key === null || !Number.isFinite(time)) return
    this.steps.delete(key)
    this.steps.set(key, time)
    if (this.steps.size > MAX_PENDING_STEPS) this.steps.delete(this.steps.keys().next().value)
  }

  /**
   * Load persisted events and try to send anything left over from a previous run (补报).
   * A failed load is not fatal: new events are still queued in memory, and every later persist
   * retries the load first (FileQueue never overwrites a file it could not read).
   */
  start() {
    this.ready = this.queue.load().then(
      () => { if (this.queue.size > 0) this.schedule(0) },
      (error) => { this.logger?.warn?.(`cannot load queue (${error?.code ?? error?.message ?? error}); will retry on next write`) },
    )
    return this.ready
  }

  /**
   * session/event listener body. Must not throw into DSH; the returned promise settles once this
   * event is queued (or skipped), never rejects.
   *
   * The host commits the final `session/event` first and then publishes `agent/generation-timing`
   * synchronously on the same stack, so this method stages the event synchronously — before its
   * first `await` — and settles the whole batch in one microtask. A host that never publishes the
   * notice therefore costs no wait: the staged event is reported without timing as before.
   *
   * `step/start` and `assistant/attempt` only maintain the in-memory step clock the request
   * duration is measured from; they never produce an event of their own.
   */
  handle(session, event) {
    if (this.stopped) return Promise.resolve()
    if (event?.type === 'step/start') {
      this.noteStep(this.stepKey(session, event.data), event.time)
      return Promise.resolve()
    }
    if (event?.type === 'assistant/attempt') {
      // A failed attempt settled; the next attempt of the same step starts after it.
      const key = this.stepKey(session, event.data)
      if (key !== null && this.steps.has(key)) this.noteStep(key, event.time)
      return Promise.resolve()
    }
    if (event?.type !== 'assistant/message' && event?.type !== 'compaction/summary') return Promise.resolve()
    // The step clock is read synchronously, before the batch is frozen in the microtask below.
    let startedAt
    if (event.type === 'assistant/message') {
      const key = this.stepKey(session, event.data)
      startedAt = key === null ? undefined : this.steps.get(key)
      if (key !== null) this.steps.delete(key)
    }
    let done
    const settled = new Promise((resolve) => { done = resolve })
    const entry = { session, event, timing: null, seq: event.seq, startedAt, done }
    // Staged synchronously: the notice for this event may arrive on this very stack.
    this.waiting.push(entry)
    this.link(entry)
    if (!this.flushScheduled) {
      this.flushScheduled = true
      this.microtask(() => { void this.drain() })
    }
    return settled
  }

  /**
   * Make a staged entry reachable by a notice: keyed by the Session object and the final event seq.
   * Only a real Session object can be a WeakMap key and only it can be matched by a notice.
   */
  link(entry) {
    if (entry.session === null || typeof entry.session !== 'object' || !Number.isSafeInteger(entry.seq)) return
    let bySeq = this.pendingBySession.get(entry.session)
    if (!bySeq) {
      bySeq = new Map()
      this.pendingBySession.set(entry.session, bySeq)
    }
    bySeq.set(entry.seq, entry)
  }

  /**
   * Close one entry's association so no later notice can attach to it. Idempotent, and it only
   * removes the key when that key still names this entry (a duplicate seq may have replaced it).
   */
  close(entry) {
    if (entry.session === null || typeof entry.session !== 'object' || !Number.isSafeInteger(entry.seq)) return
    const bySeq = this.pendingBySession.get(entry.session)
    if (!bySeq) return
    if (bySeq.get(entry.seq) !== entry) return
    bySeq.delete(entry.seq)
    if (bySeq.size === 0) this.pendingBySession.delete(entry.session)
  }

  /**
   * `agent/generation-timing` listener body: attach the notice to the staged event of the same
   * agent session and seq. A notice that matches nothing (late, unknown, or a duplicate seq that
   * already flushed) is ignored so it can never attach to a later request.
   * @param {{agent?: {session?: object}, notice?: {seq?: number, timing?: object}}} payload
   */
  receive(payload) {
    if (this.stopped) return
    const session = payload?.agent?.session
    const notice = payload?.notice
    if (!session || typeof session !== 'object' || !notice) return
    if (!Number.isSafeInteger(notice.seq)) return
    const bySeq = this.pendingBySession.get(session)
    const entry = bySeq?.get(notice.seq)
    if (!entry) return
    bySeq.delete(notice.seq)
    if (bySeq.size === 0) this.pendingBySession.delete(session)
    entry.timing = notice.timing ?? null
  }

  /**
   * One microtask: freeze this batch's timing, then map and queue every staged event in one write.
   *
   * The cutoff is this method's synchronous prologue: the batch is taken and every association is
   * closed BEFORE the first `await`, so a notice that arrives while the queue is still loading or
   * while an earlier write is in flight can no longer reach a batched entry. The waits below only
   * delay the write, never the timing decision.
   */
  async drain() {
    this.flushScheduled = false
    const entries = this.waiting
    this.waiting = []
    // Synchronous cutoff: close every association of this batch before any `await`.
    for (const entry of entries) this.close(entry)
    if (entries.length === 0) return
    await this.track(this.write(entries))
  }

  /** Register one batch so `stop()` can wait for it to be written. */
  track(batch) {
    this.batches.add(batch)
    void batch.then(() => this.batches.delete(batch), () => this.batches.delete(batch))
    return batch
  }

  /**
   * Map and queue one frozen batch, then write it once.
   *
   * Nothing here consults `stopped`: a batch that was received before the stop is still written,
   * otherwise the queue on disk would miss events this instance had already accepted and the
   * reloaded instance could never report them. Mapping and queueing are per entry, so one broken
   * event never costs the rest of the batch.
   */
  async write(entries) {
    let queued = 0
    try {
      try {
        await this.ready
      } catch {
        // The queue load failed; enqueue in memory anyway (every later persist retries the load).
      }
      for (const entry of entries) {
        try {
          const usageEvent = mapSessionEvent(entry.session, entry.event, {
            cwd: this.options.resolveCwd?.(entry.session),
            projects: this.options.projects,
            detectProject: this.options.detectProject,
            startedAt: entry.startedAt,
          }, entry.timing ?? undefined)
          if (usageEvent && this.queue.enqueue(usageEvent)) queued += 1
        } catch (error) {
          this.logger?.warn?.(`skipped ${entry.event?.type}: ${error?.message ?? error}`)
        }
      }
      if (queued > 0) {
        this.schedule(this.flushDelayMs) // no wake-up once stopping: the queue stays for 补报
        try {
          await this.queue.persist()
        } catch (error) {
          // Disk trouble is not fatal: the events stay in memory and the next write retries.
          this.logger?.warn?.(`cannot write queue: ${error?.message ?? error}`)
        }
      }
    } finally {
      // Settle even when the write failed: handle() never rejects and never hangs on disk errors.
      for (const entry of entries) entry.done()
    }
  }

  /** Keep the earliest pending wake-up; a later request never postpones an earlier one. */
  schedule(delayMs) {
    if (this.stopped) return
    const due = Date.now() + delayMs
    if (this.timer && due >= this.timerDue) return
    if (this.timer) this.clearTimer(this.timer)
    this.timerDue = due
    this.timer = this.setTimer(() => {
      this.timer = null
      this.timerDue = Infinity
      void this.flush()
    }, delayMs)
  }

  async flush() {
    let outcome
    try {
      outcome = await this.uploader.flush()
    } catch (error) {
      this.logger?.warn?.(`flush failed: ${error?.message ?? error}`)
      outcome = this.uploader.backoff()
    }
    if (outcome.state === 'backoff') this.schedule(outcome.delayMs)
    // New events arriving during an in-flight send are picked up by their own schedule() call.
    return outcome
  }

  /**
   * Resolves once every batch received before the stop is queued and written, no request is in
   * flight and the last write is on disk, so a reloaded instance can take over.
   *
   * A stop never drops what was already accepted: events staged before the stop (even ones whose
   * microtask has not run yet, or whose batch is still waiting for the queue file to load or for an
   * earlier write) are mapped and written here, and the batches already in flight are awaited.
   * Only events arriving after the stop are ignored, by `handle()`/`receive()`.
   */
  async stop() {
    this.stopped = true
    if (this.timer) this.clearTimer(this.timer)
    this.timer = null
    this.timerDue = Infinity
    const staged = this.waiting
    this.waiting = []
    for (const entry of staged) this.close(entry)
    if (staged.length > 0) await this.track(this.write(staged)).catch(() => {})
    await Promise.all([...this.batches].map((batch) => batch.catch(() => {})))
    await this.ready?.catch(() => {})
    await this.uploader.stop()
    await this.queue.writing.catch(() => {})
  }
}
