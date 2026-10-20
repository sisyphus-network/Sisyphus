import test from 'node:test'
import assert from 'node:assert/strict'
import { restartImpact } from '../src/shared/restart-impact.ts'

test('a restart counts the unfinished jobs and the other machines working for this node', () => {
  const jobs = [{ state: 'JOB_STATE_RUNNING' }, { state: 'JOB_STATE_PENDING' }, { state: 2 }, { state: 'JOB_STATE_SUCCEEDED' }, { state: 4 }, {}]
  const workers = [{ peerId: 'self' }, { peerId: 'south' }, { peerId: 'north' }, {}]
  assert.deepEqual(restartImpact(jobs, workers, 'self'), { jobs: 3, workers: 2 })
  assert.deepEqual(restartImpact([], [{ peerId: 'self' }], 'self'), { jobs: 0, workers: 0 })
  assert.deepEqual(restartImpact([], [{ peerId: 'self' }], undefined), { jobs: 0, workers: 1 })
})
