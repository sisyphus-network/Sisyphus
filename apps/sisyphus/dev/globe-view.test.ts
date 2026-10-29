import assert from 'node:assert/strict'
import { readFileSync, statSync } from 'node:fs'
import test from 'node:test'
import { autoRotateSpeed, clampDistance, decodeRegionLines, localMapProgress, nearbyRegionLines, regionSegments, zoomLimits, zoomState, zoomStep } from '../src/renderer/src/lib/globe-view.ts'

const radius = 100
const fit = 255
const limits = zoomLimits(radius, fit)

test('the globe zooms between a quarter of its radius above the surface and half its fitted size', () => {
  assert.deepEqual(limits, { min: 125, max: 510, fit: 255 })
  // The default view, 2.5 radii above the surface, is always within reach.
  assert.equal(zoomLimits(radius, 120).max, 350)
  assert.equal(clampDistance(10, limits), 125)
  assert.equal(clampDistance(9999, limits), 510)
  assert.equal(clampDistance(300, limits), 300)
})

test('each press of a zoom button moves a step, and stops at the limits', () => {
  let distance = 350
  const seen = [distance]
  while (zoomState(distance, limits).canZoomIn) {
    distance = zoomStep(distance, 'in', radius, limits)
    seen.push(distance)
    assert.ok(seen.length < 30, 'zooming in never reaches its limit')
  }
  assert.equal(distance, limits.min)
  for (let i = 1; i < seen.length; i++) assert.ok(seen[i] < seen[i - 1])
  // Out again retraces the way, as far as the far limit.
  const back = zoomStep(zoomStep(200, 'in', radius, limits), 'out', radius, limits)
  assert.ok(Math.abs(back - 200) < 1e-9)
  while (zoomState(distance, limits).canZoomOut) distance = zoomStep(distance, 'out', radius, limits)
  assert.equal(distance, limits.max)
  assert.equal(zoomState(distance, limits).canZoomIn, true)
})

test('region lines are wanted only once the globe is brought in past its fit', () => {
  assert.equal(zoomState(350, limits).regions, false)
  assert.equal(zoomState(fit, limits).regions, false)
  assert.equal(zoomState(fit * 0.8, limits).regions, true)
  assert.equal(zoomState(fit, limits).percent, 100)
  assert.equal(zoomState(fit / 2, limits).percent, 200)
})

test('the local map unfolds smoothly between the globe and a close regional view', () => {
  assert.equal(localMapProgress(fit, fit), 0)
  assert.equal(localMapProgress(fit * 0.88, fit), 0)
  assert.ok(localMapProgress(fit * 0.8, fit) > 0)
  assert.equal(localMapProgress(fit * 0.64, fit), 1)
  assert.equal(localMapProgress(fit * 0.5, fit), 1)
  const samples = [0.88, 0.82, 0.76, 0.70, 0.64].map((share) => localMapProgress(fit * share, fit))
  assert.ok(samples.every((value, index) => index === 0 || value >= samples[index - 1]))
})

test('the local map keeps nearby region lines and simplifies them before drawing', () => {
  const local = Array.from({ length: 10 }, (_, index): [number, number] => [31, 35 + index * 0.01])
  const remote: Array<[number, number]> = [[40, -100], [40.1, -100.1], [40.2, -100.2]]
  const result = nearbyRegionLines([local, remote], { lat: 31, lng: 35 })
  assert.equal(result.length, 1)
  assert.deepEqual(result[0], [local[0], local[3], local[6], local[9]])
})

test('the globe turns more slowly as it is brought in', () => {
  assert.equal(autoRotateSpeed(fit, radius, limits), 0.45)
  assert.equal(autoRotateSpeed(400, radius, limits), 0.45)
  assert.ok(autoRotateSpeed(150, radius, limits) < 0.2)
  assert.ok(autoRotateSpeed(radius, radius, limits) > 0, 'close in, it stops turning altogether')
})

test('region lines are decoded from hundredths of a degree, each point from the one before', () => {
  assert.deepEqual(decodeRegionLines([[1000, 2000, 50, -25, -10, 0]]), [[[20, 10], [19.75, 10.5], [19.75, 10.4]]])
  // Nonsense is left out, not drawn.
  assert.deepEqual(decodeRegionLines([[1, 2], [1, 2, 3], 'x', [0, 9100, 0, 0], [0, 0, 'a', 1]]), [])
  for (const data of [undefined, null, {}, 'x']) assert.deepEqual(decodeRegionLines(data), [])
})

test('region lines become one set of segments, each pair of neighbouring points once', () => {
  const lines: [number, number][][] = [[[0, 0], [0, 1], [1, 1]], [[5, 5], [6, 6]]]
  const positions = regionSegments(lines, (lat, lng) => ({ x: lng, y: lat, z: 0 }))
  assert.deepEqual([...positions], [0, 0, 0, 1, 0, 0, 1, 0, 0, 1, 1, 0, 5, 5, 0, 6, 6, 0])
})

test('the shipped region lines are small, valid and cover the world', () => {
  const file = new URL('../src/renderer/src/data/admin1-lines.json', import.meta.url)
  assert.ok(statSync(file).size < 1.5 * 1024 * 1024, 'the region lines grew past 1.5 MB')
  const raw = JSON.parse(readFileSync(file, 'utf8'))
  const lines = decodeRegionLines(raw)
  assert.equal(lines.length, raw.length, 'some shipped lines do not decode')
  const points = lines.flat()
  // States of the United States, provinces of Argentina, regions of Russia
  // and states of Australia, at least.
  for (const [name, [south, north, west, east]] of Object.entries({ US: [30, 45, -110, -80], Argentina: [-50, -25, -70, -58], Russia: [50, 65, 40, 120], Australia: [-38, -15, 120, 150] })) {
    assert.ok(points.some(([lat, lng]) => lat > south && lat < north && lng > west && lng < east), `no region lines in ${name}`)
  }
})
