import assert from 'node:assert/strict'
import { EventEmitter } from 'node:events'
import { test } from 'node:test'
import { startNodeStateSession } from '../src/main/node-state-session.ts'

class FakeStream extends EventEmitter {
  cancelled = 0
  cancel() { this.cancelled++ }
  data(value) { this.emit('data', value) }
  fail(error) { this.emit('error', error) }
  end() { this.emit('end') }
}

function clientHarness({ infoError = null, info = { peerId: 'node' } } = {}) {
  const calls = []
  const states = new FakeStream()
  const peers = new FakeStream()
  const client = {
    getNodeInfo(_request, _options, callback) { calls.push('info'); callback(infoError, info) },
    watchNodeState() { calls.push('state'); return states },
    watchPeers() { calls.push('peers'); return peers },
  }
  return { client, calls, states, peers }
}

const unimplemented = Object.assign(new Error('stream not supported'), { code: 12 })

test('fetches a fresh baseline before opening the live stream and forwards revisions', () => {
  const h = clientHarness(), seen = [], failures = []
  const stop = startNodeStateSession({
    client: h.client, isCurrent: () => true, unimplementedCode: 12,
    onReady: (info) => seen.push(['baseline', info]),
    onUpdate: (update, legacy) => seen.push(['update', update, legacy]),
    onError: (error) => failures.push(error.message),
  })

  assert.deepEqual(h.calls, ['info', 'state'])
  h.states.data({ revision: '7', peers: ['first'] })
  assert.deepEqual(seen, [
    ['baseline', { peerId: 'node' }],
    ['update', { revision: '7', peers: ['first'] }, false],
  ])
  assert.deepEqual(failures, [])
  stop()
  assert.equal(h.states.cancelled, 1)
})

test('falls back to the peer stream only when the combined stream is unimplemented', () => {
  const h = clientHarness(), seen = [], failures = []
  const stop = startNodeStateSession({
    client: h.client, isCurrent: () => true, unimplementedCode: 12,
    onReady: (info) => seen.push(['baseline', info]),
    onUpdate: (update, legacy) => seen.push(['update', update, legacy]),
    onError: (error) => failures.push(error.message),
  })

  h.states.fail(unimplemented)
  assert.deepEqual(h.calls, ['info', 'state', 'peers'])
  assert.equal(h.states.cancelled, 1)
  h.peers.data({ revision: '8', peers: ['legacy'] })
  assert.deepEqual(seen.at(-1), ['update', { revision: '8', peers: ['legacy'] }, true])
  h.states.end() // the superseded stream must not trigger a reconnect
  assert.deepEqual(failures, [])
  stop()
  assert.equal(h.peers.cancelled, 1)
})

test('stream failure and end request one reconnect, but obsolete generations are ignored', () => {
  for (const terminate of [
    (stream) => stream.fail(new Error('network lost')),
    (stream) => stream.end(),
  ]) {
    const h = clientHarness(), failures = []
    let generation = true
    const stop = startNodeStateSession({
      client: h.client, isCurrent: () => generation, unimplementedCode: 12,
      onReady() {}, onUpdate() {}, onError: (error) => failures.push(error.message),
    })
    terminate(h.states)
    assert.equal(failures.length, 1)
    generation = false
    h.states.data({ revision: 'stale' })
    h.states.fail(new Error('stale error'))
    assert.equal(failures.length, 1)
    stop()
  }
})

test('cancellation before the initial response suppresses setup and callbacks', () => {
  const h = clientHarness(), seen = []
  let current = true
  h.client.getNodeInfo = (_request, _options, callback) => { h.calls.push('info'); h.infoCallback = callback }
  const stop = startNodeStateSession({
    client: h.client, isCurrent: () => current, unimplementedCode: 12,
    onReady: (info) => seen.push(info), onUpdate: (update) => seen.push(update), onError: (error) => seen.push(error),
  })
  current = false
  stop()
  h.infoCallback(null, { peerId: 'late' })
  assert.deepEqual(h.calls, ['info'])
  assert.deepEqual(seen, [])
})

test('initial RPC failure does not open a state stream', () => {
  const error = new Error('offline')
  const h = clientHarness({ infoError: error }), failures = []
  startNodeStateSession({
    client: h.client, isCurrent: () => true, unimplementedCode: 12,
    onReady() {}, onUpdate() {}, onError: (value) => failures.push(value),
  })
  assert.deepEqual(h.calls, ['info'])
  assert.deepEqual(failures, [error])
})
