import assert from 'node:assert/strict'
import { test } from 'node:test'
import { chatRevisionChange } from '../src/renderer/src/lib/chat-summary-revisions.ts'

test('other chats and unchanged reconnect snapshots do not reload the selected chat', () => {
  const previous = new Map([['a', '4'], ['b', '2']])
  assert.equal(chatRevisionChange(previous, [{ chatId: 'a', revision: 4 }, { chatId: 'b', revision: 3 }], 'a').action, 'none')
  assert.equal(chatRevisionChange(previous, [{ chatId: 'a', revision: '4' }, { chatId: 'b', revision: 2 }], 'a').action, 'none')
  assert.equal(chatRevisionChange(new Map(), [{ chatId: 'a', revision: 4 }], 'a').action, 'none')
})

test('selected revision changes refresh it and deletions close it', () => {
  const previous = new Map([['a', '4']])
  assert.equal(chatRevisionChange(previous, [{ chatId: 'a', revision: 5n }], 'a').action, 'refresh')
  assert.equal(chatRevisionChange(previous, [], 'a').action, 'close')
  assert.equal(chatRevisionChange(previous, [], '').action, 'none')
})
