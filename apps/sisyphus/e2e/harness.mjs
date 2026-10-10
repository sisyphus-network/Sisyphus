// What the end-to-end tests stand on: a node of their own, started from the
// repository's daemon on ports nothing else has and with data of its own,
// and the real desktop app, built, started against that node with a profile
// of its own. Nothing of the user's node, data or app is touched.
import { spawn, execFileSync } from 'node:child_process'
import { mkdtempSync, existsSync, rmSync } from 'node:fs'
import { createServer as createHttpServer } from 'node:http'
import { createServer, createConnection } from 'node:net'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { _electron as electron } from 'playwright-core'

const here = dirname(fileURLToPath(import.meta.url))
const desktop = resolve(here, '..')
const daemon = process.env.SISYPHUS_DAEMON_PATH ?? resolve(desktop, '../../bin', process.platform === 'win32' ? 'sisyphusd.exe' : 'sisyphusd')

export function freePort() {
  return new Promise((done, failed) => {
    const server = createServer()
    server.once('error', failed)
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address()
      server.close(() => done(port))
    })
  })
}

function listening(port) {
  return new Promise((done) => {
    const socket = createConnection({ host: '127.0.0.1', port })
    socket.once('connect', () => { socket.destroy(); done(true) })
    socket.once('error', () => done(false))
  })
}

async function until(what, met, seconds = 30) {
  for (let left = seconds * 10; left > 0; left--) {
    if (await met()) return
    await new Promise((wake) => setTimeout(wake, 100))
  }
  throw new Error(`never happened: ${what}`)
}

/** A node of the tests' own. start() can be called again after stop(), and the node comes back as it was. */
export async function testNode() {
  if (!existsSync(daemon)) throw new Error(`no daemon at ${daemon}: run "make build" in the repository first, or set SISYPHUS_DAEMON_PATH`)
  const dataDir = mkdtempSync(join(tmpdir(), 'sisyphus-e2e-node-'))
  const [pool, api] = [await freePort(), await freePort()]
  const node = {
    dataDir, api: `127.0.0.1:${api}`, pool: `127.0.0.1:${pool}`, tokenFile: join(dataDir, 'api.token'), child: null,
    async start() {
      node.child = spawn(daemon, ['run', '--data-dir', dataDir, '--listen', node.pool, '--api-listen', node.api, '--name', 'rig', '--slots', '2', '--discovery', 'off'], { stdio: 'ignore' })
      await until('the node answering on its local API', async () => existsSync(node.tokenFile) && await listening(api))
    },
    async stop() {
      const child = node.child
      if (!child || child.exitCode !== null) return
      const gone = new Promise((done) => child.once('exit', done))
      child.kill()
      await gone
    },
    id() { return execFileSync(daemon, ['id', '--data-dir', dataDir], { encoding: 'utf8' }).trim() },
    async remove() { await node.stop(); rmSync(dataDir, { recursive: true, force: true }) },
  }
  await node.start()
  return node
}

/** A profile of the tests' own, which a second launch finds as the first left it. */
export function testProfile() {
  const dir = mkdtempSync(join(tmpdir(), 'sisyphus-e2e-profile-'))
  return { dir, remove() { rmSync(dir, { recursive: true, force: true }) } }
}

/**
 * The built app, started against a node. A machine with no graphics card of
 * its own, as a test runner is, is given WebGL drawn in software, unless the
 * test is of a window that has none.
 */
export async function launch(node, profile, { webgl = true } = {}) {
  const graphics = webgl
    ? ['--use-gl=angle', '--use-angle=swiftshader', '--enable-unsafe-swiftshader', '--ignore-gpu-blocklist']
    : ['--disable-gpu', '--disable-software-rasterizer']
  const app = await electron.launch({
    cwd: desktop,
    args: ['out/main/index.js', `--user-data-dir=${profile.dir}`, '--no-sandbox', ...graphics],
    env: { ...process.env, SISYPHUS_API_ADDRESS: node.api, SISYPHUS_API_TOKEN_FILE: node.tokenFile },
  })
  const page = await app.firstWindow()
  page.setDefaultTimeout(20000)
  await page.waitForLoadState('domcontentloaded')
  return { app, page }
}

/** Opens one of the pages the sidebar lists, showing the sidebar first if it is put away. */
export async function open(page, name) {
  const link = page.getByRole('complementary').getByRole('link', { name, exact: true })
  if (!await link.isVisible().catch(() => false)) await page.getByRole('button', { name: 'Open advanced settings' }).click()
  await link.click()
}

/**
 * A stand-in for an Ollama that a node's planner plans with. It says what it
 * was told to, in order, and keeps what it was asked. A reply is the text to
 * say, or { tool, arguments } to call one of the planner's tools.
 */
export async function standInModel(...replies) {
  const model = { asked: [], url: '' }
  const server = createHttpServer((request, response) => {
    let body = ''
    request.on('data', (piece) => { body += piece })
    request.on('end', () => {
      if (request.url === '/api/tags') return response.end('{"models":[{"name":"test-model","size":4900000000}]}')
      if (request.url === '/api/show') return response.end('{"capabilities":["completion","tools"]}')
      model.asked.push(JSON.parse(body || '{}'))
      const reply = replies[model.asked.length - 1]
      if (reply === undefined) { response.statusCode = 500; return response.end('the model has run out of things to say') }
      const message = typeof reply === 'string'
        ? { role: 'assistant', content: reply }
        : { role: 'assistant', content: '', tool_calls: [{ function: { name: reply.tool, arguments: reply.arguments } }] }
      response.end(JSON.stringify({ message, done: true }))
    })
  })
  await new Promise((done) => server.listen(0, '127.0.0.1', done))
  model.url = `http://127.0.0.1:${server.address().port}`
  model.close = () => new Promise((done) => server.close(done))
  return model
}

/** Has a node's planner plan with a model, as its owner would from the command line. */
export function planWith(node, model) {
  execFileSync(daemon, ['model', 'set', '--data-dir', node.dataDir, '--url', model.url, '--model', 'test-model'], { stdio: 'ignore' })
}

/** Whether the window can draw with WebGL, asked the way the app asks. */
export function hasWebgl(page) {
  return page.evaluate(() => {
    const canvas = document.createElement('canvas')
    return Boolean(canvas.getContext('webgl2') ?? canvas.getContext('webgl'))
  })
}

/** Makes the window the size it opens at, and returns how wide its page then is. */
export async function wide(app, page) {
  await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].setSize(1180, 800))
  await page.waitForFunction(() => window.innerWidth > 900, null, { timeout: 5000 }).catch(() => {})
  return page.evaluate(() => window.innerWidth)
}

/** Makes the window as narrow as it goes, which is narrow enough for its one-view layout. */
export async function narrow(app) {
  await app.evaluate(({ BrowserWindow }) => {
    const [window] = BrowserWindow.getAllWindows()
    const [width] = window.getMinimumSize()
    window.setSize(width, 800)
  })
}
