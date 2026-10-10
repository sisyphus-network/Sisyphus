import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { access } from 'node:fs/promises'
import { join, resolve } from 'node:path'
import { listPackage } from '@electron/asar'
import { createRequire } from 'node:module'

const { version } = createRequire(import.meta.url)('../package.json')

const platforms = {
  linux: 'linux-unpacked/resources',
  win32: 'win-unpacked/resources',
  darwin: `${process.arch === 'arm64' ? 'mac-arm64' : 'mac'}/Sisyphus.app/Contents/Resources`,
}
const resources = resolve('dist', platforms[process.platform])
const daemon = join(resources, 'bin', process.platform === 'win32' ? 'sisyphusd.exe' : 'sisyphusd')
await access(daemon)
assert.equal(execFileSync(daemon, ['version'], { encoding: 'utf8', timeout: 10000 }).trim(), `sisyphusd ${version}`)
const entries = listPackage(join(resources, 'app.asar')).map((path) => path.replaceAll('\\', '/'))
for (const path of ['/out/main/index.js', '/out/preload/index.js', '/out/renderer/index.html', '/out/renderer/logo.png']) {
  assert.ok(entries.includes(path), `Missing packaged file ${path}`)
}
// Execute the embedded daemon, not a PATH/repository substitute.
assert.ok(execFileSync(daemon, ['data-dir'], { encoding: 'utf8', timeout: 10000 }).trim())
execFileSync(process.execPath, ['--experimental-strip-types', 'dev/daemon-lifecycle-smoke.mjs', daemon, version], { stdio: 'inherit', timeout: 30000 })
console.log('Packaged renderer, preload, logo and native daemon verified.')
