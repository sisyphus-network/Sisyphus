import assert from 'node:assert/strict'
import { test } from 'node:test'
import { needsPlannerSetup } from '../src/renderer/src/lib/grpc-status.ts'

test('FAILED_PRECONDITION offers planner setup independently of error language', () => {
  assert.equal(needsPlannerSetup(9), true)
})

test('other failures and missing statuses do not masquerade as model setup', () => {
  for (const code of [undefined, 0, 1, 3, 7, 12, 13, 14, 16]) assert.equal(needsPlannerSetup(code), false)
})
