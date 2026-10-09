import assert from 'node:assert/strict'
import { test } from 'node:test'
import { startPolling } from '../src/renderer/src/lib/polling.ts'

const flush = () => new Promise((resolve) => setImmediate(resolve))
function scheduler() {
  const pending = new Set()
  return {
    pending,
    schedule(callback, delay) {
      assert.equal(delay, 5000)
      pending.add(callback)
      return () => pending.delete(callback)
    },
    tick() { const callback = [...pending][0]; pending.delete(callback); callback() },
  }
}

test('refreshes immediately and periodically without peer-list changes', async () => {
  const clock = scheduler()
  let calls = 0
  const stop = startPolling(async () => { calls++ }, 5000, clock.schedule)
  assert.equal(calls, 1)
  await flush()
  clock.tick()
  await flush()
  assert.equal(calls, 2)
  stop()
  assert.equal(clock.pending.size, 0)
})

test('does not overlap slow requests or schedule after disposal', async () => {
  const clock = scheduler()
  let resolve, calls = 0
  const stop = startPolling(() => { calls++; return new Promise((done) => { resolve = done }) }, 5000, clock.schedule)
  assert.equal(clock.pending.size, 0)
  assert.equal(calls, 1)
  stop()
  resolve()
  await flush()
  assert.equal(clock.pending.size, 0)
})

test('continues polling after a failed request', async () => {
  const clock = scheduler()
  let calls = 0
  const stop = startPolling(async () => { if (++calls === 1) throw new Error('offline') }, 5000, clock.schedule)
  await flush()
  clock.tick()
  await flush()
  assert.equal(calls, 2)
  stop()
})

test('a queued callback cannot run after disposal', async () => {
  const clock = scheduler()
  let calls = 0
  const stop = startPolling(async () => { calls++ }, 5000, clock.schedule)
  await flush()
  const callback = [...clock.pending][0]
  stop()
  callback()
  await flush()
  assert.equal(calls, 1)
})
