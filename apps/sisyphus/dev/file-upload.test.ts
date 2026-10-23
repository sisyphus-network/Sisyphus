import test from 'node:test'
import assert from 'node:assert/strict'
import { firstUploadMessage } from '../src/main/file-upload.ts'

test('empty and nonempty chat uploads carry the same private draft ownership', () => {
  for (const data of [Buffer.alloc(0), Buffer.from('attachment')]) {
    const message = firstUploadMessage({ name: 'draft.txt', private: true, chatAttachment: true }, data)
    assert.equal(message.name, 'draft.txt')
    assert.equal(message.private, true)
    assert.equal(message.chatAttachment, true)
    assert.equal(message.data, data)
  }
})

test('manual uploads retain their permanent ownership regardless of size', () => {
  for (const data of [Buffer.alloc(0), Buffer.from('manual')]) {
    for (const privateFile of [false, true]) {
      assert.equal(firstUploadMessage({ name: 'manual.txt', private: privateFile }, data).chatAttachment, false)
    }
  }
})
