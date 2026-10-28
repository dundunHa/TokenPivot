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

export class Collector {
  /**
   * @param {{
   *   stateDir: string, endpoint: string|null, getToken: () => string|undefined,
   *   projects?: {path: string, projectId: string}[], resolveCwd?: (session) => string|undefined,
   *   maxQueueItems?: number, flushDelayMs?: number, logger?: any, fetch?: Function,
   *   setTimer?: (fn: Function, ms: number) => any, clearTimer?: (handle: any) => void,
   *   uploaderOptions?: object,
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
    this.flushDelayMs = options.flushDelayMs ?? 2000
    this.timer = null
    this.timerDue = Infinity
    this.stopped = false
    this.ready = null
  }

  /** Load persisted events and try to send anything left over from a previous run (补报). */
  start() {
    this.ready = this.queue.load().then(() => {
      if (this.queue.size > 0) this.schedule(0)
    })
    return this.ready
  }

  /** session/event listener body. Must not throw into DSH. */
  async handle(session, event) {
    if (this.stopped) return
    if (event?.type !== 'assistant/message' && event?.type !== 'compaction/summary') return
    let usageEvent
    try {
      usageEvent = mapSessionEvent(session, event, {
        cwd: this.options.resolveCwd?.(session),
        projects: this.options.projects,
      })
    } catch (error) {
      this.logger?.warn?.(`skipped ${event.type}: ${error?.message ?? error}`)
      return
    }
    if (!usageEvent) return
    await this.ready
    if (this.queue.enqueue(usageEvent)) {
      await this.queue.persist()
      this.schedule(this.flushDelayMs)
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

  async stop() {
    this.stopped = true
    if (this.timer) this.clearTimer(this.timer)
    this.timer = null
    await this.ready?.catch(() => {})
    await this.queue.writing.catch(() => {})
  }
}
