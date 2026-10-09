import test from 'node:test'
import assert from 'node:assert/strict'
import { appendJobEvent, eventSequence } from '../src/renderer/src/lib/job-events.ts'

const event = (seq) => ({ seq, kind: 'log', text: 'message', workerName: '' })
test('timeline ignores duplicate and older replayed events', () => {
  const events = [event('3')]
  assert.equal(appendJobEvent(events, event('3')), events)
  assert.equal(appendJobEvent(events, event('2')), events)
  assert.equal(appendJobEvent(events, event('4')).length, 2)
})
test('uint64 event sequence remains exact beyond safe integer range', () => {
  const events = [event('9007199254740992')]
  assert.equal(appendJobEvent(events, event('9007199254740993')).length, 2)
  assert.equal(eventSequence(event('18446744073709551615')), 18446744073709551615n)
})
test('timeline retains the newest bounded events', () => {
  let events = []
  for (let seq = 1; seq <= 600; seq++) events = appendJobEvent(events, event(String(seq)))
  assert.equal(events.length, 500)
  assert.equal(events[0].seq, '101')
  assert.equal(events.at(-1).seq, '600')
})
