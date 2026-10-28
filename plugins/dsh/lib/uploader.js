/**
 * Sends queued UsageEvents to POST /api/v1/ingestion/events and applies per-item results.
 *
 * Result handling (docs/openapi/openapi.yaml):
 *   accepted / duplicate                  → dequeue
 *   conflict / expired / rejected         → dequeue + dead letter (resending the same body cannot change the outcome)
 *   rejected + FUTURE_EVENT               → keep, back off: the server has zero clock-skew tolerance, so a
 *                                           local clock slightly ahead is fixed by simply waiting
 *   retryable, missing result             → keep, back off
 *   HTTP 413 with >1 item                 → halve the batch and retry (isolates the oversized item)
 *   HTTP 413 with 1 item                  → dequeue + dead letter
 *   HTTP 400 / 401 / 403 / 415            → pause. Whole-batch 400 is about the request as a whole
 *                                           (envelope, unknown top-level field, plugin/server mismatch),
 *                                           never about one item, so splitting would only drop events.
 *                                           Resumes automatically when the configured token changes.
 *   HTTP 429 / other 4xx·5xx / network    → keep, back off (honours Retry-After, even beyond maxDelayMs)
 * The bearer token is never logged or persisted.
 */

const MAX_EVENTS = 100
const MAX_BODY_BYTES = 1024 * 1024
const BODY_BUDGET = MAX_BODY_BYTES - 1024 // headroom for {"events":[...]} framing
const encoder = new TextEncoder()

const DEQUEUE = new Set(['accepted', 'duplicate'])
const TERMINAL = new Set(['conflict', 'expired', 'rejected'])

/** Retry-After: delta-seconds or HTTP-date. Returns ms or undefined. */
export function parseRetryAfter(value, now = Date.now()) {
  if (typeof value !== 'string' || value.trim() === '') return undefined
  const v = value.trim()
  if (/^\d+$/.test(v)) return Number(v) * 1000
  const at = Date.parse(v)
  return Number.isNaN(at) ? undefined : Math.max(0, at - now)
}

export class Uploader {
  constructor({
    queue,
    endpoint,
    getToken,
    fetch: fetchImpl = globalThis.fetch,
    logger,
    batchSize = MAX_EVENTS,
    timeoutMs = 10000,
    baseDelayMs = 1000,
    maxDelayMs = 5 * 60 * 1000,
    random = Math.random,
    now = Date.now,
  }) {
    this.queue = queue
    this.endpoint = endpoint
    this.getToken = getToken
    this.fetch = fetchImpl
    this.logger = logger
    this.batchSize = Math.min(Math.max(1, batchSize), MAX_EVENTS)
    this.limit = this.batchSize
    this.timeoutMs = timeoutMs
    this.baseDelayMs = baseDelayMs
    this.maxDelayMs = maxDelayMs
    this.random = random
    this.now = now
    this.failures = 0
    this.paused = false
    this.pausedToken = undefined
    this.stopped = false
    this.running = null
  }

  /** Clear a pause (also happens automatically when the token changes). */
  resume() {
    this.paused = false
    this.pausedToken = undefined
    this.failures = 0
  }

  /** Stop after the in-flight request; returns when the current run is over. */
  async stop() {
    this.stopped = true
    await this.running?.catch(() => {})
  }

  pause(token) {
    this.paused = true
    this.pausedToken = token
    return { state: 'paused' }
  }

  backoff(retryAfterMs) {
    this.failures += 1
    const exp = Math.min(this.maxDelayMs, this.baseDelayMs * 2 ** Math.min(this.failures - 1, 20))
    // Full jitter keeps many plugin instances from retrying in lock-step after an outage.
    const jittered = Math.round(exp / 2 + this.random() * (exp / 2))
    // The server's Retry-After wins even when it exceeds our own cap.
    const delayMs = Math.max(jittered, retryAfterMs ?? 0)
    return { state: 'backoff', delayMs }
  }

  /** Drain the queue until empty, paused, or a retryable failure. Concurrent calls share one run. */
  flush() {
    if (!this.running) {
      this.running = this.drain().finally(() => { this.running = null })
    }
    return this.running
  }

  nextBatch() {
    const candidates = this.queue.peek(this.limit)
    const batch = []
    let bytes = 0
    for (const item of candidates) {
      const size = encoder.encode(JSON.stringify(item)).length + 1
      if (batch.length > 0 && bytes + size > BODY_BUDGET) break
      batch.push(item)
      bytes += size
    }
    return batch
  }

  async drain() {
    if (this.stopped) return { state: 'stopped' }
    const token = this.getToken()
    if (this.paused) {
      if (token === this.pausedToken) return { state: 'paused' }
      this.resume()
    }
    if (!this.endpoint || !token) return { state: 'disabled' }

    while (this.queue.size > 0) {
      if (this.stopped) return { state: 'stopped' }
      const batch = this.nextBatch()
      const outcome = await this.send(batch, token)
      if (outcome.state !== 'continue') return outcome
    }
    // Only an empty queue restores full batches; after a 413 split the rest keeps the smaller size.
    this.limit = this.batchSize
    this.failures = 0
    return { state: 'idle' }
  }

  async send(batch, token) {
    let response
    try {
      response = await this.fetch(this.endpoint, {
        method: 'POST',
        headers: {
          'content-type': 'application/json',
          accept: 'application/json',
          authorization: `Bearer ${token}`,
        },
        body: JSON.stringify({ events: batch }),
        signal: AbortSignal.timeout(this.timeoutMs),
      })
    } catch (error) {
      // Disconnect or timeout: the server may or may not have committed; resending the same
      // batch is safe because identity is (source, source_event_id).
      this.logger?.warn?.(`upload failed (${error?.name ?? 'Error'}); ${this.queue.size} event(s) kept for retry`)
      return this.backoff()
    }

    const status = response.status
    if (status === 200) {
      let body
      try {
        body = await response.json()
      } catch {
        this.logger?.warn?.('upload returned unreadable 200 body; will retry')
        return this.backoff()
      }
      return this.applyResults(batch, body)
    }

    // Drain the body so the connection can be reused; content is not logged.
    await response.text().catch(() => {})

    if (status === 400 || status === 401 || status === 403 || status === 415) {
      this.logger?.warn?.(`upload paused: HTTP ${status}; check the TokenPivot credential and plugin/server version (events stay queued)`)
      return this.pause(token)
    }
    if (status === 413) {
      if (batch.length > 1) {
        this.limit = Math.max(1, Math.floor(batch.length / 2))
        return { state: 'continue' }
      }
      const item = batch[0]
      this.queue.remove([item.source_event_id])
      await this.queue.persist()
      await this.queue.deadLetter([{ source_event_id: item.source_event_id, outcome: 'http_413', code: null, event: item }])
      this.logger?.warn?.('event dropped after HTTP 413 (single event exceeds the body limit)')
      return { state: 'continue' }
    }
    this.logger?.warn?.(`upload HTTP ${status}; ${this.queue.size} event(s) kept for retry`)
    return this.backoff(parseRetryAfter(response.headers.get('retry-after'), this.now()))
  }

  async applyResults(batch, body) {
    const results = Array.isArray(body?.results) ? body.results : []
    const byIndex = new Map()
    for (const r of results) {
      if (Number.isInteger(r?.index) && r.index >= 0 && r.index < batch.length) byIndex.set(r.index, r)
    }
    const remove = []
    const dead = []
    let retry = 0
    batch.forEach((item, index) => {
      const r = byIndex.get(index)
      if (r && DEQUEUE.has(r.status)) {
        remove.push(item.source_event_id)
      } else if (r && TERMINAL.has(r.status) && r.error?.code !== 'FUTURE_EVENT') {
        remove.push(item.source_event_id)
        dead.push({ source_event_id: item.source_event_id, outcome: r.status, code: r.error?.code ?? null, event: item })
      } else {
        retry += 1 // retryable, FUTURE_EVENT, unknown status or missing result: keeping it is the safe side
      }
    })
    this.queue.remove(remove)
    await this.queue.persist()
    await this.queue.deadLetter(dead)
    if (dead.length > 0) this.logger?.warn?.(`${dead.length} event(s) not accepted (see dead-letter.jsonl)`)

    if (retry > 0) return this.backoff()
    this.failures = 0
    return { state: 'continue' }
  }
}
