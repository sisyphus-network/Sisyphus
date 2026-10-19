import assert from 'node:assert/strict'
import { test } from 'node:test'
import { startBrowserNodeStateSession } from '../src/renderer/src/lib/browser-node-state-session.ts'

const tick = () => new Promise((resolve) => setImmediate(resolve))

async function until(predicate) {
  for (let attempt = 0; attempt < 20 && !predicate(); attempt++) await tick()
  assert.ok(predicate(), 'expected browser state session to reach the requested point')
}

test('browser session does not refetch snapshots while its state stream is healthy', async () => {
  let snapshots = 0
  const published = []
  const cancel = startBrowserNodeStateSession({
    getSnapshot: async () => ({ revision: `base-${++snapshots}` }),
    watchNodeState: async function* (signal) {
      yield { info: true, revision: 'live' }
      await new Promise((resolve) => signal.addEventListener('abort', resolve, { once: true }))
    },
    watchPeers: async function* () {},
    hasNodeDetails: (state) => Boolean(state.info),
    isUnimplemented: () => false,
    applyNodeState: (snapshot, state) => ({ ...snapshot, revision: state.revision }),
    applyPeers: (snapshot) => snapshot,
    onSnapshot: (snapshot) => published.push(snapshot.revision),
    onError: () => ({ revision: 'offline' }),
  })
  try {
    await tick()
    await tick()
    assert.deepEqual(published, ['base-1', 'live'])
    assert.equal(snapshots, 1)
  } finally { cancel() }
})

test('browser session fetches a fresh baseline after stream failure, not on a timer', async () => {
  let snapshots = 0
  let streamCalls = 0
  const published = []
  let releaseRetry
  const cancel = startBrowserNodeStateSession({
    getSnapshot: async () => ({ revision: `baseline-${++snapshots}` }),
    watchNodeState: async function* (signal) {
      streamCalls += 1
      if (streamCalls === 1) throw new Error('connection reset')
      yield { info: true, revision: 'live' }
    },
    watchPeers: async function* () {},
    hasNodeDetails: (state) => Boolean(state.info),
    isUnimplemented: () => false,
    applyNodeState: (snapshot, state) => ({ ...snapshot, revision: state.revision }),
    applyPeers: (snapshot) => snapshot,
    onSnapshot: (snapshot) => published.push(snapshot.revision),
    onError: () => ({ revision: 'offline' }),
    wait: () => new Promise((resolve) => { releaseRetry = resolve }),
  })

  try {
    await tick()
    assert.deepEqual(published, ['baseline-1', 'offline'])
    assert.equal(snapshots, 1)
    releaseRetry()
    await tick()
    assert.deepEqual(published, ['baseline-1', 'offline', 'baseline-2', 'live', 'offline'])
    assert.equal(snapshots, 2)
  } finally { cancel() }
})

test('browser stream reconnect backs off during outages and resets after a live update', async () => {
  const delays = []
  const releases = []
  const published = []
  let streamCalls = 0
  const cancel = startBrowserNodeStateSession({
    getSnapshot: async () => ({ revision: 'baseline' }),
    watchNodeState: async function* () {
      streamCalls++
      if (streamCalls < 3) throw new Error('connection reset')
      yield { info: true, revision: 'live' }
      throw new Error('connection reset')
    },
    watchPeers: async function* () {},
    hasNodeDetails: (state) => Boolean(state.info),
    isUnimplemented: () => false,
    applyNodeState: (snapshot, state) => ({ ...snapshot, revision: state.revision }),
    applyPeers: (snapshot) => snapshot,
    onSnapshot: (snapshot) => published.push(snapshot.revision),
    onError: () => ({ revision: 'offline' }),
    wait: (ms) => new Promise((resolve) => { delays.push(ms); releases.push(resolve) }),
  })

  try {
    await until(() => delays.length === 1)
    assert.deepEqual(delays, [1_500])
    releases.shift()()
    await until(() => delays.length === 2)
    assert.deepEqual(delays, [1_500, 3_000])
    releases.shift()()
    await until(() => delays.length === 3)
    assert.deepEqual(delays, [1_500, 3_000, 1_500])
    assert.deepEqual(published, ['baseline', 'offline', 'baseline', 'offline', 'baseline', 'live', 'offline'])
  } finally { cancel() }
})

test('browser stream reconnect delay is capped during a prolonged outage', async () => {
  const delays = []
  const releases = []
  const cancel = startBrowserNodeStateSession({
    getSnapshot: async () => ({}),
    watchNodeState: async function* () { throw new Error('connection reset') },
    watchPeers: async function* () {},
    hasNodeDetails: () => true,
    isUnimplemented: () => false,
    applyNodeState: (snapshot) => snapshot,
    applyPeers: (snapshot) => snapshot,
    onSnapshot() {},
    onError: () => ({}),
    wait: (ms) => new Promise((resolve) => { delays.push(ms); releases.push(resolve) }),
  })

  try {
    for (let count = 1; count <= 7; count++) {
      await until(() => delays.length === count)
      releases.shift()()
    }
    assert.deepEqual(delays, [1_500, 3_000, 6_000, 12_000, 24_000, 30_000, 30_000])
  } finally { cancel() }
})

test('browser session falls back to peer stream only when combined stream is unimplemented', async () => {
  let peerCalls = 0
  const published = []
  const cancel = startBrowserNodeStateSession({
    getSnapshot: async () => ({ revision: 'base' }),
    watchNodeState: async function* () { throw new Error('UNIMPLEMENTED') },
    watchPeers: async function* () { peerCalls += 1; yield { revision: 'peer' } },
    hasNodeDetails: () => true,
    isUnimplemented: (error) => /UNIMPLEMENTED/.test(error.message),
    applyNodeState: (snapshot) => snapshot,
    applyPeers: (snapshot, peers) => ({ ...snapshot, revision: peers.revision }),
    onSnapshot: (snapshot) => published.push(snapshot.revision),
    onError: () => ({ revision: 'offline' }),
  })
  try {
    await tick()
    await tick()
    assert.equal(peerCalls, 1)
    assert.deepEqual(published, ['base', 'peer', 'offline'])
  } finally { cancel() }
})

test('browser session does not use legacy fallback for transport failures', async () => {
  let peerCalls = 0
  let snapshots = 0
  const published = []
  let startedWaiting
  const waiting = new Promise((resolve) => { startedWaiting = resolve })
  const cancel = startBrowserNodeStateSession({
    getSnapshot: async () => ({ revision: `base-${++snapshots}` }),
    watchNodeState: async function* () { throw new Error('network reset') },
    watchPeers: async function* () { peerCalls += 1 },
    hasNodeDetails: () => true,
    isUnimplemented: (error) => /UNIMPLEMENTED/.test(error.message),
    applyNodeState: (snapshot) => snapshot,
    applyPeers: (snapshot) => snapshot,
    onSnapshot: (snapshot) => published.push(snapshot.revision),
    onError: () => ({ revision: 'offline' }),
    wait: (_ms, signal) => new Promise((resolve) => {
      startedWaiting()
      signal.addEventListener('abort', resolve, { once: true })
    }),
  })
  try {
    await waiting
    await tick()
    assert.equal(peerCalls, 0)
    assert.deepEqual(published, ['base-1', 'offline'])
  } finally { cancel() }
  await tick()
  assert.equal(snapshots, 1, 'aborting during retry wait must prevent a new baseline fetch')
})
