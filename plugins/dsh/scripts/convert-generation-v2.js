#!/usr/bin/env node
/**
 * 一次性转换 DSH 本地队列（queue.json）里的旧请求格式（T-070）。
 *
 * 旧客户端的事件可能没有 `generation_duration_ms`、`ttft_ms` 两个键；新服务端要求两键必填，
 * 未知写 null。本脚本只给 pending 事件补缺失的这两个键（补 null），不动任何其他字段。
 *
 * 安全边界：
 *   - 默认只读检查；只有显式 `--apply` 才写入。
 *   - 直接读 queue.json，不调用 FileQueue.load（它会建目录、修 aside、去重、裁剪）。
 *   - 不动 dead-letter.jsonl，不重投死信。
 *   - 文件不存在、没有待补键、检查模式、输入损坏都不创建目录、备份或临时文件。
 *   - apply 先备份原文件，再同目录写临时文件、fsync、原子 rename；失败保留原文件和备份。
 *   - 不改 ID、顺序、绑定信息或任何其他字段；二次运行为 0。
 *   - 事件的顶层键超出服务端认识的 11 个、必填字段缺失或越界、provider/model 带首尾空白、
 *     project_id 与 project_key 同时有效、usage 结构非法时整体中止，check/apply 都不写入。
 *
 * 用法：node convert-generation-v2.js [--dir <状态目录>] [--apply] [--help]
 */
import { promises as fs } from 'node:fs'
import { constants as fsConstants } from 'node:fs'
import { randomBytes } from 'node:crypto'
import os from 'node:os'
import path from 'node:path'

const QUEUE_FILE = 'queue.json'
const TIMING_KEYS = ['generation_duration_ms', 'ttft_ms']
const FORMAT = 1
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
  constructor(message) {
    super(message)
    this.name = 'ConversionError'
  }
}

/** 展开开头的 ~，与插件配置的 stateDir 处理一致。 */
function expandHome(value) {
  if (value === '~') return os.homedir()
  if (value.startsWith('~/')) return path.join(os.homedir(), value.slice(2))
  return value
}

function parseArgs(argv) {
  let dir = process.env.TOKENPIVOT_DSH_STATE_DIR || '~/.tokenpivot/dsh'
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
    '--apply 先备份 queue.json，再原子替换，只补缺失的 generation_duration_ms、ttft_ms（值 null）。',
  ].join('\n')
}

/** 读取并严格校验队列文件；文件不存在返回 null。 */
async function readQueue(file) {
  let text
  try {
    text = await fs.readFile(file, 'utf8')
  } catch (error) {
    if (error.code === 'ENOENT') return null
    throw new ConversionError(`读取队列文件失败：${error.code || 'unknown'}`)
  }
  let parsed
  try {
    parsed = JSON.parse(text)
  } catch {
    throw new ConversionError('队列文件不是合法 JSON')
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    throw new ConversionError('队列文件顶层不是对象')
  }
  if (parsed.format !== FORMAT) throw new ConversionError('队列文件 format 不是 1')
  if (!Array.isArray(parsed.items)) throw new ConversionError('队列文件 items 不是数组')
  const seen = new Set()
  parsed.items.forEach((item, index) => {
    if (item === null || typeof item !== 'object' || Array.isArray(item)) {
      throw new ConversionError(`第 ${index} 个事件不是对象`)
    }
    if (typeof item.source_event_id !== 'string' || item.source_event_id === '') {
      throw new ConversionError(`第 ${index} 个事件缺少 source_event_id`)
    }
    if (seen.has(item.source_event_id)) throw new ConversionError(`第 ${index} 个事件 ID 重复`)
    seen.add(item.source_event_id)
  })
  return { doc: parsed, items: parsed.items }
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
    throw new ConversionError(`第 ${index} 个事件的 usage.${key} 不是对象`)
  }
  const keys = Object.keys(metric).sort()
  if (keys.length !== 2 || keys[0] !== 'quality' || keys[1] !== 'value') {
    throw new ConversionError(`第 ${index} 个事件的 usage.${key} 只能是 {value, quality}`)
  }
  if (metric.quality !== 'known' && metric.quality !== 'estimated' && metric.quality !== 'missing') {
    throw new ConversionError(`第 ${index} 个事件的 usage.${key}.quality 不是 known/estimated/missing`)
  }
  if (metric.quality === 'missing') {
    if (metric.value !== null) throw new ConversionError(`第 ${index} 个事件的 usage.${key} 是 missing 却带数值`)
    return
  }
  if (metric.value === null || !validCount(metric.value)) {
    throw new ConversionError(`第 ${index} 个事件的 usage.${key} 缺少非负安全整数`)
  }
}

/**
 * 现行事件的必填字段与结构。结构不合法时整体中止，不补任何键：补完键的坏事件仍会被
 * 服务端整项拒绝，转换不该把「结构已损坏」伪装成「只差两个计时键」。
 */
function assertEventShape(item, index) {
  for (const key of Object.keys(item)) {
    if (!EVENT_KEY_SET.has(key)) {
      throw new ConversionError(`第 ${index} 个事件含服务端不认识的字段 ${key}`)
    }
  }
  if (!validText(item.source_event_id, 1, 256)) {
    throw new ConversionError(`第 ${index} 个事件的 source_event_id 不是 1-256 字节的字符串`)
  }
  if (!validLabel(item.provider) || !validLabel(item.model)) {
    throw new ConversionError(`第 ${index} 个事件的 provider 或 model 不是 1-128 字节的无首尾空白字符串`)
  }
  if (!validTimestamp(item.occurred_at)) {
    throw new ConversionError(`第 ${index} 个事件的 occurred_at 不是带合法日期与偏移的 RFC 3339 时间`)
  }
  if (item.usage === null || typeof item.usage !== 'object' || Array.isArray(item.usage)) {
    throw new ConversionError(`第 ${index} 个事件缺少 usage 对象`)
  }
  for (const [key, metric] of Object.entries(item.usage)) {
    if (!METRIC_KEYS.includes(key)) throw new ConversionError(`第 ${index} 个事件的 usage 含未知指标 ${key}`)
    assertMetric(metric, index, key)
  }
  if ('project_id' in item && item.project_id !== null
    && !(typeof item.project_id === 'string' && UUID_RE.test(item.project_id))) {
    throw new ConversionError(`第 ${index} 个事件的 project_id 不是 UUID 或 null`)
  }
  if ('project_key' in item && item.project_key !== null && !validLabel(item.project_key)) {
    throw new ConversionError(`第 ${index} 个事件的 project_key 不是 1-128 字节的无首尾空白字符串或 null`)
  }
  if ('status' in item && item.status !== null && !validText(item.status, 1, 64)) {
    throw new ConversionError(`第 ${index} 个事件的 status 不是 1-64 字节的字符串或 null`)
  }
  if ('duration_ms' in item && !validCount(item.duration_ms)) {
    throw new ConversionError(`第 ${index} 个事件的 duration_ms 不是 null 或非负安全整数`)
  }
  // 服务端 handler.go:553 只拒绝「有效 project_id + 非空 project_key」；两者各自单独出现或全 null 都合法。
  if (typeof item.project_key === 'string' && item.project_key !== '' && typeof item.project_id === 'string') {
    throw new ConversionError(`第 ${index} 个事件的 project_id 与 project_key 不能同时出现`)
  }
}

/** 现有计时值必须是 null 或非负安全整数；其他一律整体中止。返回该键是否缺失。 */
function missingTiming(item, index, key) {
  if (!Object.prototype.hasOwnProperty.call(item, key)) return true
  const value = item[key]
  if (value === null) return false
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) {
    throw new ConversionError(`第 ${index} 个事件的 ${key} 不是 null 或非负安全整数`)
  }
  return false
}

/** 返回补键后的副本，并统计补了几个键。 */
function fillTimings(items) {
  let patched = 0
  const out = items.map((item, index) => {
    assertEventShape(item, index)
    const missing = TIMING_KEYS.filter((key) => missingTiming(item, index, key))
    if (missing.length === 0) return item
    patched += 1
    const next = { ...item }
    for (const key of missing) next[key] = null
    return next
  })
  return { items: out, patched }
}

async function backup(file) {
  const stamp = new Date().toISOString().replace(/[:.]/g, '-')
  const target = `${file}.bak-${stamp}`
  await fs.copyFile(file, target, fsConstants.COPYFILE_EXCL)
  await syncFile(target)
  return target
}

async function syncFile(target) {
  const handle = await fs.open(target, 'r')
  try {
    await handle.sync()
  } finally {
    await handle.close()
  }
}

/** 同目录临时文件 + fsync + 原子 rename，权限 0600。 */
async function replaceAtomically(file, snapshot) {
  const tmp = `${file}.${process.pid}.${randomBytes(6).toString('hex')}.tmp`
  const handle = await fs.open(tmp, 'w', 0o600)
  try {
    await handle.writeFile(snapshot)
    await handle.sync()
  } finally {
    await handle.close()
  }
  try {
    await fs.rename(tmp, file)
  } catch (error) {
    await fs.rm(tmp, { force: true })
    throw error
  }
  await syncDir(path.dirname(file))
}

async function syncDir(dir) {
  let handle
  try {
    handle = await fs.open(dir, 'r')
    await handle.sync()
  } catch {
    // 目录 fsync 不是所有平台都支持；文件本身已经同步。
  } finally {
    await handle?.close().catch(() => {})
  }
}

async function main(argv) {
  const options = parseArgs(argv)
  if (options.help) {
    process.stdout.write(`${usage()}\n`)
    return 0
  }
  const file = path.join(options.dir, QUEUE_FILE)
  const queue = await readQueue(file)
  if (queue === null) {
    process.stdout.write(`${JSON.stringify({ mode: options.apply ? 'apply' : 'check', items: 0, convertible: 0, converted: 0, state: 'no_queue' })}\n`)
    return 0
  }
  const { items, patched } = fillTimings(queue.items)
  const mode = options.apply ? 'apply' : 'check'
  if (!options.apply) {
    process.stdout.write(`${JSON.stringify({ mode, items: items.length, convertible: patched, converted: 0, state: patched === 0 ? 'converted' : 'needs_conversion' })}\n`)
    return 0
  }
  if (patched === 0) {
    process.stdout.write(`${JSON.stringify({ mode, items: items.length, convertible: 0, converted: 0, state: 'converted' })}\n`)
    return 0
  }
  const backupFile = await backup(file)
  await replaceAtomically(file, JSON.stringify({ ...queue.doc, items }))
  process.stdout.write(`${JSON.stringify({ mode, items: items.length, convertible: patched, converted: patched, backup: path.basename(backupFile), state: 'converted' })}\n`)
  return 0
}

main(process.argv.slice(2)).then((code) => {
  process.exitCode = code
}).catch((error) => {
  const message = error instanceof ConversionError ? error.message : '转换失败：未写入任何内容'
  process.stderr.write(`${JSON.stringify({ state: 'failed', error: message })}\n`)
  process.exitCode = 1
})
