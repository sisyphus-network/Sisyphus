import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createRpcStream } from '../src/preload/rpc-stream.ts'

function harness(start = Promise.resolve(), stop = Promise.resolve()) {
  const listeners = new Map()
  const calls = []
  let lastListener
  const ipc = {
    invoke(channel, ...args) {
      calls.push([channel, ...args])
      return channel.endsWith('-start') ? start : stop
    },
    on(channel, listener) { listeners.set(channel, listener); lastListener = listener },
    removeListener(channel, listener) {
      assert.equal(listeners.get(channel), listener)
      listeners.delete(channel)
    },
  }
  return { ipc, calls, listeners, emit: (payload) => lastListener({}, payload) }
}

const flush = () => new Promise((resolve) => setImmediate(resolve))

test('delivers data and releases the listener exactly once on end', () => {
  const h = harness(), data = [], errors = []
  let ended = 0
  const cancel = createRpcStream(h.ipc, 'ask', { text: 'hello' }, (value) => data.push(value), (error) => errors.push(error), () => ended++)
  assert.equal(h.listeners.size, 1)
  assert.match(h.calls[0][1].id, /^[\da-f-]{36}$/i)
  assert.deepEqual(h.calls[0][1].request, { text: 'hello' })
  h.emit({ data: 'first' }); h.emit({ data: 'second' }); h.emit({ end: true })
  h.emit({ end: true }); h.emit({ data: 'late' }); cancel()
  assert.deepEqual(data, ['first', 'second'])
  assert.deepEqual(errors, [])
  assert.equal(ended, 1)
  assert.equal(h.listeners.size, 0)
  assert.equal(h.calls.length, 1)
})

test('a stream error releases the listener without reporting a normal end', () => {
  const h = harness(), errors = [], data = []
  let ended = 0
  createRpcStream(h.ipc, 'watchJobs', {}, (value) => data.push(value), (error) => errors.push(error), () => ended++)
  h.emit({ error: 'disconnected' }); h.emit({ end: true }); h.emit({ data: 'late' })
  assert.deepEqual(errors, ['disconnected'])
  assert.deepEqual(data, [])
  assert.equal(ended, 0)
  assert.equal(h.listeners.size, 0)
})

test('a rejected start releases the listener and reports the error once', async () => {
  const h = harness(Promise.reject(new Error('not connected'))), errors = []
  createRpcStream(h.ipc, 'ask', {}, () => {}, (error) => errors.push(error))
  await flush()
  h.emit({ error: 'duplicate' })
  assert.deepEqual(errors, ['not connected'])
  assert.equal(h.listeners.size, 0)
})

test('cancelling during start suppresses late errors and stops exactly once', async () => {
  let rejectStart
  const h = harness(new Promise((_resolve, reject) => { rejectStart = reject })), errors = [], data = []
  const cancel = createRpcStream(h.ipc, 'ask', {}, (value) => data.push(value), (error) => errors.push(error))
  cancel(); cancel()
  rejectStart(new Error('late failure'))
  h.emit({ data: 'late' }); h.emit({ error: 'late error' })
  await flush()
  assert.deepEqual(errors, [])
  assert.deepEqual(data, [])
  assert.equal(h.listeners.size, 0)
  assert.equal(h.calls.length, 2)
  assert.equal(h.calls[1][0], 'node:rpc-stream-stop')
  assert.equal(h.calls[1][1], h.calls[0][1].id)
})

test('a rejected stop is handled during teardown', async () => {
  const h = harness(Promise.resolve(), Promise.reject(new Error('window closed')))
  const cancel = createRpcStream(h.ipc, 'pullModel', {}, () => {})
  cancel()
  await flush()
  assert.equal(h.listeners.size, 0)
})

test('independent streams clean up only their own listener', () => {
  const h = harness()
  const first = createRpcStream(h.ipc, 'ask', {}, () => {})
  const second = createRpcStream(h.ipc, 'watchJobs', {}, () => {})
  assert.equal(h.listeners.size, 2)
  first()
  assert.equal(h.listeners.size, 1)
  second()
  assert.equal(h.listeners.size, 0)
})
