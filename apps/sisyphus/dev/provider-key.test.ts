import test from 'node:test'
import assert from 'node:assert/strict'
import { missingProviderKey } from '../src/renderer/src/lib/provider-key.ts'

test('required providers need a nonblank key or a matching saved key', () => {
  for (const required of [1, 'SUPPORT_YES']) {
    assert.equal(missingProviderKey(required, '', false), true)
    assert.equal(missingProviderKey(required, '  ', false), true)
    assert.equal(missingProviderKey(required, 'secret', false), false)
    assert.equal(missingProviderKey(required, '', true), false)
  }
})
test('optional, unsupported and unknown key requirements do not block', () => {
  for (const requirement of [0, 2, 'SUPPORT_NO', 'SUPPORT_UNKNOWN', undefined]) {
    assert.equal(missingProviderKey(requirement, '', false), false)
  }
})
test('a saved key for another service cannot satisfy the requirement', () => {
  assert.equal(missingProviderKey('SUPPORT_YES', '', false), true)
})
