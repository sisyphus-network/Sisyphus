import assert from 'node:assert/strict'
import { test } from 'node:test'
import { sharedStateDomainCode } from '../src/renderer/src/lib/shared-state-domain.ts'

test('shared-state domains recognize numeric and proto-loader model state values', () => {
  assert.equal(sharedStateDomainCode(6), 'modelProfiles')
  assert.equal(sharedStateDomainCode('SHARED_STATE_DOMAIN_MODEL_PROFILES'), 'modelProfiles')
  assert.equal(sharedStateDomainCode(7), 'modelLending')
  assert.equal(sharedStateDomainCode('SHARED_STATE_DOMAIN_MODEL_LENDING'), 'modelLending')
  assert.equal(sharedStateDomainCode('SHARED_STATE_DOMAIN_JOINED_POOL'), 'joinedPool')
  assert.equal(sharedStateDomainCode('unknown'), undefined)
})
