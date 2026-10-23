import test from 'node:test'
import assert from 'node:assert/strict'
import { formatCoordinates, locationFromProtocol, parseCoordinates, shareFromProtocol, shareToProtocol } from '../src/renderer/src/lib/node-location.ts'

test('coordinates are read as maps write them', () => {
  assert.deepEqual(parseCoordinates('37.2710, -79.9414'), { latitude: 37.271, longitude: -79.9414 })
  assert.deepEqual(parseCoordinates(' 37.2710 -79.9414 '), { latitude: 37.271, longitude: -79.9414 })
  assert.deepEqual(parseCoordinates('37.2710° N, 79.9414° W'), { latitude: 37.271, longitude: -79.9414 })
  assert.deepEqual(parseCoordinates('33.87 s; 151.21 e'), { latitude: -33.87, longitude: 151.21 })
  assert.deepEqual(parseCoordinates('-90,180'), { latitude: -90, longitude: 180 })
})

test('what is not a place on the Earth is refused', () => {
  for (const text of ['', 'Roanoke', '37.27', '91, 0', '0, 181', '37 E, 79 N', '-37 N, 79 W', '1,2,3', 'NaN, 0']) {
    assert.equal(parseCoordinates(text), null, text)
  }
})

test('coordinates are written back to about a metre', () => {
  assert.equal(formatCoordinates(37.271012345, -79.94140001), '37.27101, -79.9414')
  assert.equal(parseCoordinates(formatCoordinates(-33.87, 151.21))?.latitude, -33.87)
})

test('shares read as either client gives them, and go back as numbers', () => {
  assert.equal(shareFromProtocol('LOCATION_SHARE_ABOUT_1KM'), 'about-1km')
  assert.equal(shareFromProtocol('LOCATION_SHARE_CITY'), 'city')
  assert.equal(shareFromProtocol(5), 'exact')
  assert.equal(shareFromProtocol(1), 'nothing')
  // Unknown or unspecified is the privacy default.
  assert.equal(shareFromProtocol('LOCATION_SHARE_UNSPECIFIED'), 'country')
  assert.equal(shareFromProtocol(0), 'country')
  assert.equal(shareFromProtocol(undefined), 'country')
  assert.deepEqual((['nothing', 'country', 'city', 'about-1km', 'exact'] as const).map(shareToProtocol), [1, 2, 3, 4, 5])
})

test('a saved location is read whole or not at all', () => {
  assert.deepEqual(locationFromProtocol({ latitude: 37.27, longitude: -79.94, share: 'LOCATION_SHARE_EXACT' }), { latitude: 37.27, longitude: -79.94, share: 'exact' })
  // Proto3 leaves zeros out: the Gulf of Guinea is a place too.
  assert.deepEqual(locationFromProtocol({}), { latitude: 0, longitude: 0, share: 'country' })
  assert.equal(locationFromProtocol(undefined), null)
  assert.equal(locationFromProtocol({ latitude: 'x', longitude: 0 }), null)
  assert.equal(locationFromProtocol({ latitude: 95, longitude: 0 }), null)
})
