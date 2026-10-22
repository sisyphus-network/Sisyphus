import assert from 'node:assert/strict'
import { readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { after, before, test } from 'node:test'
import type { AddressInfo } from 'node:net'
import { _electron as electron, type ElectronApplication } from 'playwright-core'
import { createServer, loadConfigFromFile, type InlineConfig, type ViteDevServer } from 'vite'
import { testNode, testProfile, type TestNode } from './harness.ts'

let node: TestNode | undefined
before(async () => { node = await testNode() })
after(async () => { await node?.remove() })

test('theme mode and accent survive a real Vite HMR update in the dev renderer', async () => {
  const started = node
  assert.ok(started, 'the test node did not start')
  const profile = testProfile()
  const desktop = join(import.meta.dirname, '..')
  const sourcePath = join(desktop, 'src', 'renderer', 'src', 'App.tsx')
  const originalSource = readFileSync(sourcePath, 'utf8')
  let vite: ViteDevServer | undefined
  let app: ElectronApplication | undefined
  const hmrMessages: unknown[] = []
  try {
    const loaded = await loadConfigFromFile({ command: 'serve', mode: 'development' }, join(desktop, 'electron.vite.config.ts'), desktop)
    assert.ok(loaded, 'the Electron Vite configuration could not be loaded')
    // electron-vite's configuration holds one Vite configuration per process.
    const renderer = (loaded.config as { renderer: InlineConfig }).renderer
    vite = await createServer({
      ...renderer,
      configFile: false,
      server: {
        ...renderer.server,
        host: '127.0.0.1',
        port: 0,
        fs: { ...renderer.server?.fs, allow: [join(desktop, '..', '..')] },
      },
    })
    await vite.listen()
    const port = (vite.httpServer?.address() as AddressInfo).port
    const rendererUrl = `http://127.0.0.1:${port}`
    const sendHmrMessage: (...args: unknown[]) => void = vite.ws.send.bind(vite.ws)
    vite.ws.send = (...args: unknown[]) => {
      hmrMessages.push(args[0])
      sendHmrMessage(...args)
    }

    app = await electron.launch({
      cwd: desktop,
      args: ['out/main/index.js', `--user-data-dir=${profile.dir}`, '--no-sandbox', '--use-gl=angle', '--use-angle=swiftshader', '--enable-unsafe-swiftshader', '--ignore-gpu-blocklist'],
      env: { ...process.env as Record<string, string>, ELECTRON_RENDERER_URL: rendererUrl, SISYPHUS_API_ADDRESS: started.api, SISYPHUS_API_TOKEN_FILE: started.tokenFile },
    })
    const page = await app.firstWindow()
    page.setDefaultTimeout(20000)
    await page.getByRole('heading', { name: 'A smarter compute loop starts here.' }).waitFor()
    await page.waitForLoadState('domcontentloaded')
    await page.evaluate(() => {
      localStorage.setItem('sisyphus-theme', 'dark')
      localStorage.setItem('sisyphus-theme-style', 'pink')
    })
    await page.reload()
    await page.waitForFunction(() => document.documentElement.dataset.theme === 'dark' && document.documentElement.dataset.themeStyle === 'pink')
    await page.getByRole('heading', { name: 'A smarter compute loop starts here.' }).waitFor()

    const previousDocumentStart = await page.evaluate(() => performance.timeOrigin)
    // Change a harmless source comment so Vite sends an actual renderer HMR update.
    writeFileSync(sourcePath, `${originalSource}\n// HMR acceptance probe ${Date.now()}\n`)
    for (let left = 300; left > 0 && !hmrMessages.some((message) => JSON.stringify(message).includes('App.tsx')); left--) await new Promise((wake) => setTimeout(wake, 100))
    assert.ok(hmrMessages.some((message) => JSON.stringify(message).includes('App.tsx')), `Vite did not send an App.tsx HMR update: ${JSON.stringify(hmrMessages)}`)
    await page.waitForFunction((previous) => performance.timeOrigin !== previous, previousDocumentStart)
    assert.equal(await page.evaluate(() => localStorage.getItem('sisyphus-theme')), 'dark')
    assert.equal(await page.evaluate(() => localStorage.getItem('sisyphus-theme-style')), 'pink')
    await page.waitForFunction(() => getComputedStyle(document.documentElement).getPropertyValue('--app-accent').trim() === '#f59ac4')
    assert.equal(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--app-accent').trim()), '#f59ac4')
    await page.getByRole('heading', { name: 'A smarter compute loop starts here.' }).waitFor()
  } finally {
    writeFileSync(sourcePath, originalSource)
    await app?.close()
    profile.remove()
    await vite?.close()
  }
})
