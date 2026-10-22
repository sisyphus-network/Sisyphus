import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createRpcStream } from '../src/preload/rpc-stream.ts'

type Payload = { data?: unknown, error?: string, errorCode?: number, end?: boolean }
type Listener = (event: unknown, payload: Payload) => void
type StartRequest = { id: string, method: string, request: unknown }

function harness(start: Promise<unknown> = Promise.resolve(), stop: Promise<unknown> = Promise.resolve()) {
  const listeners = new Map<string, Listener>()
  const calls: [string, ...unknown[]][] = []
  let lastListener: Listener | undefined
  const ipc = {
    invoke(channel: string, ...args: unknown[]) {
      calls.push([channel, ...args])
      return channel.endsWith('-start') ? start : stop
    },
    on(channel: string, listener: Listener) { listeners.set(channel, listener); lastListener = listener },
    removeListener(channel: string, listener: Listener) {
      assert.equal(listeners.get(channel), listener)
      listeners.delete(channel)
    },
  }
  return { ipc, calls, listeners, emit: (payload: Payload) => lastListener!({}, payload) }
}

const flush = () => new Promise((resolve) => setImmediate(resolve))

test('forwards structured gRPC status without parsing the error message', () => {
  const h = harness(), failures: { message: string, code?: number }[] = []
  createRpcStream(h.ipc, 'ask', {}, () => {}, (message, code) => failures.push({ message, code }))
  h.emit({ error: 'המודל אינו מוגדר', errorCode: 9 })
  assert.deepEqual(failures, [{ message: 'המודל אינו מוגדר', code: 9 }])
  assert.equal(h.listeners.size, 0)
})

test('delivers data and releases the listener exactly once on end', () => {
  const h = harness(), data: unknown[] = [], errors: string[] = []
  let ended = 0
  const cancel = createRpcStream(h.ipc, 'ask', { text: 'hello' }, (value) => data.push(value), (error) => errors.push(error), () => ended++)
  assert.equal(h.listeners.size, 1)
  assert.match((h.calls[0][1] as StartRequest).id, /^[\da-f-]{36}$/i)
  assert.deepEqual((h.calls[0][1] as StartRequest).request, { text: 'hello' })
  h.emit({ data: 'first' }); h.emit({ data: 'second' }); h.emit({ end: true })
  h.emit({ end: true }); h.emit({ data: 'late' }); cancel()
  assert.deepEqual(data, ['first', 'second'])
  assert.deepEqual(errors, [])
  assert.equal(ended, 1)
  assert.equal(h.listeners.size, 0)
  assert.equal(h.calls.length, 1)
})

test('a stream error releases the listener without reporting a normal end', () => {
  const h = harness(), errors: string[] = [], data: unknown[] = []
  let ended = 0
  createRpcStream(h.ipc, 'watchJobs', {}, (value) => data.push(value), (error) => errors.push(error), () => ended++)
  h.emit({ error: 'disconnected' }); h.emit({ end: true }); h.emit({ data: 'late' })
  assert.deepEqual(errors, ['disconnected'])
  assert.deepEqual(data, [])
  assert.equal(ended, 0)
  assert.equal(h.listeners.size, 0)
})

test('a rejected start releases the listener and reports the error once', async () => {
  const h = harness(Promise.reject(new Error('not connected'))), errors: string[] = []
  createRpcStream(h.ipc, 'ask', {}, () => {}, (error) => errors.push(error))
  await flush()
  h.emit({ error: 'duplicate' })
  assert.deepEqual(errors, ['not connected'])
  assert.equal(h.listeners.size, 0)
})

test('cancelling during start suppresses late errors and stops exactly once', async () => {
  let rejectStart: ((reason: Error) => void) | undefined
  const h = harness(new Promise((_resolve, reject) => { rejectStart = reject })), errors: string[] = [], data: unknown[] = []
  const cancel = createRpcStream(h.ipc, 'ask', {}, (value) => data.push(value), (error) => errors.push(error))
  cancel(); cancel()
  rejectStart!(new Error('late failure'))
  h.emit({ data: 'late' }); h.emit({ error: 'late error' })
  await flush()
  assert.deepEqual(errors, [])
  assert.deepEqual(data, [])
  assert.equal(h.listeners.size, 0)
  assert.equal(h.calls.length, 2)
  assert.equal(h.calls[1][0], 'node:rpc-stream-stop')
  assert.equal(h.calls[1][1], (h.calls[0][1] as StartRequest).id)
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
