import test from 'node:test'
import assert from 'node:assert/strict'
import { finished, jobProgress, stateName } from '../src/renderer/src/lib/job-progress.ts'

test('states read as the node API gives them, as numbers or names', () => {
  assert.equal(stateName(2), 'running')
  assert.equal(stateName('JOB_STATE_SUCCEEDED'), 'succeeded')
  assert.equal(stateName(9), 'unspecified')
})

test('a job no worker has started yet is waiting', () => {
  const progress = jobProgress({ state: 'JOB_STATE_PENDING', tasks: [{ state: 1 }, { state: 1 }] })
  assert.deepEqual(progress, { phase: 'waiting', total: 2, done: 0, running: 0, retried: 0, workers: [] })
  assert.equal(finished(progress), false)
})

test('a running job counts its tasks by worker, busiest first', () => {
  const progress = jobProgress({
    state: 2,
    tasks: [
      { state: 3, workerName: 'laptop', attempt: 1 },
      { state: 3, workerName: 'laptop', attempt: 1 },
      { state: 2, workerName: 'r620', attempt: '2' },
      { state: 2, workerName: 'laptop', attempt: 1 },
      { state: 1, workerName: 'r620', attempt: 2 },
      { state: 1 },
    ],
  })
  assert.equal(progress.phase, 'running')
  assert.deepEqual([progress.total, progress.done, progress.running, progress.retried], [6, 2, 2, 2])
  // A task waiting again after its worker was lost is not counted for it.
  assert.deepEqual(progress.workers, [{ name: 'laptop', running: 1, done: 2 }, { name: 'r620', running: 1, done: 0 }])
})

test('a job whose tasks have started is running even before the coordinator says so', () => {
  assert.equal(jobProgress({ state: 1, tasks: [{ state: 3, workerName: 'rig' }] }).phase, 'running')
})

test('workers with as much work are listed by name', () => {
  const progress = jobProgress({ state: 2, tasks: [{ state: 2, workerName: 'b' }, { state: 2, workerName: ' a ' }] })
  assert.deepEqual(progress.workers.map((worker) => worker.name), ['a', 'b'])
})

test('a job that is over is finished, however it ended', () => {
  for (const state of ['JOB_STATE_SUCCEEDED', 4, 5]) assert.equal(finished(jobProgress({ state })), true, String(state))
  assert.deepEqual(jobProgress({ state: 3 }), { phase: 'succeeded', total: 0, done: 0, running: 0, retried: 0, workers: [] })
})
