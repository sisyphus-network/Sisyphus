import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { defaultTrayPreferences, loadTrayPreferences, quitsWhenWindowsClose, saveTrayPreferences, trayHostAvailable, trayLabelsFor, trayMenu, trayStatusLabel } from '../src/main/tray-policy.ts'

const labels = { open: 'Open Sisyphus', quit: 'Quit Sisyphus', connected: 'Connected', connecting: 'Connecting…', offline: 'Offline', stillRunningTitle: 'Still running', stillRunningBody: 'Quit from the tray.' }

test('closing the window keeps the app running only for its own node, and only with a tray to reopen it', () => {
  for (const platform of ['win32', 'linux', 'darwin']) {
    assert.equal(quitsWhenWindowsClose({ platform, managesDaemon: true, trayShown: true }), false, platform)
  }
  // A node someone else started has nothing of ours to keep running.
  assert.equal(quitsWhenWindowsClose({ platform: 'win32', managesDaemon: false, trayShown: true }), true)
  // Without an icon there would be no way back to the app, or out of it.
  assert.equal(quitsWhenWindowsClose({ platform: 'linux', managesDaemon: true, trayShown: false }), true)
  // macOS keeps apps open with no window, as it did before the tray.
  assert.equal(quitsWhenWindowsClose({ platform: 'darwin', managesDaemon: false, trayShown: false }), false)
})

test('Windows and macOS always have a tray; Linux only with a StatusNotifier host', async () => {
  const never = async () => { throw new Error('not asked') }
  assert.equal(await trayHostAvailable('win32', never), true)
  assert.equal(await trayHostAvailable('darwin', never), true)
  assert.equal(await trayHostAvailable('linux', async () => true), true)
  assert.equal(await trayHostAvailable('linux', async () => false), false)
  assert.equal(await trayHostAvailable('linux', async () => { throw new Error('no session bus') }), false)
  assert.equal(await trayHostAvailable('freebsd', async () => true), false)
})

test('the menu shows the node state above Open and Quit', () => {
  assert.deepEqual(trayMenu(labels, 'connected'), [
    { kind: 'status', label: 'Connected' },
    { kind: 'separator' },
    { kind: 'action', label: 'Open Sisyphus', action: 'open' },
    { kind: 'action', label: 'Quit Sisyphus', action: 'quit' },
  ])
  assert.equal(trayStatusLabel(labels, 'connecting'), 'Connecting…')
  assert.equal(trayStatusLabel(labels, 'disconnected'), 'Offline')
})

test('the tray speaks the window’s language, and English for one it does not know', () => {
  const dictionaries = { en: labels, he: { ...labels, open: 'פתיחת Sisyphus' } }
  assert.equal(trayLabelsFor(dictionaries, 'en', 'he').open, 'פתיחת Sisyphus')
  for (const locale of ['xx', '', null, 7, 'toString', '__proto__']) assert.equal(trayLabelsFor(dictionaries, 'en', locale), labels, String(locale))
})

test('the tray remembers its language and that it has said it keeps running', () => {
  const directory = mkdtempSync(join(tmpdir(), 'sisyphus-tray-'))
  const path = join(directory, 'preferences', 'tray.json')
  try {
    assert.deepEqual(loadTrayPreferences(path), defaultTrayPreferences)
    saveTrayPreferences(path, { locale: 'he', noticeShown: true })
    assert.deepEqual(loadTrayPreferences(path), { locale: 'he', noticeShown: true })
    assert.deepEqual(JSON.parse(readFileSync(path, 'utf8')), { locale: 'he', noticeShown: true })
    for (const [contents, expected] of [
      ['not json', defaultTrayPreferences],
      ['[]', defaultTrayPreferences],
      ['null', defaultTrayPreferences],
      ['{"locale":"","noticeShown":"yes"}', defaultTrayPreferences],
      ['{"locale":"fr"}', { locale: 'fr', noticeShown: false }],
    ]) {
      writeFileSync(path, contents)
      assert.deepEqual(loadTrayPreferences(path), expected, contents)
    }
  } finally { rmSync(directory, { recursive: true, force: true }) }
})
