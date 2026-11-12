import fs from 'node:fs'
import path from 'node:path'

/**
 * Automatic project detection: the git repository a session works in, named by its root directory.
 * Only the directory *name* is returned (and later sent as `project_key`); the path stays on this machine.
 * A linked worktree (`.git` is a file pointing into `<main>/.git/worktrees/<name>`) resolves to the main
 * repository, so every worktree of one repository lands in the same project.
 */

const CONTROL_RE = /[\u0000-\u001f\u007f-\u009f]/
const encoder = new TextEncoder()

/** OpenAPI project_key: 1–128 UTF-8 bytes, no control characters, no leading/trailing whitespace. */
export function validProjectKey(value) {
  return typeof value === 'string' && value.length > 0 && value === value.trim()
    && !CONTROL_RE.test(value) && encoder.encode(value).length <= 128
}

/** Main repository root for a `.git` *file* ("gitdir: <main>/.git/worktrees/<name>"), else the directory itself. */
function repoRootOf(dir, gitPath, stat, readFile) {
  if (!stat.isFile()) return dir
  try {
    const match = /^gitdir:\s*(.+)$/m.exec(readFile(gitPath, 'utf8'))
    const gitdir = match?.[1]?.trim().replace(/\\/g, '/')
    const at = gitdir ? gitdir.lastIndexOf('/.git/worktrees/') : -1
    if (at > 0) return gitdir.slice(0, at)
  } catch { /* unreadable pointer: fall back to the worktree directory */ }
  return dir
}

/**
 * @param {string|undefined} cwd session working directory
 * @param {{ stat?: Function, readFile?: Function }} [io] injectable for tests
 * @returns {string|null} repository directory name, or null outside a git repository / for an unusable name
 */
export function detectProjectKey(cwd, io = {}) {
  if (typeof cwd !== 'string' || cwd === '') return null
  const stat = io.stat ?? ((p) => fs.statSync(p, { throwIfNoEntry: false }))
  const readFile = io.readFile ?? fs.readFileSync
  let dir = path.resolve(cwd)
  for (;;) {
    const gitPath = path.join(dir, '.git')
    let info
    try { info = stat(gitPath) } catch { info = undefined }
    if (info) {
      const root = repoRootOf(dir, gitPath, info, readFile)
      const key = path.basename(root.replace(/[\\/]+$/, ''))
      return validProjectKey(key) ? key : null
    }
    const parent = path.dirname(dir)
    if (parent === dir) return null
    dir = parent
  }
}

/** detectProjectKey with a per-directory cache: a session reports many events from the same cwd. */
export function cachedProjectKeyDetector(io, limit = 256) {
  const cache = new Map()
  return (cwd) => {
    if (cache.has(cwd)) return cache.get(cwd)
    const key = detectProjectKey(cwd, io)
    if (cache.size >= limit) cache.delete(cache.keys().next().value)
    cache.set(cwd, key)
    return key
  }
}
