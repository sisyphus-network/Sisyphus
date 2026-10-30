import assert from 'node:assert/strict'
import { test } from 'node:test'
import { accessDraftIsDirty } from '../src/renderer/src/lib/access-draft.ts'

test('reverting an access draft allows a subsequent remote update to replace it', () => {
  const saved = { enabled: false, addresses: ['127.0.0.1:50052'], publicUrl: '' }
  const draft = { ...saved, validPort: true }
  assert.equal(accessDraftIsDirty({ ...draft, enabled: true }, saved), true)
  assert.equal(accessDraftIsDirty(draft, saved), false)
  const remote = { enabled: true, addresses: ['192.168.1.2:50052'], publicUrl: 'https://example.test' }
  const next = accessDraftIsDirty(draft, saved) ? draft : { ...remote, validPort: true }
  assert.equal(accessDraftIsDirty(next, remote), false)
  assert.deepEqual(next.addresses, remote.addresses)
  assert.equal(next.publicUrl, remote.publicUrl)
})

test('changed addresses, URLs and invalid ports remain dirty', () => {
  const saved = { enabled: false, addresses: ['127.0.0.1:50052'], publicUrl: '' }
  const draft = { ...saved, validPort: true }
  assert.equal(accessDraftIsDirty({ ...draft, addresses: ['127.0.0.1:50053'] }, saved), true)
  assert.equal(accessDraftIsDirty({ ...draft, publicUrl: 'https://example.test' }, saved), true)
  assert.equal(accessDraftIsDirty({ ...draft, validPort: false }, saved), true)
})
