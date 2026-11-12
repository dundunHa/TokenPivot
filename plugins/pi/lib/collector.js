/**
 * 采集与调度：把 PI 的 turn_end 变成队列记录，按配置与绑定状态决定是否发送。
 *
 * 生命周期（计划 §3）：工厂只注册处理器；session_start 初始化配置、队列和调度；
 * session_shutdown 幂等停止并限时等待在途请求与本地持久化。未确认事件在退出时保留。
 *
 * stderr 只输出固定原因码 + 计数 + HTTP status；绝不输出正文、路径、token 或原始异常。
 */
import { CONFIG_POLL_MS, loadConfig, normalizeServerUrl } from './config.js'
import { SqliteQueue } from './queue.js'
import { Uploader } from './uploader.js'
import { mapTurnEnd } from './mapper.js'

const SAFE_TOKEN = /^[a-z0-9_]+$/i

/** 固定码日志：只接受固定原因码与整数计数，其他内容一律丢弃。 */
export function createLogger(stream = process.stderr) {
  return {
    log(code, data = {}) {
      const parts = [code]
      for (const [key, value] of Object.entries(data)) {
        if (!SAFE_TOKEN.test(key)) continue
        if (Number.isSafeInteger(value)) parts.push(`${key}=${value}`)
        else if (typeof value === 'string' && SAFE_TOKEN.test(value)) parts.push(`${key}=${value}`)
      }
      try {
        stream.write(`tokenpivot-pi: ${parts.join(' ')}\n`)
      } catch {
        // stderr 写失败不改变任何状态
      }
    },
  }
}

/** 目的服务标识：只显示 scheme/host/port，不含路径与凭证。 */
export function serviceIdentity(serverUrl) {
  if (typeof serverUrl !== 'string' || serverUrl === '') return null
  try {
    const url = new URL(serverUrl)
    return url.origin
  } catch {
    return null
  }
}

export class Collector {
  /**
   * @param {{
   *   env?: NodeJS.ProcessEnv, configPath?: string, logger?: {log: Function},
   *   fetch?: Function, now?: () => number, random?: () => number,
   *   setTimer?: Function, clearTimer?: Function, setInterval?: Function, clearInterval?: Function,
   *   configPollMs?: number, flushDelayMs?: number, requestTimeoutMs?: number,
   *   uploaderOptions?: object,
   * }} options
   */
  constructor(options = {}) {
    this.options = options
    this.logger = options.logger ?? createLogger()
    this.now = options.now ?? Date.now
    this.random = options.random ?? Math.random
    this.setTimer = options.setTimer ?? ((fn, ms) => setTimeout(fn, ms))
    this.clearTimer = options.clearTimer ?? ((handle) => clearTimeout(handle))
    this.setInterval = options.setInterval ?? ((fn, ms) => setInterval(fn, ms))
    this.clearInterval = options.clearInterval ?? ((handle) => clearInterval(handle))
    this.flushDelayMs = options.flushDelayMs ?? 2000
    this.requestTimeoutMs = options.requestTimeoutMs ?? 30000
    this.configPollMs = options.configPollMs ?? CONFIG_POLL_MS
    // 显式构造参数优先；没给的跟随配置文件，运行中改配置也能生效。
    this.flushDelayOption = Number.isSafeInteger(options.flushDelayMs) ? options.flushDelayMs : null
    this.timeoutOption = Number.isSafeInteger(options.requestTimeoutMs) ? options.requestTimeoutMs : null
    this.maxItemsOption = Number.isSafeInteger(options.maxQueueItems) && options.maxQueueItems > 0
      ? options.maxQueueItems
      : null

    this.counters = {}
    this.loggedCounters = {}
    this.timer = null
    this.timerDue = Infinity
    this.pollTimer = null
    this.started = false
    this.stopped = false
    this.blocked = null
    this.configResult = null
    this.queue = null
    this.uploader = null
    this.pendingFlush = null
    this.startPromise = null
    /** 当前队列所在目录：配置里的 stateDir 变了就要换库。 */
    this.queueDir = null
    /** 换库已排定但还没完成：期间新事件不入旧库。 */
    this.swapPending = false
    /** 被在途上报挡住的换库：安排一次尽快执行的配置检查。 */
    this.pollSoonTimer = null
    /** 当前配置与绑定不一致的原因码（发送前核对时给出）。 */
    this.identityIssue = null
    /** HTTP 4xx 暂停的状态与当时的身份；与「配置暂不可用」分离保存。 */
    this.httpPause = null
    /** 在途入队任务：stop 时限时等待，超时明确计数未采集事件。 */
    this.inflight = new Set()
  }

  count(key, by = 1) {
    this.counters[key] = (this.counters[key] ?? 0) + by
  }

  /** 当前对外可见的状态（命令与测试用，不含敏感信息）。 */
  status() {
    const cfg = this.configResult
    const counts = this.queue?.isOpen ? this.queue.counts() : { pending: 0, dead: 0, overflow: 0 }
    return {
      state: this.blocked ? 'blocked' : (cfg?.status ?? 'unstarted'),
      reason: this.blocked ?? cfg?.reason ?? null,
      service: serviceIdentity(cfg?.config?.serverUrl ?? null),
      counts,
      paused: this.uploader ? this.uploader.paused : false,
      pauseReason: this.uploader ? this.uploader.pauseReason : null,
      queueMoving: this.swapPending,
    }
  }

  /** 幂等启动：读配置、开队列、恢复补报、启动独立配置检查。 */
  start() {
    if (this.startPromise) return this.startPromise
    this.startPromise = (async () => {
      this.refreshConfig()
      if (!this.stopped) {
        const opened = this.openQueue()
        if (opened) {
          const counts = this.queue.counts()
          if (counts.pending > 0) this.schedule(0)
        }
        this.pollTimer = this.setInterval(() => {
          this.pollConfig().catch(() => {})
        }, this.configPollMs)
        this.pollTimer?.unref?.()
        this.logger.log('started', { pending: this.queue?.isOpen ? this.queue.counts().pending : 0 })
      }
      return this
    })()
    return this.startPromise
  }

  openQueue() {
    const config = this.configResult?.config
    if (!config) return false
    try {
      const queue = new SqliteQueue({ dir: config.stateDir, maxItems: config.maxQueueItems, now: this.now })
      queue.open()
      this.queue = queue
      this.queueDir = config.stateDir
      this.uploader = new Uploader({
        queue,
        identity: () => this.requestIdentity(),
        isIdentityCurrent: (snapshot) => this.isIdentityCurrent(snapshot),
        fetch: this.options.fetch,
        logger: this.logger,
        // 换库在途时不拉旧队列的下一批：批次边界停手，等切换完成再由新队列调度。
        shouldContinueBatch: () => !this.swapPending,
        timeoutMs: this.requestTimeoutMs,
        // timeoutMs 由 applyRuntimeConfig 跟随配置更新；这里给的是初始值。
        now: this.now,
        random: this.random,
        ...this.options.uploaderOptions,
      })
      if (this.blocked) {
        const recovered = this.blocked
        this.blocked = null
        this.logger.log('queue_recovered', { reason: recovered })
      }
      return true
    } catch (error) {
      // 损坏或旧格式：保留现场，暂停采集与发送，等人工处理。
      this.blocked = typeof error?.code === 'string' && SAFE_TOKEN.test(error.code) ? error.code : 'queue_unopenable'
      this.logger.log('queue_unavailable', { reason: this.blocked })
      return false
    }
  }

  /**
   * 配置换了 stateDir：停旧队列、按新目录重开。旧目录原样保留，不迁移、不合并、不删除。
   * 调用方需确保没有在途 flush（否则关库会让在途响应无处落地）。
   */
  swapQueue() {
    // 读坏的配置没有可信目录：不能拿默认 stateDir 去换库（会关掉旧队列）。
    if (this.configResult?.status !== 'ok') return false
    // A→B→A 的往返：目录没变就不重开队列、不清 HTTP 暂停，只取消待切换。
    if (this.queueDir && this.configResult.config.stateDir === this.queueDir) {
      this.swapPending = false
      return true
    }
    this.uploader?.stop()
    this.queue?.close()
    this.queue = null
    this.uploader = null
    this.queueDir = null
    this.blocked = null
    this.httpPause = null
    const opened = this.openQueue()
    // 打不开就保持 pending，等下一轮配置检查重试同一目录，不要求重启。
    this.swapPending = !opened
    if (opened) {
      this.logger.log('queue_dir_changed')
      if (this.queue.counts().pending > 0) this.schedule(0)
    }
    return opened
  }

  /** 配置是否允许采集：只有 ok 才采集。缺凭证不采集（计划 §4.1），已有队列保留。 */
  configState() {
    const status = this.configResult?.status
    if (this.blocked) return 'blocked'
    if (status === 'ok') return 'ready'
    return status ?? 'unstarted'
  }

  /**
   * 本批要用的地址与凭证快照；不可发送时返回 null 并给出原因码。
   * 每次调用都重读配置，并用**同一次读取**的结果核对队列绑定（地址+指纹+revision）：
   * 采集后、发送前换了凭证，这里就会返回 null，不会拿新凭证发旧队列的事件。
   */
  requestIdentity() {
    this.refreshConfig()
    return this.currentIdentity()
  }

  /** 用当前 configResult 与队列绑定算身份；不一致时置 identityIssue 并返回 null。 */
  currentIdentity() {
    const state = this.configState()
    if (state !== 'ready') {
      this.identityIssue = state === 'blocked' ? 'queue_unavailable' : `config_${state}`
      return null
    }
    const config = this.configResult.config
    if (!config.serverUrl || !config.token) {
      this.identityIssue = 'config_incomplete'
      return null
    }
    if (!this.queue?.isOpen) {
      this.identityIssue = 'queue_unavailable'
      return null
    }
    const binding = this.queue.binding()
    if (!binding) {
      this.identityIssue = 'binding_missing'
      return null
    }
    if (binding.serverUrl !== config.serverUrl) {
      this.identityIssue = 'server_changed'
      return null
    }
    const fingerprint = config.fingerprint ?? null
    if (binding.fingerprint !== fingerprint) {
      // 空指纹 + 现在有凭证：由 bindFirstCredential 在发送前完成首次绑定。
      this.identityIssue = binding.fingerprint === null ? 'pending_credential' : 'token_changed'
      return null
    }
    this.identityIssue = null
    return {
      endpoint: config.serverUrl,
      token: config.token,
      fingerprint: binding.fingerprint,
      revision: binding.revision,
    }
  }

  /** 本批实际使用的地址/凭证是否仍是当前绑定；不一致时整批保留。 */
  isIdentityCurrent(snapshot) {
    if (!snapshot) return false
    const now = this.requestIdentity()
    return !!now &&
      now.endpoint === snapshot.endpoint &&
      now.token === snapshot.token &&
      now.fingerprint === snapshot.fingerprint &&
      now.revision === snapshot.revision
  }

  /** 暂停上报；HTTP 4xx 暂停单独保存状态与当时身份，配置错误不会冲掉它。 */
  pauseUploader(reason) {
    if (!this.uploader) return { state: 'disabled', reason }
    if (!(this.uploader.paused && this.uploader.pauseReason === reason)) this.uploader.pause(reason)
    if (typeof reason === 'string' && reason.startsWith('http_')) {
      const config = this.configResult?.config
      this.httpPause = {
        reason,
        serverUrl: config?.serverUrl ?? null,
        fingerprint: config?.fingerprint ?? null,
      }
    }
    return { state: 'paused', reason }
  }

  /** 显式解除暂停（用户确认同源、或有效身份确实变了）。 */
  resumeUploader() {
    this.httpPause = null
    this.uploader?.resume()
  }

  /**
   * 自动解除暂停的条件：
   *  - 有 HTTP 暂停：只有当前身份与暂停时不同（确实换过配置）才解除；
   *    身份恢复成暂停时那份（中途读过临时错误配置）就继续暂停。
   *  - 没有 HTTP 暂停：配置不可用或绑定问题修好即可恢复。
   */
  maybeResume() {
    if (!this.uploader?.paused) return false
    if (this.httpPause) {
      const config = this.configResult?.config
      const same = this.httpPause.serverUrl === (config?.serverUrl ?? null) &&
        this.httpPause.fingerprint === (config?.fingerprint ?? null)
      if (same) {
        // 配置回到暂停时的同一身份：保持暂停，只把原因码归位，别让 30 秒检查把 401 冲掉。
        if (this.uploader.pauseReason !== this.httpPause.reason) this.uploader.pause(this.httpPause.reason)
        return false
      }
      this.resumeUploader()
      return true
    }
    this.uploader.resume()
    return true
  }

  /** 在途上报挡住换库时，安排一次尽快执行的配置检查。 */
  nudgeConfigCheck() {
    if (this.stopped || this.pollSoonTimer) return
    this.pollSoonTimer = this.setTimer(() => {
      this.pollSoonTimer = null
      this.pollConfig().catch(() => {})
    }, 0)
    this.pollSoonTimer?.unref?.()
  }

  /** 换库前提：没有在途上报，也没有在途入队。 */
  canSwapQueue() {
    return !this.pendingFlush && this.inflight.size === 0
  }

  refreshConfig() {
    const previous = this.configResult
    let loaded
    try {
      loaded = loadConfig({ env: this.options.env ?? process.env })
    } catch {
      loaded = { status: 'error', reason: 'config_unreadable' }
    }
    this.configResult = loaded
    // 只在状态或原因变化时记一行：缺凭证时每轮采集都重读配置，不能每轮刷屏。
    if (loaded.reason && (loaded.reason !== previous?.reason || loaded.status !== previous?.status)) {
      this.logger.log('config_state', { reason: loaded.reason })
    }
    this.applyRuntimeConfig()
    return loaded
  }

  /**
   * 运行时可变的配置参数：flushDelayMs / requestTimeoutMs / maxQueueItems。
   * 显式构造参数优先；没给的就跟随配置文件，同目录下改配置也立即生效。
   */
  applyRuntimeConfig() {
    // 只有解析成功的配置才谈参数生效；读坏时的默认值不是用户意图。
    if (this.configResult?.status !== 'ok') return
    const config = this.configResult.config
    if (!config) return
    if (this.flushDelayOption === null) this.flushDelayMs = config.flushDelayMs
    if (this.timeoutOption === null) {
      this.requestTimeoutMs = config.requestTimeoutMs
      if (this.uploader) this.uploader.timeoutMs = this.requestTimeoutMs
    }
    // 容量属于目录：切库尚未完成时，新目录的容量不能穿透到仍在用的旧队列。
    if (this.maxItemsOption === null && this.queue && this.configResult?.status === 'ok' &&
        this.queueDir === config.stateDir) {
      this.queue.maxItems = config.maxQueueItems
    }
  }

  /**
   * 独立于事件的配置检查：配置修好或 token 回到已绑定值即可恢复，无需新事件也能唤醒。
   * 同时负责：stateDir 换了就换库；上次队列打不开就重试同一目录。
   */
  async pollConfig() {
    if (this.stopped) return { state: 'stopped' }
    this.logChangedCounters()
    this.refreshConfig()

    // 只有解析成功的配置才决定目录：读坏了（或没写地址）不能把队列挪去默认目录。
    const desiredDir = this.configResult?.status === 'ok' ? this.configResult.config.stateDir : null
    if (desiredDir && this.queueDir && desiredDir !== this.queueDir) this.swapPending = true
    if (this.swapPending && this.configResult?.status === 'ok' && desiredDir) {
      // 换库要等上传与在途入队都结束：旧库关掉时在途操作无处落地。
      if (!this.canSwapQueue()) return { state: 'deferred', reason: 'queue_busy' }
      if (!this.swapQueue()) return { state: 'blocked', reason: this.blocked }
    } else if (!this.swapPending && !this.queue?.isOpen && this.configResult?.config) {
      // 上次打开失败（损坏/权限/临时 IO）：每个检查周期重试同一目录，不要求重启 PI。
      this.blocked = null
      if (!this.openQueue()) return { state: 'blocked', reason: this.blocked }
    }

    const state = this.configState()
    if (state !== 'ready') {
      if (this.uploader && !(this.uploader.paused && this.uploader.pauseReason === `config_${state}`)) {
        this.pauseUploader(`config_${state}`)
      }
      return { state }
    }

    let binding = this.checkBinding()
    if (binding.ok && binding.pendingCredential) {
      // 首次拿到凭证：先完成绑定，再发旧事件。
      const config = this.configResult.config
      const bound = this.queue.bindFirstCredential({
        serverUrl: config.serverUrl,
        fingerprint: config.fingerprint ?? null,
      })
      if (!bound.ok) {
        this.count(`paused_${bound.reason}`)
        return this.pauseUploader(bound.reason)
      }
      binding = this.checkBinding()
    }
    if (!binding.ok) {
      if (this.queue.counts().pending > 0) {
        this.count(`paused_${binding.reason}`)
        return this.pauseUploader(binding.reason)
      }
      return { state }
    }
    this.maybeResume()
    if (this.queue.counts().pending > 0) this.schedule(0)
    return { state }
  }

  /**
   * 对比本地绑定与当前配置。返回 {ok:true} 或 {ok:false, reason}；
   * 未绑定但有事件也视为不通过（暂停，等用户处理）。
   */
  checkBinding() {
    if (!this.queue?.isOpen) return { ok: false, reason: 'queue_unavailable' }
    const config = this.configResult?.config
    if (!config?.serverUrl) return { ok: false, reason: 'server_url_missing' }
    const binding = this.queue.binding()
    if (!binding) {
      if (this.queue.counts().pending + this.queue.counts().overflow > 0) {
        return { ok: false, reason: 'binding_missing' }
      }
      return { ok: true, unbound: true }
    }
    if (binding.serverUrl !== config.serverUrl) return { ok: false, reason: 'server_changed' }
    const fingerprint = config.fingerprint ?? null
    if (binding.fingerprint !== fingerprint) {
      // 还没有凭证时首次拿到凭证由入队事务绑定，不算轮换。
      if (binding.fingerprint === null) return { ok: true, pendingCredential: true }
      return { ok: false, reason: 'token_changed' }
    }
    return { ok: true }
  }

  /** turn_end 处理器主体。必须不向 PI 抛异常。 */
  async handleTurnEnd(event, ctx) {
    if (this.stopped || !this.queue?.isOpen) return { skipped: 'not_ready' }
    if (event?.type !== 'turn_end') return { skipped: 'not_turn_end' }
    // 每次采集前重读配置：目录、凭证的变化不必等 30 秒检查才生效。
    this.refreshConfig()

    // 配置已指向新目录、但 30 秒检查还没轮到：采集前先换库，别把新事件写进旧目录。
    // 有在途上报或入队时不换，也不往旧队列写——这期间暂停新采集并计数。
    const desiredDir = this.configResult?.status === 'ok' ? this.configResult.config.stateDir : null
    if (desiredDir && this.queueDir && desiredDir !== this.queueDir) this.swapPending = true
    if (this.swapPending && desiredDir) {
      if (!this.canSwapQueue()) {
        this.count('skip_queue_moving')
        return { skipped: 'queue_moving' }
      }
      if (!this.swapQueue()) return { skipped: 'blocked' }
    }

    const state = this.configState()
    if (state !== 'ready') {
      // 缺凭证属于「已安装待配置」：不开始新采集，已有队列原样保留（计划 §4.1）。
      if (state === 'no_token') {
        this.count('skip_collect_disabled_no_token')
        return { skipped: 'collect_disabled_no_token' }
      }
      this.count(`skip_${state}`)
      return { skipped: state }
    }

    let sessionId
    try {
      sessionId = ctx?.sessionManager?.getSessionId?.()
    } catch {
      sessionId = undefined
    }
    const mapped = mapTurnEnd({
      sessionId,
      messageEntryId: event.messageEntryId,
      message: event.message,
      cwd: typeof ctx?.cwd === 'string' ? ctx.cwd : undefined,
      projects: this.configResult.config.projects,
    })
    if ('skip' in mapped) {
      this.count(`skip_${mapped.skip}`)
      return { skipped: mapped.skip }
    }
    if (mapped.invalid > 0) this.count('usage_invalid', mapped.invalid)

    return this.trackEnqueue(this.enqueue(mapped.event))
  }

  /** 记录在途入队：stop 时要能等它，超时要能明确计数。 */
  trackEnqueue(task) {
    this.inflight.add(task)
    task.finally(() => {
      this.inflight.delete(task)
    }).catch(() => {})
    return task
  }

  /**
   * 入队；SQLITE_BUSY 用异步退避重试，绝不同步等待。
   * 队列实例在进入时固定：切库或关库不会让这批重试落到另一个库。
   */
  async enqueue(event) {
    const queue = this.queue
    const config = this.configResult.config
    const payload = { event, serverUrl: config.serverUrl, fingerprint: config.fingerprint ?? null }
    const maxAttempts = this.options.enqueueAttempts ?? 5
    for (let attempt = 0; attempt < maxAttempts; attempt += 1) {
      if (this.stopped) {
        this.count('enqueue_aborted')
        this.logger.log('enqueue_aborted', { reason: 'stopped' })
        return { state: 'failed', reason: 'stopped' }
      }
      const result = queue.enqueue(payload)
      if (result.busy) {
        this.count('queue_busy')
        await this.sleep(this.options.enqueueRetryMs ?? 50, attempt)
        continue
      }
      if (result.state === 'queued') {
        this.count('queued')
        this.schedule(this.flushDelayMs)
      } else if (result.state === 'overflow') {
        this.count('overflow')
        this.logger.log('queue_overflow', { count: this.counters.overflow })
      } else if (result.state === 'duplicate') {
        this.count('duplicate')
      } else if (result.state === 'conflict') {
        this.count('conflict')
        this.logger.log('queue_conflict')
      } else if (result.state === 'paused') {
        this.count(`paused_${result.reason}`)
        this.pauseUploader(result.reason)
      } else {
        this.count('queue_failed')
        this.logger.log('queue_failed', { reason: result.reason ?? 'io' })
      }
      return result
    }
    // 重试耗尽：明确记为未采集，不宣称成功。
    this.count('queue_failed')
    this.logger.log('queue_failed', { reason: 'busy' })
    return { state: 'failed', reason: 'busy' }
  }

  sleep(ms, attempt = 0) {
    const delay = Math.min(1000, ms * 2 ** attempt)
    return new Promise((resolve) => {
      const timer = this.setTimer(resolve, delay)
      timer?.unref?.()
    })
  }

  /** session_compact：压缩可能合并两次摘要请求的用量，首版不支持请求级统计，只计数。 */
  handleCompact() {
    this.count('compact_skipped')
    return { skipped: 'compact' }
  }

  /** 保留最早的一次唤醒；后续请求不会推后已排定的时间。 */
  schedule(delayMs) {
    if (this.stopped || !this.uploader) return
    const due = this.now() + delayMs
    if (this.timer && due >= this.timerDue) return
    if (this.timer) this.clearTimer(this.timer)
    this.timerDue = due
    this.timer = this.setTimer(() => {
      this.timer = null
      this.timerDue = Infinity
      this.runFlush().catch(() => {})
    }, delayMs)
    this.timer?.unref?.()
  }

  runFlush() {
    if (!this.pendingFlush) {
      this.pendingFlush = this.flushOnce().finally(() => {
        this.pendingFlush = null
      })
    }
    return this.pendingFlush
  }

  /** 锁外发送：先做绑定与配置检查，再交给 uploader 走 HTTP。 */
  async flushOnce() {
    if (this.stopped) return { state: 'stopped' }
    if (this.swapPending) return { state: 'deferred', reason: 'queue_moving' }
    if (this.configState() !== 'ready') return { state: 'disabled' }
    let pending
    try {
      pending = this.queue.counts().pending
    } catch {
      return { state: 'failed', reason: 'queue_unavailable' }
    }
    if (pending === 0) return { state: 'idle' }
    let binding = this.checkBinding()
    if (binding.ok && binding.pendingCredential) {
      // 上次只有地址、没有凭证；现在首次拿到凭证：先把空指纹绑定掉，再发旧事件。
      const config = this.configResult.config
      const bound = this.queue.bindFirstCredential({
        serverUrl: config.serverUrl,
        fingerprint: config.fingerprint ?? null,
      })
      if (!bound.ok) {
        this.count(`paused_${bound.reason}`)
        return this.pauseUploader(bound.reason)
      }
      binding = this.checkBinding()
    }
    if (!binding.ok) {
      this.count(`paused_${binding.reason}`)
      return this.pauseUploader(binding.reason)
    }
    // 发送前用同一次读取核对地址+指纹+revision；不一致立即暂停，绝不拿新凭证发旧队列。
    if (!this.currentIdentity()) {
      const reason = this.identityIssue ?? 'identity_unavailable'
      this.count(`paused_${reason}`)
      return this.pauseUploader(reason)
    }
    let outcome
    try {
      outcome = await this.uploader.flush()
    } catch {
      this.count('flush_failed')
      this.logger.log('flush_failed', { reason: 'internal' })
      return this.uploader.backoff()
    }
    if (outcome.state === 'deferred') {
      // 换库被在途上报挡住：尽快做一次配置检查把切换推进去，不重复上报。
      this.nudgeConfigCheck()
    } else if (outcome.state === 'backoff') this.schedule(outcome.delayMs)
    else if (outcome.state === 'paused') {
      this.count('upload_paused')
      this.pauseUploader(outcome.reason)
    } else if (outcome.state === 'unavailable' || outcome.state === 'identity_changed') {
      // 在途期间配置/绑定变了（含其他进程更新了绑定）：整批保留并暂停。
      const reason = this.identityIssue ??
        (outcome.state === 'identity_changed' ? 'binding_changed' : 'identity_unavailable')
      this.count(`paused_${reason}`)
      return this.pauseUploader(reason)
    }
    return outcome
  }

  /**
   * /tokenpivot-confirm-same-source 的实现：用户断言新凭证仍属原 source。
   * 以当前配置的 token 指纹为目标，按库中观察到的旧指纹 + revision 做条件更新。
   */
  async confirmSameSource() {
    const config = this.configResult?.config
    if (!config?.serverUrl) return { ok: false, reason: 'config_incomplete' }
    if (!config.fingerprint) return { ok: false, reason: 'token_missing' }
    if (!this.queue?.isOpen) return { ok: false, reason: 'queue_unavailable' }
    const normalized = normalizeServerUrl(config.serverUrl)
    if ('error' in normalized) return { ok: false, reason: normalized.error }
    const binding = this.queue.binding()
    if (!binding) return { ok: false, reason: 'binding_missing' }
    const result = this.queue.confirmSameSource({
      serverUrl: normalized.endpoint,
      observedFingerprint: binding.fingerprint,
      observedRevision: binding.revision,
      newFingerprint: config.fingerprint,
    })
    if (result.ok) {
      this.logger.log('binding_confirmed', { revision: this.queue.binding()?.revision ?? 0 })
      this.resumeUploader()
      this.schedule(0)
    } else {
      this.logger.log('binding_confirm_failed', { reason: result.reason ?? 'unknown' })
    }
    return result
  }

  /** 幂等停止：幂等、限时等待在途请求与本地写入，不清空未确认事件。 */
  async stop({ timeoutMs = 3000 } = {}) {
    if (this.stopped) return
    this.stopped = true
    if (this.timer) this.clearTimer(this.timer)
    this.timer = null
    this.timerDue = Infinity
    if (this.pollSoonTimer) this.clearTimer(this.pollSoonTimer)
    this.pollSoonTimer = null
    if (this.pollTimer) this.clearInterval(this.pollTimer)
    this.pollTimer = null
    this.uploader?.stop()
    // 等在途入队：BUSY 退避中的任务若被直接关库，事件会悄悄丢掉。
    const inflight = [...this.inflight]
    const wait = Promise.allSettled([this.startPromise, this.pendingFlush, ...inflight].filter(Boolean))
    let timer
    const timeout = new Promise((resolve) => {
      timer = this.setTimer(() => resolve('timeout'), timeoutMs)
      timer?.unref?.()
    })
    const raced = await Promise.race([wait.then(() => 'done'), timeout])
    if (timer) this.clearTimer(timer)
    const unfinished = this.inflight.size
    if (unfinished > 0) {
      // 限时到了还没落地：明确记为未采集，不宣称成功。
      this.count('enqueue_unflushed', unfinished)
      this.logger.log('enqueue_unflushed', { count: unfinished })
    }
    const counts = this.queue?.isOpen ? this.queue.counts() : { pending: 0, dead: 0, overflow: 0 }
    this.logger.log('stopped', {
      pending: counts.pending,
      timedout: raced === 'timeout' ? 1 : 0,
    })
    this.logCounters()
    this.queue?.close()
  }

  /** 运行期也能看到跳过计数：只在计数变化时输出，避免每 30 秒刷屏。 */
  logChangedCounters() {
    const changed = {}
    for (const [key, value] of Object.entries(this.counters)) {
      if (this.loggedCounters[key] !== value) changed[key] = value
    }
    this.loggedCounters = { ...this.counters }
    const entries = Object.entries(changed)
    if (entries.length === 0) return
    for (const [key, value] of entries) this.logger.log('count', { name: key, n: value })
  }

  logCounters() {
    const entries = Object.entries(this.counters).filter(([, value]) => value > 0)
    for (const [key, value] of entries) this.logger.log('count', { name: key, n: value })
  }
}
