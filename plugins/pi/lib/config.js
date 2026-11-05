/**
 * PI 插件配置：默认 `~/.tokenpivot/pi/config.json`，可用 TOKENPIVOT_PI_CONFIG 指定路径。
 *
 * 环境变量 TOKENPIVOT_PI_TOKEN 优先于配置文件里的 token，这样凭证可以不落盘。
 * 本模块只做读取与校验：不创建目录、不改权限、不发网络请求、不写日志。
 * 所有失败都以固定的原因码返回，绝不把配置值、token 或原始异常带进原因码。
 */
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { createHash } from 'node:crypto'

export const INGESTION_PATH = '/api/v1/ingestion/events'
export const DEFAULT_STATE_DIR = path.join('.tokenpivot', 'pi')
export const DEFAULT_MAX_QUEUE_ITEMS = 10000
export const DEFAULT_FLUSH_DELAY_MS = 2000
export const DEFAULT_REQUEST_TIMEOUT_MS = 30000
/** 独立于事件的配置检查周期（计划 §6）。 */
export const CONFIG_POLL_MS = 30000

const LOOPBACK = new Set(['localhost', '127.0.0.1', '[::1]'])
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

export function expandHome(p, home = os.homedir()) {
  if (typeof p !== 'string' || p === '') return null
  if (p === '~') return home
  if (p.startsWith('~/')) return path.join(home, p.slice(2))
  return p
}

/**
 * 凭证指纹：SHA-256(`TokenPivotPI/token/v1\0` + token) 的完整十六进制摘要。
 * 单向指纹而非加密：只用于检测 token 字节是否变化，不落盘明文。
 */
export function tokenFingerprint(token) {
  if (typeof token !== 'string' || token === '') return null
  return createHash('sha256').update(`TokenPivotPI/token/v1\0${token}`).digest('hex')
}

/**
 * 规范化服务地址：只允许 https（回环地址可用 http），拒绝 URL 用户信息，
 * 强制以 /api/v1/ingestion/events 结尾。查询串不静默丢弃——带查询串即判为不合法。
 * @returns {{endpoint: string} | {error: string}}
 */
export function normalizeServerUrl(value) {
  if (typeof value !== 'string' || value.trim() === '') return { error: 'server_url_missing' }
  let url
  try {
    url = new URL(value.trim())
  } catch {
    return { error: 'server_url_invalid' }
  }
  if (url.username !== '' || url.password !== '') return { error: 'server_url_userinfo' }
  if (url.protocol !== 'https:' && !(url.protocol === 'http:' && LOOPBACK.has(url.hostname))) {
    return { error: 'server_url_scheme' }
  }
  if (url.search !== '') return { error: 'server_url_query' }
  url.hash = ''
  let base = url.pathname.replace(/\/+$/, '')
  if (base.endsWith(INGESTION_PATH)) base = base.slice(0, -INGESTION_PATH.length)
  url.pathname = `${base}${INGESTION_PATH}`
  return { endpoint: url.toString() }
}

/** 文件与目录不得向组/其他用户开放；软链不跟随后 chmod，直接判为不满足。 */
function checkPrivatePerms(fsImpl, file, dir) {
  let link
  try {
    link = fsImpl.lstatSync(file)
  } catch {
    return 'config_unreadable'
  }
  if (link.isSymbolicLink()) return 'config_symlink'
  if ((link.mode & 0o077) !== 0) return 'config_permissions'
  let dirStat
  try {
    dirStat = fsImpl.statSync(dir)
  } catch {
    return 'config_unreadable'
  }
  if (!dirStat.isDirectory() || (dirStat.mode & 0o077) !== 0) return 'config_permissions'
  return null
}

const isPositiveInt = (v) => Number.isSafeInteger(v) && v > 0
const isNonNegativeInt = (v) => Number.isSafeInteger(v) && v >= 0

/**
 * 读取并校验配置。返回值里 token 只在内存中存在，调用方不得写日志。
 *
 * status 语义：
 *   ok            地址与凭证齐备，可采集并发送
 *   no_token      地址有效、凭证为空：入队但不发送（不丢）
 *   unconfigured  没有服务地址：已安装待配置，不开始采集
 *   paused        权限/软链/地址不合法：暂停采集与发送，保留既有队列
 *   error         配置文件读不了或不是合法 JSON：暂停，不覆盖既有数据
 */
export function loadConfig(options = {}) {
  const env = options.env ?? process.env
  const fsImpl = options.fs ?? fs
  const home = options.home ?? os.homedir()
  const rawPath = typeof env.TOKENPIVOT_PI_CONFIG === 'string' ? env.TOKENPIVOT_PI_CONFIG.trim() : ''
  const configPath = expandHome(rawPath || path.join(home, DEFAULT_STATE_DIR, 'config.json'), home) ?? ''

  const config = {
    configPath,
    stateDir: expandHome(path.join(home, DEFAULT_STATE_DIR), home),
    maxQueueItems: DEFAULT_MAX_QUEUE_ITEMS,
    flushDelayMs: DEFAULT_FLUSH_DELAY_MS,
    requestTimeoutMs: DEFAULT_REQUEST_TIMEOUT_MS,
    projects: [],
    serverUrl: null,
    token: undefined,
    fingerprint: null,
  }
  const fail = (status, reason) => ({ status, reason, config })

  let text
  try {
    text = fsImpl.readFileSync(configPath, 'utf8')
  } catch (error) {
    if (error?.code === 'ENOENT') return fail('unconfigured', 'config_missing')
    return fail('error', 'config_unreadable')
  }

  let parsed
  try {
    parsed = JSON.parse(text)
  } catch {
    return fail('error', 'config_invalid')
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return fail('error', 'config_invalid')

  const stateDir = expandHome(parsed.stateDir, home)
  if (typeof stateDir === 'string' && stateDir !== '') config.stateDir = stateDir
  if (isPositiveInt(parsed.maxQueueItems)) config.maxQueueItems = parsed.maxQueueItems
  if (isNonNegativeInt(parsed.flushDelayMs)) config.flushDelayMs = parsed.flushDelayMs
  if (isPositiveInt(parsed.requestTimeoutMs)) config.requestTimeoutMs = parsed.requestTimeoutMs

  if (Array.isArray(parsed.projects)) {
    for (const entry of parsed.projects) {
      const dir = typeof entry?.path === 'string' ? entry.path.trim() : ''
      const projectId = typeof entry?.projectId === 'string' ? entry.projectId.trim() : ''
      // 无效映射条目直接忽略：不能因为一条写错就上报到错误的项目。
      if (dir === '' || !UUID_RE.test(projectId)) continue
      config.projects.push({ path: dir, projectId: projectId.toLowerCase() })
    }
  }

  const envToken = typeof env.TOKENPIVOT_PI_TOKEN === 'string' ? env.TOKENPIVOT_PI_TOKEN.trim() : ''
  const fileToken = typeof parsed.token === 'string' ? parsed.token.trim() : ''
  const tokenFromFile = envToken === '' && fileToken !== ''
  config.token = envToken !== '' ? envToken : fileToken !== '' ? fileToken : undefined
  config.fingerprint = tokenFingerprint(config.token ?? null)

  // 配置文件里存 token 时要求 0600/0700；不满足就暂停启用，不改动任何权限位。
  if (tokenFromFile) {
    const denied = checkPrivatePerms(fsImpl, configPath, path.dirname(configPath))
    if (denied) return fail('paused', denied)
  }

  const normalized = normalizeServerUrl(parsed.serverUrl)
  if ('error' in normalized) {
    // 没有地址属于「待配置」；写了不合法地址属于需要用户修的暂停状态。
    if (normalized.error === 'server_url_missing') return fail('unconfigured', 'server_url_missing')
    return fail('paused', normalized.error)
  }
  config.serverUrl = normalized.endpoint
  config.normalizedServerUrl = normalized.endpoint
  if (!config.token) return fail('no_token', 'token_missing')
  return { status: 'ok', reason: null, config }
}
