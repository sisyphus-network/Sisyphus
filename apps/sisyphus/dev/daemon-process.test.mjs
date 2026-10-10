import test from 'node:test'
import assert from 'node:assert/strict'
import { EventEmitter } from 'node:events'
import { createServer } from 'node:net'
import { spawn } from 'node:child_process'
import { DesktopDaemon, daemonShutdownGraceMs, localApiAddress, portIsUnused } from '../src/main/daemon-process.ts'

test('only explicit loopback API addresses may start a daemon', () => {
  for (const endpoint of ['example.com:50051', '0.0.0.0:50051', '127.0.0.1:0', 'localhost:65536', 'unix:/tmp/node']) assert.equal(localApiAddress(endpoint), null)
  assert.deepEqual(localApiAddress('127.0.0.1:50051'), { host: '127.0.0.1', port: 50051 })
  assert.deepEqual(localApiAddress('[::1]:50051'), { host: '::1', port: 50051 })
})

test('real port probe never takes over a running service', async () => {
  const server = createServer((socket) => socket.end())
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  const endpoint = `127.0.0.1:${server.address().port}`
  try { assert.equal(await portIsUnused(endpoint), false) }
  finally { await new Promise((resolve) => server.close(resolve)) }
  assert.equal(await portIsUnused(endpoint), true)
  assert.equal(await portIsUnused('example.com:50051'), false)
})

function fixture(overrides = {}) {
  const child = new EventEmitter()
  const signals = []
  child.kill = (signal) => { signals.push(signal); child.emit('close', 0, signal); return true }
  const launches = []
  const errors = []
  const daemon = new DesktopDaemon({
    executable: '/test path/sisyphusd', endpoint: '127.0.0.1:50051', dataDir: '/test path/data', enabled: true,
    unused: async () => true,
    launch: (executable, args) => { launches.push({ executable, args }); return child },
    onError: (error) => errors.push(error), ...overrides,
  })
  return { daemon, child, signals, launches, errors }
}

test('launches once with separate arguments and loopback-only networking', async () => {
  const { daemon, launches, signals } = fixture()
  await Promise.all([daemon.ensureStarted(), daemon.ensureStarted()])
  await daemon.ensureStarted()
  assert.deepEqual(launches, [{ executable: '/test path/sisyphusd', args: ['run', '--api-listen', '127.0.0.1:50051', '--data-dir', '/test path/data', '--listen', '127.0.0.1:7700', '--discovery', 'off'] }])
  await daemon.stop()
  assert.deepEqual(signals, ['SIGTERM'])
  await daemon.stop()
  await daemon.ensureStarted()
  assert.equal(launches.length, 1)
  assert.deepEqual(signals, ['SIGTERM'])
})

test('occupied default pool port falls back without taking over its service', async () => {
  const server = createServer(socket => socket.end())
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve) })
  const occupied = `127.0.0.1:${server.address().port}`
  try {
    const { daemon, launches } = fixture({ unused: endpoint => endpoint.endsWith(':50051') ? Promise.resolve(true) : portIsUnused(occupied) })
    await daemon.ensureStarted()
    assert.equal(launches[0].args[6], '127.0.0.1:0')
    assert.equal(await portIsUnused(occupied), false)
    await daemon.stop()
  } finally { await new Promise(resolve => server.close(resolve)) }
})

test('API on the preferred pool port cannot conflict with the pool listener', async () => {
  const { daemon, launches } = fixture({ endpoint: '127.0.0.1:7700' })
  await daemon.ensureStarted()
  assert.equal(launches[0].args[6], '127.0.0.1:0')
  await daemon.stop()
})

test('quit during the second port probe still cannot spawn', async () => {
  let release
  const { daemon, launches } = fixture({ unused: endpoint => endpoint.endsWith(':50051') ? Promise.resolve(true) : new Promise(resolve => { release = resolve }) })
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
  }
})

test('quit during probing cannot spawn a late orphan', async () => {
  let release
  const { daemon, launches } = fixture({ unused: () => new Promise((resolve) => { release = resolve }) })
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
  await other.daemon.ensureStarted()
  assert.match(other.errors[0], /stopped \(1\)/)
  assert.equal(other.launches.length, 1)
  await other.daemon.stop()
})

test('an actual owned child is stopped and reaped, without touching other processes', async () => {
  let child
  const { daemon } = fixture({ launch: () => {
    child = spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], { stdio: 'ignore' })
    return child
  } })
  await daemon.ensureStarted()
  await new Promise((resolve, reject) => { child.once('spawn', resolve); child.once('error', reject) })
  const closed = new Promise((resolve) => child.once('close', resolve))
  await daemon.stop()
  await closed
  assert.ok(child.exitCode !== null || child.signalCode !== null)
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
