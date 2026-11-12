/**
 * TokenPivot 用量上报 — PI 扩展（T-047）。
 *
 * 只上报具备请求级最终用量的 assistant 终态调用（turn_end），写本地 SQLite 队列，
 * 再以同一 source_event_id 重传到 POST /api/v1/ingestion/events。
 * 不采集 Prompt、回答、工具内容、路径、会话标题或供应商密钥。
 *
 * 安装与配置见 plugins/pi/README.md。工厂阶段只注册处理器，不启动定时器或连接。
 */

import { Collector, createLogger } from './lib/collector.js'

export const name = 'tokenpivot-pi'

/**
 * @param {import('@earendil-works/pi-coding-agent').ExtensionAPI} pi
 */
export default function tokenpivotUsage(pi) {
  const logger = createLogger(process.stderr)
  /** @type {Collector|null} */
  let active = null

  const ensure = () => {
    if (!active || active.stopped) active = new Collector({ logger })
    return active
  }

  pi.on('session_start', () => {
    // 只初始化配置、队列和调度；不扫描历史会话，也不回填 fork 复制出来的条目。
    void ensure().start().catch(() => logger.log('start_failed'))
  })

  pi.on('turn_end', async (event, ctx) => {
    const collector = active
    if (!collector || collector.stopped) return
    try {
      await collector.handleTurnEnd(event, ctx)
    } catch {
      // 绝不把插件异常抛回 PI；计数在 collector 内部完成。
      logger.log('handle_failed')
    }
  })

  pi.on('session_compact', () => {
    active?.handleCompact()
  })

  pi.on('session_shutdown', async () => {
    const collector = active
    active = null
    if (collector) await collector.stop().catch(() => {})
  })

  pi.registerCommand('tokenpivot-confirm-same-source', {
    description: '确认新接入凭证仍属原 source，继续发送本机队列里未确认的旧事件（TokenPivot）',
    handler: async (_args, ctx) => {
      const collector = ensure()
      await collector.start().catch(() => {})
      const before = collector.status()
      const result = await collector.confirmSameSource()
      if (result.ok) {
        const revision = collector.queue?.binding()?.revision ?? 0
        const wasRotating = before.paused && before.pauseReason === 'token_changed'
        notify(
          ctx,
          `TokenPivot：已确认新凭证仍属原 source（${before.service ?? '未配置'}），bindingRevision=${revision}；`
            + (wasRotating ? '旧队列继续发送。' : '本地绑定无需变更。'),
          'info',
        )
        return
      }
      // 服务地址不匹配 / 条件已变化 / 还没有凭证：一律保持暂停，不发送旧事件。
      notify(ctx, `TokenPivot：未确认（${result.reason}）；队列保持暂停，未发送任何旧事件。`, 'warning')
    },
  })
}

function notify(ctx, message, level) {
  try {
    ctx?.ui?.notify?.(message, level)
  } catch {
    // 无 UI（例如 headless）时静默：状态仍可通过 stderr 的固定码观察到。
  }
}
