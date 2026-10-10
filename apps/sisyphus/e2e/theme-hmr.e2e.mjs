import assert from 'node:assert/strict'
import { readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { after, before, test } from 'node:test'
import { _electron as electron } from 'playwright-core'
import { createServer, loadConfigFromFile } from 'vite'
import { testNode, testProfile } from './harness.mjs'

let node
before(async () => { node = await testNode() })
after(async () => { await node?.remove() })

test('theme mode and accent survive a real Vite HMR update in the dev renderer', async () => {
  const profile = testProfile()
  const desktop = join(import.meta.dirname, '..')
  const sourcePath = join(desktop, 'src', 'renderer', 'src', 'App.tsx')
  const originalSource = readFileSync(sourcePath, 'utf8')
  let vite
  let app
  const hmrMessages = []
  try {
    const loaded = await loadConfigFromFile({ command: 'serve', mode: 'development' }, join(desktop, 'electron.vite.config.ts'), desktop)
    assert.ok(loaded, 'the Electron Vite configuration could not be loaded')
    vite = await createServer({
      ...loaded.config.renderer,
      configFile: false,
      server: {
        ...loaded.config.renderer.server,
        host: '127.0.0.1',
        port: 0,
        fs: { ...loaded.config.renderer.server.fs, allow: [join(desktop, '..', '..')] },
      },
    })
    await vite.listen()
    const port = vite.httpServer.address().port
    const rendererUrl = `http://127.0.0.1:${port}`
    const sendHmrMessage = vite.ws.send.bind(vite.ws)
    vite.ws.send = (payload, ...args) => {
      hmrMessages.push(payload)
      return sendHmrMessage(payload, ...args)
    }

    app = await electron.launch({
      cwd: desktop,
      args: ['out/main/index.js', `--user-data-dir=${profile.dir}`, '--no-sandbox', '--use-gl=angle', '--use-angle=swiftshader', '--enable-unsafe-swiftshader', '--ignore-gpu-blocklist'],
      env: { ...process.env, ELECTRON_RENDERER_URL: rendererUrl, SISYPHUS_API_ADDRESS: node.api, SISYPHUS_API_TOKEN_FILE: node.tokenFile },
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
