import assert from 'node:assert/strict'
import { test } from 'node:test'
import { sharedStateDomainCode } from '../src/renderer/src/lib/shared-state-domain.ts'

test('shared-state domains accept grpc-js enum names and browser numeric values', () => {
  assert.equal(sharedStateDomainCode('SHARED_STATE_DOMAIN_WORKERS'), 'workers')
  assert.equal(sharedStateDomainCode('SHARED_STATE_DOMAIN_JOINED_POOL'), 'joinedPool')
  assert.equal(sharedStateDomainCode(3), 'workers')
  assert.equal(sharedStateDomainCode(5), 'joinedPool')
  assert.equal(sharedStateDomainCode('SHARED_STATE_DOMAIN_PEERS'), undefined)
})
