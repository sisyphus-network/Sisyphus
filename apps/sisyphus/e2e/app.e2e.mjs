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
