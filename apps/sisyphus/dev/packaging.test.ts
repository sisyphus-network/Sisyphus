import test from 'node:test'
import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { packageResourcesDirectory } from './package-layout.ts'

const require = createRequire(import.meta.url)
const config = require('../electron-builder.config.cjs')

test('packaging puts only the application, native daemon, window icon and tray icons in the runtime', () => {
  assert.deepEqual(config.files, ['out/**/*', 'package.json'])
  assert.deepEqual(config.extraResources, [
    { from: '.packaging/bin', to: 'bin', filter: ['sisyphusd', 'sisyphusd.exe'] },
    { from: 'out/renderer', to: 'web-ui' },
    { from: 'build/icons/256x256.png', to: 'icon.png' },
    { from: 'build/tray', to: 'tray' },
  ])
  assert.equal(config.asar, true)
  assert.deepEqual(config.linux.target, ['tar.gz', 'AppImage'])
  assert.deepEqual(config.win.target, ['zip', 'nsis'])
  assert.deepEqual(config.mac.target, ['zip', 'dmg'])
})

test('every package carries an icon generated from the transparent app icon artwork', () => {
  assert.ok(existsSync(resolve(import.meta.dirname, '../../../assets/app-icon.png')))
  for (const path of [config.win.icon, config.mac.icon, config.nsis.installerIcon, config.nsis.uninstallerIcon, config.dmg.icon, `${config.linux.icon}/512x512.png`, config.extraResources[1].from]) {
    assert.ok(existsSync(resolve(import.meta.dirname, '..', path)), `Missing icon ${path}`)
  }
})

test('cross-platform builds fail rather than package a mismatched daemon', async () => {
  await assert.rejects(config.beforePack({ electronPlatformName: process.platform === 'linux' ? 'win32' : 'linux', arch: 1 }), /matching operating system/)
})

test('package verification locates the unpacked app for every shipped architecture', () => {
  assert.equal(packageResourcesDirectory('linux', 'x64'), 'linux-unpacked/resources')
  assert.equal(packageResourcesDirectory('linux', 'arm64'), 'linux-arm64-unpacked/resources')
  assert.equal(packageResourcesDirectory('win32', 'x64'), 'win-unpacked/resources')
  assert.equal(packageResourcesDirectory('win32', 'arm64'), 'win-arm64-unpacked/resources')
  assert.equal(packageResourcesDirectory('darwin', 'x64'), 'mac/Sisyphus.app/Contents/Resources')
  assert.equal(packageResourcesDirectory('darwin', 'arm64'), 'mac-arm64/Sisyphus.app/Contents/Resources')
  assert.throws(() => packageResourcesDirectory('freebsd', 'x64'), /Unsupported package platform/)
})

// The installed app loads its pages from files, where "/logo.png" means the
// root of the disk, so an image or link in the pages names its file relative
// to them or through an import, never from "/".
test('the pages name no file from the root of the disk', () => {
  const root = resolve(import.meta.dirname, '../src/renderer/src')
  const sources = readdirSync(root, { recursive: true }).filter((path): path is string => typeof path === 'string' && /\.(tsx?|css)$/.test(path))
  for (const source of sources) {
    const text = readFileSync(resolve(root, source), 'utf8')
    assert.doesNotMatch(text, /(?:src|href)=["']\/(?!\/)|url\(["']?\/(?!\/)/, `${source} names a file from the root of the disk`)
  }
})
