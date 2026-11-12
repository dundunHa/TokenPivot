/**
 * PI turn_end → TokenPivot UsageEvent（接口定义见 docs/openapi/openapi.yaml 的 UsageEvent）。
 *
 * 只映射具备请求级最终用量的 assistant 终态消息：
 *   - turn_end 的 message.usage 是**最终**用量；stopReason 为 pending/deferred 的不是终态。
 *   - occurred_at = message.timestamp + message.durationMs；缺 durationMs 就没有可证实的完成时间，
 *     一律跳过（不拿开始时间或采集时间替代）。
 *   - source_event_id 带上 sessionId：entryId 只在会话内唯一，两个会话同 entryId 仍是两条事件。
 *
 * 不采集：Prompt、回答、工具内容、cwd（只在本地用于项目匹配）、会话标题。
 */
/** 接口上限：0 ≤ value ≤ 2^53-1。 */
export const MAX_METRIC = Number.MAX_SAFE_INTEGER

const CONTROL_RE = /[\u0000-\u001f\u007f-\u009f]/
const WS = '\\u0009-\\u000d\\u0020\\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000'
const EDGE_WS_RE = new RegExp(`^[${WS}]|[${WS}]$`)
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
const encoder = new TextEncoder()

const byteLength = (s) => encoder.encode(s).length

/** provider/model：1–128 UTF-8 字节，无首尾空白、无控制字符。 */
export function validName(value) {
  return typeof value === 'string' && value.length > 0 && !EDGE_WS_RE.test(value)
    && !CONTROL_RE.test(value) && byteLength(value) <= 128
}

/** source_event_id：1–256 UTF-8 字节，无控制字符。 */
export function validEventId(value) {
  return typeof value === 'string' && value.length > 0 && !CONTROL_RE.test(value) && byteLength(value) <= 256
}

const isCount = (v) => Number.isSafeInteger(v) && v >= 0 && v <= MAX_METRIC
const known = (value) => ({ value, quality: 'known' })
const estimated = (value) => ({ value, quality: 'estimated' })
const MISSING = Object.freeze({ value: null, quality: 'missing' })

/**
 * PI 的 Usage 字段名（@earendil-works/pi-ai 的 Usage）：
 * input / output / cacheRead / cacheWrite / cacheWrite1h? / reasoning? / totalTokens。
 *
 * 口径规则（计划 §4）：
 *   - cacheWrite1h 是 cacheWrite 的子集，不再相加；reasoning 是 output 的子集，不加到 output 或 total。
 *   - 服务端 input 含缓存（cacheRead + cacheWrite），来源 input 的口径按 total 恒等关系判断：
 *       total = input + output + cacheRead + cacheWrite → 服务端 input = input + cacheRead + cacheWrite
 *       total = input + output                            → 服务端 input = input
 *       缓存全为已知零                                      → 两式等价，input 精确
 *     证据不足 → 保留来源 input 但标 estimated（未知缓存不当已知零，避免服务端推导出虚假 known）。
 *   - total 有效则保留来源数值；不取最大值钳制，不补 0。
 *
 * @returns {{usage: object}|{skip: string}} skip 为固定原因码
 */
export function mapUsage(u) {
  if (!u || typeof u !== 'object') return { skip: 'usage_missing' }

  let invalid = 0
  const read = (key) => {
    const v = u[key]
    if (v === undefined || v === null) return MISSING
    if (!isCount(v)) {
      invalid += 1
      return MISSING
    }
    return known(v)
  }

  const input = read('input')
  const output = read('output')
  const cacheRead = read('cacheRead')
  const cacheWrite = read('cacheWrite')
  const total = read('totalTokens')
  const reasoning = read('reasoning')

  const allMissing = [input, output, cacheRead, cacheWrite, total, reasoning]
    .every((m) => m.quality === 'missing')
  if (allMissing) return { skip: 'usage_missing' }

  const cacheKnown = cacheRead.quality === 'known' && cacheWrite.quality === 'known'
  const cacheSum = cacheKnown ? cacheRead.value + cacheWrite.value : null
  const cacheZero = cacheKnown && cacheSum === 0

  let inputOut = MISSING
  let totalOut = MISSING

  if (input.quality === 'known') {
    if (total.quality === 'known' && output.quality === 'known') {
      const plain = input.value + output.value
      if (cacheZero) {
        inputOut = known(input.value)
        // 缓存为零时两式等价；若 total 连 input+output 都不等，只能承认它不可解释。
        totalOut = total.value === plain ? known(total.value) : estimated(total.value)
      } else if (cacheKnown) {
        const withCache = plain + cacheSum
        if (total.value === withCache) {
          inputOut = known(input.value + cacheSum)
          totalOut = known(total.value)
        } else if (total.value === plain && cacheSum <= input.value) {
          inputOut = known(input.value)
          totalOut = known(total.value)
        } else {
          inputOut = estimated(input.value)
          totalOut = estimated(total.value)
        }
      } else {
        // 缓存未报告，恒等关系无法核实：保留原值但降级为 estimated。
        inputOut = estimated(input.value)
        totalOut = estimated(total.value)
      }
    } else if (total.quality === 'known') {
      // 缺 output，无法核实输入口径；未知缓存不当已知零。
      inputOut = cacheZero ? known(input.value) : estimated(input.value)
      totalOut = known(total.value)
    } else {
      // total 缺失：服务端会用 input + output 推导，input 的质量必须如实反映证据强度。
      inputOut = cacheZero ? known(input.value) : estimated(input.value)
      totalOut = MISSING
    }
  } else if (total.quality === 'known') {
    totalOut = known(total.value)
  }

  if (inputOut.quality === 'known' && inputOut.value > MAX_METRIC) return { skip: 'usage_overflow' }
  if (totalOut.quality !== 'missing' && totalOut.value > MAX_METRIC) return { skip: 'usage_overflow' }
  // 已证实 input 是父项、cache 是子项；已知父项小于已知子项说明口径自相矛盾，不增大数值去凑校验。
  if (inputOut.quality === 'known' && cacheKnown && cacheSum > inputOut.value) return { skip: 'subset_violation' }

  // reasoning 是 output 子集：超出 output 时如实报缺失，不钳制、也不加到 output。
  const reasoningOut = reasoning.quality === 'known'
    && output.quality === 'known' && reasoning.value <= output.value
    ? known(reasoning.value)
    : MISSING
  if (reasoning.quality === 'known' && reasoningOut.quality === 'missing') invalid += 1

  return {
    usage: {
      input_tokens: inputOut,
      output_tokens: output,
      total_tokens: totalOut,
      cache_read_tokens: cacheRead,
      cache_write_tokens: cacheWrite,
      reasoning_tokens: reasoningOut,
    },
    invalid,
  }
}

/** RFC 3339 UTC，毫秒精度（接口允许最多 6 位小数）。 */
export function formatTime(ms) {
  if (!Number.isFinite(ms)) return null
  const d = new Date(ms)
  return Number.isNaN(d.getTime()) ? null : d.toISOString()
}

function normalizeDir(p) {
  if (typeof p !== 'string' || p.length === 0) return null
  let s = p.replace(/\\/g, '/')
  while (s.length > 1 && s.endsWith('/')) s = s.slice(0, -1)
  return s
}

/**
 * 本地目录 → project_id：按路径边界最长匹配（`/foo/bar2` 不算 `/foo/bar` 的子目录）。
 * 路径只在本机参与匹配，从不上报。未匹配为 null。
 */
export function resolveProject(cwd, mappings) {
  const dir = normalizeDir(cwd)
  if (!dir || !Array.isArray(mappings)) return null
  let best = null
  let bestLen = -1
  for (const m of mappings) {
    const base = normalizeDir(m?.path)
    const id = typeof m?.projectId === 'string' ? m.projectId.trim().toLowerCase() : ''
    if (!base || !UUID_RE.test(id)) continue
    const hit = dir === base || dir.startsWith(base === '/' ? '/' : `${base}/`)
    if (hit && base.length > bestLen) {
      best = id
      bestLen = base.length
    }
  }
  return best
}

/**
 * turn_end → UsageEvent。
 * @param {{
 *   sessionId: string,
 *   messageEntryId: string,
 *   message: any,
 *   cwd?: string,
 *   projects?: {path: string, projectId: string}[],
 * }} input
 * @returns {{event: object, invalid: number} | {skip: string}}
 */
export function mapTurnEnd({ sessionId, messageEntryId, message, cwd, projects }) {
  if (!message || typeof message !== 'object' || message.role !== 'assistant') return { skip: 'not_assistant' }
  const stopReason = message.stopReason
  if (stopReason === 'pending') return { skip: 'pending' }
  if (stopReason === 'deferred') return { skip: 'deferred' }
  // error / aborted 也只在事实齐全时采集：下面 durationMs 与 usage 的校验就是这道闸。

  const durationMs = message.durationMs
  if (typeof durationMs !== 'number' || !Number.isFinite(durationMs) || durationMs < 0) {
    return { skip: 'no_duration' }
  }
  const timestamp = message.timestamp
  if (typeof timestamp !== 'number' || !Number.isFinite(timestamp)) return { skip: 'no_time' }

  if (typeof sessionId !== 'string' || sessionId === '') return { skip: 'invalid_id' }
  if (typeof messageEntryId !== 'string' || messageEntryId === '') return { skip: 'invalid_id' }
  const sourceEventId = `pi:msg:${sessionId}:${messageEntryId}`
  if (!validEventId(sourceEventId)) return { skip: 'invalid_id' }

  if (!validName(message.provider)) return { skip: 'no_provider' }
  // 实际归属优先 responseModel（供应商路由后可不同于请求模型）；无效时回落到 message.model。
  const model = validName(message.responseModel) ? message.responseModel : message.model
  if (!validName(model)) return { skip: 'no_model' }

  const mapped = mapUsage(message.usage)
  if ('skip' in mapped) return { skip: mapped.skip }

  const occurredAt = formatTime(timestamp + durationMs)
  if (!occurredAt) return { skip: 'no_time' }

  return {
    event: {
      source_event_id: sourceEventId,
      project_id: resolveProject(cwd, projects),
      provider: message.provider,
      model,
      occurred_at: occurredAt,
      usage: mapped.usage,
      // 不把 aborted 宣称成功：只有 error 映射为 error，其余是 null（来源未提供）。
      status: stopReason === 'error' ? 'error' : null,
      // durationMs 是单调时钟测得的耗时，接口只收整数毫秒。
      duration_ms: Math.round(durationMs),
    },
    invalid: mapped.invalid ?? 0,
  }
}
