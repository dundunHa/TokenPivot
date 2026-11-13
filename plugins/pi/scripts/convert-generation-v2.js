#!/usr/bin/env node
/**
 * 一次性转换 PI 本地队列（queue.sqlite）里 pending 事件的旧请求格式（T-070）。
 *
 * 旧客户端事件可能没有 `generation_duration_ms`、`ttft_ms` 两个键；新服务端要求两键必填，
 * 未知写 null。本脚本只给 pending 事件的 payload 补缺失的两个键（补 null），其他一律不动。
 *
 * 安全边界：
 *   - 默认只读检查，只有显式 `--apply` 才写入；从不调用 SqliteQueue.open（它会写 PRAGMA、
 *     建表、设置 user_version，相当于“顺便修复”）。
 *   - 预检和检查都用独立只读 DatabaseSync；只处理 status='pending' 的行，dead/overflow 不动。
 *   - 存在 rollback journal（queue.sqlite-journal，含零字节）或 WAL/SHM 现场时，在任何可写
 *     打开之前停止并保留现场，交人工处理，不自行触发恢复。
 *   - 无待转换项时不建目录、不备份、不写库、不改版本。
 *   - apply 先备份数据库文件，再在一个 BEGIN IMMEDIATE 事务里按事件 ID 更新仍为 pending 的
 *     payload，失败回滚。
 *   - 不改 dead/overflow/meta、URL 绑定、token 指纹、revision、created_at/updated_at、rowid、
 *     user_version 或 journal 配置。
 *   - payload 的顶层键超出服务端认识的 11 个、必填字段缺失或越界、provider/model 带首尾空白、
 *     project_id 与 project_key 同时有效、usage 结构非法时整体中止，check/apply 都不写入。
 *
 * 用法：node convert-generation-v2.js [--dir <状态目录>] [--apply] [--help]
 */
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { DatabaseSync } from 'node:sqlite'

const QUEUE_FILE = 'queue.sqlite'
const JOURNAL_FILE = `${QUEUE_FILE}-journal`
const WAL_FILE = `${QUEUE_FILE}-wal`
const SHM_FILE = `${QUEUE_FILE}-shm`
const TIMING_KEYS = ['generation_duration_ms', 'ttft_ms']
const FORMAT_VERSION = 1
const REQUIRED_COLUMNS = ['id', 'payload', 'status', 'created_at', 'updated_at']
const METRIC_KEYS = ['input_tokens', 'output_tokens', 'total_tokens',
  'cache_read_tokens', 'cache_write_tokens', 'reasoning_tokens']
// 服务端 decodeEvent 只认识这 11 个顶层键；多出来的键会让整个事件被拒。
const EVENT_KEYS = ['source_event_id', 'project_id', 'project_key', 'provider', 'model',
  'occurred_at', 'usage', 'status', 'duration_ms', 'generation_duration_ms', 'ttft_ms']
const EVENT_KEY_SET = new Set(EVENT_KEYS)
const CONTROL_RE = /[\u0000-\u001f\u007f-\u009f]/
// 服务端 surroundingSpace 的 Unicode White_Space 集合：Go 的 \s 漏掉 U+00A0 和 U+3000。
const WS = '\\u0009-\\u000d\\u0020\\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000'
const EDGE_WS_RE = new RegExp(`^[${WS}]|[${WS}]$`)
const encoder = new TextEncoder()
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
const TIMESTAMP_RE = /^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:[0-5]\d(?:\.\d{1,6})?(?:[Zz]|[+-]\d{2}:\d{2})$/
const TIMESTAMP_PARTS = /^(\d{4})-(\d{2})-(\d{2})[Tt](\d{2}):(\d{2}):\d{2}/
const DAYS_IN_MONTH = [31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31]

class ConversionError extends Error {
  constructor(message, state = 'failed') {
    super(message)
    this.name = 'ConversionError'
    this.state = state
  }
}

/** 展开开头的 ~，与插件 stateDir 的处理一致。 */
function expandHome(value) {
  if (value === '~') return os.homedir()
  if (value.startsWith('~/')) return path.join(os.homedir(), value.slice(2))
  return value
}

function parseArgs(argv) {
  let dir = process.env.TOKENPIVOT_PI_STATE_DIR || '~/.pi/agent/tokenpivot'
  let apply = false
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i]
    if (arg === '--apply') { apply = true; continue }
    if (arg === '--dir') {
      const next = argv[i + 1]
      if (!next) throw new ConversionError('--dir 需要一个目录参数')
      dir = next
      i += 1
      continue
    }
    if (arg === '--help' || arg === '-h') return { help: true }
    throw new ConversionError(`未知参数：${arg}`)
  }
  return { help: false, apply, dir: expandHome(dir) }
}

function usage() {
  return [
    'usage: node convert-generation-v2.js [--dir <状态目录>] [--apply]',
    '',
    '不带 --apply 时只读检查：报告 pending 事件里有多少需要补两个计时键。',
    '--apply 先备份 queue.sqlite，再在一个 BEGIN IMMEDIATE 事务里只补缺失的',
    'generation_duration_ms、ttft_ms（值 null）。dead/overflow/meta 和绑定信息不动。',
  ].join('\n')
}

/** 可写打开之前先看恢复单元：任何 rollback journal（含零字节）或 WAL/SHM 都要保留现场停下。 */
function assertRecoveryUnitClean(dir) {
  for (const [name, state, message] of [
    [JOURNAL_FILE, 'journal_present', '发现 rollback journal（含零字节）：保持停写并交人工处理，本脚本不触发恢复'],
    [WAL_FILE, 'unexpected_wal', '发现 WAL 文件：当前队列只支持 DELETE journal，交人工处理'],
    [SHM_FILE, 'unexpected_wal', '发现 SHM 文件：当前队列只支持 DELETE journal，交人工处理'],
  ]) {
    const file = path.join(dir, name)
    try {
      fs.statSync(file)
    } catch (error) {
      if (error.code === 'ENOENT') continue
      throw new ConversionError(`读取 ${name} 失败：${error.code || 'unknown'}`)
    }
    throw new ConversionError(message, state)
  }
}

/** 只读预检现行表/字段、journal 模式和 user_version。 */
function preflight(db) {
  const journal = String(db.prepare('PRAGMA journal_mode').get()?.journal_mode ?? '').toLowerCase()
  if (journal !== 'delete') {
    throw new ConversionError(`journal_mode 是 ${journal || 'unknown'}，当前队列只支持 DELETE，交人工处理`, 'unexpected_journal_mode')
  }
  const version = Number(db.prepare('PRAGMA user_version').get()?.user_version ?? 0)
  if (version !== FORMAT_VERSION) {
    throw new ConversionError('queue.sqlite 的 user_version 不是 1，需先按现行队列格式处理', 'legacy_state')
  }
  for (const table of ['meta', 'events']) {
    const row = db.prepare("SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?").get(table)
    if (!row) throw new ConversionError(`queue.sqlite 缺少 ${table} 表`)
  }
  const columns = new Set(db.prepare('PRAGMA table_info(events)').all().map((column) => column.name))
  for (const column of REQUIRED_COLUMNS) {
    if (!columns.has(column)) throw new ConversionError(`events 表缺少 ${column} 列`)
  }
}

/** 计数类字段只能是 null 或非负安全整数。 */
function validCount(value) {
  return value === null || (typeof value === 'number' && Number.isSafeInteger(value) && value >= 0)
}

/** 服务端 validText：UTF-8 字节数在 [minimum, maximum] 内且不含 C0/C1 控制字符。 */
function validText(value, minimum, maximum) {
  if (typeof value !== 'string' || CONTROL_RE.test(value)) return false
  const size = encoder.encode(value).length
  return size >= minimum && size <= maximum
}

/** 服务端 validLabel：provider/model/project_key 的 1-128 字节，且首尾无 Unicode White_Space。 */
function validLabel(value) {
  return validText(value, 1, 128) && !EDGE_WS_RE.test(value)
}

/**
 * 服务端 parseOccurredAt 的格式边界：形状、真实日历、偏移范围；不看 180 天窗口这类业务时间规则。
 */
function validTimestamp(value) {
  if (typeof value !== 'string' || !TIMESTAMP_RE.test(value)) return false
  const [, years, months, days, hours, minutes] = TIMESTAMP_PARTS.exec(value)
  const year = Number(years)
  const month = Number(months)
  const day = Number(days)
  if (month < 1 || month > 12 || Number(hours) > 23 || Number(minutes) > 59) return false
  const leap = (year % 4 === 0 && year % 100 !== 0) || year % 400 === 0
  const lastDay = month === 2 && leap ? 29 : DAYS_IN_MONTH[month - 1]
  if (day < 1 || day > lastDay) return false
  if (value.endsWith('Z') || value.endsWith('z')) return true
  // 偏移是末尾 6 个字符，含正负号：先去掉符号再分别看小时（0-23）和分钟（0-59）。
  const offset = value.slice(-6)
  return Number(offset.slice(1, 3)) <= 23 && Number(offset.slice(4, 6)) <= 59
}

/** usage 的每个指标只能是 known/estimated 带值、missing 带 null 三种形态之一。 */
function assertMetric(metric, index, key) {
  if (metric === null || typeof metric !== 'object' || Array.isArray(metric)) {
    throw new ConversionError(`第 ${index} 个 pending 事件的 usage.${key} 不是对象`)
  }
  const keys = Object.keys(metric).sort()
  if (keys.length !== 2 || keys[0] !== 'quality' || keys[1] !== 'value') {
    throw new ConversionError(`第 ${index} 个 pending 事件的 usage.${key} 只能是 {value, quality}`)
  }
  if (metric.quality !== 'known' && metric.quality !== 'estimated' && metric.quality !== 'missing') {
    throw new ConversionError(`第 ${index} 个 pending 事件的 usage.${key}.quality 不是 known/estimated/missing`)
  }
  if (metric.quality === 'missing') {
    if (metric.value !== null) throw new ConversionError(`第 ${index} 个 pending 事件的 usage.${key} 是 missing 却带数值`)
    return
  }
  if (metric.value === null || !validCount(metric.value)) {
    throw new ConversionError(`第 ${index} 个 pending 事件的 usage.${key} 缺少非负安全整数`)
  }
}

/**
 * 现行事件的必填字段与结构。结构不合法时整体中止，不补任何键：补完键的坏事件仍会被
 * 服务端整项拒绝，转换不该把「结构已损坏」伪装成「只差两个计时键」。
 */
function assertEventShape(payload, index) {
  for (const key of Object.keys(payload)) {
    if (!EVENT_KEY_SET.has(key)) {
      throw new ConversionError(`第 ${index} 个 pending 事件含服务端不认识的字段 ${key}`)
    }
  }
  if (!validText(payload.source_event_id, 1, 256)) {
    throw new ConversionError(`第 ${index} 个 pending 事件的 source_event_id 不是 1-256 字节的字符串`)
  }
  if (!validLabel(payload.provider) || !validLabel(payload.model)) {
    throw new ConversionError(`第 ${index} 个 pending 事件的 provider 或 model 不是 1-128 字节的无首尾空白字符串`)
  }
  if (!validTimestamp(payload.occurred_at)) {
    throw new ConversionError(`第 ${index} 个 pending 事件的 occurred_at 不是带合法日期与偏移的 RFC 3339 时间`)
  }
  if (payload.usage === null || typeof payload.usage !== 'object' || Array.isArray(payload.usage)) {
    throw new ConversionError(`第 ${index} 个 pending 事件缺少 usage 对象`)
  }
  for (const [key, metric] of Object.entries(payload.usage)) {
    if (!METRIC_KEYS.includes(key)) throw new ConversionError(`第 ${index} 个 pending 事件的 usage 含未知指标 ${key}`)
    assertMetric(metric, index, key)
  }
  if ('project_id' in payload && payload.project_id !== null
    && !(typeof payload.project_id === 'string' && UUID_RE.test(payload.project_id))) {
    throw new ConversionError(`第 ${index} 个 pending 事件的 project_id 不是 UUID 或 null`)
  }
  if ('project_key' in payload && payload.project_key !== null && !validLabel(payload.project_key)) {
    throw new ConversionError(`第 ${index} 个 pending 事件的 project_key 不是 1-128 字节的无首尾空白字符串或 null`)
  }
  if ('status' in payload && payload.status !== null && !validText(payload.status, 1, 64)) {
    throw new ConversionError(`第 ${index} 个 pending 事件的 status 不是 1-64 字节的字符串或 null`)
  }
  if ('duration_ms' in payload && !validCount(payload.duration_ms)) {
    throw new ConversionError(`第 ${index} 个 pending 事件的 duration_ms 不是 null 或非负安全整数`)
  }
  // 服务端 handler.go:553 只拒绝「有效 project_id + 非空 project_key」；两者各自单独出现或全 null 都合法。
  if (typeof payload.project_key === 'string' && payload.project_key !== '' && typeof payload.project_id === 'string') {
    throw new ConversionError(`第 ${index} 个 pending 事件的 project_id 与 project_key 不能同时出现`)
  }
}

/** 现有计时值必须是 null 或非负安全整数；其他一律整体中止。返回该键是否缺失。 */
function missingTiming(payload, index, key) {
  if (!Object.prototype.hasOwnProperty.call(payload, key)) return true
  const value = payload[key]
  if (value === null) return false
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) {
    throw new ConversionError(`第 ${index} 个 pending 事件的 ${key} 不是 null 或非负安全整数`)
  }
  return false
}

/** 解析 payload 并算出补键后的文本；结构异常整体中止。 */
function planPayload(row, index) {
  let payload
  try {
    payload = JSON.parse(row.payload)
  } catch {
    throw new ConversionError(`第 ${index} 个 pending 事件的 payload 不是合法 JSON`)
  }
  if (payload === null || typeof payload !== 'object' || Array.isArray(payload)) {
    throw new ConversionError(`第 ${index} 个 pending 事件的 payload 顶层不是对象`)
  }
  if (payload.source_event_id !== row.id) {
    throw new ConversionError(`第 ${index} 个 pending 事件的 payload 与事件 ID 不一致`)
  }
  assertEventShape(payload, index)
  const missing = TIMING_KEYS.filter((key) => missingTiming(payload, index, key))
  if (missing.length === 0) return null
  const next = { ...payload }
  for (const key of missing) next[key] = null
  return JSON.stringify(next)
}

function backup(dbPath) {
  const stamp = new Date().toISOString().replace(/[:.]/g, '-')
  const target = `${dbPath}.bak-${stamp}`
  fs.copyFileSync(dbPath, target, fs.constants.COPYFILE_EXCL)
  const handle = fs.openSync(target, 'r')
  try {
    fs.fsyncSync(handle)
  } finally {
    fs.closeSync(handle)
  }
  return target
}

function main(argv) {
  const options = parseArgs(argv)
  if (options.help) {
    process.stdout.write(`${usage()}\n`)
    return 0
  }
  const mode = options.apply ? 'apply' : 'check'
  const dbPath = path.join(options.dir, QUEUE_FILE)
  if (!fs.existsSync(dbPath)) {
    process.stdout.write(`${JSON.stringify({ mode, pending: 0, convertible: 0, converted: 0, state: 'no_queue' })}\n`)
    return 0
  }
  assertRecoveryUnitClean(options.dir)
  // 预检与检查都是只读打开：不建目录、不写文件、不触发任何修复。
  const reader = new DatabaseSync(dbPath, { readOnly: true, timeout: 0 })
  let rows
  try {
    preflight(reader)
    rows = reader.prepare("SELECT rowid, id, payload FROM events WHERE status = 'pending' ORDER BY created_at, rowid").all()
  } finally {
    reader.close()
  }
  const updates = []
  rows.forEach((row, index) => {
    const next = planPayload(row, index)
    if (next !== null) updates.push({ id: row.id, payload: next })
  })
  if (!options.apply) {
    process.stdout.write(`${JSON.stringify({ mode, pending: rows.length, convertible: updates.length, converted: 0, state: updates.length === 0 ? 'converted' : 'needs_conversion' })}\n`)
    return 0
  }
  if (updates.length === 0) {
    process.stdout.write(`${JSON.stringify({ mode, pending: rows.length, convertible: 0, converted: 0, state: 'converted' })}\n`)
    return 0
  }
  const backupPath = backup(dbPath)
  const db = new DatabaseSync(dbPath, { timeout: 0 })
  let inTransaction = false
  try {
    db.exec('BEGIN IMMEDIATE')
    inTransaction = true
    const statement = db.prepare("UPDATE events SET payload = ? WHERE id = ? AND status = 'pending'")
    for (const update of updates) {
      const result = statement.run(update.payload, update.id)
      if (result.changes !== 1) throw new ConversionError('有待转换事件已不是 pending，回滚且不改动任何行', 'concurrent_change')
    }
    db.exec('COMMIT')
    inTransaction = false
  } catch (error) {
    if (inTransaction) {
      try { db.exec('ROLLBACK') } catch { /* 连接已中止时无需再次回滚 */ }
    }
    throw error instanceof ConversionError
      ? error
      : new ConversionError('写入失败：事务已回滚，原文件和备份都保留')
  } finally {
    db.close()
  }
  process.stdout.write(`${JSON.stringify({ mode, pending: rows.length, convertible: updates.length, converted: updates.length, backup: path.basename(backupPath), state: 'converted' })}\n`)
  return 0
}

try {
  process.exitCode = main(process.argv.slice(2))
} catch (error) {
  const state = error instanceof ConversionError ? error.state : 'failed'
  const message = error instanceof ConversionError ? error.message : '转换失败：未写入任何内容'
  process.stderr.write(`${JSON.stringify({ state, error: message })}\n`)
  process.exitCode = 1
}
