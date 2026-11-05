/**
 * 上报：POST /api/v1/ingestion/events（Bearer 凭证），逐项应用结果。
 *
 * 结果分类（docs/openapi/openapi.yaml 的 ItemResult）：
 *   accepted / duplicate            → 确认出队
 *   conflict / expired / rejected   → 写死信后出队（同身份原内容不可能再成功）
 *   rejected + FUTURE_EVENT         → 保留并退避（服务端零时钟偏差，等一下就能过）
 *   retryable / 未知状态 / 缺结果    → 保留并退避，绝不推断成功
 *   HTTP 413 多条                   → 对半拆批；单条仍超限 → 死信
 *   HTTP 400/401/403/415            → 暂停自动发送，保持队列
 *   HTTP 429 / 其它 4xx·5xx / 网络  → 保留并按 Retry-After 或指数退避
 *   3xx（不跟随重定向）              → 保留并退避
 *
 * 安全：Authorization 只发往配置端点；不跟随重定向；日志只含固定错误码、计数与 HTTP status。
 */
const MAX_EVENTS = 100
const MAX_BODY_BYTES = 1024 * 1024
/** 留 1 KiB 余量给 {"events":[...]} 外壳。 */
const BODY_BUDGET = MAX_BODY_BYTES - 1024

const DEQUEUE = new Set(['accepted', 'duplicate'])
const TERMINAL = new Set(['conflict', 'expired', 'rejected'])
const PAUSE_STATUSES = new Set([400, 401, 403, 415])

/** Retry-After：delta-seconds 或 HTTP-date，返回毫秒或 undefined。 */
export function parseRetryAfter(value, now = Date.now()) {
  if (typeof value !== 'string' || value.trim() === '') return undefined
  const v = value.trim()
  if (/^\d+$/.test(v)) return Number(v) * 1000
  const at = Date.parse(v)
  return Number.isNaN(at) ? undefined : Math.max(0, at - now)
}

export class Uploader {
  /**
   * @param {{
   *   queue: import('./queue.js').SqliteQueue,
   *   getEndpoint: () => string|null,
   *   getToken: () => string|undefined,
   *   fetch?: Function, logger?: {log: Function},
   *   timeoutMs?: number, batchSize?: number, baseDelayMs?: number, maxDelayMs?: number,
   *   now?: () => number, random?: () => number,
   *   isBindingCurrent?: () => boolean,
   * }} options
   */
  constructor(options) {
    this.queue = options.queue
    this.getEndpoint = options.getEndpoint
    this.getToken = options.getToken
    this.fetch = options.fetch ?? globalThis.fetch
    this.logger = options.logger
    this.timeoutMs = options.timeoutMs ?? 30000
    this.batchSize = Math.min(Math.max(1, options.batchSize ?? MAX_EVENTS), MAX_EVENTS)
    this.baseDelayMs = options.baseDelayMs ?? 1000
    this.maxDelayMs = options.maxDelayMs ?? 5 * 60 * 1000
    this.now = options.now ?? Date.now
    this.random = options.random ?? Math.random
    // 响应确认前再对比一次绑定与配置：期间被改过就不落结果，整批保留等下一轮。
    this.isBindingCurrent = options.isBindingCurrent ?? (() => true)
    this.limit = this.batchSize
    this.failures = 0
    this.paused = false
    this.pauseReason = null
    this.stopped = false
    this.running = null
    this.controller = null
  }

  /** 清暂停（配置检查或用户确认之后调用）。 */
  resume() {
    this.paused = false
    this.pauseReason = null
    this.failures = 0
  }

  pause(reason) {
    this.paused = true
    this.pauseReason = reason
    this.logger?.log('upload_paused', { reason })
    return { state: 'paused', reason }
  }

  backoff(retryAfterMs) {
    this.failures += 1
    const exp = Math.min(this.maxDelayMs, this.baseDelayMs * 2 ** Math.min(this.failures - 1, 20))
    // 全抖动避免多个 PI 进程在服务恢复时同时重试。
    const jittered = Math.round(exp / 2 + this.random() * (exp / 2))
    const delayMs = Math.max(jittered, retryAfterMs ?? 0)
    return { state: 'backoff', delayMs }
  }

  /** 停止调度：取消在途网络请求，未确认事件留在库里。 */
  stop() {
    this.stopped = true
    this.controller?.abort()
  }

  /** 排空队列，直到空、暂停或需要退避。并发调用共享同一次运行。 */
  flush() {
    if (!this.running) {
      this.running = this.drain().finally(() => {
        this.running = null
      })
    }
    return this.running
  }

  nextBatch() {
    const peeked = this.queue.peek(this.limit)
    if (peeked.busy || peeked.failed) return { items: [], busy: peeked.busy, failed: peeked.failed }
    const items = []
    let bytes = 2 // []
    for (const item of peeked.items) {
      const size = Buffer.byteLength(item.payloadText, 'utf8') + 1
      if (items.length > 0 && bytes + size > BODY_BUDGET) break
      items.push(item)
      bytes += size
    }
    return { items }
  }

  async drain() {
    if (this.stopped) return { state: 'stopped' }
    if (this.paused) return { state: 'paused', reason: this.pauseReason }
    const endpoint = this.getEndpoint()
    const token = this.getToken()
    if (!endpoint || !token) return { state: 'disabled' }
    let pending
    try {
      pending = this.queue.counts().pending
    } catch {
      return { state: 'failed', reason: 'queue_unavailable' }
    }
    if (pending === 0) return { state: 'idle' }

    while (!this.stopped) {
      const batch = this.nextBatch()
      if (batch.busy) return this.backoff()
      if (batch.failed) return { state: 'failed', reason: batch.failed }
      if (batch.items.length === 0) break
      const outcome = await this.send(batch.items, endpoint, token)
      if (outcome.state !== 'continue') return outcome
    }
    this.limit = this.batchSize
    this.failures = 0
    return { state: 'idle' }
  }

  body(items) {
    return `{"events":[${items.map((item) => item.payloadText).join(',')}]}`
  }

  async send(items, endpoint, token) {
    this.controller = new AbortController()
    const timer = setTimeout(() => this.controller?.abort(), this.timeoutMs)
    timer.unref?.()
    let response
    try {
      response = await this.fetch(endpoint, {
        method: 'POST',
        headers: {
          'content-type': 'application/json',
          accept: 'application/json',
          authorization: `Bearer ${token}`,
        },
        body: this.body(items),
        // 不跟随重定向：3xx 一律保留重试，Authorization 不会发往 Location。
        redirect: 'manual',
        signal: this.controller.signal,
      })
    } catch (error) {
      const aborted = this.controller?.signal?.aborted === true
      this.logger?.log('upload_failed', { reason: aborted ? 'timeout' : 'network', count: items.length })
      return this.backoff()
    } finally {
      clearTimeout(timer)
      this.controller = null
    }

    const status = response?.status
    if (status === 200) {
      let body
      try {
        body = await response.json()
      } catch {
        this.logger?.log('upload_malformed', { status })
        return this.backoff()
      }
      return this.applyResults(items, body)
    }

    // 读完响应体以复用连接，内容不记录。
    await response.text().catch(() => {})

    if (typeof status !== 'number') {
      this.logger?.log('upload_failed', { reason: 'no_status', count: items.length })
      return this.backoff()
    }
    if (status >= 300 && status < 400) {
      this.logger?.log('upload_redirect_refused', { status })
      return this.backoff()
    }
    if (PAUSE_STATUSES.has(status)) {
      return this.pause(`http_${status}`)
    }
    if (status === 413) {
      if (items.length > 1) {
        this.limit = Math.max(1, Math.floor(items.length / 2))
        this.logger?.log('upload_split', { status, count: items.length })
        return { state: 'continue' }
      }
      const item = items[0]
      const applied = this.queue.applyOutcome({
        dead: [{ id: item.id, outcome: 'http_413', code: null }],
      })
      if (applied.busy || applied.failed) return this.backoff()
      this.logger?.log('event_dead', { reason: 'payload_too_large', count: 1 })
      return { state: 'continue' }
    }
    this.logger?.log('upload_failed', { status, count: items.length })
    return this.backoff(parseRetryAfter(response.headers?.get?.('retry-after'), this.now()))
  }

  applyResults(items, body) {
    if (!this.isBindingCurrent()) {
      this.logger?.log('upload_binding_changed', { count: items.length })
      return this.backoff()
    }
    const results = Array.isArray(body?.results) ? body.results : null
    // 畸形 200：results 必须与输入等长、index 必须一一覆盖；否则整批保留，绝不推断成功。
    if (!results || results.length !== items.length) {
      this.logger?.log('upload_malformed', { status: 200 })
      return this.backoff()
    }
    const byIndex = new Map()
    for (const result of results) {
      const index = result?.index
      if (!Number.isInteger(index) || index < 0 || index >= items.length || byIndex.has(index)) {
        this.logger?.log('upload_malformed', { status: 200 })
        return this.backoff()
      }
      byIndex.set(index, result)
    }

    const remove = []
    const dead = []
    let keep = 0
    items.forEach((item, index) => {
      const result = byIndex.get(index)
      const status = result?.status
      if (DEQUEUE.has(status)) {
        remove.push(item.id)
      } else if (TERMINAL.has(status)) {
        const code = result?.error?.code
        if (status === 'rejected' && code === 'FUTURE_EVENT') {
          keep += 1
        } else if (status === 'rejected' && typeof code !== 'string') {
          keep += 1 // 永久拒绝却没给稳定码：不替服务端判死刑
        } else {
          remove.push(item.id)
          dead.push({ id: item.id, outcome: status, code: typeof code === 'string' ? code : null })
        }
      } else {
        keep += 1
      }
    })

    const applied = this.queue.applyOutcome({ remove, dead })
    if (applied.busy) return this.backoff()
    if (applied.failed) return { state: 'failed', reason: applied.reason }
    if (dead.length > 0) this.logger?.log('event_dead', { count: dead.length })
    if (remove.length > 0) this.logger?.log('upload_accepted', { count: remove.length })
    if (keep > 0) return this.backoff()
    this.failures = 0
    return { state: 'continue' }
  }
}
