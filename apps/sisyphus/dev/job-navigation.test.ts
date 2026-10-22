import assert from 'node:assert/strict'
import { test } from 'node:test'
import { getJobHref, getSelectedJobId, includeLinkedJob } from '../src/renderer/src/lib/job-navigation.ts'

test('job links round-trip IDs without creating extra parameters or fragments', () => {
  for (const id of ['abc-123', 'a&tab=files#fragment', 'job with spaces', 'עבודה/1', '100%']) {
    const url = new URL(getJobHref(id), 'https://example.invalid')
    assert.equal(url.pathname, '/operations')
    assert.equal(url.hash, '')
    assert.deepEqual([...url.searchParams.keys()], ['job'])
    assert.equal(getSelectedJobId(url.search), id)
  }
})

test('an ordinary operations route has no selected job', () => {
  assert.equal(getSelectedJobId(''), '')
  assert.equal(getSelectedJobId('?tab=files'), '')
})

test('a linked job absent from the list is included once', () => {
  const list = [{ jobId: 'other' }], job = { jobId: 'selected' }
  assert.deepEqual(includeLinkedJob(list, job, 'selected'), [job, ...list])
  assert.equal(list.length, 1)
})

test('live list data takes precedence over a stale direct fetch', () => {
  const jobs = [{ jobId: 'selected', state: 'done' }]
  assert.equal(includeLinkedJob(jobs, { jobId: 'selected', state: 'running' }, 'selected'), jobs)
})

test('missing responses and responses for previous links are ignored', () => {
  const jobs = [{ jobId: 'other' }]
  assert.equal(includeLinkedJob(jobs, undefined, 'selected'), jobs)
  assert.equal(includeLinkedJob(jobs, { jobId: 'previous' }, 'selected'), jobs)
  assert.equal(includeLinkedJob(jobs, { jobId: 'previous' }, ''), jobs)
})
