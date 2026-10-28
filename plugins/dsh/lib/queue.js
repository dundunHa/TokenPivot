/**
 * Durable local queue of frozen UsageEvents.
 *
 * - Each queued item is the exact JSON body that will be sent; a retry re-sends identical bytes
 *   under the same source_event_id (T-045: 重传沿用同一事件ID).
 * - State is a single JSON file written atomically (tmp + rename), mode 0600, directory 0700.
 * - Contains usage numbers, provider/model, project_id, event ids only. Never tokens or content.
 */
import { promises as fs } from 'node:fs'
import path from 'node:path'

const FILE = 'queue.json'
const DEAD_LETTER = 'dead-letter.jsonl'
const FORMAT = 1

export class FileQueue {
  /**
   * @param {{ dir: string, maxItems?: number, logger?: { warn: Function } }} options
   */
  constructor({ dir, maxItems = 10000, logger }) {
    this.dir = dir
    this.maxItems = maxItems
    this.logger = logger
    this.items = []
    this.ids = new Set()
    this.dropped = 0
    this.writing = Promise.resolve()
  }

  get file() { return path.join(this.dir, FILE) }
  get size() { return this.items.length }

  async load() {
    await fs.mkdir(this.dir, { recursive: true, mode: 0o700 })
    let text
    try {
      text = await fs.readFile(this.file, 'utf8')
    } catch (error) {
      if (error.code === 'ENOENT') return
      throw error
    }
    let parsed
    try {
      parsed = JSON.parse(text)
    } catch {
      // A corrupt file must not block new events; keep it aside for inspection.
      const aside = `${this.file}.corrupt-${Date.now()}`
      await fs.rename(this.file, aside)
      this.logger?.warn?.(`queue file unreadable, moved aside to ${path.basename(aside)}`)
      return
    }
    const items = Array.isArray(parsed?.items) ? parsed.items : []
    for (const item of items) {
      if (item && typeof item.source_event_id === 'string' && !this.ids.has(item.source_event_id)) {
        this.items.push(item)
        this.ids.add(item.source_event_id)
      }
    }
  }

  /** @returns {boolean} false when the event id is already queued */
  enqueue(event) {
    if (this.ids.has(event.source_event_id)) return false
    this.items.push(structuredClone(event))
    this.ids.add(event.source_event_id)
    while (this.items.length > this.maxItems) {
      const old = this.items.shift()
      this.ids.delete(old.source_event_id)
      this.dropped += 1
    }
    if (this.dropped > 0 && this.items.length === this.maxItems) {
      this.logger?.warn?.(`queue full (${this.maxItems}); dropped ${this.dropped} oldest event(s) so far`)
    }
    return true
  }

  peek(n) { return this.items.slice(0, n) }

  /** Remove by event id (order-independent: results map back to ids, not positions). */
  remove(ids) {
    const drop = new Set(ids)
    if (drop.size === 0) return
    this.items = this.items.filter((item) => !drop.has(item.source_event_id))
    for (const id of drop) this.ids.delete(id)
  }

  /** Serialize writes so concurrent persist() calls never interleave. */
  persist() {
    const snapshot = JSON.stringify({ format: FORMAT, items: this.items })
    this.writing = this.writing.catch(() => {}).then(async () => {
      await fs.mkdir(this.dir, { recursive: true, mode: 0o700 })
      const tmp = `${this.file}.${process.pid}.tmp`
      await fs.writeFile(tmp, snapshot, { mode: 0o600 })
      await fs.rename(tmp, this.file)
    })
    return this.writing
  }

  /** Terminal outcomes kept for diagnosis: event id, outcome, error code. No usage payload. */
  async deadLetter(records) {
    if (records.length === 0) return
    const lines = records.map((r) => JSON.stringify({ at: new Date().toISOString(), ...r })).join('\n') + '\n'
    await fs.appendFile(path.join(this.dir, DEAD_LETTER), lines, { mode: 0o600 })
  }
}
