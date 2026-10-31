import assert from 'node:assert/strict'
import { readFileSync, statSync } from 'node:fs'
import test from 'node:test'
import { BufferGeometry, Float32BufferAttribute, PerspectiveCamera, Quaternion, Vector3 } from 'three'
import { createOrbitCameraTransition, interpolateOrbitPosition, unindexGlobeGeometry } from '../src/renderer/src/lib/globe-geometry.ts'
import { groupCoincidentPoints, peerMarkerStrength } from '../src/renderer/src/lib/globe-markers.ts'
import { autoRotateSpeed, clampDistance, clampMapCenter, decodeRegionLines, globeFitDistance, localMapThresholdReached, mapLocationAtOffset, mapUnitsPerPixel, mapZoomAnchorShift, mercatorLatitude, nearestMapPoint, nearbyRegionLines, projectionLineOpacity, regionSegments, zoomLimits, zoomState, zoomStep } from '../src/renderer/src/lib/globe-view.ts'

const radius = 100
const fit = 255
const limits = zoomLimits(radius, fit)

test('camera position and orientation stay aligned through southern and opposite-hemisphere transitions', () => {
  for (const start of [new Vector3(80, -220, -90), new Vector3(0, 0, -250)]) {
    const end = new Vector3(0, 0, 300)
    const camera = new PerspectiveCamera()
    camera.position.copy(start)
    camera.lookAt(0, 0, 0)
    const startOrientation = camera.quaternion.clone()
    camera.position.copy(end)
    camera.lookAt(0, 0, 0)
    const endOrientation = camera.quaternion.clone()
    const move = createOrbitCameraTransition(start, end, startOrientation, endOrientation)
    const position = new Vector3()
    const orientation = new Quaternion()
    for (let index = 0; index <= 100; index++) {
      move(index / 100, position, orientation)
      const forward = new Vector3(0, 0, -1).applyQuaternion(orientation)
      assert.ok(forward.dot(position.clone().negate().normalize()) > 0.999999)
    }
    assert.ok(position.distanceTo(end) < 1e-8)
    assert.ok(Math.abs(orientation.dot(endOrientation)) > 0.999999)
  }
})

test('camera transition goes around the globe even between opposite hemispheres', () => {
  const from = new Vector3(0, 0, -250)
  const to = new Vector3(0, 0, 300)
  const result = new Vector3()
  for (let i = 0; i <= 100; i++) {
    const progress = i / 100
    interpolateOrbitPosition(from, to, progress, result)
    assert.ok(Math.abs(result.length() - (250 + 50 * progress)) < 1e-8)
  }
  assert.ok(interpolateOrbitPosition(from, to, 0, result).distanceTo(from) < 1e-8)
  assert.ok(interpolateOrbitPosition(from, to, 1, result).distanceTo(to) < 1e-8)
})

test('peer marker strength grows gradually and remains bounded', () => {
  assert.equal(peerMarkerStrength(1), 1)
  assert.ok(peerMarkerStrength(2) > 1)
  assert.ok(peerMarkerStrength(4) > peerMarkerStrength(2))
  assert.equal(peerMarkerStrength(1000), 1.65)
})

test('only coincident peers are grouped, without changing their locations', () => {
  const groups = groupCoincidentPoints([{ id: 'a', lat: 30, lng: 35 }, { id: 'b', lat: 30, lng: 35 }, { id: 'c', lat: 30.001, lng: 35 }])
  assert.deepEqual(groups.map(({ id, lat, lng, members }) => ({ id, lat, lng, members })), [
    { id: 'a', lat: 30, lng: 35, members: ['a', 'b'] },
    { id: 'c', lat: 30.001, lng: 35, members: ['c'] },
  ])
})

test('map panning stays inside the projected world for narrow and oversized viewports', () => {
  const scale = 100
  const halfWidth = 30
  const halfHeight = 20
  const upper = clampMapCenter(1e6, 1e6, 30, scale, halfWidth, halfHeight)
  assert.ok(Math.abs(upper.x - (Math.PI * scale - halfWidth)) < 1e-9)
  assert.ok(Math.abs(upper.y - ((mercatorLatitude(85) - mercatorLatitude(30)) * scale - halfHeight)) < 1e-9)
  const lower = clampMapCenter(-1e6, -1e6, 30, scale, halfWidth, halfHeight)
  assert.ok(Math.abs(lower.x - (-Math.PI * scale + halfWidth)) < 1e-9)
  assert.ok(Math.abs(lower.y - ((mercatorLatitude(-85) - mercatorLatitude(30)) * scale + halfHeight)) < 1e-9)
  assert.deepEqual(clampMapCenter(5, 10, 0, scale, halfWidth, halfHeight), { x: 5, y: 10 })
  const oversized = clampMapCenter(1e6, 1e6, 30, scale, 10000, 10000)
  assert.equal(oversized.x, 0)
  assert.ok(Math.abs(oversized.y + mercatorLatitude(30) * scale) < 1e-9)
})

test('unindexing country outlines preserves the metadata used by globe layer updates', () => {
  const coordinates = [[[0, 0], [1, 0], [1, 1], [0, 0]]]
  const geometry = new BufferGeometry() as BufferGeometry & { parameters: unknown }
  geometry.setAttribute('position', new Float32BufferAttribute([0, 0, 0, 1, 0, 0, 1, 1, 0], 3))
  geometry.setIndex([0, 1, 1, 2])
  geometry.parameters = { geoJson: { coordinates }, resolution: 5 }
  const unindexed = unindexGlobeGeometry(geometry) as BufferGeometry & { parameters: typeof geometry.parameters }
  assert.equal(unindexed.index, null)
  assert.equal(unindexed.getAttribute('position').count, 4)
  assert.equal(unindexed.parameters, geometry.parameters)
  geometry.dispose()
  unindexed.dispose()
})

test('projection outlines remain visible and soften symmetrically through the transition', () => {
  assert.equal(projectionLineOpacity(0), 1)
  assert.equal(projectionLineOpacity(0.5), 0.7)
  assert.equal(projectionLineOpacity(1), 1)
  for (let index = 0; index <= 100; index++) {
    const progress = index / 100
    const opacity = projectionLineOpacity(progress)
    assert.ok(opacity >= 0.7 && opacity <= 1)
    assert.ok(Math.abs(opacity - projectionLineOpacity(1 - progress)) < 1e-12)
  }
})

test('full-pane globe fit preserves the relative map threshold across aspect ratios', () => {
  const squareFit = globeFitDistance(radius, 45)
  for (const aspect of [0.25, 0.5, 1, 2]) {
    const viewportFit = globeFitDistance(radius, 45, aspect)
    if (aspect < 1) assert.ok(viewportFit > squareFit)
    assert.equal(localMapThresholdReached(viewportFit * 0.9, viewportFit), false)
    assert.equal(localMapThresholdReached(viewportFit * 0.8, viewportFit), true)
  }
})

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

test('the local map activates at the first detailed-region zoom threshold', () => {
  assert.equal(localMapThresholdReached(fit * 0.851, fit), false)
  assert.equal(localMapThresholdReached(fit * 0.85, fit), false)
  assert.equal(localMapThresholdReached(fit * 0.849, fit), true)
  assert.equal(localMapThresholdReached(fit * 0.5, fit), true)
})

test('zooming the map keeps the location under the cursor fixed', () => {
  const oldDistance = 200
  const newDistance = 100
  const cursor = { x: 140, y: -80 }
  const oldUnits = mapUnitsPerPixel(oldDistance, 45, 800)
  const newUnits = mapUnitsPerPixel(newDistance, 45, 800)
  const shift = mapZoomAnchorShift(oldDistance, newDistance, 45, 800, cursor.x, cursor.y)
  assert.ok(Math.abs(shift.x + cursor.x * newUnits - cursor.x * oldUnits) < 1e-9)
  assert.ok(Math.abs(shift.y - cursor.y * newUnits + cursor.y * oldUnits) < 1e-9)
})

test('returning from a panned map points the globe at the panned location', () => {
  const scale = 100
  const center = mapLocationAtOffset({ lat: 0, lng: 170 }, scale * Math.PI / 9, scale * mercatorLatitude(30), scale)
  assert.ok(Math.abs(center.lat - 30) < 1e-9)
  assert.ok(Math.abs(center.lng + 170) < 1e-9)
  const farWest = mapLocationAtOffset({ lat: 0, lng: 0 }, -scale * 5 * Math.PI, 0, scale)
  assert.ok(Math.abs(farWest.lng + 180) < 1e-9)
})

test('map hover and click choose the closest point within the touch target', () => {
  const points = [{ id: 'node', x: 100, y: 100 }, { id: 'peer', x: 110, y: 100 }]
  assert.equal(nearestMapPoint(points, 109, 101)?.id, 'peer')
  assert.equal(nearestMapPoint(points, 85, 100)?.id, 'node')
  assert.equal(nearestMapPoint(points, 70, 100), undefined)
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
