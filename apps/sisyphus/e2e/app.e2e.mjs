import assert from 'node:assert/strict'
import { after, afterEach, before, beforeEach, test } from 'node:test'
import { hasWebgl, launch, narrow, open, planWith, poolInvitation, poolMembers, standInModel, standInOpenAI, testNode, testProfile, until, wide } from './harness.mjs'

let node
before(async () => { node = await testNode() })
after(async () => { await node?.remove() })

// What the shared node said while a test ran, shown when the test fails:
// without it, a test that waits on the node in vain says only that it
// waited.
let said = 0
beforeEach(() => { said = node.output.length })
afterEach((t) => {
  if (!t.passed) console.error(`node output during "${t.name}":\n${node.output.slice(said).slice(-8000)}`)
})

async function openSettingsCategory(page, category) {
  await open(page, 'Settings')
  await page.getByRole('navigation', { name: 'Settings' }).getByRole('link', { name: new RegExp(`^${category}`, 'i') }).click()
}

// Whether the window says its node is connected: a badge in the header of a
// narrow window, and in a wide one the daemon status in the sidebar, whose
// dot alone does not say it.
function connectionState(page, label) {
  return page.getByRole('banner').getByText(label, { exact: true })
    .or(page.getByRole('complementary').getByRole('status', { name: `Daemon: ${label}` }))
}

async function selectProvider(page, name) {
  await page.getByRole('button', { name: 'Provider', exact: true }).click()
  await page.getByRole('dialog', { name: 'Provider' }).getByRole('button', { name: new RegExp(`^${name}`, 'i') }).click()
}

async function selectLoadedModel(page, name) {
  await page.getByRole('button', { name: 'Model', exact: true }).click()
  await page.getByRole('dialog', { name: 'Model' }).getByRole('button', { name, exact: true }).click()
}

async function openProviderModelStep(page, { provider, profile, url, apiKey }) {
  await selectProvider(page, provider)
  await page.getByRole('textbox', { name: 'Profile name' }).fill(profile)
  await page.getByRole('button', { name: 'Continue', exact: true }).click()
  await page.getByRole('textbox', { name: 'Service URL' }).fill(url)
  if (apiKey) await page.getByLabel('API key').fill(apiKey)
  await page.getByText('models available', { exact: true }).waitFor()
  await page.getByRole('button', { name: 'Continue', exact: true }).click()
}

async function waitForConnectedDaemon(page, options) {
  await page.getByRole('status', { name: 'Daemon: Connected' }).waitFor(options)
}

test('the window shows the node it is connected to', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await page.getByRole('heading', { name: 'A smarter compute loop starts here.' }).waitFor()
    await open(page, 'Node overview')
    await page.getByRole('heading', { name: 'Node overview', level: 1 }).waitFor()
    await connectionState(page, 'Connected').waitFor()
    await page.getByText('Daemon version dev', { exact: true }).waitFor()
    // The node the window describes is the one it was started against,
    // and that node's own worker is listed with what it can run.
    await page.getByText(node.id(), { exact: true }).waitFor()
    // A coordinator has no joined pool; don't show an error-shaped pool panel
    // for the normal standalone-node case.
    await page.getByText('Joined pool', { exact: true }).waitFor({ state: 'detached' })
    const worker = page.getByRole('article').filter({ hasText: 'rig' })
    await worker.getByText('primes').waitFor()
    assert.match(await worker.innerText(), /0\/2\s*Tasks/)
  } finally {
    await app.close()
    profile.remove()
  }
})

test('closing the window quits the app when its node is not the app’s own, and leaves that node running', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await page.getByRole('heading', { name: 'A smarter compute loop starts here.' }).waitFor()
    // The tray keeps only a node the app started working with the window
    // closed; this one was started by hand, so the app closes with its window.
    const closed = new Promise((resolve) => app.once('close', resolve))
    await page.close()
    await Promise.race([closed, new Promise((_, reject) => setTimeout(() => reject(new Error('the app kept running with its window closed')), 15_000))])
    // The node someone else started is theirs, and keeps running.
    assert.equal(node.child.exitCode, null)
    assert.equal(node.child.signalCode, null)
  } finally {
    await app.close().catch(() => { /* already closed */ })
    profile.remove()
  }
})

test('location discovery controls configure an independently managed daemon and persist across restart', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await openSettingsCategory(page, 'This node')
    await page.getByRole('heading', { name: 'Location discovery' }).waitFor()
    const gateway = page.getByRole('switch', { name: 'Ask my gateway' })
    const country = page.getByRole('switch', { name: 'Use ipapi.co as a last resort' })
    await gateway.waitFor()
    assert.equal(await gateway.isDisabled(), false)
    assert.equal(await country.isDisabled(), false)
    const stun = page.getByRole('textbox', { name: 'Ask a STUN server' })
    assert.equal(await stun.isDisabled(), false)
    await country.click()
    await stun.fill('stun.example.org:3478')
    const save = page.getByRole('button', { name: 'Save discovery settings' })
    assert.equal(await save.isEnabled(), true)
    await save.click()
    await page.getByText('Discovery settings saved', { exact: true }).waitFor()
    await page.getByText('Saved. Restart the daemon to apply these changes.', { exact: true }).waitFor()
    await node.stop()
    await node.start()
    await page.waitForFunction(() => document.querySelector('[role="switch"][aria-label="Use ipapi.co as a last resort"]')?.getAttribute('aria-checked') === 'true')
    assert.equal(await stun.inputValue(), 'stun.example.org:3478')
  } finally {
    await app.close()
    profile.remove()
  }
})

test('hosting a pool is the app’s own node’s setting, so a node run by hand keeps its own address', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await openSettingsCategory(page, 'This node')
    await page.getByRole('heading', { name: 'Let other machines join this pool' }).waitFor()
    await page.getByText('This app is connected to a node it did not start.', { exact: false }).waitFor()
    assert.equal(await page.getByRole('switch', { name: 'Let other machines join this pool' }).isDisabled(), true)
    assert.equal(await page.getByRole('combobox', { name: 'Address' }).isDisabled(), true)
    assert.equal(await page.getByRole('button', { name: 'Save hosting settings' }).count(), 0)
    // Nor can it restart a node it did not start.
    assert.equal(await page.getByRole('button', { name: 'Restart node' }).count(), 0)
    await open(page, 'Node overview')
    await page.getByRole('heading', { name: 'Node overview', level: 1 }).waitFor()
    assert.equal(await page.getByRole('heading', { name: 'Other machines join this pool here' }).count(), 0)
  } finally {
    await app.close()
    profile.remove()
  }
})

function poolLimitsCard(page) {
  return page.locator('.settings-card').filter({ has: page.getByRole('heading', { name: 'Pool limits' }) })
}

test('pool limits configure an independently managed daemon and persist across restart', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await openSettingsCategory(page, 'This node')
    await page.getByRole('heading', { name: 'Pool limits' }).waitFor()
    const card = poolLimitsCard(page)
    const jobs = page.getByRole('textbox', { name: 'Jobs per member' })
    const tasks = page.getByRole('textbox', { name: 'Tasks per member' })
    const timeout = page.getByRole('textbox', { name: 'Longest task run' })
    const store = page.getByRole('textbox', { name: 'Storage per member' })
    const upload = page.getByRole('textbox', { name: 'Upload per task attempt' })
    await jobs.waitFor()
    for (const field of [jobs, tasks, timeout, store, upload]) {
      assert.equal(await field.isDisabled(), false)
      assert.equal(await field.inputValue(), '')
      assert.equal(await field.getAttribute('placeholder'), 'No limit')
    }
    // The test node runs the coordinator role, so nothing says the limits do nothing.
    assert.equal(await page.getByText(/does not run the coordinator role/).count(), 0)
    const save = page.getByRole('button', { name: 'Save pool limits' })
    assert.equal(await save.isEnabled(), false)

    // A count past what the daemon keeps is refused here, before saving.
    await jobs.fill('2147483648')
    await page.getByText('Enter a whole number from 0 to 2147483647, or leave it empty for no limit.', { exact: true }).waitFor()
    assert.equal(await save.isEnabled(), false)
    // A byte limit that fits 64 bits but not the daemon's store is refused by
    // the daemon, and its reason is shown.
    await jobs.fill('3')
    await store.fill('10000000')
    await page.getByRole('combobox', { name: 'Unit for Storage per member' }).selectOption('TB')
    await save.click()
    await page.getByText('Could not save pool limits', { exact: true }).waitFor()
    await page.getByText(/max_store_bytes_per_client may be at most 9223372036854775807/).waitFor()

    await tasks.fill('40')
    await timeout.fill('90')
    await page.getByRole('combobox', { name: 'Unit for Longest task run' }).selectOption('minutes')
    await store.fill('1.5')
    await page.getByRole('combobox', { name: 'Unit for Storage per member' }).selectOption('GB')
    await upload.fill('250')
    await page.getByRole('combobox', { name: 'Unit for Upload per task attempt' }).selectOption('MB')
    await save.click()
    await page.getByText('Pool limits saved', { exact: true }).waitFor()
    await card.getByText('Saved. Restart the daemon to apply these changes.', { exact: true }).waitFor()
    await card.getByText('In effect until restart: No limit').first().waitFor()

    await node.stop()
    await node.start()
    // Once restarted, the daemon runs with what was saved: nothing is left to restart for.
    await card.getByText('Changes take effect the next time the daemon starts.', { exact: true }).waitFor({ timeout: 30000 })
    assert.equal(await card.getByText(/^In effect until restart:/).count(), 0)
    assert.equal(await jobs.inputValue(), '3')
    assert.equal(await tasks.inputValue(), '40')
    assert.equal(await timeout.inputValue(), '90')
    assert.equal(await page.getByRole('combobox', { name: 'Unit for Longest task run' }).inputValue(), 'minutes')
    assert.equal(await store.inputValue(), '1.5')
    assert.equal(await upload.inputValue(), '250')
    assert.equal(await save.isEnabled(), false)
  } finally {
    await app.close()
    profile.remove()
  }

  // A fresh window reads the saved limits from the daemon, the only place they are kept.
  const second = testProfile()
  const reopened = await launch(node, second)
  try {
    await openSettingsCategory(reopened.page, 'This node')
    const store = reopened.page.getByRole('textbox', { name: 'Storage per member' })
    await reopened.page.waitForFunction(() => [...document.querySelectorAll('input')].some((input) => input.value === '1.5'))
    assert.equal(await store.inputValue(), '1.5')
    assert.equal(await reopened.page.getByRole('combobox', { name: 'Unit for Storage per member' }).inputValue(), 'GB')
    assert.equal(await reopened.page.getByRole('textbox', { name: 'Longest task run' }).inputValue(), '90')
    // Leave the shared node as it was found: no limits, running.
    for (const name of ['Jobs per member', 'Tasks per member', 'Longest task run', 'Storage per member', 'Upload per task attempt']) {
      await reopened.page.getByRole('textbox', { name }).fill('')
    }
    await reopened.page.getByRole('button', { name: 'Save pool limits' }).click()
    await reopened.page.getByText('Pool limits saved', { exact: true }).waitFor()
    await node.stop()
    await node.start()
    await poolLimitsCard(reopened.page).getByText('Changes take effect the next time the daemon starts.', { exact: true }).waitFor({ timeout: 30000 })
  } finally {
    await reopened.app.close()
    second.remove()
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
    await page.locator('.workspace-desktop-stage').waitFor()
    await page.locator('.workspace-history-slot').waitFor({ state: 'attached' })
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

    // Keyboard resizing follows the current writing direction and stays
    // bounded by the same minimum/maximum as pointer resizing.
    await page.evaluate(() => document.documentElement.dir = 'rtl')
    await restoredHistory.focus()
    await restoredHistory.press('ArrowLeft')
    await page.waitForFunction(() => Number(document.querySelector('.workspace-history-resize-handle')?.getAttribute('aria-valuenow')) > 280)
    const rtlWidth = Number(await restoredHistory.getAttribute('aria-valuenow'))
    await restoredHistory.press('End')
    await page.waitForFunction((maximum) => Number(document.querySelector('.workspace-history-resize-handle')?.getAttribute('aria-valuenow')) === maximum, maxHistoryWidth)
    await restoredHistory.press('ArrowRight')
    await page.waitForFunction((maximum) => Number(document.querySelector('.workspace-history-resize-handle')?.getAttribute('aria-valuenow')) === maximum, maxHistoryWidth)
    await restoredHistory.press('Home')
    await page.waitForFunction(() => Number(document.querySelector('.workspace-history-resize-handle')?.getAttribute('aria-valuenow')) === 280)
    assert.ok(rtlWidth > 280 && rtlWidth <= maxHistoryWidth)
    await page.evaluate(() => document.documentElement.dir = 'ltr')
  } finally {
    await app?.close()
    profile.remove()
  }
})

test('workspace panels resize and persist in RTL', async (t) => {
  const profile = testProfile()
  let app
  try {
    ({ app } = await launch(node, profile))
    let page = await app.firstWindow()
    if (!await hasWebgl(page)) {
      assert.ok(!process.env.CI, 'the window was given no WebGL, though it was started with WebGL drawn in software')
      return t.skip('this machine gives the window no WebGL, so topology resizing is unavailable')
    }
    await wide(app, page)
    await page.evaluate(() => {
      localStorage.setItem('sisyphus-locale', 'he')
      localStorage.setItem('sisyphus-workspace-layout', JSON.stringify({ chat: 48, network: 52, historyOpen: true, historyWidth: 300, topologyOpen: true }))
    })
    await page.reload()
    await page.waitForFunction(() => document.documentElement.dir === 'rtl')
    const topologyHandle = page.locator('.workspace-topology-resize-handle')
    const topologySlot = page.locator('.workspace-topology-slot')
    const historyHandle = page.locator('.workspace-history-resize-handle')
    const historySlot = page.locator('.workspace-history-slot')
    await topologyHandle.waitFor()
    await page.waitForFunction(() => document.querySelector('.workspace-history-slot')?.getAttribute('data-open') === 'true')

    const initialTopology = await topologySlot.evaluate((element) => element.getBoundingClientRect().width)
    let box = await topologyHandle.boundingBox()
    assert.ok(box)
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
    await page.mouse.down()
    await page.mouse.move(box.x + box.width / 2 + 72, box.y + box.height / 2, { steps: 8 })
    await page.mouse.up()
    await page.waitForFunction((width) => (document.querySelector('.workspace-topology-slot')?.getBoundingClientRect().width ?? 0) > width + 24, initialTopology)

    const initialHistory = await historySlot.evaluate((element) => element.getBoundingClientRect().width)
    box = await historyHandle.boundingBox()
    assert.ok(box)
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
    await page.mouse.down()
    await page.mouse.move(box.x + box.width / 2 - 72, box.y + box.height / 2, { steps: 8 })
    await page.mouse.up()
    await page.waitForFunction((width) => (document.querySelector('.workspace-history-slot')?.getBoundingClientRect().width ?? 0) > width + 24, initialHistory)
    const resizedHistory = await historySlot.evaluate((element) => element.getBoundingClientRect().width)
    await page.waitForFunction(() => {
      const layout = JSON.parse(localStorage.getItem('sisyphus-workspace-layout') || '{}')
      return layout.historyWidth > 300 && layout.network > 52
    })
    const resizedLayout = await page.evaluate(() => JSON.parse(localStorage.getItem('sisyphus-workspace-layout') || '{}'))

    await app.close()
    app = undefined
    ;({ app, page } = await launch(node, profile))
    await page.waitForFunction(() => document.documentElement.dir === 'rtl')
    await page.waitForFunction(() => {
      const layout = JSON.parse(localStorage.getItem('sisyphus-workspace-layout') || '{}')
      return layout.topologyOpen === true && layout.historyOpen === true &&
        document.querySelector('.workspace-topology-slot')?.getAttribute('data-open') === 'true' &&
        document.querySelector('.workspace-history-slot')?.getAttribute('data-open') === 'true'
    })
    await page.waitForTimeout(450)
    const restored = await page.evaluate(() => ({
      layout: JSON.parse(localStorage.getItem('sisyphus-workspace-layout') || '{}'),
      topology: document.querySelector('.workspace-topology-slot')?.getBoundingClientRect().width ?? 0,
      history: document.querySelector('.workspace-history-slot')?.getBoundingClientRect().width ?? 0,
      stage: document.querySelector('.workspace-desktop-stage')?.getBoundingClientRect().width ?? 0,
    }))
    assert.ok(Math.abs(restored.layout.network - resizedLayout.network) < 0.1, `topology split changed after RTL relaunch: ${resizedLayout.network}% → ${restored.layout.network}%`)
    const restoredPanelArea = restored.stage - restored.history - 34
    const expectedTopology = Math.min(restoredPanelArea * restored.layout.network / 100, restoredPanelArea * 0.7, restoredPanelArea - 280)
    assert.ok(Math.abs(restored.topology - expectedTopology) < 4, `topology pane does not match its restored split: expected ${expectedTopology}px, got ${restored.topology}px`)
    assert.ok(Math.abs(restored.history - resizedHistory) < 4, `history width changed after RTL relaunch: ${resizedHistory}px → ${restored.history}px (${JSON.stringify(restored.layout)})`)
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

test('a double-click on a workspace panel handle closes and reopens the panel, and a single click does not', async (t) => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await wide(app, page)
    await page.evaluate(() => localStorage.setItem('sisyphus-workspace-layout', JSON.stringify({ chat: 58, network: 42, historyOpen: true, historyWidth: 320, topologyOpen: true })))
    await page.reload()
    const opened = (slot) => page.evaluate((slot) => document.querySelector(slot)?.getAttribute('data-open'), slot)
    const toggles = async (name, slot) => {
      const handle = page.getByRole('separator', { name })
      const waitForPanelTransition = async () => page.locator(slot).evaluate(async (element) => {
        await Promise.all(element.getAnimations({ subtree: true }).map((animation) => animation.finished.catch(() => undefined)))
        await new Promise(requestAnimationFrame)
      })
      await handle.waitFor()
      await page.waitForFunction((slot) => document.querySelector(slot)?.getAttribute('data-open') === 'true', slot)
      const box = await handle.boundingBox()
      assert.ok(box, `the ${name} handle has no box`)
      const x = box.x + box.width / 2
      const y = box.y + box.height / 2
      // One click is not a toggle: a handle is pressed on its way to a drag.
      await page.mouse.click(x, y)
      // Let the 450 ms double-tap window expire so the next double-click
      // cannot accidentally pair its first click with this single click.
      await page.waitForTimeout(600)
      assert.equal(await opened(slot), 'true', `one click on the ${name} handle closed the panel`)
      await page.mouse.dblclick(x, y)
      await page.waitForFunction((slot) => document.querySelector(slot)?.getAttribute('data-open') === 'false', slot)
      // The handle stays where it can be found, and a double-click on it
      // where it now is brings the panel back.
      await waitForPanelTransition()
      const closed = await handle.boundingBox()
      assert.ok(closed, `the collapsed ${name} handle disappeared`)
      await page.mouse.dblclick(closed.x + closed.width / 2, closed.y + closed.height / 2)
      await page.waitForFunction((slot) => document.querySelector(slot)?.getAttribute('data-open') === 'true', slot)
      // The next handle can move while this panel or a nested element is
      // transitioning. Wait for subtree animations before measuring/clicking it.
      await waitForPanelTransition()
    }

    await toggles('Resize conversation history panel', '.workspace-history-slot')
    const layout = await page.evaluate(() => JSON.parse(localStorage.getItem('sisyphus-workspace-layout') || '{}'))
    assert.equal(layout.historyOpen, true, 'the reopened history panel was not saved as open')

    if (!await hasWebgl(page)) {
      assert.ok(!process.env.CI, 'the window was given no WebGL, though it was started with WebGL drawn in software')
      return t.skip('this machine gives the window no WebGL, even drawn in software, so the topology handle is not available')
    }
    await toggles('Resize topology panel', '.workspace-topology-slot')
  } finally {
    await app.close()
    profile.remove()
  }
})

// Dragged narrower, a panel stops at its minimum width, stays open until the
// drag goes 72 px past that minimum, and closes beyond it. Which way is
// narrower follows the writing direction (#98).
test('a workspace panel dragged narrower stops at its minimum width and closes only past its threshold, in LTR and RTL', async (t) => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await wide(app, page)
    const webgl = await hasWebgl(page)
    if (!webgl) assert.ok(!process.env.CI, 'the window was given no WebGL, though it was started with WebGL drawn in software')
    const state = (slot, handle) => page.evaluate(({ slot, handle }) => ({
      open: document.querySelector(slot)?.getAttribute('data-open'),
      width: Number(document.querySelector(handle)?.getAttribute('aria-valuenow')),
    }), { slot, handle })
    // narrows drags a panel's handle, in one gesture, to raw widths that
    // first clamp at the minimum, then sit just inside the threshold, then
    // go past it. sign is which way of the pointer makes the panel narrower.
    const narrows = async ({ name, slot, handle, sign }) => {
      // By class: in Hebrew the handle's name is translated.
      const separator = page.locator(handle)
      await separator.waitFor()
      await page.waitForFunction((slot) => document.querySelector(slot)?.getAttribute('data-open') === 'true', slot)
      await page.waitForTimeout(450)
      const start = (await state(slot, handle)).width
      assert.ok(start > 320, `${name}: the panel starts at ${start}px, too narrow to test its minimum`)
      const box = await separator.boundingBox()
      assert.ok(box, `${name}: the handle has no box`)
      const x = box.x + box.width / 2
      const y = box.y + box.height / 2
      const to = (width) => page.mouse.move(x + sign * (start - width), y, { steps: 8 })
      await page.mouse.move(x, y)
      await page.mouse.down()
      try {
        await to(240)
        await page.waitForFunction(({ slot, handle }) => document.querySelector(slot)?.getAttribute('data-open') === 'true' &&
          Math.abs(Number(document.querySelector(handle)?.getAttribute('aria-valuenow')) - 280) <= 1, { slot, handle })
        await to(212)
        await page.waitForTimeout(150)
        const inside = await state(slot, handle)
        assert.ok(inside.open === 'true' && Math.abs(inside.width - 280) <= 1, `${name}: at 212 px the panel was ${JSON.stringify(inside)}, want open at its 280 px minimum`)
        await to(200)
        await page.waitForFunction((slot) => document.querySelector(slot)?.getAttribute('data-open') === 'false', slot)
      } finally {
        await page.mouse.up()
      }
    }

    for (const dir of ['ltr', 'rtl']) {
      await page.evaluate((dir) => {
        localStorage.setItem('sisyphus-locale', dir === 'rtl' ? 'he' : 'en')
        localStorage.setItem('sisyphus-workspace-layout', JSON.stringify({ chat: 50, network: 50, historyOpen: true, historyWidth: 420, topologyOpen: true }))
      }, dir)
      await page.reload()
      await page.waitForFunction((dir) => document.documentElement.dir === dir, dir)
      // History sits at the start edge: narrower is toward it.
      await narrows({ name: 'Resize conversation history panel', slot: '.workspace-history-slot', handle: '.workspace-history-resize-handle', sign: dir === 'rtl' ? 1 : -1 })
      // Topology sits at the end edge: narrower is toward that.
      if (webgl) await narrows({ name: 'Resize topology panel', slot: '.workspace-topology-slot', handle: '.workspace-topology-resize-handle', sign: dir === 'rtl' ? -1 : 1 })
    }
    if (!webgl) t.diagnostic('this machine gives the window no WebGL, so only the history panel was checked')
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
      const previous = await canvas.evaluate((element) => ({ width: element.width, height: element.height }))
      await app.evaluate(({ BrowserWindow }, [w, h]) => BrowserWindow.getAllWindows()[0].setContentSize(w, h), [nextWidth, nextHeight])
      await page.waitForFunction(([w, h]) => Math.abs(window.innerWidth - w) < 5 && Math.abs(window.innerHeight - h) < 5, [nextWidth, nextHeight])
      await page.waitForFunction(([width, height]) => {
        const canvasEl = document.querySelector('.network-globe-stage canvas')
        return canvasEl && (canvasEl.width !== width || canvasEl.height !== height)
      }, [previous.width, previous.height])
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
    await touch.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: Math.round(viewportWidth * 0.55), y: 360 }] })
    await touch.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] })
    await page.waitForTimeout(300)
    assert.equal(await page.locator('.workspace-mobile-pane[data-active="true"]').getByRole('img', { name: /This node/ }).count(), 0, 'a swipe should not switch views; mobile navigation is dock-driven')
    await dockButtons.nth(1).click()
    await page.locator('.workspace-mobile-pane[data-active="true"]').getByRole('img', { name: /This node/ }).waitFor()
    await dockButtons.nth(0).click()
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

test('the composer grows with what is typed, up to a cap, then scrolls, in both layouts', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    const composer = page.getByRole('textbox', { name: 'Message your Sisyphus planner…' })
    // Its height, the height its text needs, and whether it scrolls, once
    // a resize has had a frame and its 150 ms transition to settle.
    const measure = async () => {
      await page.waitForTimeout(400)
      return composer.evaluate((field) => ({ height: field.getBoundingClientRect().height, needs: field.scrollHeight, scrolls: getComputedStyle(field).overflowY }))
    }
    const lines = (n) => Array.from({ length: n }, (_, i) => `line ${i + 1}`).join('\n')
    for (const [layout, size] of [['desktop', wide], ['compact', narrow]]) {
      await size(app, page)
      await composer.waitFor()
      await page.waitForFunction(() => !document.querySelector('textarea')?.disabled)
      await composer.fill('')
      const empty = await measure()
      assert.ok(empty.height >= 35 && empty.height <= 45, `${layout}: an empty composer is ${empty.height}px high`)

      await composer.fill(lines(3))
      const three = await measure()
      assert.ok(three.height > empty.height + 20, `${layout}: three lines left the composer ${three.height}px high, from ${empty.height}px`)
      assert.equal(three.scrolls, 'hidden', `${layout}: three lines that fit should not scroll`)

      // Far more than fits: it stops at its cap and scrolls instead.
      await composer.fill(lines(30))
      const many = await measure()
      assert.ok(Math.abs(many.height - 144) <= 1, `${layout}: thirty lines left the composer ${many.height}px high, want its 144px cap`)
      assert.ok(many.needs > many.height, `${layout}: thirty lines need ${many.needs}px, no more than the ${many.height}px shown`)
      assert.equal(many.scrolls, 'auto', `${layout}: a composer at its cap should scroll`)

      // And it shrinks back when the text goes.
      await composer.fill('')
      const cleared = await measure()
      assert.ok(Math.abs(cleared.height - empty.height) <= 1, `${layout}: a cleared composer is ${cleared.height}px high, not the ${empty.height}px it began at`)
    }
  } finally {
    await app.close()
    profile.remove()
  }
})

test('an active conversation survives desktop and compact workspace remounts', async () => {
  const model = await standInModel('The active conversation survived the layout change.')
  planWith(node, model)
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await wide(app, page)
    await open(page, 'Node overview')
    await connectionState(page, 'Connected').waitFor()
    await page.getByRole('complementary').getByRole('link', { name: 'Workspace', exact: true }).click()
    await page.getByRole('heading', { name: 'A smarter compute loop starts here.' }).waitFor()
    const composer = page.getByRole('textbox', { name: 'Message your Sisyphus planner…' })
    await composer.fill('Keep this conversation while resizing.')
    const send = page.getByRole('button', { name: 'Send message' })
    await send.waitFor({ state: 'visible' })
    await page.waitForFunction(() => {
      const button = [...document.querySelectorAll('button')].find((candidate) => candidate.getAttribute('aria-label') === 'Send message')
      return button instanceof HTMLButtonElement && !button.disabled
    })
    await send.click()
    await page.getByText('The active conversation survived the layout change.').waitFor({ timeout: 30000 })

    await narrow(app)
    await page.locator('.workspace-shell--compact').waitFor()
    await page.getByRole('paragraph').filter({ hasText: 'Keep this conversation while resizing.' }).waitFor()
    await page.getByText('The active conversation survived the layout change.').waitFor()

    await wide(app, page)
    await page.locator('.workspace-desktop-stage').waitFor()
    await page.getByRole('paragraph').filter({ hasText: 'Keep this conversation while resizing.' }).waitFor()
    await page.getByText('The active conversation survived the layout change.').waitFor()
  } finally {
    await app.close()
    profile.remove()
    await model.close()
  }
})

// Every advanced route shares one header, which sits beside the sidebar at
// the top of the window, with the page's content directly under it (#99).
test('each advanced page has the one shared header beside the sidebar, with no band between it and the content, in LTR and RTL', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await wide(app, page)
    for (const dir of ['ltr', 'rtl']) {
      await page.evaluate((dir) => localStorage.setItem('sisyphus-locale', dir === 'rtl' ? 'he' : 'en'), dir)
      await page.reload()
      await page.waitForFunction((dir) => document.documentElement.dir === dir, dir)
      for (const route of ['/node', '/operations', '/settings', '/settings/general']) {
        await page.evaluate((route) => { window.location.hash = route }, route)
        await page.waitForFunction((route) => location.hash === `#${route}` && document.querySelector('.route-transition') && document.querySelector('.app-global-header') && document.querySelector('.advanced-sidebar'), route)
        // Let the route's fade settle before measuring.
        await page.waitForTimeout(300)
        const at = await page.evaluate(() => {
          const box = (selector) => { const rect = document.querySelector(selector)?.getBoundingClientRect(); return rect && { top: rect.top, bottom: rect.bottom, left: rect.left, right: rect.right } }
          return {
            headers: document.querySelectorAll('.app-global-header').length,
            header: box('.app-global-header'),
            sidebar: box('.advanced-sidebar'),
            main: box('.app-main'),
            content: box('.route-transition'),
          }
        })
        const where = `${dir} ${route}: ${JSON.stringify(at)}`
        assert.equal(at.headers, 1, `${where}: want exactly one shared header`)
        assert.ok(Math.abs(at.header.top) <= 1 && Math.abs(at.header.top - at.sidebar.top) <= 1, `${where}: the header is not at the top, level with the sidebar`)
        const beside = dir === 'rtl' ? at.header.right - at.sidebar.left : at.header.left - at.sidebar.right
        assert.ok(Math.abs(beside) <= 1, `${where}: the header is ${beside}px from the sidebar's edge, want it directly beside`)
        assert.ok(Math.abs(at.main.top - at.header.bottom) <= 1 && Math.abs(at.content.top - at.header.bottom) <= 1, `${where}: a band between the header and the content`)
      }
    }
  } finally {
    await app.close()
    profile.remove()
  }
})

test('advanced navigation switches between the sidebar and mobile dock as the window resizes', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await wide(app, page)
    await open(page, 'Node overview')
    await page.getByRole('heading', { name: 'Node overview', level: 1 }).waitFor()
    await page.getByRole('complementary').getByRole('link', { name: 'Settings', exact: true }).waitFor()
    const mobileNavigation = page.locator('.app-mobile-dock')
    await mobileNavigation.waitFor({ state: 'detached' })

    await narrow(app)
    await page.waitForFunction(() => window.innerWidth <= 900)
    await mobileNavigation.waitFor()
    await page.getByRole('complementary').waitFor({ state: 'detached' })
    await mobileNavigation.getByRole('button', { name: 'Settings', exact: true }).click()
    await page.getByRole('heading', { name: 'Settings', level: 1 }).waitFor()

    await wide(app, page)
    await page.waitForFunction(() => window.innerWidth > 900)
    await page.getByRole('complementary').getByRole('link', { name: 'Settings', exact: true }).waitFor()
    await mobileNavigation.waitFor({ state: 'detached' })
    await page.getByRole('heading', { name: 'Settings', level: 1 }).waitFor()
  } finally {
    await app.close()
    profile.remove()
  }
})

test('collapsed sidebar navigation tooltips stay beside the sidebar and inside the window in LTR and RTL', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await wide(app, page)
    await open(page, 'Node overview')
    await page.getByRole('heading', { name: 'Node overview', level: 1 }).waitFor()
    const sidebar = page.locator('.advanced-sidebar')
    for (const dir of ['ltr', 'rtl']) {
      await page.evaluate((dir) => {
        localStorage.setItem('sisyphus-locale', dir === 'rtl' ? 'he' : 'en')
        localStorage.setItem('sisyphus-advanced-sidebar-open', 'true')
      }, dir)
      await page.reload()
      await page.waitForFunction((dir) => document.documentElement.dir === dir, dir)
      await sidebar.waitFor()
      await sidebar.locator('.advanced-sidebar__toggle').click()
      await page.waitForFunction(() => document.querySelector('.advanced-sidebar')?.getAttribute('data-open') === 'false')

      const item = sidebar.locator('a[href="#/node"]')
      const label = (await item.innerText()).trim()
      await item.hover()
      const tooltip = page.locator('[data-slot="tooltip-content"]', { hasText: label }).last()
      await tooltip.waitFor()
      const placement = await page.evaluate((tooltip) => {
        const popup = tooltip.getBoundingClientRect()
        const sidebar = document.querySelector('.advanced-sidebar').getBoundingClientRect()
        const viewport = { width: window.innerWidth, height: window.innerHeight }
        const center = document.elementFromPoint(popup.left + popup.width / 2, popup.top + popup.height / 2)
        return {
          popup: { left: popup.left, right: popup.right, top: popup.top, bottom: popup.bottom },
          sidebar: { left: sidebar.left, right: sidebar.right },
          popupCenterX: popup.left + popup.width / 2,
          viewport,
          topLayer: center?.closest('[data-slot="tooltip-content"]') === tooltip,
          direction: document.documentElement.dir,
        }
      }, await tooltip.elementHandle())
      const where = `${dir}: ${JSON.stringify(placement)}`
      assert.ok(placement.popup.left >= 0 && placement.popup.right <= placement.viewport.width, `${where}: tooltip is clipped horizontally`)
      assert.ok(placement.popup.top >= 0 && placement.popup.bottom <= placement.viewport.height, `${where}: tooltip is clipped vertically`)
      assert.ok(placement.topLayer, `${where}: tooltip is hidden under page content`)
      if (dir === 'ltr') assert.ok(placement.popupCenterX > placement.sidebar.right, `${where}: tooltip is not on the content side of the LTR sidebar`)
      else assert.ok(placement.popupCenterX < placement.sidebar.left, `${where}: tooltip is not on the content side of the RTL sidebar`)
      await page.mouse.move(0, 0)
      await tooltip.waitFor({ state: 'detached' })
    }
  } finally {
    await app.close()
    profile.remove()
  }
})

// Where a page has no data to show, it shows the shared state component,
// centred where the data would be, not a blank card: on a node of its own,
// the empty Jobs, Files and Pool lists and the empty peer list, and the
// whole page when the node is unavailable (#99).
test('pages with nothing to show use the shared empty and unavailable states, centred where the data would be', async () => {
  const fresh = await testNode()
  const profile = testProfile()
  const { app, page } = await launch(fresh, profile)
  try {
    await wide(app, page)
    // centred finds the shared state that says text, and reports how far its
    // icon is from the middle of the box it is centred in.
    const centred = (text, within, timeout) => page.waitForFunction(({ text, within }) => {
      const state = [...document.querySelectorAll('[role="status"]')].find((element) => element.textContent?.includes(text))
      const icon = state?.querySelector(':scope > span[aria-hidden="true"]')
      const box = within ? document.querySelector(within) : state?.parentElement
      if (!state || !icon || !box || getComputedStyle(state).textAlign !== 'center') return null
      const style = getComputedStyle(box)
      const rect = box.getBoundingClientRect()
      // clientWidth leaves out a scrollbar, which the content cannot use.
      const middle = rect.left + box.clientLeft + parseFloat(style.paddingLeft) + (box.clientWidth - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight)) / 2
      const iconRect = icon.getBoundingClientRect()
      const off = Math.abs(iconRect.left + iconRect.width / 2 - middle)
      return off <= 2 ? { off } : null
    }, { text, within }, { timeout })

    await open(page, 'Operations')
    await centred('No jobs yet.')
    await page.getByRole('tab', { name: 'Files' }).click()
    await centred('No files stored.')
    await page.getByRole('tab', { name: 'Pool', exact: true }).click()
    await centred('No pool members reported.')
    await open(page, 'Node overview')
    await centred('No peers connected yet')

    // With the node gone, the whole page gives way to the same component,
    // in the middle of the content area.
    await fresh.stop()
    await centred('Daemon unavailable', '.app-main', 30000)
  } finally {
    await app.close()
    profile.remove()
    await fresh.remove()
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

test('the Pool view joins a coordinator with an invitation', async () => {
  const joiningNode = await testNode()
  const coordinator = await testNode()
  const invitation = poolInvitation(coordinator)
  const profile = testProfile()
  const { app, page } = await launch(joiningNode, profile)
  try {
    await open(page, 'Operations')
    await page.getByRole('tab', { name: 'Pool', exact: true }).click()
    await page.getByPlaceholder('Coordinator host:port').fill(coordinator.pool)
    await page.getByPlaceholder('Invitation token').fill(invitation)
    await page.getByRole('button', { name: 'Join another pool' }).click()
    await page.getByText('Joined pool', { exact: true }).waitFor({ timeout: 10000 }).catch(async () => {
      const state = await page.evaluate(() => ({
        inputs: [...document.querySelectorAll('input')].map((input) => ({ placeholder: input.placeholder, value: input.value })),
        toasts: [...document.querySelectorAll('[data-sonner-toast]')].map((toast) => toast.textContent?.trim()),
      }))
      throw new Error(`pool join did not succeed: ${JSON.stringify(state)}\n${await page.locator('body').innerText()}`)
    })
    assert.equal(await page.getByPlaceholder('Coordinator host:port').inputValue(), '')
    assert.equal(await page.getByPlaceholder('Invitation token').inputValue(), '')
    assert.ok(poolMembers(coordinator).includes(joiningNode.id()), 'the coordinator did not record the node joined through the UI')
  } finally {
    await app.close()
    profile.remove()
    await joiningNode.remove()
    await coordinator.remove()
  }
})

test('node overview streams the joined pool without exposing worker counts to worker-only nodes', async () => {
  const coordinator = await testNode()
  const worker = await testNode({ role: 'worker', coordinator, name: 'joining rig' })
  let secondWorker
  const profile = testProfile()
  const { app, page } = await launch(worker, profile)
  try {
    await open(page, 'Node overview')
    await page.getByText('Joined pool', { exact: true }).waitFor().catch(async () => {
      throw new Error(`joined-pool panel did not appear:\n${await page.locator('body').innerText()}\nworker daemon:\n${worker.output}`)
    })
    const poolSection = page.locator('[class*=rounded-2xl]').filter({ hasText: 'Joined pool' }).last()
    await poolSection.getByText('rig', { exact: true }).waitFor()
    await poolSection.getByText('Connected', { exact: true }).waitFor().catch(async () => {
      const apiState = await page.evaluate(() => window.sisyphus.call('getJoinedPool').catch((error) => String(error)))
      const streamEvents = await page.evaluate(() => new Promise((resolve) => {
        const events = []
        const stop = window.sisyphus.stream('watchJoinedPool', {}, (event) => events.push(event))
        setTimeout(() => { stop(); resolve(events) }, 1000)
      }))
      throw new Error(`worker did not reach the coordinator in the UI:\n${worker.output}\nAPI says: ${JSON.stringify(apiState)}\nstream says: ${JSON.stringify(streamEvents)}\n${await page.locator('body').innerText()}`)
    })
    await poolSection.getByText('Worker', { exact: true }).waitFor()
    await poolSection.getByText('Workers in pool', { exact: true }).waitFor({ state: 'detached' })
    await coordinator.stop()
    await poolSection.getByText('Reconnecting', { exact: true }).waitFor({ timeout: 15000 })
    await poolSection.getByText('rig', { exact: true }).waitFor()
    await coordinator.start()
    await poolSection.getByText('Connected', { exact: true }).waitFor({ timeout: 15000 })
    const clientInvitation = poolInvitation(coordinator, 'client')
    const clientJoin = await page.evaluate(({ address, invitation }) => window.sisyphus.call('joinPool', { address, invitation }), { address: coordinator.pool, invitation: clientInvitation })
    await page.getByText('Worker, Client', { exact: true }).waitFor().catch(async () => {
      const current = await page.evaluate(() => window.sisyphus.call('getJoinedPool'))
      throw new Error(`client role did not reach the renderer: ${JSON.stringify({ clientJoin, current })}\n${await page.locator('body').innerText()}`)
    })
    await page.getByText('Workers in pool', { exact: true }).waitFor()
    const joinedState = await page.evaluate(() => window.sisyphus.call('getJoinedPool'))
    await page.getByText(String(joinedState.workerCount), { exact: true }).last().waitFor()
    secondWorker = await testNode({ role: 'worker', coordinator, name: 'second worker' })
    await page.getByText(String(joinedState.workerCount + 1), { exact: true }).last().waitFor({ timeout: 15000 })
    await coordinator.stop()
    await poolSection.getByText('Reconnecting', { exact: true }).waitFor({ timeout: 15000 })
    await page.getByText('Workers in pool', { exact: true }).waitFor({ state: 'detached' })
    await coordinator.start()
    await poolSection.getByText('Connected', { exact: true }).waitFor({ timeout: 15000 })
    await page.getByText('Workers in pool', { exact: true }).waitFor()
    await page.getByText(String(joinedState.workerCount + 1), { exact: true }).last().waitFor({ timeout: 15000 })
  } finally {
    await app.close()
    profile.remove()
    await secondWorker?.remove()
    await worker.remove()
    await coordinator.remove()
  }
})

test('node overview leaves the joined pool once asked to confirm, and the node stops', async () => {
  const coordinator = await testNode()
  const worker = await testNode({ role: 'worker', coordinator, name: 'leaving rig' })
  const profile = testProfile()
  const { app, page } = await launch(worker, profile)
  try {
    await open(page, 'Node overview')
    const poolSection = page.locator('[class*=rounded-2xl]').filter({ hasText: 'Joined pool' }).last()
    await poolSection.getByText('Connected', { exact: true }).waitFor({ timeout: 15000 })
    assert.ok(poolMembers(coordinator).includes(worker.id()), 'the worker is not a member before it leaves')
    // Asked, and turned down, nothing happens.
    await poolSection.getByRole('button', { name: 'Leave pool' }).click()
    const confirm = page.getByRole('dialog', { name: 'Leave this pool?' })
    await confirm.waitFor()
    await confirm.getByRole('button', { name: 'Cancel' }).click()
    await confirm.waitFor({ state: 'detached' })
    assert.ok(poolMembers(coordinator).includes(worker.id()), 'cancelling took the worker out of the pool')
    // Confirmed, the node leaves, and stops of its own accord.
    const exited = new Promise((done) => worker.child.once('exit', (code) => done(code)))
    await poolSection.getByRole('button', { name: 'Leave pool' }).click()
    await confirm.getByRole('button', { name: 'Leave pool' }).click()
    await page.getByText('Left the pool. This node is stopping.').waitFor().catch(async () => {
      throw new Error(`the leave was not confirmed:\n${await page.locator('body').innerText()}\nworker daemon:\n${worker.output}`)
    })
    assert.equal(await exited, 0, `the node did not stop cleanly after leaving:\n${worker.output}`)
    assert.ok(!poolMembers(coordinator).includes(worker.id()), 'the coordinator still lists the node that left')
  } finally {
    await app.close()
    profile.remove()
    await worker.remove()
    await coordinator.remove()
  }
})

test('a language chosen with the keyboard turns the window round, and is still chosen at the next start', async () => {
  const profile = testProfile()
  let { app, page } = await launch(node, profile)
  try {
    await openSettingsCategory(page, 'General')
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

test('Settings pickers open in full view, above every card and inside the window, tall or as short as it goes', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    // Opens a picker and checks its choices can all be seen and reached:
    // inside the window, clear of its own trigger, and not under a card.
    const opensInView = async (category, name, height, within, stayOnCategory = false) => {
      if (!stayOnCategory && !await page.getByRole('heading', { name: category, level: 1 }).isVisible().catch(() => false)) {
        await openSettingsCategory(page, category)
      }
      if (within) await page.getByRole('button', { name: within }).first().click()
      const trigger = name === 'Language'
        ? page.locator('#settings-language')
        : name === 'Accent'
          ? page.locator('#settings-theme-style')
        : page.getByRole('button', { name, exact: true })
      await trigger.waitFor({ timeout: 30000 })
      await trigger.scrollIntoViewIfNeeded()
      await trigger.click()
      const menu = page.getByRole('dialog', { name, exact: true })
      await menu.waitFor()
      await menu.evaluate(async (element) => {
        await new Promise(requestAnimationFrame)
        await Promise.all(element.getAnimations().map((animation) => animation.finished.catch(() => undefined)))
        await new Promise(requestAnimationFrame)
      })
      const anchor = await trigger.boundingBox()
      const seen = await menu.evaluate((menu, { name, anchor }) => {
        const box = menu.getBoundingClientRect()
        const covered = [...menu.querySelectorAll('button')].filter((choice) => {
          const at = choice.getBoundingClientRect()
          if (at.bottom <= box.top || at.top >= box.bottom) return false // scrolled out of the menu's own view
          const x = at.left + at.width / 2
          const y = Math.max(at.top, box.top) + Math.min(at.height, box.bottom - at.top) / 2
          return !menu.contains(document.elementFromPoint(x, y))
        }).map((choice) => choice.textContent?.trim())
        return {
          box: { left: box.left, top: box.top, right: box.right, bottom: box.bottom },
          trigger: anchor,
          window: { width: window.innerWidth, height: window.innerHeight },
          // The trigger stays in sight to show what is chosen: the menu may
          // meet its edge, but is not over its middle.
          overlapsTrigger: Boolean(anchor && menu.contains(document.elementFromPoint(anchor.x + anchor.width / 2, anchor.y + anchor.height / 2))),
          covered,
        }
      }, { name, anchor })
      const where = `${name} in ${category}, ${height}px high: ${JSON.stringify(seen)}`
      assert.ok(seen.box.top >= 0 && seen.box.left >= 0 && seen.box.bottom <= seen.window.height && seen.box.right <= seen.window.width, `the picker goes past the window: ${where}`)
      if (seen.window.width > 767) assert.ok(!seen.overlapsTrigger, `the picker is over the middle of its own trigger: ${where}`)
      assert.deepEqual(seen.covered, [], `choices are under something else: ${where}`)
      if (within && name === 'Provider') {
        const wizard = page.getByRole('dialog', { name: within, exact: true })
        await page.waitForFunction(() => document.querySelector('[role="dialog"][aria-label="Provider"]')?.contains(document.activeElement))
        assert.equal(await menu.evaluate((element) => element.contains(document.activeElement)), true, 'opening the nested Provider picker must move keyboard focus into its choices')
        await page.keyboard.press('Escape')
        await menu.waitFor({ state: 'detached' })
        await wizard.waitFor()
        assert.equal(await wizard.isVisible(), true, 'Escape should dismiss only the Provider picker, not its wizard')

        await trigger.click()
        await menu.waitFor()
        await page.waitForFunction(() => document.querySelector('[role="dialog"][aria-label="Provider"]')?.contains(document.activeElement))
        assert.equal(await menu.evaluate((element) => element.contains(document.activeElement)), true, 'reopening the Provider picker must restore its initial keyboard focus')
        await menu.getByRole('button').filter({ hasText: 'Anthropic' }).first().focus()
        await page.keyboard.press('Enter')
        await menu.waitFor({ state: 'detached' })
        assert.equal(await wizard.getByRole('button', { name: 'Provider', exact: true }).inputValue(), 'Anthropic')
        await wizard.getByRole('button', { name: 'Cancel', exact: true }).click()
        await wizard.waitFor({ state: 'detached' })
        return
      }
      // Its trigger closes it as it opened it. On compact widths the selector
      // is a modal drawer, so Escape is the accessible dismissal path.
      if (seen.window.width <= 767) await page.keyboard.press('Escape')
      else await trigger.click()
      await menu.waitFor({ state: 'detached' })
      if (within) {
        const holder = page.getByRole('dialog', { name: within, exact: true })
        await holder.getByRole('button', { name: 'Cancel', exact: true }).click()
        await holder.waitFor({ state: 'detached' })
      }
    }

    // As tall as the tests' window, and as short as the app allows (650).
    for (const height of [800, 650]) {
      let resized = false
      for (let attempt = 0; attempt < 3 && !resized; attempt++) {
        await app.evaluate(({ BrowserWindow }, height) => BrowserWindow.getAllWindows()[0].setSize(1180, height), height)
        resized = await page.waitForFunction((height) => Math.abs(window.outerHeight - height) < 60, height, { timeout: 1500 }).then(() => true, () => false)
      }
      assert.ok(resized, `the window did not reach the requested ${height}px height after retries`)
      await opensInView('General', 'Language', height)
      await opensInView('General', 'Accent', height)
      // This one opens over the wizard's own dialog, not only over a card.
      await opensInView('Model provider', 'Provider', height, 'Add provider')
    }

    // A phone-width window exercises the same anchored popovers in the
    // compact settings layout, where the trigger and menu have much less room.
    await openSettingsCategory(page, 'General')
    let narrowWindow = false
    for (let attempt = 0; attempt < 3 && !narrowWindow; attempt++) {
      await app.evaluate(({ BrowserWindow }) => {
        const window = BrowserWindow.getAllWindows()[0]
        window.setMinimumSize(320, 320)
        window.setSize(390, 800)
      })
      narrowWindow = await page.waitForFunction(() => Math.abs(window.outerWidth - 390) < 60, null, { timeout: 1500 }).then(() => true, () => false)
    }
    assert.ok(narrowWindow, 'the window did not reach a phone-width viewport after retries')
    await opensInView('General', 'Language', 800, undefined, true)
    await opensInView('General', 'Accent', 800, undefined, true)
    await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].setSize(1180, 800))
    await page.waitForFunction(() => Math.abs(window.outerWidth - 1180) < 60)
    await page.getByRole('heading', { name: 'General', level: 1 }).waitFor({ state: 'visible' })
    await page.locator('#settings-language').waitFor({ state: 'visible' })
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))))

    // A keyboard or short landscape viewport can leave less than the normal
    // 120px menu floor. The menu must use the actual remaining room instead.
    if (!await page.getByRole('heading', { name: 'General', level: 1 }).isVisible().catch(() => false)) {
      await openSettingsCategory(page, 'General')
    }
    const trigger = page.getByRole('button', { name: 'Language', exact: true })
    await trigger.scrollIntoViewIfNeeded()
    const restoreViewport = await trigger.evaluate((input) => {
      const root = input.closest('.relative')?.parentElement?.parentElement
      if (!root) throw new Error('could not find the Language selector root')
      const previousStyle = root.getAttribute('style')
      const previousHeight = Object.getOwnPropertyDescriptor(window, 'innerHeight')
      root.style.position = 'fixed'
      root.style.top = '100px'
      root.style.left = '100px'
      root.style.width = '300px'
      Object.defineProperty(window, 'innerHeight', { configurable: true, value: 250 })
      window.dispatchEvent(new Event('resize'))
      return { previousStyle, previousHeight }
    })
    try {
      await trigger.click()
      const menu = page.getByRole('dialog', { name: 'Language', exact: true })
      await menu.waitFor()
      await page.waitForTimeout(300)
      const bounds = await menu.boundingBox()
      assert.ok(bounds && bounds.y >= 0 && bounds.y + bounds.height <= 250, `the picker exceeds a 250px viewport: ${JSON.stringify(bounds)}`)
    } finally {
      await trigger.click()
      await page.evaluate(({ previousStyle, previousHeight }) => {
        const input = document.querySelector('#settings-language')
        const root = input?.closest('.relative')?.parentElement?.parentElement
        if (root) {
          if (previousStyle === null) root.removeAttribute('style')
          else root.setAttribute('style', previousStyle)
        }
        if (previousHeight) Object.defineProperty(window, 'innerHeight', previousHeight)
        else Reflect.deleteProperty(window, 'innerHeight')
        window.dispatchEvent(new Event('resize'))
      }, restoreViewport)
    }
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

    await openSettingsCategory(page, 'General')
    await page.getByRole('heading', { name: 'General', level: 1 }).waitFor()
    const themeSwitch = page.getByRole('group', { name: /Switch theme:/ })
    const systemMode = themeSwitch.getByRole('button', { name: 'System', exact: true })
    const lightMode = themeSwitch.getByRole('button', { name: 'Light', exact: true })
    const darkMode = themeSwitch.getByRole('button', { name: 'Dark', exact: true })
    await systemMode.click()
    assert.equal(await systemMode.getAttribute('aria-pressed'), 'true')
    assert.equal(await page.evaluate(() => localStorage.getItem('sisyphus-theme')), 'system')
    // System follows the operating system's setting, and keeps following it
    // when that changes while the app is open, with no explicit theme set.
    const html = page.locator('html')
    for (const colorScheme of ['dark', 'light']) {
      await page.emulateMedia({ colorScheme })
      await page.waitForFunction((dark) => document.documentElement.classList.contains('dark') === dark, colorScheme === 'dark')
      assert.equal(await html.getAttribute('data-theme'), null, `System mode should not pin a theme while the system is ${colorScheme}`)
    }
    await page.emulateMedia({ colorScheme: null })
    await lightMode.click()
    assert.equal(await lightMode.getAttribute('aria-pressed'), 'true')
    assert.equal(await page.locator('html').getAttribute('data-theme'), 'light')
    assert.equal(await page.evaluate(() => localStorage.getItem('sisyphus-theme')), 'light')
    await darkMode.click()
    assert.equal(await darkMode.getAttribute('aria-pressed'), 'true')
    assert.equal(await page.locator('html').getAttribute('data-theme'), 'dark')
    assert.equal(await page.evaluate(() => localStorage.getItem('sisyphus-theme')), 'dark')

    await narrow(app)
    const switchBounds = await themeSwitch.boundingBox()
    const modeBounds = await Promise.all([systemMode, lightMode, darkMode].map((button) => button.boundingBox()))
    assert.ok(switchBounds && modeBounds.every((bounds) => bounds && bounds.x >= switchBounds.x && bounds.x + bounds.width <= switchBounds.x + switchBounds.width), 'theme options should fit inside the switch at the narrow window width')
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth), 'the narrow General settings view should not overflow horizontally')
    await wide(app, page)

    const accentPicker = page.getByRole('button', { name: 'Accent', exact: true })
    await accentPicker.scrollIntoViewIfNeeded()
    await accentPicker.click()
    const accentMenu = page.getByRole('dialog', { name: 'Accent' })
    await accentMenu.waitFor()
    await page.waitForTimeout(300)
    const menuBounds = await accentMenu.boundingBox()
    const viewportHeight = await page.evaluate(() => window.innerHeight)
    assert.ok(menuBounds && menuBounds.y >= 0 && menuBounds.y + menuBounds.height <= viewportHeight, `the accent popover is clipped: ${JSON.stringify({ menuBounds, viewportHeight })}`)
    assert.equal(await accentMenu.evaluate((element) => getComputedStyle(element.parentElement).position), 'fixed')
    assert.equal(await page.evaluate(({ x, y }) => document.elementFromPoint(x, y)?.closest('[role="dialog"]')?.getAttribute('aria-label'), { x: menuBounds.x + menuBounds.width / 2, y: menuBounds.y + menuBounds.height / 2 }), 'Accent', 'the popover is underneath another Settings layer')
    await accentMenu.getByRole('button', { name: 'Blue' }).click()
    await page.waitForFunction(() => document.documentElement.dataset.themeStyle === 'blue')
    assert.equal(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--app-accent').trim()), '#83adff')
    await accentPicker.click()
    await page.getByRole('dialog', { name: 'Accent' }).getByRole('button', { name: 'Pink' }).click()
    await page.waitForFunction(() => document.documentElement.dataset.themeStyle === 'pink')
    assert.equal(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--app-accent').trim()), initialAccent)
    await page.getByRole('complementary').getByRole('link', { name: 'Workspace', exact: true }).click()
    await page.getByRole('heading', { name: 'A smarter compute loop starts here.' }).waitFor()
    assert.equal(await page.locator('html').getAttribute('data-theme'), 'dark')
    assert.equal(await page.locator('html').getAttribute('data-theme-style'), 'pink')
    assert.equal(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--app-accent').trim()), initialAccent)

    await page.reload()
    await page.waitForFunction(() => document.documentElement.dataset.theme === 'dark' && document.documentElement.dataset.themeStyle === 'pink')
    assert.equal(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--app-accent').trim()), initialAccent)

    await page.evaluate(() => {
      localStorage.setItem('sisyphus-theme', 'light')
      localStorage.setItem('sisyphus-theme-style', 'blue')
    })
    await page.reload()
    await page.waitForFunction(() => document.documentElement.dataset.theme === 'light' && document.documentElement.dataset.themeStyle === 'blue')
    assert.equal(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--app-accent').trim()), '#4f8cff')
  } finally {
    await app.close()
    profile.remove()
  }
})

test('model settings load and save a local provider used by the planner', async () => {
  const model = await standInModel('The configured planner replied.')
  const apiKey = 'ollama-proxy-test-token'
  planWith(node, model)
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await openSettingsCategory(page, 'Model provider')
    await page.getByRole('button', { name: 'Add provider' }).first().click()
    const wizard = page.getByRole('dialog', { name: 'Add provider' })
    const wizardBounds = await wizard.boundingBox()
    assert.ok(wizardBounds?.width > 900, `the desktop provider wizard is only ${wizardBounds?.width ?? 0}px wide`)
    await page.mouse.click(16, Math.round((wizardBounds?.y ?? 0) + (wizardBounds?.height ?? 0) / 2))
    await wizard.waitFor()
    await page.keyboard.press('Escape')
    await wizard.waitFor()
    await openProviderModelStep(page, { provider: 'Ollama', profile: 'Local test model', url: model.url, apiKey })
    await selectLoadedModel(page, 'test-model')
    await page.getByRole('button', { name: 'Save and activate' }).click()
    await page.getByText('Model configuration saved').waitFor()

    await open(page, 'Workspace')
    await page.getByRole('textbox', { name: 'Message your Sisyphus planner…' }).fill('Does the saved local model answer?')
    await page.getByRole('button', { name: 'Send message' }).click()
    await page.getByText('The configured planner replied.').waitFor({ timeout: 30000 })
    assert.ok(model.authorizations.includes(`Bearer ${apiKey}`), 'the optional Ollama API key was not sent to the service')
    assert.equal(model.asked.length, 1)
    assert.match(JSON.stringify(model.asked[0].messages), /Does the saved local model answer\?/)
  } finally {
    await app.close()
    profile.remove()
    await model.close()
  }
})

test('assistant math and Mermaid markdown render with the patched KaTeX dependency', async () => {
  const model = await standInModel('The answer is $$x^2 + 1$$.\n\n```mermaid\ngraph TD\n  A[Start] --> B[Done]\n```')
  planWith(node, model)
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await page.getByRole('textbox', { name: 'Message your Sisyphus planner…' }).fill('Show math and a small diagram.')
    await page.getByRole('button', { name: 'Send message' }).click()
    const answer = page.locator('.is-assistant').filter({ hasText: 'The answer is' }).last()
    await answer.locator('.katex').waitFor()
    await answer.locator('[data-streamdown="mermaid"] svg[aria-roledescription="flowchart-v2"]').waitFor()
    assert.match(await answer.innerText(), /The answer is/)
  } finally {
    await app.close()
    profile.remove()
    await model.close()
  }
})

test('model provider setup retries a failed model-list request', async () => {
  const model = await standInModel()
  model.failTags = true
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await openSettingsCategory(page, 'Model provider')
    await page.getByRole('button', { name: 'Add provider' }).first().click()
    await selectProvider(page, 'Ollama')
    await page.getByRole('textbox', { name: 'Profile name' }).fill('Retry test provider')
    await page.getByRole('button', { name: 'Continue', exact: true }).click()
    await page.getByRole('textbox', { name: 'Service URL' }).fill(model.url)

    const connection = page.getByRole('region', { name: 'Connection' })
    await connection.getByText('Could not list models', { exact: true }).waitFor()
    const retry = connection.getByRole('button', { name: 'Load models', exact: true })
    await until('the initial model-list request', () => model.tagRequests === 1)
    assert.equal(model.tagRequests, 1)

    model.failTags = false
    await retry.click()
    await connection.getByText('models available', { exact: true }).waitFor()
    await until('the retried model-list request', () => model.tagRequests === 2)
    assert.equal(model.tagRequests, 2, 'the explicit retry should make exactly one new request')

    let resized = false
    for (let attempt = 0; attempt < 3 && !resized; attempt++) {
      await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].setSize(1180, 650))
      resized = await page.waitForFunction(() => Math.abs(window.outerHeight - 650) < 60, null, { timeout: 1500 }).then(() => true, () => false)
    }
    assert.ok(resized, 'the provider wizard window did not reach the short desktop viewport')
    await page.getByRole('button', { name: 'Continue', exact: true }).click()
    const modelTrigger = page.getByRole('button', { name: 'Model', exact: true })
    await modelTrigger.focus()
    await page.keyboard.press('Enter')
    const modelPicker = page.getByRole('dialog', { name: 'Model', exact: true })
    await modelPicker.waitFor()
    await modelPicker.evaluate(async (element) => {
      await new Promise(requestAnimationFrame)
      await Promise.all(element.getAnimations().map((animation) => animation.finished.catch(() => undefined)))
    })
    const bounds = await modelPicker.boundingBox()
    const viewport = await page.evaluate(() => ({ width: innerWidth, height: innerHeight }))
    assert.ok(bounds && bounds.x >= 0 && bounds.y >= 0 && bounds.x + bounds.width <= viewport.width && bounds.y + bounds.height <= viewport.height, `the Model picker leaves the short viewport: ${JSON.stringify({ bounds, viewport })}`)
    await modelPicker.getByRole('button', { name: 'test-model', exact: true }).waitFor()
    await page.keyboard.press('Escape')
    await modelPicker.waitFor({ state: 'detached' })
    assert.equal(await page.getByRole('dialog', { name: 'Add provider', exact: true }).isVisible(), true, 'Escape should close the model picker without closing its wizard')
    await modelTrigger.focus()
    await page.keyboard.press('Enter')
    await page.getByRole('dialog', { name: 'Model', exact: true }).getByRole('button', { name: 'test-model', exact: true }).click()
  } finally {
    await app.close()
    profile.remove()
    await model.close()
  }
})

test('model provider setup ignores a stale model-list response after changing services', async () => {
  const stale = await standInModel()
  stale.tags = [{ name: 'stale-model', size: 1000 }]
  stale.tagDelayMs = 1800
  const current = await standInModel()
  current.tags = [{ name: 'current-model', size: 2000 }]
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await openSettingsCategory(page, 'Model provider')
    await page.getByRole('button', { name: 'Add provider' }).first().click()
    await selectProvider(page, 'Ollama')
    await page.getByRole('textbox', { name: 'Profile name' }).fill('Stale response test')
    await page.getByRole('button', { name: 'Continue', exact: true }).click()

    const serviceUrl = page.getByRole('textbox', { name: 'Service URL' })
    await serviceUrl.fill(stale.url)
    await until('the first service model-list request', () => stale.tagRequests === 1)
    await serviceUrl.fill(current.url)

    const connection = page.getByRole('region', { name: 'Connection' })
    await connection.getByText('models available', { exact: true }).waitFor()
    await until('the current service model-list response', () => current.tagResponses === 1)
    await until('the stale service request to settle', () => stale.tagSettled === 1)

    await page.getByRole('button', { name: 'Continue', exact: true }).click()
    await page.getByRole('button', { name: 'Model', exact: true }).click()
    const modelPicker = page.getByRole('dialog', { name: 'Model' })
    await modelPicker.getByRole('button', { name: 'current-model', exact: true }).waitFor()
    assert.equal(await modelPicker.getByRole('button', { name: 'stale-model', exact: true }).count(), 0)
  } finally {
    await app.close()
    profile.remove()
    await stale.close()
    await current.close()
  }
})

test('model lending inspects Ollama, saves selected models and shows restart state', async () => {
  const ollama = await standInModel()
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await openSettingsCategory(page, 'This node')
    await page.getByText('Lend Ollama models to the pool', { exact: true }).waitFor()
    await page.getByRole('textbox', { name: 'Ollama address' }).fill(ollama.url)
    await page.getByRole('button', { name: 'Check connection' }).click()
    await page.getByRole('checkbox', { name: 'test-model' }).waitFor()
    await page.getByRole('checkbox', { name: 'second-model' }).uncheck()
    await page.getByRole('checkbox', { name: 'Make models from this Ollama endpoint available to the pool' }).check()
    await page.getByRole('spinbutton', { name: /GPU layers/ }).fill('24')
    await page.getByRole('spinbutton', { name: /CPU threads/ }).fill('6')
    await page.getByRole('spinbutton', { name: /Context length/ }).fill('8192')
    await page.getByRole('textbox', { name: /Keep loaded/ }).fill('12m')
    await page.getByRole('button', { name: 'Save lending settings' }).click()
    await page.getByText('Restart to apply lending or address changes', { exact: true }).first().waitFor()
    await page.reload()
    await openSettingsCategory(page, 'This node')
    await page.getByRole('button', { name: 'Check connection' }).click()
    await page.getByRole('checkbox', { name: 'Make models from this Ollama endpoint available to the pool', checked: true }).waitFor()
    await page.getByRole('checkbox', { name: 'test-model', checked: true }).waitFor()
    await page.getByRole('checkbox', { name: 'second-model', checked: false }).waitFor()
    assert.equal(await page.getByRole('spinbutton', { name: /GPU layers/ }).inputValue(), '24')
    assert.equal(await page.getByRole('spinbutton', { name: /CPU threads/ }).inputValue(), '6')
    assert.equal(await page.getByRole('spinbutton', { name: /Context length/ }).inputValue(), '8192')
    assert.equal(await page.getByRole('textbox', { name: /Keep loaded/ }).inputValue(), '12m')
    await page.getByText('Restart to apply lending or address changes', { exact: true }).first().waitFor()
  } finally {
    await app.close()
    profile.remove()
    await ollama.close()
  }
})

test('model settings download a local model and report completion', async () => {
  const model = await standInModel('The downloaded planner replied.')
  const apiKey = 'ollama-download-test-token'
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await openSettingsCategory(page, 'Model provider')
    await page.getByRole('button', { name: 'Add provider' }).first().click()
    await openProviderModelStep(page, { provider: 'Ollama', profile: 'Download test model', url: model.url, apiKey })
    await page.getByRole('textbox', { name: 'Model name to download (e.g. llama3.1:8b)' }).fill('downloaded-test-model')
    await page.getByRole('button', { name: 'Download', exact: true }).click()
    const progress = page.getByRole('progressbar')
    await progress.waitFor()
    await page.waitForFunction(() => {
      const value = Number(document.querySelector('[role="progressbar"]')?.getAttribute('aria-valuenow'))
      return value > 0 && value < 100
    })
    await page.getByText('Model downloaded', { exact: true }).waitFor({ timeout: 30000 })
    assert.deepEqual(model.pulled, ['downloaded-test-model'])
    assert.ok(model.authorizations.includes(`Bearer ${apiKey}`), 'the optional Ollama API key was not sent for model download')
    await page.getByRole('button', { name: 'Remove model downloaded-test-model' }).click()
    await page.getByRole('dialog', { name: 'Remove Ollama model?' }).getByRole('button', { name: 'Remove model', exact: true }).click()
    await page.getByText('Model removed', { exact: true }).waitFor()
    assert.deepEqual(model.removed, ['downloaded-test-model'])
    await selectLoadedModel(page, 'test-model')
    await page.getByRole('button', { name: 'Save and activate' }).click()
    await page.getByText('Model configuration saved').waitFor()

    await open(page, 'Workspace')
    await page.getByRole('textbox', { name: 'Message your Sisyphus planner…' }).fill('Is the planner still available after download?')
    await page.getByRole('button', { name: 'Send message' }).click()
    await page.getByText('The downloaded planner replied.').waitFor({ timeout: 30000 })
  } finally {
    await app.close()
    profile.remove()
    await model.close()
  }
})

test('model settings save an OpenAI-compatible API key on the node and use it for planner requests', async () => {
  const service = await standInOpenAI('The OpenAI-compatible planner replied.')
  const apiKey = 'sk-test-e2e-not-a-real-secret'
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await openSettingsCategory(page, 'Model provider')
    await page.getByRole('button', { name: 'Add provider' }).first().click()
    await selectProvider(page, 'OpenAI')
    await page.getByRole('textbox', { name: 'Profile name' }).fill('OpenAI test provider')
    await page.getByRole('button', { name: 'Continue', exact: true }).click()
    await page.getByRole('textbox', { name: 'Service URL' }).fill(service.url)
    await page.getByLabel('API key').fill(apiKey)
    await page.getByText('models available', { exact: true }).waitFor()
    await page.getByRole('button', { name: 'Continue', exact: true }).click()
    await selectLoadedModel(page, 'test-openai-model')
    await page.getByRole('button', { name: 'Save and activate' }).click()
    await page.getByText('Model configuration saved').waitFor()

    // The UI receives only hasApiKey, never the stored secret itself.
    await page.locator('[data-slot="card"]')
      .filter({ hasText: 'OpenAI test provider' })
      .getByRole('button', { name: 'Edit OpenAI test provider', exact: true })
      .click()
    await page.getByRole('button', { name: 'Continue', exact: true }).click()
    const apiKeyInput = page.getByLabel('API key')
    assert.equal(await apiKeyInput.inputValue(), '')
    await apiKeyInput.focus()
    assert.equal(await apiKeyInput.getAttribute('placeholder'), 'Leave empty to keep saved key')
    assert.equal(await page.getByText(apiKey, { exact: true }).count(), 0)
    assert.ok(service.authorizations.includes(`Bearer ${apiKey}`), 'model listing did not receive the API key')
    await page.getByRole('dialog', { name: 'Edit' }).getByRole('button', { name: 'Cancel' }).click()

    await open(page, 'Workspace')
    await page.getByRole('textbox', { name: 'Message your Sisyphus planner…' }).fill('Use the configured OpenAI-compatible provider.')
    await page.getByRole('button', { name: 'Send message' }).click()
    await page.getByText('The OpenAI-compatible planner replied.').waitFor({ timeout: 30000 })
    assert.equal(service.asked.length, 1)
    assert.match(JSON.stringify(service.asked[0].messages), /Use the configured OpenAI-compatible provider\./)
    assert.equal(service.authorizations.at(-1), `Bearer ${apiKey}`, 'planner request did not use the saved key')
  } finally {
    await app.close()
    profile.remove()
    await service.close()
  }
})

test('development-only sample provider profiles are absent from the production app', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await page.evaluate(() => { window.location.hash = '/settings/models' })
    await page.getByRole('heading', { name: 'Model provider', level: 1 }).waitFor()

    for (const sample of ['Ollama Local', 'Ollama Remote', 'OpenAI Production', 'OpenAI Compatible', 'Anthropic Cloud', 'Anthropic Gateway']) {
      assert.equal(await page.getByRole('heading', { name: sample, exact: true }).count(), 0, `${sample} is a development preview, not a production profile`)
    }
    await page.getByRole('button', { name: 'Add provider' }).first().waitFor()
  } finally {
    await app.close()
    profile.remove()
  }
})

test('Access receives its empty paired-device state and keeps pairing disabled while remote access is off', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await openSettingsCategory(page, 'Access')
    await page.getByRole('status', { name: 'Remote access is off', exact: true }).waitFor()
    await page.getByRole('heading', { name: 'Paired devices', exact: true }).waitFor()
    await page.getByText('No devices have been paired yet.', { exact: true }).waitFor()

    const accessSwitch = page.getByRole('switch', { name: 'Remote access' })
    assert.equal(await accessSwitch.getAttribute('aria-checked'), 'false')
    const pairing = page.getByRole('button', { name: 'Pair a browser', exact: true })
    assert.equal(await pairing.isDisabled(), true, 'a browser can be paired while remote access is off')

    const interfacePicker = page.getByRole('combobox', { name: 'Node interface address', exact: true })
    await interfacePicker.waitFor()
    const triggerStructure = await interfacePicker.evaluate((trigger) => {
      const root = trigger.parentElement
      const focusable = [...(root?.querySelectorAll('button:not(:disabled), a[href], input:not(:disabled), [tabindex="0"]') ?? [])]
        .filter((item) => item.getClientRects().length > 0)
      return { count: focusable.length, onlyTrigger: focusable.length === 1 && focusable[0] === trigger, wrapperRole: root?.getAttribute('role') ?? null }
    })
    assert.deepEqual(triggerStructure, { count: 1, onlyTrigger: true, wrapperRole: null }, 'the interface selector must expose exactly one focusable combobox, not a wrapper button around it')
    await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].setSize(1180, 650))
    await page.waitForFunction(() => Math.abs(window.outerHeight - 650) < 60, null, { timeout: 5000 })
    await interfacePicker.focus()
    await page.keyboard.press('Enter')
    const interfaceMenu = page.getByRole('dialog', { name: 'Node interface address', exact: true })
    await interfaceMenu.waitFor()
    const ltrBounds = await interfaceMenu.evaluate((menu) => {
      const rect = menu.getBoundingClientRect()
      return { bounds: { left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom }, viewport: { width: innerWidth, height: innerHeight } }
    })
    assert.ok(ltrBounds.bounds.left >= 0 && ltrBounds.bounds.top >= 0 && ltrBounds.bounds.right <= ltrBounds.viewport.width && ltrBounds.bounds.bottom <= ltrBounds.viewport.height, `the LTR interface picker leaves the short viewport: ${JSON.stringify(ltrBounds)}`)
    await page.keyboard.press('Escape')
    await interfaceMenu.waitFor({ state: 'detached' })

    // The picker is used with Hebrew UI too: keep the control and search
    // direction RTL, but leave the interface list LTR so names and IPs read
    // naturally. Its popup must remain anchored inside the viewport.
    await page.evaluate(() => localStorage.setItem('sisyphus-locale', 'he'))
    await page.reload()
    await page.waitForFunction(() => document.documentElement.lang === 'he' && document.documentElement.dir === 'rtl')
    const hebrewPicker = page.getByRole('combobox', { name: 'כתובת ההאזנה של הצומת', exact: true })
    await hebrewPicker.focus()
    await page.keyboard.press('Enter')
    const hebrewMenu = page.getByRole('dialog', { name: 'כתובת ההאזנה של הצומת', exact: true })
    await hebrewMenu.waitFor()
    const search = hebrewMenu.getByRole('searchbox', { name: 'חיפוש ממשקים או כתובות…', exact: true })
    const listbox = hebrewMenu.getByRole('listbox', { name: 'כתובת ההאזנה של הצומת', exact: true })
    const directionAndBounds = await hebrewMenu.evaluate((menu) => {
      const rect = menu.getBoundingClientRect()
      const list = menu.querySelector('[role="listbox"]')
      const input = menu.querySelector('input[type="search"]')
      return {
        menu: getComputedStyle(menu).direction,
        list: list && getComputedStyle(list).direction,
        search: input && getComputedStyle(input).direction,
        bounds: { left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom },
        viewport: { width: innerWidth, height: innerHeight },
      }
    })
    assert.equal(directionAndBounds.menu, 'rtl')
    assert.equal(directionAndBounds.list, 'ltr', 'network names and numeric IP addresses should keep their natural reading order')
    assert.equal(directionAndBounds.search, 'rtl', 'the Hebrew search prompt should align RTL')
    assert.equal(directionAndBounds.viewport.height <= 700, true, 'the Hebrew picker should be exercised in the short window')
    assert.ok(directionAndBounds.bounds.left >= 0 && directionAndBounds.bounds.top >= 0 && directionAndBounds.bounds.right <= directionAndBounds.viewport.width && directionAndBounds.bounds.bottom <= directionAndBounds.viewport.height, `the RTL interface picker leaves the short viewport: ${JSON.stringify(directionAndBounds)}`)
    await search.fill('definitely-no-interface-match')
    await hebrewMenu.getByText('לא נמצאו ממשקים תואמים.', { exact: true }).waitFor()
    await hebrewMenu.getByRole('button', { name: 'סיום', exact: true }).click()
    await hebrewMenu.waitFor({ state: 'detached' })
    assert.equal(await hebrewPicker.getAttribute('aria-expanded'), 'false')

    await hebrewPicker.focus()
    await page.keyboard.press('Enter')
    await hebrewMenu.waitFor()
    await page.keyboard.press('Escape')
    await hebrewMenu.waitFor({ state: 'detached' })
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
    // Keep the desktop layout, but leave enough vertical content to exercise
    // restoration even when Settings otherwise nearly fits in the window.
    await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].setSize(1180, 620))
    await page.waitForFunction(() => window.innerWidth > 900 && window.innerHeight < 700)
    await openSettingsCategory(page, 'Access')
    const sidebar = page.locator('.app-shell-body--advanced > aside')
    const header = page.locator('.app-global-header')
    const main = page.locator('.app-shell--settings .app-main')
    const settings = page.locator('.settings-page')
    const settingsScroller = main
    await settingsScroller.waitFor()
    // The access card hydrates asynchronously; wait for its saved OFF-state
    // content before measuring or setting the scroll position.
    await page.getByText('HTTPS', { exact: true }).waitFor()

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
        contentRight: mainRect.left + mainElement.clientWidth - Number.parseFloat(mainStyle.paddingRight),
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
    assert.ok(layout.mainScrollHeight > layout.mainClientHeight, 'the shared main content area does not scroll')
    assert.ok(layout.settingsScrollHeight <= layout.settingsClientHeight + 2, 'Settings has an unnecessary nested scroll area')

    await main.hover()
    await page.mouse.wheel(0, 180)
    await page.waitForFunction(() => Number(document.querySelector('.app-shell--settings .app-main')?.scrollTop) > 40)
    const savedSettingsTop = await settingsScroller.evaluate((element) => element.scrollTop)
    await page.waitForFunction(() => Number(JSON.parse(localStorage.getItem('sisyphus-page-scroll-positions') ?? '{}')['/settings/access']) > 40, null, { timeout: 20000 }).catch(async () => {
      const state = await page.evaluate(() => ({ hash: location.hash, path: location.hash.slice(1), positions: localStorage.getItem('sisyphus-page-scroll-positions'), scrollTop: document.querySelector('.app-shell--settings .app-main')?.scrollTop }))
      throw new Error(`Settings scroll position was not saved: ${JSON.stringify(state)}`)
    })
    await page.getByRole('complementary').getByRole('link', { name: 'Node overview', exact: true }).click()
    await page.getByRole('heading', { name: 'Node overview', level: 1 }).waitFor()
    await page.getByRole('complementary').getByRole('link', { name: 'Settings', exact: true }).click()
    await page.getByRole('navigation', { name: 'Settings' }).getByRole('link', { name: /^Access/ }).click()
    await settingsScroller.waitFor()
    try {
      await page.waitForFunction((top) => Math.abs(Number(document.querySelector('.app-shell--settings .app-main')?.scrollTop) - top) < 2, savedSettingsTop)
    } catch (error) {
      const scrollState = await settingsScroller.evaluate((element) => ({
        top: element.scrollTop,
        maxTop: element.scrollHeight - element.clientHeight,
        scrollHeight: element.scrollHeight,
        clientHeight: element.clientHeight,
        saved: JSON.parse(localStorage.getItem('sisyphus-page-scroll-positions') ?? '{}')['/settings/access'],
      }))
      throw new Error(`Settings scroll did not restore from ${savedSettingsTop}: ${JSON.stringify(scrollState)}`, { cause: error })
    }
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
    await connectionState(page, 'Connected').waitFor()
    await node.stop()
    await page.getByText('Daemon unavailable').first().waitFor({ timeout: 30000 })
    // The window keeps its frame: the sidebar is still there to use.
    await page.getByRole('complementary').getByRole('link', { name: 'Settings', exact: true }).waitFor()
    await node.start()
    await connectionState(page, 'Connected').waitFor({ timeout: 60000 })
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
    await connectionState(page, 'Connected').waitFor()

    await node.stop()
    await page.getByText('Daemon unavailable').first().waitFor({ timeout: 30000 })
    // The shared advanced shell stays navigable while Operations is offline.
    await page.getByRole('complementary').getByRole('link', { name: 'Operations', exact: true }).waitFor()

    await node.start()
    await connectionState(page, 'Connected').waitFor({ timeout: 60000 })
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

test('Settings replaces stale content while offline and restores its form after reconnect', async () => {
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await openSettingsCategory(page, 'Model provider')
    await page.getByRole('button', { name: 'Add provider' }).first().waitFor()
    await connectionState(page, 'Connected').waitFor()

    await node.stop()
    await page.getByText('Daemon unavailable').first().waitFor({ timeout: 30000 })
    await page.getByText('Daemon unavailable').last().waitFor()
    assert.equal(await page.getByRole('button', { name: 'Add provider' }).count(), 0, 'stale model settings remain interactive while the daemon is offline')

    await node.start()
    await connectionState(page, 'Connected').waitFor({ timeout: 60000 })
    await page.getByRole('button', { name: 'Add provider' }).first().waitFor({ timeout: 30000 })
    await page.getByRole('button', { name: 'Add provider' }).first().click()
    await page.getByRole('button', { name: 'Provider', exact: true }).waitFor({ timeout: 30000 })
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

test('a job the planner runs is tracked in the chat, task by task and worker by worker', async () => {
  const model = await standInModel(
    { tool: 'run_job', arguments: { workload: 'primes', params: { from: 0, to: 1000 }, tasks: 4 } },
    'There are 168 primes below 1000.',
  )
  planWith(node, model)
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await page.getByRole('heading', { name: 'A smarter compute loop starts here.' }).waitFor()
    await page.getByRole('textbox', { name: 'Message your Sisyphus planner…' }).fill('How many primes are there below 1000?')
    await page.getByRole('button', { name: 'Send message' }).click()
    await page.getByText('There are 168 primes below 1000.').waitFor({ timeout: 30000 })

    // Once the job is over, the tracker stays as its summary: every task done,
    // and the worker that did them.
    const tracker = page.getByRole('group', { name: 'Job progress' })
    await tracker.getByText('Computed on the pool').waitFor()
    await tracker.getByText('4 of 4 tasks done').waitFor()
    await tracker.getByRole('listitem').filter({ hasText: 'rig' }).getByText('0 running · 4 done').waitFor()
    const bar = tracker.getByRole('progressbar', { name: 'Job progress' })
    assert.equal(await bar.getAttribute('aria-valuenow'), '4')
    assert.equal(await bar.getAttribute('aria-valuemax'), '4')
  } finally {
    await app.close()
    profile.remove()
    await model.close()
  }
})

test('reloading the Electron renderer cancels an in-flight planner stream', async () => {
  const model = await standInModel({ defer: true, content: 'This late answer must not reach the reloaded window.' })
  planWith(node, model)
  const profile = testProfile()
  const { app, page } = await launch(node, profile)
  try {
    await page.getByRole('heading', { name: 'A smarter compute loop starts here.' }).waitFor()
    await page.getByRole('textbox', { name: 'Message your Sisyphus planner…' }).fill('Hold this response while I reload.')
    await page.getByRole('button', { name: 'Send message' }).click()
    await model.waitForRequests(1)

    // A full-frame navigation exercises the actual Electron teardown path:
    // the renderer-owned stream must be cancelled through IPC and gRPC.
    await page.reload()
    await page.getByRole('heading', { name: 'A smarter compute loop starts here.' }).waitFor()
    await model.waitForAbort()
    model.releasePending()
    await page.waitForTimeout(250)
    assert.equal(await page.getByText('This late answer must not reach the reloaded window.').count(), 0)
  } finally {
    await app.close()
    profile.remove()
    await model.close()
  }
})
