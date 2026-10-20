import test from 'node:test'
import assert from 'node:assert/strict'
import { waitForConnectedSnapshot } from '../src/shared/wait-for-connected-snapshot.ts'

function snapshot(status) { return { status } }

function makeApi({ initial = snapshot('disconnected'), subscribeImmediately = false } = {}) {
  let listener
  let removed = 0
  return {
    api: {
      getSnapshot: async () => initial,
      onSnapshot(callback) {
        listener = callback
        if (subscribeImmediately) callback(snapshot('connected'))
        return () => { removed++ }
      },
    },
    connect() { listener?.(snapshot('connected')) },
    removed: () => removed,
  }
}

test('resolves when the node reports a connected snapshot and removes its listener', async () => {
  const source = makeApi()
  const waiting = waitForConnectedSnapshot(source.api, 100, 'timed out')
  source.connect()
  await waiting
  assert.equal(source.removed(), 1)
})

test('does not miss a connection reported synchronously during subscription', async () => {
  const source = makeApi({ subscribeImmediately: true })
  await waitForConnectedSnapshot(source.api, 100, 'timed out')
  assert.equal(source.removed(), 1)
})

test('rejects on timeout and removes its listener', async () => {
  const source = makeApi()
  await assert.rejects(waitForConnectedSnapshot(source.api, 5, 'timed out'), /timed out/)
  assert.equal(source.removed(), 1)
})
