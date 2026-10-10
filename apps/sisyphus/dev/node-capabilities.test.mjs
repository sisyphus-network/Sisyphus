import test from 'node:test'
import assert from 'node:assert/strict'
import { supportsChatFileReferences } from '../src/renderer/src/lib/node-capabilities.ts'

test('chat attachments require explicit node support, not an optimistic default', () => {
  for (const capabilities of [undefined, null, [], 'chat-file-references-v1', ['chat-file-references-v1'], ['chat-attachment-retention-v1'], ['unrelated']]) {
    assert.equal(supportsChatFileReferences({ capabilities }), false)
  }
  assert.equal(supportsChatFileReferences({ capabilities: ['unrelated', 'chat-file-references-v1', 'chat-attachment-retention-v1'] }), true)
})
