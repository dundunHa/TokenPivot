/**
 * 上报：POST /api/v1/ingestion/events（Bearer 凭证），逐项应用结果。
 *
 * 结果分类（docs/openapi/openapi.yaml 的 ItemResult）：
 *   accepted / duplicate            → 确认出队（不得带 error）
 *   conflict / expired              → 写死信后出队（不得带 error）
 *   rejected + 白名单错误码          → FUTURE_EVENT 保留退避，其余死信
 *   rejected 未知错误码 / 缺 error   → 畸形项：整批保留退避，不入库服务端字符串
 *   retryable                       → 保留并退避（错误码须在白名单内）
 *   未知状态 / 缺结果 / 数量不符     → 保留并退避，绝不推断成功
 *   HTTP 413 多条                   → 对半拆批；单条仍超限 → 死信
 *   HTTP 400/401/403/415            → 暂停自动发送，保持队列
 *   HTTP 429 / 其它 4xx·5xx / 网络  → 保留并按 Retry-After 或指数退避
 *   3xx（不跟随重定向）              → 保留并退避
 *
 * 凭证与超时：每批发车前重新读取当前地址与凭证；响应处理前再确认本批实际使用的
 * 地址/指纹/revision 仍是当前绑定，变了就整批保留并结束本轮 drain。
 * 超时覆盖到响应体读完为止：头已到而 body 挂起同样超时，也能被 stop() 打断。
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
/** 不得携带 error 的状态（OpenAPI：前四种状态没有 error 字段）。 */
const WITHOUT_ERROR = new Set(['accepted', 'duplicate', 'conflict', 'expired'])
/** 只有文档列出的错误码才允许落进死信；其余一律按畸形项保留。 */
const REJECTED_CODES = new Set([
  'INVALID_ARGUMENT',
  'FUTURE_EVENT',
  'PROJECT_INVALID',
  'USAGE_UNSUPPORTED',
  'USAGE_OVERFLOW',
  'NORMALIZATION_INCOMPATIBLE',
])
const RETRYABLE_CODES = new Set(['UNAVAILABLE', 'INTERNAL', 'DEADLINE_EXCEEDED'])
const DEAD_OUTCOME = { conflict: 'conflict', expired: 'expired', rejected: 'rejected' }

/**
 * 长度按 Unicode code point 计：OpenAPI 的 128/512 是字符数，
 * JS 的 .length 数 UTF-16 code unit（一个 emoji 算 2），会把合法值误判成超长。
 * 截断的 surrogate 在 for...of 里各算一个码点，不抛异常。
 */
function codePointLength(value) {
  let count = 0
  for (const _codePoint of value) count += 1
  return count
}

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
   *   identity: () => ({endpoint: string, token: string, fingerprint: string|null, revision: number|null}|null),
   *   isIdentityCurrent?: (snapshot: object) => boolean,
   *   fetch?: Function, logger?: {log: Function},
   *   timeoutMs?: number, batchSize?: number, baseDelayMs?: number, maxDelayMs?: number,
   *   now?: () => number, random?: () => number,
   * }} options
   */
  constructor(options) {
    this.queue = options.queue
    // 每批发送前调用：返回本批要用的地址与凭证快照；返回 null 表示当前不可发送。
    this.getIdentity = options.identity ?? (() => null)
    // 响应处理前调用：本批实际使用的地址/凭证是否仍是当前绑定。
    this.isIdentityCurrent = options.isIdentityCurrent ?? (() => true)
    this.fetch = options.fetch ?? globalThis.fetch
    this.logger = options.logger
    this.timeoutMs = options.timeoutMs ?? 30000
    this.batchSize = Math.min(Math.max(1, options.batchSize ?? MAX_EVENTS), MAX_EVENTS)
    this.baseDelayMs = options.baseDelayMs ?? 1000
    this.maxDelayMs = options.maxDelayMs ?? 5 * 60 * 1000
    this.now = options.now ?? Date.now
    this.random = options.random ?? Math.random
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

  /** 停止调度：取消在途网络请求（含挂起的响应体），未确认事件留在库里。 */
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
    if (!this.getIdentity()) return { state: 'unavailable' }
    let pending
    try {
      pending = this.queue.counts().pending
    } catch {
      return { state: 'failed', reason: 'queue_unavailable' }
    }
    if (pending === 0) return { state: 'idle' }

    while (!this.stopped) {
      if (this.paused) return { state: 'paused', reason: this.pauseReason }
      // 每批都重新读一次：在途期间凭证被轮换，后续批次不得沿用旧凭证。
      // 每批都重新读：配置与绑定在同一次读取里核对，不一致时 getIdentity() 返回 null。
      const identity = this.getIdentity()
      if (!identity) return { state: 'unavailable' }
      const batch = this.nextBatch()
      if (batch.busy) return this.backoff()
      if (batch.failed) return { state: 'failed', reason: batch.failed }
      if (batch.items.length === 0) break
      const outcome = await this.send(batch.items, identity)
      if (outcome.state !== 'continue') return outcome
    }
    this.limit = this.batchSize
    this.failures = 0
    return { state: 'idle' }
  }

  body(items) {
    return `{"events":[${items.map((item) => item.payloadText).join(',')}]}`
  }

  /** 收尾：超时、停止、网络失败都保留整批，不推断成功。 */
  failed(items) {
    const reason = this.stopped ? 'stopped' : this.timedOut ? 'timeout' : 'network'
    this.logger?.log('upload_failed', { reason, count: items.length })
    return this.stopped ? { state: 'stopped' } : this.backoff()
  }

  /**
   * 发一批。超时定时器一路覆盖到响应体读完；abort 同时能打断挂起的 body，
   * 因此 stop() 不会留下一个永远挂起的 Promise。
   */
  async send(items, identity) {
    const controller = new AbortController()
    this.controller = controller
    this.timedOut = false
    const timer = setTimeout(() => {
      this.timedOut = true
      controller.abort()
    }, this.timeoutMs)
    timer.unref?.()
    const aborted = new Promise((_resolve, reject) => {
      if (controller.signal.aborted) return reject(new Error('aborted'))
      controller.signal.addEventListener('abort', () => reject(new Error('aborted')), { once: true })
    })
    // abort 可能发生在 race 已结束之后：接住这次拒绝，避免未处理的 rejection。
    aborted.catch(() => {})
    const readBody = (response, format) => Promise.race([
      format === 'json' ? response.json() : response.text(),
      aborted,
    ])

    try {
      let response
      try {
        response = await this.fetch(identity.endpoint, {
          method: 'POST',
          headers: {
            'content-type': 'application/json',
            accept: 'application/json',
            authorization: `Bearer ${identity.token}`,
          },
          body: this.body(items),
          // 不跟随重定向：3xx 一律保留重试，Authorization 不会发往 Location。
          redirect: 'manual',
          signal: controller.signal,
        })
      } catch {
        return this.failed(items)
      }

      // 本批实际使用的地址/凭证是否仍是当前绑定：变了就整批保留并暂停，不再发下一批。
      if (!this.isIdentityCurrent(identity)) return this.identityChanged(items)

      const status = response?.status
      if (status === 200) {
        let body
        try {
          body = await readBody(response, 'json')
        } catch {
          // 中止（超时/停止）与「200 但正文不是 JSON」要分开记：后者属于畸形响应。
          if (this.stopped || this.timedOut) return this.failed(items)
          return this.malformed(items)
        }
        if (!this.isIdentityCurrent(identity)) return this.identityChanged(items)
        return this.applyResults(items, body)
      }

      // 读完响应体以复用连接，内容不记录；挂起同样算超时。
      try {
        await readBody(response, 'text')
      } catch {
        return this.failed(items)
      }

      if (typeof status !== 'number') {
        this.logger?.log('upload_failed', { reason: 'no_status', count: items.length })
        return this.backoff()
      }
      if (status >= 300 && status < 400) {
        this.logger?.log('upload_redirect_refused', { status })
        return this.backoff()
      }
      if (PAUSE_STATUSES.has(status)) {
        if (!this.isIdentityCurrent(identity)) return this.identityChanged(items)
        return this.pause(`http_${status}`)
      }
      if (status === 413) {
        if (!this.isIdentityCurrent(identity)) return this.identityChanged(items)
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
    } finally {
      clearTimeout(timer)
      this.controller = null
    }
  }

  /**
   * 单项结果校验：字段缺失、禁止字段、未知状态、白名单外错误码都算畸形，
   * 整批保留退避——服务端的自由文本不会被写进队列。
   */
  validResult(result) {
    const status = result?.status
    if (typeof status !== 'string') return false
    if (WITHOUT_ERROR.has(status)) return result.error === undefined
    const error = result.error
    if (!error || typeof error !== 'object' || typeof error.code !== 'string') return false
    // error.message 必填：1–512 个 Unicode 字符，只供人读；缺了就不算可用的逐项结果。
    const message = error.message
    if (typeof message !== 'string') return false
    const messageLength = codePointLength(message)
    if (messageLength < 1 || messageLength > 512) return false
    if (status === 'rejected') return REJECTED_CODES.has(error.code)
    if (status === 'retryable') return RETRYABLE_CODES.has(error.code)
    return false
  }

  /** 本批身份在途变化：整批保留，交给 collector 暂停并记原因。 */
  identityChanged(items) {
    this.logger?.log('upload_binding_changed', { count: items.length })
    return { state: 'identity_changed' }
  }

  /** 响应信封：request_id 必填（1–128 字符），results 必填数组。 */
  validBody(body) {
    const requestId = body?.request_id
    if (typeof requestId !== 'string') return false
    const length = codePointLength(requestId)
    if (length < 1 || length > 128) return false
    return Array.isArray(body?.results)
  }

  malformed(items) {
    this.logger?.log('upload_malformed', { status: 200, count: items.length })
    return this.backoff()
  }

  applyResults(items, body) {
    if (!this.validBody(body)) return this.malformed(items)
    const results = body.results
    // 畸形 200：results 必须与输入等长、index 必须一一覆盖；否则整批保留，绝不推断成功。
    if (!results || results.length !== items.length) return this.malformed(items)
    const byIndex = new Map()
    for (const result of results) {
      const index = result?.index
      if (!Number.isInteger(index) || index < 0 || index >= items.length || byIndex.has(index)) {
        return this.malformed(items)
      }
      if (!this.validResult(result)) return this.malformed(items)
      byIndex.set(index, result)
    }

    const remove = []
    const dead = []
    let keep = 0
    items.forEach((item, index) => {
      const result = byIndex.get(index)
      const status = result.status
      if (DEQUEUE.has(status)) {
        remove.push(item.id)
      } else if (status === 'rejected' && result.error.code === 'FUTURE_EVENT') {
        keep += 1
      } else if (TERMINAL.has(status)) {
        remove.push(item.id)
        dead.push({ id: item.id, outcome: DEAD_OUTCOME[status], code: result.error?.code ?? null })
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
