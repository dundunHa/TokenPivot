/**
 * Sends queued UsageEvents to POST /api/v1/ingestion/events and applies per-item results.
 *
 * Result handling (docs/openapi/openapi.yaml):
 *   accepted / duplicate                  → dequeue
 *   conflict / expired / rejected         → dequeue + dead letter (resending the same body cannot change the outcome)
 *   retryable, missing result             → keep, back off
 *   HTTP 400 / 413 with >1 item           → halve the batch and retry (isolates the offending item)
 *   HTTP 400 / 413 with 1 item            → dequeue + dead letter
 *   HTTP 401 / 403 / 415                  → pause (credential or plugin defect; retrying would not help)
 *   HTTP 429 / 5xx / network / timeout    → keep, back off (honours Retry-After)
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
    this.running = null
  }

  /** Clear a pause (e.g. after the token was changed). */
  resume() {
    this.paused = false
    this.failures = 0
  }

  backoff(retryAfterMs) {
    this.failures += 1
    const exp = Math.min(this.maxDelayMs, this.baseDelayMs * 2 ** Math.min(this.failures - 1, 20))
    // Full jitter keeps many plugin instances from retrying in lock-step after an outage.
    const jittered = Math.round(exp / 2 + this.random() * (exp / 2))
    const delayMs = Math.min(this.maxDelayMs, Math.max(jittered, retryAfterMs ?? 0))
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
    if (this.paused) return { state: 'paused' }
    const token = this.getToken()
    if (!this.endpoint || !token) return { state: 'disabled' }

    while (this.queue.size > 0) {
      const batch = this.nextBatch()
      const outcome = await this.send(batch, token)
      if (outcome.state !== 'continue') return outcome
    }
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

    if (status === 401 || status === 403 || status === 415) {
      this.paused = true
      this.logger?.warn?.(`upload paused: HTTP ${status}; check the TokenPivot credential/plugin version`)
      return { state: 'paused' }
    }
    if (status === 400 || status === 413) {
      if (batch.length > 1) {
        this.limit = Math.max(1, Math.floor(batch.length / 2))
        return { state: 'continue' }
      }
      const id = batch[0].source_event_id
      this.queue.remove([id])
      await this.queue.persist()
      await this.queue.deadLetter([{ source_event_id: id, outcome: `http_${status}` }])
      this.logger?.warn?.(`event dropped after HTTP ${status}`)
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
      } else if (r && TERMINAL.has(r.status)) {
        remove.push(item.source_event_id)
        dead.push({ source_event_id: item.source_event_id, outcome: r.status, code: r.error?.code ?? null })
      } else {
        retry += 1 // retryable, unknown status, or result missing: keeping it is the safe side
      }
    })
    this.queue.remove(remove)
    await this.queue.persist()
    await this.queue.deadLetter(dead)
    if (dead.length > 0) this.logger?.warn?.(`${dead.length} event(s) not accepted (see dead-letter.jsonl)`)

    if (retry > 0) return this.backoff()
    this.failures = 0
    this.limit = this.batchSize
    return { state: 'continue' }
  }
}
