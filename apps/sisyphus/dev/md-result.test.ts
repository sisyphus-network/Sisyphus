import test from 'node:test'
import assert from 'node:assert/strict'
import { formatNumber, parseMdResult } from '../src/renderer/src/lib/md-result.ts'

const json = (value: unknown) => new TextEncoder().encode(JSON.stringify(value))

test('an md result is summed up for the page', () => {
  const summary = parseMdResult(json({
    replicas: 2, atoms: 8867, picoseconds_each: 5, total_nanoseconds: 0.01,
    mean_potential_energy_kj_mol: -116000.5, potential_energy_stdev_kj_mol: 427.7,
    runs: [
      { index: 1, seed: 2, platform: 'CPU', ns_per_day: 4.16, potential_energy_kj_mol: -116228.3, final: 'c' },
      { index: 0, seed: 1, platform: 'CUDA', ns_per_day: 120.5, potential_energy_kj_mol: -115702.8 },
    ],
  }))
  assert.deepEqual(summary, {
    replicas: 2, atoms: 8867, picoseconds: 5, meanEnergy: -116000.5, spread: 427.7,
    runs: [
      { index: 0, seed: 1, platform: 'CUDA', nsPerDay: 120.5, energy: -115702.8 },
      { index: 1, seed: 2, platform: 'CPU', nsPerDay: 4.16, energy: -116228.3 },
    ],
  })
})

test('what is not an md result is shown raw instead', () => {
  for (const result of [undefined, new Uint8Array(), new TextEncoder().encode('not json'), json({ count: 9592 }), json({ runs: [] }), json(null)]) {
    assert.equal(parseMdResult(result), null)
  }
  // A replica missing a field still shows, with a dash.
  const summary = parseMdResult(json({ replicas: 1, mean_potential_energy_kj_mol: -1, runs: [{ seed: 'x' }] }))
  assert.ok(summary)
  assert.equal(summary.runs[0].index, 0)
  assert.equal(formatNumber(summary.runs[0].seed, 0), '—')
  assert.equal(summary.runs[0].platform, '')
})

test('numbers are written to the digits asked', () => {
  assert.equal(formatNumber(-116348.86089, 1, 'en-US'), '-116,348.9')
  assert.equal(formatNumber(4.495960210987987, 2, 'en-US'), '4.50')
  assert.equal(formatNumber(Number.NaN, 2), '—')
})
