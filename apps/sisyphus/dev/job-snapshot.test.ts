import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createJobSnapshotGate } from '../src/renderer/src/lib/job-snapshot.ts'

test('a late list snapshot cannot overwrite a newer streamed job update', () => {
  const gate = createJobSnapshotGate()
  const list = gate.begin()

  gate.changed() // WatchJobs delivered a newer complete snapshot.

  assert.equal(gate.accept(list), false)
})

test('only the newest concurrent list request can refresh jobs', () => {
  const gate = createJobSnapshotGate()
  const first = gate.begin()
  const second = gate.begin()

  assert.equal(gate.accept(first), false)
  assert.equal(gate.accept(second), true)
})

test('a refresh started after a stream update may apply its fresh snapshot once', () => {
  const gate = createJobSnapshotGate()
  const stale = gate.begin()
  gate.changed()
  const fresh = gate.begin()

  assert.equal(gate.accept(stale), false)
  assert.equal(gate.accept(fresh), true)
  assert.equal(gate.accept(fresh), false)
})
