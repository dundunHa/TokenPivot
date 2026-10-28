/**
 * Durable local queue of frozen UsageEvents.
 *
 * - Each queued item is the exact JSON body that will be sent; a retry re-sends identical bytes
 *   under the same source_event_id (T-045: 重传沿用同一事件ID).
 * - State is a single JSON file written atomically (tmp + rename), mode 0600, directory 0700.
 * - Contains usage numbers, provider/model, project_id, event ids only. Never tokens or content.
 */
import { promises as fs } from 'node:fs'
import { randomBytes } from 'node:crypto'
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
    this.loaded = false
    this.writing = Promise.resolve()
  }

  get file() { return path.join(this.dir, FILE) }
  get size() { return this.items.length }

  /**
   * Read the persisted queue. Items already enqueued in memory (e.g. while an earlier load failed)
   * are kept after the persisted ones. Until a load succeeds, persist() refuses to write, so an
   * unreadable-but-intact file is never overwritten with a partial snapshot.
   */
  async load() {
    if (this.loaded) return
    await fs.mkdir(this.dir, { recursive: true, mode: 0o700 })
    let text
    try {
      text = await fs.readFile(this.file, 'utf8')
    } catch (error) {
      if (error.code !== 'ENOENT') throw error
      this.loaded = true
      return
    }
    let parsed
    try {
      parsed = JSON.parse(text)
    } catch {
      // A corrupt file must not block new events; keep it aside for inspection.
      const aside = `${this.file}.corrupt-${Date.now()}`
      await fs.rename(this.file, aside)
      this.logger?.warn?.(`queue file unreadable, moved aside to ${path.basename(aside)}`)
      this.loaded = true
      return
    }
    const persisted = []
    const seen = new Set()
    for (const item of Array.isArray(parsed?.items) ? parsed.items : []) {
      if (item && typeof item.source_event_id === 'string' && !seen.has(item.source_event_id)) {
        persisted.push(item)
        seen.add(item.source_event_id)
      }
    }
    const pending = this.items.filter((item) => !seen.has(item.source_event_id))
    this.items = [...persisted, ...pending]
    this.ids = new Set(this.items.map((item) => item.source_event_id))
    this.trim()
    this.loaded = true
  }

  trim() {
    while (this.items.length > this.maxItems) {
      const old = this.items.shift()
      this.ids.delete(old.source_event_id)
      this.dropped += 1
    }
  }

  /** @returns {boolean} false when the event id is already queued */
  enqueue(event) {
    if (this.ids.has(event.source_event_id)) return false
    this.items.push(structuredClone(event))
    this.ids.add(event.source_event_id)
    this.trim()
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

  /**
   * Serialize writes so concurrent persist() calls never interleave. Each write goes to a unique
   * tmp file, is fsync'ed, then renamed over the queue file (and the directory fsync'ed best effort),
   * so a crash or power loss leaves either the old or the new complete file.
   */
  persist() {
    this.writing = this.writing.catch(() => {}).then(async () => {
      if (!this.loaded) await this.load() // throws if the existing file still cannot be read
      // Snapshot at write time: always the newest in-memory state, never an older one.
      const snapshot = JSON.stringify({ format: FORMAT, items: this.items })
      await fs.mkdir(this.dir, { recursive: true, mode: 0o700 })
      const tmp = `${this.file}.${process.pid}.${randomBytes(6).toString('hex')}.tmp`
      const handle = await fs.open(tmp, 'w', 0o600)
      try {
        await handle.writeFile(snapshot)
        await handle.sync()
      } finally {
        await handle.close()
      }
      await fs.rename(tmp, this.file)
      await syncDir(this.dir)
    })
    return this.writing
  }

  /**
   * Terminal outcomes kept for diagnosis and manual re-submission: event id, outcome, error code and
   * the frozen event body (usage numbers, provider/model, project_id — no token, no content).
   */
  async deadLetter(records) {
    if (records.length === 0) return
    const lines = records.map((r) => JSON.stringify({ at: new Date().toISOString(), ...r })).join('\n') + '\n'
    await fs.appendFile(path.join(this.dir, DEAD_LETTER), lines, { mode: 0o600 })
  }
}

async function syncDir(dir) {
  let handle
  try {
    handle = await fs.open(dir, 'r')
    await handle.sync()
  } catch {
    // Not supported on every platform; the file itself is already synced.
  } finally {
    await handle?.close().catch(() => {})
  }
}
