/**
 * 本地持久队列：Node 内置 node:sqlite（`queue.sqlite`）。
 *
 * 设计要点（计划 §5）：
 *   - journal_mode=DELETE、synchronous=FULL；无 WAL、无 PID 文件、无锁龄抢占、无独立 compact。
 *   - 事件表以事件 ID 为主键，payload 写入后不可变；同 ID 不同内容报冲突，绝不覆盖首次内容。
 *   - 绑定元数据表记录 normalizedServerUrl、acceptedTokenFingerprint、bindingRevision。
 *   - 入队、容量处理、死信/overflow、确认出队、绑定更新都在短 BEGIN IMMEDIATE 事务内完成；
 *     事务内没有 await、没有网络、没有文件 IO 以外的等待。
 *   - API 全同步（timeout=0）：SQLITE_BUSY 立即返回给调用方，由调用方异步退避，绝不长同步等待阻塞 PI。
 *
 * 只支持本机本地文件系统；stateDir 0700、数据库 0600。数据库与 rollback journal 是恢复单元，
 * 运行中不移动、不替换、不单删 journal；损坏时保留现场并暂停，不覆盖重建。
 */
import fs from 'node:fs'
import path from 'node:path'
import { DatabaseSync } from 'node:sqlite'

export const QUEUE_FILE = 'queue.sqlite'
export const FORMAT_VERSION = 1
/** 旧版（JSON 快照）状态文件名：出现且非空时要求显式迁移。 */
const LEGACY_FILES = ['queue.json']

const SQLITE_BUSY = 5
const SQLITE_LOCKED = 6
const SQLITE_CORRUPT = 11
const SQLITE_NOTADB = 26

export class QueueError extends Error {
  constructor(code, options) {
    super(code, options)
    this.code = code
  }
}

const isBusy = (error) => error?.errcode === SQLITE_BUSY || error?.errcode === SQLITE_LOCKED
const isCorrupt = (error) => error?.errcode === SQLITE_CORRUPT || error?.errcode === SQLITE_NOTADB
const nowMs = () => Date.now()

export class SqliteQueue {
  /**
   * @param {{
   *   dir: string, maxItems?: number, now?: () => number, fsImpl?: typeof fs,
   * }} options
   */
  constructor({ dir, maxItems = 10000, now = nowMs, fsImpl = fs }) {
    this.dir = dir
    this.file = path.join(dir, QUEUE_FILE)
    this.maxItems = Number.isSafeInteger(maxItems) && maxItems > 0 ? maxItems : 10000
    this.now = now
    this.fs = fsImpl
    this.db = null
  }

  open() {
    ensurePrivateDir(this.fs, this.dir)
    for (const legacy of LEGACY_FILES) {
      const legacyPath = path.join(this.dir, legacy)
      try {
        if (this.fs.statSync(legacyPath).size > 0) throw new QueueError('legacy_state')
      } catch (error) {
        if (error instanceof QueueError) throw error
        // ENOENT 是正常情况
      }
    }
    if (this.fs.existsSync(this.file)) {
      const stat = this.fs.lstatSync(this.file)
      if (stat.isSymbolicLink()) throw new QueueError('queue_symlink')
      if (!stat.isFile()) throw new QueueError('queue_invalid')
    }

    let db
    try {
      db = new DatabaseSync(this.file, { timeout: 0 })
    } catch (error) {
      throw new QueueError('queue_unopenable', { cause: error })
    }
    this.db = db
    try {
      if ((this.fs.statSync(this.file).mode & 0o077) !== 0) this.fs.chmodSync(this.file, 0o600)
      db.exec('PRAGMA journal_mode = DELETE')
      db.exec('PRAGMA synchronous = FULL')
      db.exec('PRAGMA busy_timeout = 0')
      const existing = Number(db.prepare('PRAGMA user_version').get()?.user_version ?? 0)
      // 别的格式版本不猜、不迁移、不覆盖：暂停并等显式迁移。
      if (existing !== 0 && existing !== FORMAT_VERSION) throw new QueueError('legacy_state')
      if (existing === 0) db.exec(`PRAGMA user_version = ${FORMAT_VERSION}`)
      db.exec(`CREATE TABLE IF NOT EXISTS meta (
        key TEXT PRIMARY KEY,
        value TEXT NOT NULL
      )`)
      db.exec(`CREATE TABLE IF NOT EXISTS events (
        id TEXT PRIMARY KEY,
        payload TEXT NOT NULL,
        status TEXT NOT NULL CHECK (status IN ('pending', 'dead', 'overflow')),
        outcome TEXT,
        code TEXT,
        created_at INTEGER NOT NULL,
        updated_at INTEGER NOT NULL
      )`)
      db.exec('CREATE INDEX IF NOT EXISTS events_status ON events (status, created_at)')
    } catch (error) {
      this.close()
      throw new QueueError(isCorrupt(error) ? 'queue_corrupt' : 'queue_unopenable', { cause: error })
    }
    return this
  }

  close() {
    try {
      this.db?.close()
    } catch {
      // 关闭失败不改变任何已提交状态
    }
    this.db = null
  }

  get isOpen() {
    return this.db !== null
  }

  /**
   * 短事务：BEGIN IMMEDIATE 后在同一个同步块内读、判、写。
   * @returns {{value?: any, busy?: boolean, error?: Error}}
   */
  transaction(fn) {
    if (!this.db) return { error: new QueueError('queue_closed') }
    try {
      this.db.exec('BEGIN IMMEDIATE')
    } catch (error) {
      return { error, busy: isBusy(error) }
    }
    let value
    try {
      value = fn()
    } catch (error) {
      try {
        this.db.exec('ROLLBACK')
      } catch {
        // 事务已结束则忽略
      }
      return { error, busy: isBusy(error) }
    }
    try {
      this.db.exec('COMMIT')
    } catch (error) {
      try {
        this.db.exec('ROLLBACK')
      } catch {
        // COMMIT 失败后事务可能已结束
      }
      return { error, busy: isBusy(error) }
    }
    return { value }
  }

  /** 绑定元数据；未绑定时为 null。只读，不取写锁。 */
  binding() {
    return this.readBindingRaw()
  }

  countPending() {
    const row = this.db.prepare("SELECT COUNT(*) AS n FROM events WHERE status = 'pending'").get()
    return Number(row?.n ?? 0)
  }

  counts() {
    const rows = this.db.prepare('SELECT status, COUNT(*) AS n FROM events GROUP BY status').all()
    const out = { pending: 0, dead: 0, overflow: 0 }
    for (const row of rows) out[row.status] = Number(row.n)
    return out
  }

  /**
   * 入队并冻结事件体。
   *
   * 绑定规则：空库首次有效配置在事务内绑定；绑定缺失但库里已有事件时暂停；
   * 服务地址变化一律暂停（换服务必须新 stateDir）；token 指纹变化暂停等待用户确认；
   * 还没有凭证时（fingerprint=null）首次拿到凭证视为绑定该凭证，不算轮换。
   *
   * @param {{event: object, serverUrl: string, fingerprint: string|null}} input
   * @returns {{state: 'queued'|'overflow'|'duplicate'|'conflict'|'paused', reason?: string, busy?: boolean, failed?: boolean}}
   */
  enqueue({ event, serverUrl, fingerprint }) {
    const payload = JSON.stringify(event)
    const id = event.source_event_id
    const result = this.transaction(() => {
      const binding = this.readBindingRaw()
      if (!binding) {
        if (this.countAllRaw() > 0) return { state: 'paused', reason: 'binding_missing' }
        this.writeBindingRaw({ serverUrl, fingerprint, revision: 1 })
      } else if (binding.serverUrl !== serverUrl) {
        return { state: 'paused', reason: 'server_changed' }
      } else if (binding.fingerprint !== fingerprint) {
        if (binding.fingerprint === null && fingerprint !== null) {
          this.writeBindingRaw({ serverUrl, fingerprint, revision: binding.revision + 1 })
        } else {
          return { state: 'paused', reason: 'token_changed' }
        }
      }

      const existing = this.db.prepare('SELECT payload FROM events WHERE id = ?').get(id)
      if (existing) {
        return existing.payload === payload ? { state: 'duplicate' } : { state: 'conflict' }
      }

      const status = this.countPending() >= this.maxItems ? 'overflow' : 'pending'
      const at = this.now()
      this.db.prepare(
        'INSERT INTO events (id, payload, status, outcome, code, created_at, updated_at) VALUES (?, ?, ?, NULL, NULL, ?, ?)',
      ).run(id, payload, status, at, at)
      return { state: status === 'overflow' ? 'overflow' : 'queued' }
    })
    if (result.error) {
      const failed = failure(result)
      return failed.busy ? { state: 'busy', busy: true } : { state: 'failed', failed: true, reason: failed.reason }
    }
    return result.value
  }

  readBindingRaw() {
    const rows = this.db.prepare('SELECT key, value FROM meta').all()
    const map = new Map(rows.map((r) => [r.key, r.value]))
    if (!map.has('normalizedServerUrl')) return null
    const revision = Number(map.get('bindingRevision') ?? '0')
    const fingerprint = map.get('acceptedTokenFingerprint') ?? ''
    return {
      serverUrl: map.get('normalizedServerUrl'),
      fingerprint: fingerprint === '' ? null : fingerprint,
      revision: Number.isSafeInteger(revision) ? revision : 0,
    }
  }

  writeBindingRaw({ serverUrl, fingerprint, revision }) {
    const at = this.now()
    const set = this.db.prepare(
      'INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value',
    )
    set.run('normalizedServerUrl', serverUrl)
    set.run('acceptedTokenFingerprint', fingerprint ?? '')
    set.run('bindingRevision', String(revision))
    set.run('boundAt', String(at))
  }

  countAllRaw() {
    const row = this.db.prepare('SELECT COUNT(*) AS n FROM events').get()
    return Number(row?.n ?? 0)
  }

  /**
   * 取最老的一批 pending（含精确 payload 文本，重传字节不变）。
   * @param {number} limit
   * @returns {{items: {id: string, payloadText: string}[], busy?: boolean, failed?: boolean}}
   */
  peek(limit) {
    try {
      const rows = this.db.prepare(
        "SELECT id, payload FROM events WHERE status = 'pending' ORDER BY created_at, rowid LIMIT ?",
      ).all(limit)
      return { items: rows.map((r) => ({ id: r.id, payloadText: r.payload })) }
    } catch (error) {
      return { items: [], ...failure({ error, busy: isBusy(error) }) }
    }
  }

  /**
   * 只按事件 ID 删除已确认的 pending 记录，并在同一事务里把终态项改成 dead。
   * 并行上传时另一个 uploader 可能已经删过，这里对不存在的 ID 静默忽略。
   * @param {{remove?: string[], dead?: {id: string, outcome: string, code: string|null}[]}} outcome
   */
  applyOutcome({ remove = [], dead = [] }) {
    const apply = () => {
      const at = this.now()
      // 先转死信再删除：同一个 ID 既在 remove 又在 dead 时，记录必须留下来当死信。
      const deadIds = new Set(dead.map((record) => record.id))
      const mark = this.db.prepare(
        "UPDATE events SET status = 'dead', outcome = ?, code = ?, updated_at = ? WHERE id = ? AND status IN ('pending', 'overflow')",
      )
      for (const record of dead) mark.run(record.outcome, record.code ?? null, at, record.id)
      const del = this.db.prepare("DELETE FROM events WHERE id = ? AND status IN ('pending', 'overflow')")
      for (const id of remove) {
        if (!deadIds.has(id)) del.run(id)
      }
      return { removed: remove.filter((id) => !deadIds.has(id)).length, dead: dead.length }
    }
    const result = this.transaction(apply)
    if (result.error) return failure(result)
    return result.value
  }

  /**
   * 首次拿到凭证时把空指纹绑定成该凭证（服务端 source 由认证决定，本地只记录指纹）。
   * 已有非空指纹则不覆盖：那是轮换，必须走 confirmSameSource。
   */
  bindFirstCredential({ serverUrl, fingerprint }) {
    const apply = () => {
      const binding = this.readBindingRaw()
      if (!binding) return { ok: false, reason: 'binding_missing' }
      if (binding.serverUrl !== serverUrl) return { ok: false, reason: 'server_changed' }
      if (binding.fingerprint === fingerprint) return { ok: true, unchanged: true }
      if (binding.fingerprint !== null) return { ok: false, reason: 'stale_binding' }
      this.writeBindingRaw({ serverUrl, fingerprint, revision: binding.revision + 1 })
      return { ok: true, revision: binding.revision + 1 }
    }
    const result = this.transaction(apply)
    if (result.error) return failure(result)
    return result.value
  }

  /**
   * 用户确认「新凭证仍属原 source」：按观察到的旧指纹 + revision 做条件更新。
   * 服务地址不匹配或条件已变化都保持暂停，禁止后写覆盖。
   */
  confirmSameSource({ serverUrl, observedFingerprint, observedRevision, newFingerprint }) {
    const apply = () => {
      const binding = this.readBindingRaw()
      if (!binding) return { ok: false, reason: 'binding_missing' }
      if (binding.serverUrl !== serverUrl) return { ok: false, reason: 'server_changed' }
      if (binding.fingerprint !== observedFingerprint || binding.revision !== observedRevision) {
        return { ok: false, reason: 'stale_binding' }
      }
      if (binding.fingerprint === newFingerprint) return { ok: true, unchanged: true }
      this.writeBindingRaw({ serverUrl, fingerprint: newFingerprint, revision: binding.revision + 1 })
      return { ok: true, revision: binding.revision + 1 }
    }
    const result = this.transaction(apply)
    if (result.error) return failure(result)
    return result.value
  }
}

function failure(result) {
  if (result.busy) return { busy: true }
  const error = result.error
  if (error?.code === 'queue_closed') return { failed: true, reason: 'queue_closed' }
  return { failed: true, reason: 'queue_io' }
}

function ensurePrivateDir(fsImpl, dir) {
  fsImpl.mkdirSync(dir, { recursive: true, mode: 0o700 })
  let stat
  try {
    stat = fsImpl.lstatSync(dir)
  } catch {
    throw new QueueError('state_dir_invalid')
  }
  if (stat.isSymbolicLink()) throw new QueueError('state_dir_symlink')
  if (!stat.isDirectory()) throw new QueueError('state_dir_invalid')
  if ((stat.mode & 0o077) !== 0) fsImpl.chmodSync(dir, 0o700)
}
