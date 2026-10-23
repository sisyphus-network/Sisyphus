import test from 'node:test'
import assert from 'node:assert/strict'
import { EventEmitter } from 'node:events'
import { createServer } from 'node:net'
import type { Server } from 'node:net'
import { spawn } from 'node:child_process'
import type { ChildProcess } from 'node:child_process'
import { DesktopDaemon, daemonShutdownGraceMs, localApiAddress, portIsUnused } from '../src/main/daemon-process.ts'
import { restartManagedNode } from '../src/main/restart-managed-node.ts'

type Options = ConstructorParameters<typeof DesktopDaemon>[0]
type Snapshot = { status: string }

// A fake child has only the events and kill the daemon uses, so it is cast
// to a ChildProcess where it is handed over.
class FakeChild extends EventEmitter {
  kill: (signal?: NodeJS.Signals | number) => boolean = () => true
}

function portOf(server: Server): number {
  const address = server.address()
  assert.ok(address && typeof address === 'object', 'the server is not listening on a port')
  return address.port
}

test('only explicit loopback API addresses may start a daemon', () => {
  for (const endpoint of ['example.com:50051', '0.0.0.0:50051', '127.0.0.1:0', 'localhost:65536', 'unix:/tmp/node']) assert.equal(localApiAddress(endpoint), null)
  assert.deepEqual(localApiAddress('127.0.0.1:50051'), { host: '127.0.0.1', port: 50051 })
  assert.deepEqual(localApiAddress('[::1]:50051'), { host: '::1', port: 50051 })
})

test('real port probe never takes over a running service', async () => {
  const server = createServer((socket) => socket.end())
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', () => resolve()))
  const endpoint = `127.0.0.1:${portOf(server)}`
  try { assert.equal(await portIsUnused(endpoint), false) }
  finally { await new Promise<void>((resolve) => server.close(() => resolve())) }
  assert.equal(await portIsUnused(endpoint), true)
  assert.equal(await portIsUnused('example.com:50051'), false)
})

function fixture(overrides: Partial<Options> = {}) {
  const child = new FakeChild()
  const signals: (NodeJS.Signals | number | undefined)[] = []
  child.kill = (signal) => { signals.push(signal); child.emit('close', 0, signal); return true }
  const launches: { executable: string, args: string[] }[] = []
  const errors: string[] = []
  const daemon = new DesktopDaemon({
    executable: '/test path/sisyphusd', endpoint: '127.0.0.1:50051', webListenAddress: '127.0.0.1:50052', dataDir: '/test path/data', enabled: true,
    unused: async () => true,
    launch: (executable, args) => { launches.push({ executable, args }); return child as unknown as ChildProcess },
    onError: (error) => errors.push(error), ...overrides,
  })
  return { daemon, child, signals, launches, errors }
}

test('launches once with separate arguments and loopback-only networking', async () => {
  const { daemon, launches, signals } = fixture()
  assert.equal(daemon.managesDaemon, false)
  await Promise.all([daemon.ensureStarted(), daemon.ensureStarted()])
  assert.equal(daemon.managesDaemon, true)
  await daemon.ensureStarted()
  assert.deepEqual(launches, [{ executable: '/test path/sisyphusd', args: ['run', '--api-listen', '127.0.0.1:50051', '--data-dir', '/test path/data', '--listen', '127.0.0.1:7700', '--discovery', 'off', '--web-listen', '127.0.0.1:50052'] }])
  await daemon.stop()
  assert.equal(daemon.managesDaemon, true)
  assert.deepEqual(signals, ['SIGTERM'])
  await daemon.stop()
  await daemon.ensureStarted()
  assert.equal(launches.length, 1)
  assert.deepEqual(signals, ['SIGTERM'])
})

test('passes only explicitly enabled location discovery flags to the managed daemon', async () => {
  const { daemon, launches } = fixture({ locationDiscovery: { locateGateway: true, locateCountry: false, stunServer: '' } })
  assert.equal(daemon.ownsProcess, false)
  await daemon.ensureStarted()
  assert.equal(daemon.ownsProcess, true)
  assert.deepEqual(launches[0].args.slice(-1), ['--locate-gateway'])
  await daemon.stop()
  assert.equal(daemon.ownsProcess, false)

  const both = fixture({ locationDiscovery: { locateGateway: true, locateCountry: true, stunServer: '' } })
  await both.daemon.ensureStarted()
  assert.deepEqual(both.launches[0].args.slice(-2), ['--locate-gateway', '--locate-country'])
  await both.daemon.stop()

  const namedStun = fixture({ locationDiscovery: { locateGateway: false, locateCountry: false, stunServer: 'stun.example.org:3478' } })
  await namedStun.daemon.ensureStarted()
  assert.deepEqual(namedStun.launches[0].args.slice(-2), ['--stun-server', 'stun.example.org:3478'])
  await namedStun.daemon.stop()
})

test('a hosting node listens where the owner chose and is discoverable', async () => {
  const { daemon, launches } = fixture({ hostingListen: '192.168.137.133:7700', unused: async (address) => address !== '127.0.0.1:7700' })
  await daemon.ensureStarted()
  assert.deepEqual(launches[0].args, ['run', '--api-listen', '127.0.0.1:50051', '--data-dir', '/test path/data', '--listen', '192.168.137.133:7700', '--web-listen', '127.0.0.1:50052'])
  await daemon.stop()
})

test('the container flags are worked out as each start happens, so a restart picks up what was saved since', async () => {
  let flags: string[] = []
  let asked = 0
  const { daemon, launches } = fixture({ containers: async () => { asked++; return flags } })
  await daemon.ensureStarted()
  assert.equal(asked, 1)
  assert.equal(launches[0].args.includes('--containers'), false)
  flags = ['--containers', '--containers-read-only', '--containers-tmp-mb', '1024']
  await daemon.restart()
  assert.equal(asked, 2)
  assert.deepEqual(launches[1].args.slice(-4), flags)
  await daemon.stop()

  // A node stopped while Docker is being asked is not started after all.
  let release = () => {}
  const slow = fixture({ containers: () => new Promise((done) => { release = () => done(['--containers']) }) })
  const starting = slow.daemon.ensureStarted()
  await new Promise((wake) => setImmediate(wake))
  const stopping = slow.daemon.stop()
  release()
  await Promise.all([starting, stopping])
  assert.equal(slow.launches.length, 0)

  // A port someone else holds is never ours, and Docker is not asked about it.
  let unusedAsked = 0
  const taken = fixture({ unused: async () => false, containers: async () => { unusedAsked++; return [] } })
  await taken.daemon.ensureStarted()
  assert.equal(unusedAsked, 0)
  assert.equal(taken.launches.length, 0)
})

test('a restart stops the owned node gracefully and starts it again with changed settings', async () => {
  const { daemon, launches, signals } = fixture({ unused: async (address) => address !== '127.0.0.1:7700' })
  await daemon.ensureStarted()
  assert.deepEqual(launches[0].args.slice(5, 8), ['--listen', '127.0.0.1:0', '--discovery'])
  await daemon.restart({ hostingListen: '192.168.137.133:7700', locationDiscovery: { locateGateway: true, locateCountry: false, stunServer: '' } })
  assert.deepEqual(signals, ['SIGTERM'])
  assert.equal(launches.length, 2)
  assert.deepEqual(launches[1].args, ['run', '--api-listen', '127.0.0.1:50051', '--data-dir', '/test path/data', '--listen', '192.168.137.133:7700', '--web-listen', '127.0.0.1:50052', '--locate-gateway'])
  assert.equal(daemon.ownsProcess, true)
  await daemon.restart({ hostingListen: undefined })
  assert.equal(launches.length, 3)
  assert.deepEqual(launches[2].args.slice(5, 8), ['--listen', '127.0.0.1:0', '--discovery'])
  await daemon.stop()
})

test('a child that exits immediately after restart cannot apply running settings', { timeout: 5000 }, async () => {
  const children: FakeChild[] = []
  const listeners = new Set<(snapshot: Snapshot) => void>()
  let current: Snapshot = { status: 'connected' }
  let childStopped!: () => void
  const stopped = new Promise<void>((resolve) => { childStopped = resolve })
  const daemon = new DesktopDaemon({
    executable: '/test/sisyphusd', endpoint: '127.0.0.1:50051', dataDir: '/test/data', enabled: true,
    unused: async () => true,
    launch: () => {
      const child = new FakeChild()
      child.kill = (signal) => { setImmediate(() => child.emit('close', 0, signal)); return true }
      children.push(child)
      if (children.length === 2) setImmediate(() => child.emit('exit', 1, null))
      return child as unknown as ChildProcess
    },
    onError: () => {
      current = { status: 'disconnected' }
      for (const listener of listeners) listener(current)
      childStopped()
    },
  })
  await daemon.ensureStarted()
  let runningSettings = 'old'
  await assert.rejects(restartManagedNode({
    restart: () => daemon.restart({ hostingListen: '192.168.1.5:7700' }),
    reconnect: () => {
      current = { status: 'connecting' }
      for (const listener of listeners) listener(current)
    },
    snapshots: {
      getSnapshot: async () => current,
      onSnapshot: (listener) => { listeners.add(listener); return () => listeners.delete(listener) },
    },
    timeoutMs: 20,
    timeoutMessage: 'reconnect timed out',
    onConnected: () => { runningSettings = 'new' },
  }), /reconnect timed out/)
  await stopped
  assert.equal(children.length, 2)
  assert.equal(daemon.ownsProcess, false)
  assert.equal(runningSettings, 'old')
  await daemon.stop()
})

test('a node the app did not start cannot be restarted from the app', async () => {
  for (const options of [{ unused: async () => false }, { enabled: false }]) {
    const { daemon, launches, signals } = fixture(options)
    await daemon.ensureStarted()
    await assert.rejects(daemon.restart(), /did not start the node/)
    assert.equal(launches.length, 0)
    assert.deepEqual(signals, [])
  }
})

test('refuses an invalid or occupied web API address without spawning the daemon', async () => {
  const refused: Partial<Options>[] = [
    { webListenAddress: '0.0.0.0:50052' },
    { webListenAddress: 'localhost:50052' },
    { unused: async address => Promise.resolve(address !== '127.0.0.1:50052') },
  ]
  for (const options of refused) {
    const { daemon, launches, errors } = fixture(options)
    await daemon.ensureStarted()
    assert.equal(launches.length, 0)
    assert.match(errors[0], /browser API address/)
    await daemon.stop()
  }
})

test('occupied default pool port falls back without taking over its service', async () => {
  const server = createServer(socket => socket.end())
  await new Promise<void>((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', () => resolve()) })
  const occupied = `127.0.0.1:${portOf(server)}`
  try {
    const { daemon, launches } = fixture({ unused: endpoint => endpoint === '127.0.0.1:50051' || endpoint === '127.0.0.1:50052' ? Promise.resolve(true) : portIsUnused(occupied) })
    await daemon.ensureStarted()
    assert.equal(launches[0].args[6], '127.0.0.1:0')
    assert.equal(await portIsUnused(occupied), false)
    await daemon.stop()
  } finally { await new Promise<void>(resolve => server.close(() => resolve())) }
})

test('API on the preferred pool port cannot conflict with the pool listener', async () => {
  const { daemon, launches } = fixture({ endpoint: '127.0.0.1:7700' })
  await daemon.ensureStarted()
  assert.equal(launches[0].args[6], '127.0.0.1:0')
  await daemon.stop()
})

test('quit during the second port probe still cannot spawn', async () => {
  let release: (unused: boolean) => void = () => {}
  const { daemon, launches } = fixture({ unused: endpoint => endpoint.endsWith(':50051') ? Promise.resolve(true) : new Promise<boolean>(resolve => { release = resolve }) })
  const start = daemon.ensureStarted()
  await new Promise(resolve => setImmediate(resolve))
  const stop = daemon.stop()
  release(true)
  await Promise.all([start, stop])
  assert.equal(launches.length, 0)
})

test('external, disabled and remote daemons are never started or stopped', async () => {
  for (const options of [{ unused: async () => false }, { enabled: false }, { endpoint: 'remote:50051' }]) {
    const { daemon, launches, signals } = fixture(options)
    await daemon.ensureStarted()
    await daemon.stop()
    assert.equal(launches.length, 0)
    assert.deepEqual(signals, [])
    assert.equal(daemon.managesDaemon, false)
  }
})

test('quit during probing cannot spawn a late orphan', async () => {
  let release: (unused: boolean) => void = () => {}
  const { daemon, launches } = fixture({ unused: () => new Promise<boolean>((resolve) => { release = resolve }) })
  const start = daemon.ensureStarted()
  const stop = daemon.stop()
  release(true)
  await Promise.all([start, stop])
  assert.equal(launches.length, 0)
})

test('launch failure and unexpected exit are reported without a restart storm', async () => {
  const { daemon, child, errors, launches } = fixture()
  await daemon.ensureStarted()
  child.emit('error', new Error('not installed'))
  child.emit('close')
  await daemon.ensureStarted()
  assert.match(errors[0], /not installed/)
  assert.equal(launches.length, 1)
  await daemon.stop()
  const other = fixture()
  await other.daemon.ensureStarted()
  other.child.emit('exit', 1, null)
  assert.equal(other.daemon.ownsProcess, false)
  assert.equal(other.daemon.managesDaemon, true)
  await other.daemon.ensureStarted()
  assert.match(other.errors[0], /stopped \(1\)/)
  assert.equal(other.launches.length, 1)
  await other.daemon.stop()
})

test('an actual owned child is stopped and reaped, without touching other processes', async () => {
  let child: ChildProcess | undefined
  const { daemon } = fixture({ launch: () => {
    child = spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], { stdio: 'ignore' })
    return child
  } })
  await daemon.ensureStarted()
  assert.ok(child, 'the daemon did not launch a child')
  const owned = child
  await new Promise((resolve, reject) => { owned.once('spawn', resolve); owned.once('error', reject) })
  const closed = new Promise((resolve) => owned.once('close', resolve))
  await daemon.stop()
  await closed
  assert.ok(owned.exitCode !== null || owned.signalCode !== null)
})

test('probe errors become a connection diagnostic, not an unhandled rejection', async () => {
  const { daemon, errors, launches } = fixture({ unused: async () => { throw new Error('probe failed') } })
  await daemon.ensureStarted()
  assert.match(errors[0], /probe failed/)
  assert.equal(launches.length, 0)
  await daemon.stop()
})

test('shutdown waits for a delayed graceful close without force killing', async () => {
  const { daemon, child, signals } = fixture({ shutdownGraceMs: 200 })
  child.kill = signal => { signals.push(signal); setTimeout(() => child.emit('close', 0, signal), 30); return true }
  await daemon.ensureStarted()
  await daemon.stop()
  assert.deepEqual(signals, ['SIGTERM'])
})

test('shutdown has a bounded fallback if an owned child ignores termination', async () => {
  assert.equal(daemonShutdownGraceMs, 30000)
  const { daemon, child, signals } = fixture({ shutdownGraceMs: 20 })
  child.kill = (signal) => { signals.push(signal); return true }
  await daemon.ensureStarted()
  await daemon.stop()
  assert.deepEqual(signals, ['SIGTERM', 'SIGKILL'])
})
