import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { checkDocker, containerFlags, containerLaunch, containerState, defaultContainerSettings, dockerCheckFromEnvironment, loadContainerSettings, normalizeContainerSettings, sameContainerSettings, saveContainerSettings, validateContainerSettings } from '../src/main/container-settings.ts'
import type { ContainerSettings, DockerCheck, DockerStatus } from '../src/main/container-settings.ts'

type Run = NonNullable<NonNullable<Parameters<typeof checkDocker>[0]>['run']>
type Ran = Awaited<ReturnType<Run>>

const on: ContainerSettings = { enabled: true, network: false, readOnly: true, tmpMb: 1024 }

test('containers are off, offline and read-only unless saved otherwise, and malformed preferences fall back', () => {
  const directory = mkdtempSync(join(tmpdir(), 'sisyphus-containers-'))
  try {
    assert.deepEqual(defaultContainerSettings, { enabled: false, network: false, readOnly: true, tmpMb: 1024 })
    assert.deepEqual(loadContainerSettings(join(directory, 'missing.json')), defaultContainerSettings)
    writeFileSync(join(directory, 'broken.json'), '{ not json')
    assert.deepEqual(loadContainerSettings(join(directory, 'broken.json')), defaultContainerSettings)
    for (const value of [null, [], 'on', 1]) assert.deepEqual(normalizeContainerSettings(value), defaultContainerSettings)
    // Only true turns something on, and only false turns read-only off.
    assert.deepEqual(normalizeContainerSettings({ enabled: 'yes', network: 1, readOnly: 'no', tmpMb: 0 }), defaultContainerSettings)
    assert.deepEqual(normalizeContainerSettings({ enabled: true, network: true, readOnly: false, tmpMb: 256 }), { enabled: true, network: true, readOnly: false, tmpMb: 256 })
    for (const tmpMb of [0, -1, 1.5, 65537, '512']) assert.equal(normalizeContainerSettings({ tmpMb }).tmpMb, 1024, String(tmpMb))
  } finally { rmSync(directory, { recursive: true, force: true }) }
})

test('saving keeps only valid settings, privately, and they load back', () => {
  const directory = mkdtempSync(join(tmpdir(), 'sisyphus-containers-'))
  const path = join(directory, 'nested', 'containers.json')
  try {
    assert.deepEqual(saveContainerSettings(path, { ...on, network: true, tmpMb: 2048, extra: 'dropped' }), { ...on, network: true, tmpMb: 2048 })
    assert.deepEqual(JSON.parse(readFileSync(path, 'utf8')), { ...on, network: true, tmpMb: 2048 })
    assert.deepEqual(loadContainerSettings(path), { ...on, network: true, tmpMb: 2048 })
    if (process.platform !== 'win32') assert.equal(statSync(path).mode & 0o777, 0o600)
    for (const value of [null, [], { ...on, enabled: 'true' }, { ...on, network: undefined }, { ...on, readOnly: 1 }]) {
      assert.throws(() => saveContainerSettings(path, value), /Invalid container settings/)
    }
    for (const tmpMb of [0, 65537, 1.5, '1024']) assert.throws(() => validateContainerSettings({ ...on, tmpMb }), /from 1 to 65536/)
    // A refused save leaves what was saved before.
    assert.deepEqual(loadContainerSettings(path), { ...on, network: true, tmpMb: 2048 })
  } finally { rmSync(directory, { recursive: true, force: true }) }
})

test('the daemon is given --containers and the confinement the owner chose, and nothing while off', () => {
  assert.deepEqual(containerFlags(defaultContainerSettings), [])
  assert.deepEqual(containerFlags({ ...defaultContainerSettings, network: true, readOnly: false }), [])
  assert.deepEqual(containerFlags(on), ['--containers', '--containers-read-only', '--containers-tmp-mb', '1024'])
  assert.deepEqual(containerFlags({ ...on, tmpMb: 64, network: true }), ['--containers', '--containers-read-only', '--containers-tmp-mb', '64', '--containers-network'])
  // The daemon refuses --containers-tmp-mb without --containers-read-only.
  assert.deepEqual(containerFlags({ ...on, readOnly: false, tmpMb: 64 }), ['--containers'])
})

test('a restart is needed only when the app’s own node would run containers differently', () => {
  const state = (saved: ContainerSettings, running: ContainerSettings, managed = true) => containerState({ managed, saved, running, unavailable: false, docker: null })
  assert.equal(state(on, { ...on, enabled: false }).restartNeeded, true)
  assert.equal(state({ ...on, enabled: false }, on).restartNeeded, true)
  assert.equal(state(on, on).restartNeeded, false)
  assert.equal(state({ ...on, network: true }, on).restartNeeded, true)
  assert.equal(state({ ...on, tmpMb: 512 }, on).restartNeeded, true)
  // Nothing the node does changes: containers stay off, or /tmp is unsized.
  assert.equal(state({ ...defaultContainerSettings, network: true }, defaultContainerSettings).restartNeeded, false)
  assert.equal(state({ ...on, readOnly: false, tmpMb: 512 }, { ...on, readOnly: false }).restartNeeded, false)
  assert.equal(state(on, defaultContainerSettings, false).restartNeeded, false)
  assert.equal(sameContainerSettings(on, { ...on }), true)
})

test('saved containers start only when Docker answers, so the node still starts without it', async () => {
  let asked = 0
  const ready: DockerCheck = async () => { asked++; return { state: 'ready', version: '29.0.1' } }
  assert.deepEqual(await containerLaunch(defaultContainerSettings, ready), { flags: [], running: defaultContainerSettings, unavailable: false, docker: null })
  assert.equal(asked, 0, 'Docker is not asked while containers are off')
  assert.deepEqual(await containerLaunch(on, ready), { flags: containerFlags(on), running: on, unavailable: false, docker: { state: 'ready', version: '29.0.1' } })
  const stopped: DockerStatus = { state: 'stopped', detail: 'Cannot connect to the Docker daemon.' }
  assert.deepEqual(await containerLaunch(on, async () => stopped), { flags: [], running: { ...on, enabled: false }, unavailable: true, docker: stopped })
  assert.deepEqual((await containerLaunch(on, async () => ({ state: 'missing' }))).flags, [])
})

test('Docker is told apart as answering, not installed, or installed but not running', async () => {
  const calls: { command: string, args: string[], timeoutMs: number }[] = []
  const run = (result: Partial<Ran>): Run => async (command, args, timeoutMs) => { calls.push({ command, args, timeoutMs }); return { stdout: '', stderr: '', error: null, ...result } }
  assert.deepEqual(await checkDocker({ run: run({ stdout: '29.6.2\n' }) }), { state: 'ready', version: '29.6.2' })
  assert.deepEqual(calls[0], { command: 'docker', args: ['version', '--format', '{{.Server.Version}}'], timeoutMs: 5000 })
  const missing = Object.assign(new Error('spawn docker ENOENT'), { code: 'ENOENT' })
  assert.deepEqual(await checkDocker({ run: run({ error: missing }) }), { state: 'missing' })
  const refused = Object.assign(new Error('Command failed'), { code: 1 })
  assert.deepEqual(await checkDocker({ run: run({ error: refused, stderr: 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock.\nIs the docker daemon running?\n' }) }),
    { state: 'stopped', detail: 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock.' })
  const hung = Object.assign(new Error('Command failed'), { killed: true, signal: 'SIGTERM' })
  assert.deepEqual(await checkDocker({ timeoutMs: 3000, run: run({ error: hung }) }), { state: 'stopped', detail: 'Docker did not answer within 3 seconds.' })
  // A client with no server answers with an empty server version.
  assert.equal((await checkDocker({ run: run({ stdout: '\n' }) })).state, 'stopped')
  assert.equal((await checkDocker({ run: async () => { throw new Error('boom') } })).state, 'stopped')
})

test('a real Docker check of a command that is not there says Docker is missing, without throwing', async () => {
  assert.deepEqual(await checkDocker({ command: 'sisyphus-no-such-docker-command', timeoutMs: 2000 }), { state: 'missing' })
})

test('end-to-end tests can stand in for Docker, and anything else leaves the real check', async () => {
  assert.deepEqual(await dockerCheckFromEnvironment('ready:27.3.1')?.(), { state: 'ready', version: '27.3.1' })
  assert.deepEqual(await dockerCheckFromEnvironment('missing')?.(), { state: 'missing' })
  assert.equal((await dockerCheckFromEnvironment('stopped')?.())?.state, 'stopped')
  for (const value of [undefined, '', 'ready:', 'yes']) assert.equal(dockerCheckFromEnvironment(value), null)
})
