import test from 'node:test'
import assert from 'node:assert/strict'
import { restartManagedNode } from '../src/main/restart-managed-node.ts'

function source() {
  let current = { status: 'connected' }
  const listeners = new Set()
  return {
    source: {
      getSnapshot: async () => current,
      onSnapshot(callback) { listeners.add(callback); return () => listeners.delete(callback) },
    },
    set(status) { current = { status }; for (const callback of listeners) callback(current) },
  }
}

test('runtime settings are applied only after a connected snapshot', async () => {
  const snapshots = source()
  const order = []
  const restarting = restartManagedNode({
    restart: async () => { order.push('restart') },
    reconnect: () => { order.push('reconnect'); snapshots.set('connecting') },
    snapshots: snapshots.source,
    timeoutMs: 100,
    timeoutMessage: 'timed out',
    onConnected: () => order.push('apply'),
  })
  await new Promise((resolve) => setImmediate(resolve))
  assert.deepEqual(order, ['restart', 'reconnect'])
  snapshots.set('connected')
  await restarting
  assert.deepEqual(order, ['restart', 'reconnect', 'apply'])
})

test('runtime settings are not applied when the node does not reconnect', async () => {
  const snapshots = source()
  const order = []
  await assert.rejects(restartManagedNode({
    restart: async () => { order.push('restart') },
    reconnect: () => { order.push('reconnect'); snapshots.set('connecting') },
    snapshots: snapshots.source,
    timeoutMs: 5,
    timeoutMessage: 'timed out',
    onConnected: () => order.push('apply'),
  }), /timed out/)
  assert.deepEqual(order, ['restart', 'reconnect'])
})
