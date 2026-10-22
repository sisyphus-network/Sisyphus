import assert from 'node:assert/strict'
import { test } from 'node:test'
import { countSettingChanges, pendingChangeCount, savePendingSettings } from '../src/renderer/src/lib/settings-save.ts'

test('dirty counts settings, returns to zero when reverted, and does not depend on property order', () => {
  const saved = { enabled: false, address: '127.0.0.1', port: 7700 }
  assert.equal(countSettingChanges({ ...saved, enabled: true, port: 7800 }, saved), 2)
  assert.equal(countSettingChanges({ port: 7700, enabled: false, address: '127.0.0.1' }, saved), 0)
  assert.equal(countSettingChanges({ ...saved, port: NaN }, saved), 1)
})

test('dock counts individual settings and includes invalid drafts until corrected', () => {
  const entries = new Map([
    ['network', { changeCount: 2, disabled: false, getSave: () => async () => {} }],
    ['location', { changeCount: 1, disabled: true, getSave: () => async () => {} }],
  ])
  assert.equal(pendingChangeCount(entries.values()), 3)
  entries.delete('network')
  assert.equal(pendingChangeCount(entries.values()), 1)
  entries.delete('location')
  assert.equal(pendingChangeCount(entries.values()), 0)
})

test('an invalid section prevents all writes', async () => {
  let writes = 0
  const entry = disabled => ({ disabled, getSave: () => async () => { writes++ } })
  assert.deepEqual(await savePendingSettings([entry(false), entry(true)]), [])
  assert.equal(writes, 0)
})

test('settings writes are serialized and a failed section does not skip the others', async () => {
  const events = []
  let release
  const first = new Promise(resolve => { release = resolve })
  const result = savePendingSettings([
    { disabled: false, getSave: () => async () => { events.push('first-start'); await first; events.push('first-end') } },
    { disabled: false, getSave: () => async () => { events.push('second'); throw new Error('offline') } },
    { disabled: false, getSave: () => async () => { events.push('third') } },
  ])
  assert.deepEqual(events, ['first-start'])
  release()
  assert.deepEqual((await result).map(item => item.status), ['fulfilled', 'rejected', 'fulfilled'])
  assert.deepEqual(events, ['first-start', 'first-end', 'second', 'third'])
})

test('a save captures the current drafts before previous writes update callbacks', async () => {
  const calls = []
  let second = async () => { calls.push('draft') }
  await savePendingSettings([
    { disabled: false, getSave: () => async () => { second = async () => { calls.push('replaced') } } },
    { disabled: false, getSave: () => second },
  ])
  assert.deepEqual(calls, ['draft'])
})
