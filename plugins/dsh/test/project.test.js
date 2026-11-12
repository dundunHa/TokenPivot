import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, mkdir, writeFile, realpath } from 'node:fs/promises'
import os from 'node:os'
import path from 'node:path'
import { detectProjectKey, cachedProjectKeyDetector, validProjectKey } from '../lib/project.js'
import { mapSessionEvent } from '../lib/mapper.js'

const root = async () => realpath(await mkdtemp(path.join(os.tmpdir(), 'tp-proj-')))

test('detectProjectKey: nearest git root up the tree names the project, subdirectories included', async () => {
  const base = await root()
  await mkdir(path.join(base, 'TokenPivot', '.git'), { recursive: true })
  await mkdir(path.join(base, 'TokenPivot', 'web', 'src'), { recursive: true })
  assert.equal(detectProjectKey(path.join(base, 'TokenPivot')), 'TokenPivot')
  assert.equal(detectProjectKey(path.join(base, 'TokenPivot', 'web', 'src')), 'TokenPivot')
})

test('detectProjectKey: nested repository wins over the outer one', async () => {
  const base = await root()
  await mkdir(path.join(base, 'outer', '.git'), { recursive: true })
  await mkdir(path.join(base, 'outer', 'inner', '.git'), { recursive: true })
  assert.equal(detectProjectKey(path.join(base, 'outer', 'inner')), 'inner')
})

test('detectProjectKey: a linked worktree resolves to the main repository name', async () => {
  const base = await root()
  const wt = path.join(base, 'wt', 'feature-x')
  await mkdir(wt, { recursive: true })
  await writeFile(path.join(wt, '.git'), `gitdir: ${base}/TokenPivot/.git/worktrees/feature-x\n`)
  assert.equal(detectProjectKey(wt), 'TokenPivot')
  const win = path.join(base, 'wt', 'win')
  await mkdir(win, { recursive: true })
  await writeFile(path.join(win, '.git'), 'gitdir: C:\\code\\TokenPivot\\.git\\worktrees\\win\n')
  assert.equal(detectProjectKey(win), 'TokenPivot')
})

test('detectProjectKey: submodule .git file (not a worktree) names the submodule directory', async () => {
  const base = await root()
  const sub = path.join(base, 'super', 'libs', 'dep')
  await mkdir(sub, { recursive: true })
  await writeFile(path.join(sub, '.git'), 'gitdir: ../../.git/modules/dep\n')
  assert.equal(detectProjectKey(sub), 'dep')
})

test('detectProjectKey: no repository, no cwd or an unusable name give null', async () => {
  const base = await root()
  await mkdir(path.join(base, 'plain'), { recursive: true })
  assert.equal(detectProjectKey(path.join(base, 'plain'), { stat: () => undefined }), null)
  assert.equal(detectProjectKey(undefined), null)
  assert.equal(detectProjectKey(''), null)
  assert.equal(detectProjectKey('/', { stat: (p) => (p === '/.git' ? { isFile: () => false } : undefined) }), null)
  assert.equal(detectProjectKey('/x/y', { stat: (p) => (p === '/x/y/.git' ? { isFile: () => false } : undefined), readFile: () => '' }), 'y')
  assert.equal(detectProjectKey('/x/' + 'é'.repeat(70), { stat: (p) => ({ isFile: () => false }) }), null) // 140 bytes
  assert.equal(detectProjectKey('/x/bad\u0001name', { stat: () => ({ isFile: () => false }) }), null)
})

test('detectProjectKey: a stat that throws is treated as "no .git here"', () => {
  let calls = 0
  const key = detectProjectKey('/a/b/c', { stat: (p) => { calls += 1; if (p === '/a/.git') return { isFile: () => false }; throw new Error('EACCES') } })
  assert.equal(key, 'a')
  assert.ok(calls >= 3)
})

test('validProjectKey matches the server rules', () => {
  assert.ok(validProjectKey('TokenPivot'))
  assert.ok(validProjectKey('我的项目'))
  assert.ok(!validProjectKey(''))
  assert.ok(!validProjectKey(' lead'))
  assert.ok(!validProjectKey('trail '))
  assert.ok(!validProjectKey('a\nb'))
  assert.ok(!validProjectKey('a'.repeat(129)))
  assert.ok(validProjectKey('a'.repeat(128)))
})

test('cachedProjectKeyDetector looks each directory up once and stays bounded', () => {
  let stats = 0
  const detect = cachedProjectKeyDetector({ stat: () => { stats += 1; return { isFile: () => false } } }, 2)
  assert.equal(detect('/p/one'), 'one')
  assert.equal(detect('/p/one'), 'one')
  assert.equal(stats, 1)
  detect('/p/two'); detect('/p/three')
  assert.equal(detect('/p/one'), 'one') // evicted, looked up again
  assert.equal(stats, 4)
})

const PID = '018f3c5e-2b3c-7d4e-9f0a-1b2c3d4e5f60'
const session = { id: 's-1', header: { id: 's-1', cwd: '/work/app/sub' } }
const event = {
  type: 'assistant/message',
  time: Date.UTC(2026, 9, 28, 12, 0, 0),
  data: { message: { role: 'assistant', id: 'm-1', source: { provider: 'p', model: 'm' } }, usage: { inputTokens: 1, outputTokens: 1, totalTokens: 2 } },
}

test('mapper: detected repository name is sent as project_key, nothing else about the path', () => {
  const e = mapSessionEvent(session, event, { detectProject: (cwd) => (cwd === '/work/app/sub' ? 'app' : null) })
  assert.equal(e.project_id, null)
  assert.equal(e.project_key, 'app')
  assert.ok(!JSON.stringify(e).includes('/work'))
})

test('mapper: an explicit mapping wins and the event never carries both', () => {
  const e = mapSessionEvent(session, event, {
    projects: [{ path: '/work/app', projectId: PID }],
    detectProject: () => 'app',
  })
  assert.equal(e.project_id, PID)
  assert.ok(!('project_key' in e))
})

test('mapper: no detection result means no project_key field at all', () => {
  assert.ok(!('project_key' in mapSessionEvent(session, event, { detectProject: () => null })))
  assert.ok(!('project_key' in mapSessionEvent(session, event, {})))
})
