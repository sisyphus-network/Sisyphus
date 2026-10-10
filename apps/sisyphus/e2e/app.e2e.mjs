import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { hasWebgl, launch, narrow, open, planWith, standInModel, testNode, testProfile, wide } from './harness.mjs'

let node
before(async () => { node = await testNode() })
after(async () => { await node?.remove() })

test('the window shows the node it is connected to', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await page.getByRole('heading', { name: 'A smarter compute loop starts here.' }).waitFor()
    await open(page, 'Node overview')
    await page.getByRole('heading', { name: 'Node overview', level: 1 }).waitFor()
    await page.getByRole('banner').getByText('Connected', { exact: true }).waitFor()
    // The node the window describes is the one it was started against,
    // and that node's own worker is listed with what it can run.
    await page.getByText(node.id(), { exact: true }).waitFor()
    const worker = page.getByRole('article').filter({ hasText: 'rig' })
    await worker.getByText('primes').waitFor()
    assert.match(await worker.innerText(), /0\/2\s*Tasks/)
  } finally {
    await app.close()
    profile.remove()
  }
})

// The topology is a globe drawn with WebGL. A window that has none is not
// offered it: no pane, no handle to open one, no entry in the dock of a
// narrow window. What is left fills the window and works.
test('a window with no WebGL offers no topology, and its workspace and chat still work', async () => {
  const model = await standInModel('Still here.')
  planWith(node, model)
  const profile = testProfile()
  const { app, page } = await launch(node, profile, { webgl: false })
  try {
    const desktopWidth = await wide(app, page)
    assert.ok(desktopWidth > 900, `the window is ${desktopWidth} pixels wide, which is its one-view layout`)
    await page.getByRole('heading', { name: 'A smarter compute loop starts here.' }).waitFor()
    assert.equal(await page.locator('canvas').count(), 0, 'a globe was drawn with no WebGL to draw it')
    assert.equal(await page.getByRole('separator', { name: 'Resize topology panel' }).count(), 0, 'a handle for the topology panel is offered')
    assert.equal(await page.locator('.workspace-topology-slot').count(), 0, 'the topology pane is there, empty')
    assert.equal(await page.getByRole('img', { name: /This node/ }).count(), 0)
    // The chat has the room the topology would have had, to the window's edge.
    const [chat, width] = [await page.locator('.workspace-chat-desktop-pane').boundingBox(), await page.evaluate(() => window.innerWidth)]
    assert.ok(chat.x + chat.width > width - 24, `the chat ends at ${chat.x + chat.width} of ${width}`)
    // And it answers.
    await page.getByRole('textbox', { name: 'Message your Sisyphus planner…' }).fill('Are you there?')
    await page.getByRole('button', { name: 'Send message' }).click()
    await page.getByText('Still here.').waitFor({ timeout: 30000 })

    // Narrow, the window has one view, so no dock to choose between views.
    await narrow(app)
    await page.locator('.workspace-shell--compact').waitFor()
    assert.equal(await page.getByRole('button', { name: 'Known network topology' }).count(), 0, 'the dock offers the topology')
    assert.equal(await page.locator('.workspace-mobile-dock').count(), 0, 'a dock is shown with one view in it')
    await page.getByRole('textbox', { name: 'Message your Sisyphus planner…' }).waitFor()
  } finally {
    await app.close()
    profile.remove()
    await model.close()
  }
})

test('a window with WebGL offers the topology, wide and narrow', async (t) => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    // Say why, where the topology cannot be there, and not only that a
    // handle was not found: a machine that will not draw WebGL even in
    // software, or a screen too small for the window's wide layout.
    if (!await hasWebgl(page)) {
      assert.ok(!process.env.CI, 'the window was given no WebGL, though it was started with WebGL drawn in software')
      return t.skip('this machine gives the window no WebGL, even drawn in software, so there is no topology to test')
    }
    const width = await wide(app, page)
    assert.ok(width > 900, `the window is ${width} pixels wide, which is its one-view layout: the screen is too small for the wide one`)
    await page.getByRole('separator', { name: 'Resize topology panel' }).waitFor()
    await page.getByRole('img', { name: /This node/ }).waitFor()
    await narrow(app)
    await page.locator('.workspace-shell--compact').waitFor()
    await page.locator('.workspace-mobile-dock').getByRole('button', { name: 'Known network topology' }).click()
    await page.locator('.workspace-mobile-pane[data-active="true"]').getByRole('img', { name: /This node/ }).waitFor()
  } finally {
    await app.close()
    profile.remove()
  }
})

test('workspace panels retain their open state and widths after relaunch', async (t) => {
  const profile = testProfile()
  let app
  try {
    ({ app } = await launch(node, profile))
    let page = await app.firstWindow()
    if (!await hasWebgl(page)) {
      assert.ok(!process.env.CI, 'the window was given no WebGL, though it was started with WebGL drawn in software')
      return t.skip('this machine gives the window no WebGL, even drawn in software, so panel persistence is not testable')
    }
    await wide(app, page)
    await page.evaluate(() => localStorage.setItem('sisyphus-workspace-layout', JSON.stringify({ chat: 58, network: 42, historyOpen: false, historyWidth: 340, topologyOpen: true })))
    await page.reload()
    const topology = page.getByRole('separator', { name: 'Resize topology panel' })
    const history = page.getByRole('separator', { name: 'Resize conversation history panel' })
    await topology.waitFor()
    await history.waitFor()
    await page.waitForFunction(() => document.querySelector('.workspace-history-slot')?.getAttribute('data-open') === 'false')
    await page.waitForFunction(() => document.querySelector('.workspace-topology-slot')?.getAttribute('data-open') === 'true')
    const seeded = await page.evaluate(() => JSON.parse(localStorage.getItem('sisyphus-workspace-layout') || '{}'))
    assert.equal(seeded.historyOpen, false)
    assert.equal(seeded.historyWidth, 340)
    assert.equal(seeded.topologyOpen, true)
    assert.equal(seeded.network, 42)

    await app.close()
    app = undefined
    ;({ app, page } = await launch(node, profile))
    await wide(app, page)
    await page.waitForFunction(() => document.querySelector('.workspace-history-slot')?.getAttribute('data-open') === 'false')
    await page.waitForFunction(() => document.querySelector('.workspace-topology-slot')?.getAttribute('data-open') === 'true')
    const restored = await page.evaluate(() => JSON.parse(localStorage.getItem('sisyphus-workspace-layout') || '{}'))
    assert.equal(restored.historyOpen, false)
    assert.equal(restored.historyWidth, 340)
    assert.equal(restored.topologyOpen, true)
    assert.equal(restored.network, 42)

    // The collapsed history handle remains available, and can bring its pane back.
    const restoredHistory = page.getByRole('separator', { name: 'Resize conversation history panel' })
    const maxHistoryWidth = Number(await restoredHistory.getAttribute('aria-valuemax'))
    assert.ok(maxHistoryWidth >= 280, `history panel has no available width (${maxHistoryWidth})`)
    await page.getByRole('button', { name: 'Open conversation history' }).click()
    await page.waitForFunction(() => document.querySelector('.workspace-history-slot')?.getAttribute('data-open') === 'true')
    await page.waitForFunction((minimum) => {
      const slot = document.querySelector('.workspace-history-slot')
      return slot?.getAttribute('data-open') === 'true' && slot.getBoundingClientRect().width >= minimum
    }, Math.min(280, maxHistoryWidth))
    const restoredWidth = await page.locator('.workspace-history-slot').evaluate((slot) => slot.getBoundingClientRect().width)
    assert.ok(restoredWidth >= 280, `history panel reopened at ${restoredWidth}px`)
    await page.locator('button[aria-pressed="true"][aria-label="Close conversation history"]').click()
    await page.waitForFunction(() => document.querySelector('.workspace-history-slot')?.getAttribute('data-open') === 'false')
    const historyHandleBox = await restoredHistory.boundingBox()
    assert.ok(historyHandleBox, 'the collapsed history handle is visible')
    const historyHandleX = historyHandleBox.x + historyHandleBox.width / 2
    const historyHandleY = historyHandleBox.y + historyHandleBox.height / 2
    await page.mouse.click(historyHandleX, historyHandleY)
    await page.waitForTimeout(80)
    await page.mouse.click(historyHandleX, historyHandleY)
    await page.waitForFunction(() => {
      const slot = document.querySelector('.workspace-history-slot')
      return slot?.getAttribute('data-open') === 'true' && slot.getBoundingClientRect().width >= 280
    }, Math.min(280, maxHistoryWidth))
  } finally {
    await app?.close()
    profile.remove()
  }
})

test('a collapsed workspace panel reopens by dragging its visible handle', async (t) => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    if (!await hasWebgl(page)) {
      assert.ok(!process.env.CI, 'the window was given no WebGL, though it was started with WebGL drawn in software')
      return t.skip('this machine gives the window no WebGL, even drawn in software, so the topology handle is not available')
    }
    await wide(app, page)
    const handle = page.getByRole('separator', { name: 'Resize topology panel' })
    await handle.waitFor()
    const slot = page.locator('.workspace-topology-slot')
    await page.waitForFunction(() => document.querySelector('.workspace-topology-slot')?.getAttribute('data-open') === 'true')
    const box = await handle.boundingBox()
    assert.ok(box)
    // Pull toward chat beyond the closing threshold. Pointer capture must keep
    // the same drag alive even though the handle/panel geometry changes.
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
    await page.mouse.down()
    await page.mouse.move(box.x + box.width / 2 + 430, box.y + box.height / 2, { steps: 12 })
    await page.waitForFunction(() => document.querySelector('.workspace-topology-slot')?.getAttribute('data-open') === 'false')
    const closedHandle = await handle.boundingBox()
    assert.ok(closedHandle, 'the collapsed resize handle disappeared')
    // Keep holding the original pointer and drag back across the handle to reopen.
    await page.mouse.move(closedHandle.x + closedHandle.width / 2 - 220, closedHandle.y + closedHandle.height / 2, { steps: 10 })
    await page.waitForFunction(() => document.querySelector('.workspace-topology-slot')?.getAttribute('data-open') === 'true')
    await page.mouse.up()
    await page.waitForTimeout(400)
    assert.equal(await slot.getAttribute('data-open'), 'true')

    // Keyboard double-click equivalent: Enter on a focused, collapsed handle
    // opens the same pane without requiring a pointer gesture.
    await handle.focus()
    await handle.press('Enter')
    await page.waitForFunction(() => document.querySelector('.workspace-topology-slot')?.getAttribute('data-open') === 'false')
    await handle.press('Enter')
    await page.waitForFunction(() => document.querySelector('.workspace-topology-slot')?.getAttribute('data-open') === 'true')
  } finally {
    await app.close()
    profile.remove()
  }
})

test('the topology canvas stays mounted and stable through repeated advanced-route transitions', async (t) => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    if (!await hasWebgl(page)) {
      assert.ok(!process.env.CI, 'the window was given no WebGL, though it was started with WebGL drawn in software')
      return t.skip('this machine gives the window no WebGL, so the topology canvas is unavailable')
    }
    await wide(app, page)
    const topology = page.getByRole('separator', { name: 'Resize topology panel' })
    await topology.waitFor()
    const canvas = page.locator('.workspace-network canvas')
    await canvas.waitFor()
    const identity = await canvas.evaluate((element) => ({ tag: element.tagName, width: element.width, height: element.height }))
    const initialBounds = await canvas.boundingBox()
    assert.ok(initialBounds && initialBounds.width > 0 && initialBounds.height > 0)

    for (const route of ['Node overview', 'Operations', 'Settings', 'Workspace', 'Operations', 'Workspace', 'Settings', 'Workspace']) {
      if (route === 'Workspace') {
        await page.getByRole('button', { name: 'Open advanced settings' }).click().catch(() => {})
        const workspaceLink = page.getByRole('complementary').getByRole('link', { name: 'Workspace', exact: true })
        if (await workspaceLink.isVisible().catch(() => false)) await workspaceLink.click()
        else await page.getByRole('button', { name: 'Open advanced settings' }).click()
      } else {
        await open(page, route)
      }
      await page.waitForTimeout(260)
      assert.equal(await canvas.count(), 1, `the globe canvas was removed after navigating to ${route}`)
      const current = await canvas.evaluate((element) => ({ tag: element.tagName, width: element.width, height: element.height }))
      assert.equal(current.tag, identity.tag)
      assert.equal(current.width, identity.width)
      assert.equal(current.height, identity.height)
    }
    const finalBounds = await canvas.boundingBox()
    assert.ok(finalBounds && finalBounds.width > 0 && finalBounds.height > 0)
  } finally {
    await app.close()
    profile.remove()
  }
})

test('the topology canvas stays fitted through live window resizes', async (t) => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    if (!await hasWebgl(page)) {
      assert.ok(!process.env.CI, 'the window was given no WebGL, though it was started with WebGL drawn in software')
      return t.skip('this machine gives the window no WebGL, so the topology canvas is unavailable')
    }
    const width = await wide(app, page)
    assert.ok(width > 900)
    const stage = page.locator('.network-globe-stage')
    const canvas = stage.locator('canvas')
    await canvas.waitFor()
    const initialBox = await stage.boundingBox()
    const initial = await canvas.evaluate((element) => ({ width: element.width, height: element.height }))
    assert.ok(initialBox && initialBox.width > 0 && initialBox.height > 0)
    assert.ok(initial.width > 0 && initial.height > 0)

    let observedBufferResize = false
    for (const [nextWidth, nextHeight] of [[1040, 720], [1280, 900], [980, 760], [1180, 800]]) {
      await app.evaluate(({ BrowserWindow }, [w, h]) => BrowserWindow.getAllWindows()[0].setSize(w, h), [nextWidth, nextHeight])
      await page.waitForFunction(([w, h]) => Math.abs(window.innerWidth - w) < 30 && Math.abs(window.innerHeight - h) < 80, [nextWidth, nextHeight])
      await page.waitForFunction(() => {
        const stageEl = document.querySelector('.network-globe-stage')
        const canvasEl = stageEl?.querySelector('canvas')
        if (!stageEl || !canvasEl) return false
        const rect = stageEl.getBoundingClientRect()
        return rect.width > 0 && rect.height > 0 && canvasEl.width > 0 && canvasEl.height > 0
      })
      const box = await stage.boundingBox()
      const dimensions = await canvas.evaluate((element) => ({ width: element.width, height: element.height }))
      if (dimensions.width !== initial.width || dimensions.height !== initial.height) observedBufferResize = true
      assert.ok(box && box.width > 0 && box.height > 0, `globe stage collapsed at ${nextWidth}x${nextHeight}`)
      assert.ok(dimensions.width > 0 && dimensions.height > 0, `WebGL drawing buffer collapsed at ${nextWidth}x${nextHeight}`)
      assert.ok(Math.abs(box.width - box.height) <= 2, `globe viewport is not square at ${nextWidth}x${nextHeight}`)
    }
    const final = await canvas.evaluate((element) => ({ width: element.width, height: element.height }))
    assert.ok(final.width > 0 && final.height > 0)
    assert.ok(observedBufferResize, 'the WebGL drawing buffer never responded to any intermediate window size')
  } finally {
    await app.close()
    profile.remove()
  }
})

test('the workspace switches layouts live across tablet and desktop widths', async (t) => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    if (!await hasWebgl(page)) {
      assert.ok(!process.env.CI, 'the window was given no WebGL, though it was started with WebGL drawn in software')
      return t.skip('this machine gives the window no WebGL, so the two-view mobile layout is unavailable')
    }
    await wide(app, page)
    const network = page.getByRole('img', { name: /This node/ })
    await network.waitFor()

    await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].setSize(820, 760))
    await page.locator('.workspace-shell--compact').waitFor()
    const dock = page.locator('.workspace-mobile-dock')
    await dock.waitFor()
    const dockButtons = dock.getByRole('button')
    assert.equal(await dockButtons.count(), 2)
    const dockBox = await dock.boundingBox()
    const viewportWidth = await page.evaluate(() => window.innerWidth)
    assert.ok(dockBox && dockBox.x >= 0 && dockBox.x + dockBox.width <= viewportWidth, 'the dock exceeds the tablet viewport')
    assert.ok(Math.abs(dockBox.x + dockBox.width / 2 - viewportWidth / 2) <= 2, 'the dock is not centered in the tablet viewport')
    const touch = await page.context().newCDPSession(page)
    await touch.send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 1 })
    await touch.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: Math.round(viewportWidth * 0.72), y: 360 }] })
    await touch.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] })
    await touch.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: Math.round(viewportWidth * 0.72), y: 360 }] })
    await touch.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: Math.round(viewportWidth * 0.55), y: 360 }] })
    await touch.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] })
    await page.locator('.workspace-mobile-pane[data-active="true"]').getByRole('img', { name: /This node/ }).waitFor()
    await touch.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: Math.round(viewportWidth * 0.45), y: 360 }] })
    await touch.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: Math.round(viewportWidth * 0.65), y: 360 }] })
    await touch.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] })
    await page.locator('.workspace-mobile-pane[data-active="true"] textarea').waitFor()

    const composer = page.locator('.workspace-mobile-pane[data-active="true"] textarea')
    await composer.focus()
    await page.waitForFunction(() => {
      const dockElement = document.querySelector('.workspace-mobile-dock')
      return dockElement && Number.parseFloat(getComputedStyle(dockElement).height) < 2
    })
    await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].setSize(820, 650))
    await page.waitForFunction(() => window.innerHeight < 760)
    const composerBox = await composer.boundingBox()
    const viewportHeight = await page.evaluate(() => window.innerHeight)
    assert.ok(composerBox && composerBox.y < viewportHeight && composerBox.y + composerBox.height <= viewportHeight, 'the focused composer fell below the resized viewport')
    await composer.evaluate((element) => element.blur())
    await page.waitForFunction(() => {
      const dockElement = document.querySelector('.workspace-mobile-dock')
      return dockElement && Number.parseFloat(getComputedStyle(dockElement).height) >= 70
    })
    await touch.detach()

    await dock.getByRole('button', { name: 'Known network topology' }).click()
    await page.locator('.workspace-mobile-pane[data-active="true"]').getByRole('img', { name: /This node/ }).waitFor()

    await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].setSize(1180, 800))
    await page.locator('.workspace-shell:not(.workspace-shell--compact)').waitFor()
    await page.getByRole('separator', { name: 'Resize topology panel' }).waitFor()

    await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].setSize(680, 760))
    await page.locator('.workspace-shell--compact').waitFor()
    await dock.waitFor()
    await page.locator('.workspace-mobile-pane[data-active="true"]').getByRole('img', { name: /This node/ }).waitFor()
    const narrowDock = await dock.boundingBox()
    const narrowWidth = await page.evaluate(() => window.innerWidth)
    assert.ok(narrowDock && narrowDock.x >= 0 && narrowDock.x + narrowDock.width <= narrowWidth, 'the dock exceeds the narrow phone viewport')
    assert.ok(Math.abs(narrowDock.x + narrowDock.width / 2 - narrowWidth / 2) <= 2, 'the dock is not centered after the live resize')
  } finally {
    await app.close()
    profile.remove()
  }
})

test('a job submitted in the window runs on the pool and shows its result', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await open(page, 'Operations')
    assert.equal(await page.getByRole('textbox', { name: 'Workload' }).inputValue(), 'primes')
    await page.getByRole('textbox', { name: 'Parameters (JSON)' }).fill('{"from":0,"to":100000}')
    await page.getByRole('button', { name: 'Submit' }).click()
    await page.getByText('Succeeded', { exact: true }).waitFor()
    // Its result is there to open, and is the count the pool computed.
    const result = page.getByRole('group').filter({ hasText: 'Job result' })
    await result.click()
    await page.getByText('9592').first().waitFor()
    // And its events, from the node, say it was submitted and its tasks ran.
    await page.getByRole('button', { name: 'Log' }).click()
    await page.getByText(/submitted/i).first().waitFor()
    await page.getByText(/task-succeeded|succeeded/i).first().waitFor()
  } finally {
    await app.close()
    profile.remove()
  }
})

test('a file stored as private is listed, and an invitation is issued', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await open(page, 'Operations')
    await page.getByRole('tab', { name: 'Files' }).click()
    await page.getByRole('checkbox', { name: 'Encrypt as a private file' }).check()
    // What losing the key costs is said beside the switch that asks for it.
    await page.getByText(/private\.key/).first().waitFor()
    const choosing = page.waitForEvent('filechooser')
    await page.getByText('Choose file', { exact: true }).last().click()
    await (await choosing).setFiles({ name: 'notes.txt', mimeType: 'text/plain', buffer: Buffer.from('the boulder and the hill\n') })
    await page.getByText('notes.txt').first().waitFor()
    assert.equal(await page.getByText('No files stored.').count(), 0)

    await page.getByRole('tab', { name: 'Pool' }).click()
    await page.getByRole('combobox', { name: 'Invitation lifetime' }).selectOption({ label: '15 minutes' })
    await page.getByRole('button', { name: 'Invite worker' }).click()
    // An invitation is the node's ID and a token, joined by a colon.
    await page.getByText(new RegExp(`${node.id()}:[0-9a-f]{32}`)).first().waitFor()
  } finally {
    await app.close()
    profile.remove()
  }
})

test('a language chosen with the keyboard turns the window round, and is still chosen at the next start', async () => {
  const profile = testProfile()
  let { app, page } = await launch(node, profile)
  try {
    await open(page, 'Settings')
    assert.equal(await page.locator('html').getAttribute('dir'), 'ltr')
    await page.getByRole('button', { name: 'Language' }).focus()
    await page.keyboard.press('Enter')
    const choices = page.getByRole('dialog', { name: 'Language' })
    await choices.waitFor()
    // The first choice has the focus, and Tab stays among the choices.
    // It is given the focus a frame after it opens.
    await page.waitForFunction(() => document.querySelector('[role="dialog"]')?.contains(document.activeElement))
    await page.keyboard.press('Tab')
    assert.ok(await choices.evaluate((dialog) => dialog.contains(document.activeElement)), 'Tab left the choices')
    await choices.getByRole('button', { name: /עברית/ }).focus()
    await page.keyboard.press('Enter')
    await page.waitForFunction(() => document.documentElement.dir === 'rtl')
    assert.equal(await page.locator('html').getAttribute('lang'), 'he')
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, 'the window scrolls sideways in Hebrew')

    await app.close()
    ;({ app, page } = await launch(node, profile))
    await page.waitForFunction(() => document.documentElement.dir === 'rtl')
    assert.equal(await page.locator('html').getAttribute('lang'), 'he')
  } finally {
    await app.close()
    profile.remove()
  }
})

test('theme mode and accent survive settings/workspace navigation and renderer reload', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await page.evaluate(() => {
      localStorage.setItem('sisyphus-theme', 'dark')
      localStorage.setItem('sisyphus-theme-style', 'pink')
    })
    await page.reload()
    await page.waitForFunction(() => document.documentElement.dataset.theme === 'dark' && document.documentElement.dataset.themeStyle === 'pink')
    const initialAccent = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--app-accent').trim())
    assert.equal(initialAccent, '#f59ac4')

    await open(page, 'Settings')
    await page.getByRole('heading', { name: 'Settings', level: 1 }).waitFor()
    await page.getByRole('complementary').getByRole('link', { name: 'Workspace', exact: true }).click()
    await page.getByRole('heading', { name: 'A smarter compute loop starts here.' }).waitFor()
    assert.equal(await page.locator('html').getAttribute('data-theme'), 'dark')
    assert.equal(await page.locator('html').getAttribute('data-theme-style'), 'pink')
    assert.equal(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--app-accent').trim()), initialAccent)

    await page.reload()
    await page.waitForFunction(() => document.documentElement.dataset.theme === 'dark' && document.documentElement.dataset.themeStyle === 'pink')
    assert.equal(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--app-accent').trim()), initialAccent)
  } finally {
    await app.close()
    profile.remove()
  }
})

test('desktop Settings keeps the shell fixed and restores its own scroll position', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await wide(app, page)
    await open(page, 'Settings')
    const sidebar = page.locator('.app-shell-body--advanced > aside')
    const header = page.locator('.app-global-header')
    const main = page.locator('.app-shell--settings .app-main')
    const settings = page.locator('.settings-page')
    const settingsScroller = page.locator('.settings-page__scroll')
    await settingsScroller.waitFor()

    const layout = await page.evaluate(() => {
      const sidebarRect = document.querySelector('.app-shell-body--advanced > aside').getBoundingClientRect()
      const headerRect = document.querySelector('.app-global-header').getBoundingClientRect()
      const mainElement = document.querySelector('.app-shell--settings .app-main')
      const mainRect = mainElement.getBoundingClientRect()
      const mainStyle = getComputedStyle(mainElement)
      const settingsRect = document.querySelector('.settings-page').getBoundingClientRect()
      const scroller = document.querySelector('.settings-page__scroll')
      return {
        sidebarRight: sidebarRect.right,
        headerLeft: headerRect.left,
        headerRight: headerRect.right,
        mainLeft: mainRect.left,
        mainRight: mainRect.right,
        contentLeft: mainRect.left + Number.parseFloat(mainStyle.paddingLeft),
        contentRight: mainRect.right - Number.parseFloat(mainStyle.paddingRight),
        settingsLeft: settingsRect.left,
        settingsRight: settingsRect.right,
        documentHeight: document.documentElement.scrollHeight,
        viewportHeight: window.innerHeight,
        mainScrollHeight: document.querySelector('.app-shell--settings .app-main').scrollHeight,
        mainClientHeight: document.querySelector('.app-shell--settings .app-main').clientHeight,
        settingsScrollHeight: scroller.scrollHeight,
        settingsClientHeight: scroller.clientHeight,
      }
    })
    assert.ok(Math.abs(layout.headerLeft - layout.sidebarRight) <= 1, 'the global header has a gap from the sidebar')
    assert.ok(Math.abs(layout.headerRight - layout.mainRight) <= 1, 'the header does not span the full content column')
    assert.ok(Math.abs(layout.settingsLeft - layout.contentLeft) <= 1 && Math.abs(layout.settingsRight - layout.contentRight) <= 1, 'Settings is narrower than the full padded content width')
    assert.ok(layout.documentHeight <= layout.viewportHeight + 2, 'the desktop document itself scrolls')
    assert.ok(layout.mainScrollHeight <= layout.mainClientHeight + 2, 'the Settings main shell scrolls instead of its inner list')
    assert.ok(layout.settingsScrollHeight > layout.settingsClientHeight, 'the settings list has no independent scroll area')

    await settingsScroller.evaluate((element) => { element.scrollTop = Math.min(180, element.scrollHeight - element.clientHeight) })
    await page.waitForFunction(() => Number(document.querySelector('.settings-page__scroll')?.scrollTop) > 40)
    const savedSettingsTop = await page.locator('.settings-page__scroll').evaluate((element) => element.scrollTop)
    await page.waitForFunction(() => Number(JSON.parse(localStorage.getItem('sisyphus-page-scroll-positions') ?? '{}')['/settings']) > 40)
    await page.getByRole('complementary').getByRole('link', { name: 'Node overview', exact: true }).click()
    await page.getByRole('heading', { name: 'Node overview', level: 1 }).waitFor()
    await page.getByRole('complementary').getByRole('link', { name: 'Settings', exact: true }).click()
    await settingsScroller.waitFor()
    await page.waitForFunction((top) => Math.abs(Number(document.querySelector('.settings-page__scroll')?.scrollTop) - top) < 2, savedSettingsTop)
    assert.ok(savedSettingsTop > 40, 'the Settings scroll position was not meaningfully saved')
  } finally {
    await app.close()
    profile.remove()
  }
})

test('a node that stops is said to be gone, and the window finds it again when it returns', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await open(page, 'Node overview')
    await page.getByRole('banner').getByText('Connected', { exact: true }).waitFor()
    await node.stop()
    await page.getByText('Daemon unavailable').first().waitFor({ timeout: 30000 })
    // The window keeps its frame: the sidebar is still there to use.
    await page.getByRole('complementary').getByRole('link', { name: 'Settings', exact: true }).waitFor()
    await node.start()
    await page.getByRole('banner').getByText('Connected', { exact: true }).waitFor({ timeout: 60000 })
    await page.getByText(node.id(), { exact: true }).waitFor()
  } finally {
    await app.close()
    profile.remove()
    if (node.child?.exitCode !== null) await node.start()
  }
})

test('Operations recovers in place when its node disconnects and reconnects', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await open(page, 'Operations')
    await page.getByRole('textbox', { name: 'Workload' }).waitFor()
    await page.getByRole('banner').getByText('Connected', { exact: true }).waitFor()

    await node.stop()
    await page.getByText('Daemon unavailable').first().waitFor({ timeout: 30000 })
    // The shared advanced shell stays navigable while Operations is offline.
    await page.getByRole('complementary').getByRole('link', { name: 'Operations', exact: true }).waitFor()

    await node.start()
    await page.getByRole('banner').getByText('Connected', { exact: true }).waitFor({ timeout: 60000 })
    // Recovery returns to the same route and restores its interactive form
    // without a reload or a manual reconnect action.
    await page.getByRole('textbox', { name: 'Workload' }).waitFor({ timeout: 30000 })
    await page.getByRole('button', { name: 'Submit' }).waitFor()
  } finally {
    await app.close()
    profile.remove()
    if (node.child?.exitCode !== null) await node.start()
  }
})

test('a question with a file attached is planned, computed on the pool and answered, and the file is private', async () => {
  // The model asks for a job, reads its result and answers; then answers a second turn.
  const model = await standInModel(
    { tool: 'run_job', arguments: { workload: 'primes', params: { from: 0, to: 100 } } },
    'There are 25 primes below 100.',
    'And the notes are about a boulder.',
  )
  planWith(node, model)
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await page.getByRole('heading', { name: 'A smarter compute loop starts here.' }).waitFor()
    const choosing = page.waitForEvent('filechooser')
    await page.getByRole('button', { name: 'Add files' }).click()
    await (await choosing).setFiles({ name: 'notes.txt', mimeType: 'text/plain', buffer: Buffer.from('the boulder and the hill\n') })
    await page.getByText('notes.txt').first().waitFor()
    await page.getByRole('textbox', { name: 'Message your Sisyphus planner…' }).fill('How many primes are there below 100?')
    await page.getByRole('button', { name: 'Send message' }).click()
    await page.getByText('There are 25 primes below 100.').waitFor({ timeout: 30000 })

    // The model was told of the file as a private one, by name and content
    // ID, and never sent what is in it.
    const first = JSON.stringify(model.asked[0].messages)
    assert.match(first, /\[Sisyphus attachments\]/)
    assert.match(first, /notes\.txt.*cid:[a-z0-9]+.*private:true/)
    assert.ok(!first.includes('the boulder and the hill'), 'the file\'s content was sent to the model')
    // The job it ran was a private one, since the conversation has a private file.
    assert.match(JSON.stringify(model.asked[1].messages), /"count\\?":25/)

    // A second turn carries the conversation on, with the first in it.
    await page.getByRole('textbox', { name: 'Message your Sisyphus planner…' }).fill('And what are the notes about?')
    await page.getByRole('button', { name: 'Send message' }).click()
    await page.getByText('And the notes are about a boulder.').waitFor({ timeout: 30000 })
    assert.match(JSON.stringify(model.asked[2].messages), /How many primes are there below 100\?/)

    // The conversation is kept, and is there in the history.
    await page.getByRole('button', { name: 'Open conversation history' }).click()
    await page.getByRole('navigation', { name: /conversation history/i })
      .getByRole('button', { name: /How many primes are there below 100\?/ })
      .first()
      .waitFor()
  } finally {
    await app.close()
    profile.remove()
    await model.close()
  }
})
