import assert from 'node:assert/strict'
import { test } from 'node:test'
import { nodeInfoFromSharedStateEvent } from '../src/renderer/src/lib/node-shared-state.ts'

test('reads NODE shared state from the Connect oneof shape', () => {
  const info = { peerId: 'connect-node' }
  assert.equal(nodeInfoFromSharedStateEvent({ state: { case: 'node', value: { info } } }), info)
})

test('reads NODE shared state from the grpc-js oneof shape', () => {
  const info = { peerId: 'grpc-node' }
  assert.equal(nodeInfoFromSharedStateEvent({ state: 'node', node: { info } }), info)
})

test('does not treat another domain or a missing NODE payload as node info', () => {
  assert.equal(nodeInfoFromSharedStateEvent({ state: { case: 'workers', value: { info: {} } } }), undefined)
  assert.equal(nodeInfoFromSharedStateEvent({ state: 'node', node: undefined }), undefined)
})
