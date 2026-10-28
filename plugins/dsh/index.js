/**
 * TokenPivot usage collector — DSH host plugin (T-045).
 *
 * Listens to DSH's post-commit `session/event` feed, turns final per-request usage
 * (`assistant/message`, `compaction/summary`) into TokenPivot UsageEvents, stores them in a
 * durable local queue and uploads them to POST /api/v1/ingestion/events, retrying with the
 * same source_event_id until the server gives a final answer.
 *
 * Not installed by default. See plugins/dsh/README.md.
 */
import Schema from '@deepseek-ai/schemastery'
import { Collector, expandHome, ingestionEndpoint } from './lib/collector.js'

export const name = 'tokenpivot-usage'

export const Config = Schema.object({
  serverUrl: Schema.string().default('').description('TokenPivot 服务地址，如 https://tokenpivot.example.com（只允许 https；本机回环地址可用 http）'),
  token: Schema.string().role('secret').default('').volatile().description('接入凭证（tpk_ 开头），只保存在 DSH 配置中，不写入队列文件和日志'),
  projects: Schema.array(Schema.object({
    path: Schema.string().required().description('本地项目目录（会话工作目录在它下面即归属该项目）'),
    projectId: Schema.string().required().description('TokenPivot 项目 ID（UUID）'),
  })).default([]).description('本地目录 → 项目映射；没匹配上的用量上报为未归属。目录只在本机用于匹配，不上报'),
  stateDir: Schema.string().default('~/.tokenpivot/dsh').description('本地队列目录'),
  maxQueueItems: Schema.natural().min(100).default(10000).description('本地队列上限，超出丢弃最旧的事件'),
})

/** Volatile config values are read through `.get()`; plain values are used as is. */
const read = (value) => (value && typeof value.get === 'function' ? value.get() : value)

export function apply(ctx, config) {
  const endpoint = ingestionEndpoint(config.serverUrl)
  if (!endpoint) ctx.logger.warn('tokenpivot-usage: serverUrl missing or not https; events are queued locally but not uploaded')

  const collector = new Collector({
    stateDir: expandHome(config.stateDir),
    endpoint,
    getToken: () => {
      const token = read(config.token)
      return typeof token === 'string' && token.trim() !== '' ? token.trim() : undefined
    },
    projects: config.projects,
    // Session header cwd is the directory the session was opened in; it stays on this machine.
    resolveCwd: (session) => session?.header?.cwd,
    maxQueueItems: config.maxQueueItems,
    logger: {
      warn: (message) => ctx.logger.warn(`tokenpivot-usage: ${message}`),
    },
  })

  collector.start().catch((error) => ctx.logger.warn(`tokenpivot-usage: cannot load queue: ${error?.message ?? error}`))

  ctx.on('session/event', (session, event) => {
    collector.handle(session, event).catch((error) => {
      ctx.logger.warn(`tokenpivot-usage: cannot queue event: ${error?.message ?? error}`)
    })
  })

  ctx.effect(() => () => { void collector.stop() })
}
