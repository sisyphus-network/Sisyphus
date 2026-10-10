import test from 'node:test'
import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
const require = createRequire(import.meta.url)
const config = require('../electron-builder.config.cjs')

test('packaging puts only the application and native daemon in the runtime', () => {
  assert.deepEqual(config.files, ['out/**/*', 'package.json'])
  assert.deepEqual(config.extraResources, [{ from: '.packaging/bin', to: 'bin', filter: ['sisyphusd', 'sisyphusd.exe'] }])
  assert.equal(config.asar, true)
  assert.deepEqual(config.linux.target, ['tar.gz'])
  assert.deepEqual(config.win.target, ['zip'])
  assert.deepEqual(config.mac.target, ['zip'])
})

test('cross-platform builds fail rather than package a mismatched daemon', async () => {
  await assert.rejects(config.beforePack({ electronPlatformName: process.platform === 'linux' ? 'win32' : 'linux', arch: 1 }), /matching operating system/)
})
