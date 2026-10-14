import test from 'node:test'
import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { existsSync } from 'node:fs'
import { resolve } from 'node:path'
const require = createRequire(import.meta.url)
const config = require('../electron-builder.config.cjs')

test('packaging puts only the application, native daemon and window icon in the runtime', () => {
  assert.deepEqual(config.files, ['out/**/*', 'package.json'])
  assert.deepEqual(config.extraResources, [
    { from: '.packaging/bin', to: 'bin', filter: ['sisyphusd', 'sisyphusd.exe'] },
    { from: 'build/icons/256x256.png', to: 'icon.png' },
  ])
  assert.equal(config.asar, true)
  assert.deepEqual(config.linux.target, ['tar.gz', 'AppImage'])
  assert.deepEqual(config.win.target, ['zip', 'nsis'])
  assert.deepEqual(config.mac.target, ['zip', 'dmg'])
})

test('every package carries an icon drawn from the logo', () => {
  for (const path of [config.win.icon, config.mac.icon, config.nsis.installerIcon, config.nsis.uninstallerIcon, config.dmg.icon, `${config.linux.icon}/512x512.png`, config.extraResources[1].from]) {
    assert.ok(existsSync(resolve(import.meta.dirname, '..', path)), `Missing icon ${path}`)
  }
})

test('cross-platform builds fail rather than package a mismatched daemon', async () => {
  await assert.rejects(config.beforePack({ electronPlatformName: process.platform === 'linux' ? 'win32' : 'linux', arch: 1 }), /matching operating system/)
})
