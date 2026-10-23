import test from 'node:test'
import assert from 'node:assert/strict'
import { restartManagedNode } from '../src/main/restart-managed-node.ts'

type Snapshot = { status: string }

function source() {
  let current: Snapshot = { status: 'connected' }
  const listeners = new Set<(snapshot: Snapshot) => void>()
  return {
    source: {
      getSnapshot: async () => current,
      onSnapshot(callback: (snapshot: Snapshot) => void) { listeners.add(callback); return () => listeners.delete(callback) },
    },
    set(status: string) { current = { status }; for (const callback of listeners) callback(current) },
  }
}

test('runtime settings are applied only after a connected snapshot', async () => {
  const snapshots = source()
  const order: string[] = []
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
  const order: string[] = []
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
