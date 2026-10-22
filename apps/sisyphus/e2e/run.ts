// Runs the end-to-end tests. They drive the real app in a real window, so
// on Linux with no display, as on a test runner, they are given one that
// nobody sees.
import { spawnSync } from 'node:child_process'
import { readdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const tests = readdirSync(here).filter((name) => name.endsWith('.e2e.ts')).map((name) => join(here, name))
const node = [process.execPath, '--experimental-strip-types', '--test', '--test-concurrency=1', ...tests]
// A self-hosted runner may inherit the maintainer's live desktop session.
// Keep Electron E2E isolated from that window manager, just like on hosted CI.
const unseen = process.platform === 'linux' && (process.env.CI || (!process.env.DISPLAY && !process.env.WAYLAND_DISPLAY))
const [command, ...args] = unseen ? ['xvfb-run', '-a', '-s', '-screen 0 1400x900x24', ...node] : node
const run = spawnSync(command, args, { stdio: 'inherit' })
if (run.error) console.error(unseen ? `could not start xvfb-run, which gives the tests a display: ${run.error.message}` : run.error.message)
process.exit(run.status ?? 1)
