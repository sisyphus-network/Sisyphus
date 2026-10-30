import assert from 'node:assert/strict'
import { test } from 'node:test'
import { sharedStateDomainCode } from '../src/renderer/src/lib/shared-state-domain.ts'

test('shared-state domains accept grpc-js enum names and browser numeric values', () => {
  assert.equal(sharedStateDomainCode('SHARED_STATE_DOMAIN_NODE'), 'node')
  assert.equal(sharedStateDomainCode('SHARED_STATE_DOMAIN_WORKERS'), 'workers')
  assert.equal(sharedStateDomainCode('SHARED_STATE_DOMAIN_JOINED_POOL'), 'joinedPool')
  assert.equal(sharedStateDomainCode(1), 'node')
  assert.equal(sharedStateDomainCode(3), 'workers')
  assert.equal(sharedStateDomainCode(5), 'joinedPool')
  assert.equal(sharedStateDomainCode(6), 'modelProfiles')
  assert.equal(sharedStateDomainCode('SHARED_STATE_DOMAIN_MODEL_PROFILES'), 'modelProfiles')
  assert.equal(sharedStateDomainCode(7), 'modelLending')
  assert.equal(sharedStateDomainCode('SHARED_STATE_DOMAIN_MODEL_LENDING'), 'modelLending')
  assert.equal(sharedStateDomainCode(8), 'files')
  assert.equal(sharedStateDomainCode('SHARED_STATE_DOMAIN_FILES'), 'files')
  assert.equal(sharedStateDomainCode(9), 'poolLimits')
  assert.equal(sharedStateDomainCode('SHARED_STATE_DOMAIN_POOL_LIMITS'), 'poolLimits')
  assert.equal(sharedStateDomainCode(10), 'locationDiscovery')
  assert.equal(sharedStateDomainCode('SHARED_STATE_DOMAIN_LOCATION_DISCOVERY'), 'locationDiscovery')
  assert.equal(sharedStateDomainCode('SHARED_STATE_DOMAIN_PEERS'), undefined)
  assert.equal(sharedStateDomainCode('unknown'), undefined)
})
