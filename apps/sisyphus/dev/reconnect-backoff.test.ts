import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createReconnectBackoff } from '../src/main/reconnect-backoff.ts'

test('reconnect delay doubles to the cap and stays bounded', () => {
  const retry = createReconnectBackoff(1_000, 15_000)
  assert.deepEqual(Array.from({ length: 8 }, () => retry.next()), [1_000, 2_000, 4_000, 8_000, 15_000, 15_000, 15_000, 15_000])
})

test('a successful connection resets the retry delay', () => {
  const retry = createReconnectBackoff(500, 8_000)
  assert.deepEqual([retry.next(), retry.next(), retry.next()], [500, 1_000, 2_000])
  retry.reset()
  assert.equal(retry.next(), 500)
})

test('invalid retry bounds are rejected', () => {
  assert.throws(() => createReconnectBackoff(0, 1), RangeError)
  assert.throws(() => createReconnectBackoff(2, 1), RangeError)
})
