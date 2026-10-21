import test from 'node:test'
import assert from 'node:assert/strict'
import { offeredWorkloads } from '../src/renderer/src/lib/worker-workloads.ts'

test('a worker that serves no model is not said to chat', () => {
  const workloads = ['chat', 'container', 'embed', 'md', 'primes', 'prompts', 'transcode', 'wordcount']
  assert.deepEqual(offeredWorkloads({ workloads, models: [] }), ['container', 'md', 'primes', 'transcode', 'wordcount'])
  assert.deepEqual(offeredWorkloads({ workloads }), ['container', 'md', 'primes', 'transcode', 'wordcount'])
})

test('a worker that serves a model keeps its model workloads', () => {
  const workloads = ['chat', 'embed', 'primes', 'prompts', 'wordcount']
  assert.deepEqual(offeredWorkloads({ workloads, models: ['llama3.1:8b'] }), workloads)
  assert.deepEqual(offeredWorkloads({}), [])
})
