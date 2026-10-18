import assert from 'node:assert/strict'
import { test } from 'node:test'
import { startBrowserNodeStateSession } from '../src/renderer/src/lib/browser-node-state-session.ts'

const tick = () => new Promise((resolve) => setImmediate(resolve))

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
