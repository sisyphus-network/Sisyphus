import test from 'node:test'
import assert from 'node:assert/strict'
import { cityEstimateFromNodeInfo, cityTableAttribution, cityTableFromProtocol, cityTableStatus, formatEdition, formatSize, ownerPinned, placeName } from '../src/renderer/src/lib/city-table.ts'

test('a table is read as either client gives it', () => {
  // The desktop's gRPC client: 64-bit numbers as strings.
  const desktop = cityTableFromProtocol({ installed: true, edition: '2026-10', sizeBytes: '93123456', downloadedAtMs: '1792000000000', attribution: 'IP Geolocation by DB-IP', attributionUrl: 'https://db-ip.com' })
  assert.deepEqual(desktop, { installed: true, edition: '2026-10', sizeBytes: 93123456, downloadedAtMs: 1792000000000, attribution: 'IP Geolocation by DB-IP' })
  // The browser's, bigints, and the daemon's own snake_case names.
  assert.deepEqual(cityTableFromProtocol({ installed: true, edition: '2026-10', sizeBytes: 93123456n, downloadedAtMs: 1792000000000n }), desktop)
  assert.deepEqual(cityTableFromProtocol({ installed: true, edition: '2026-10', size_bytes: '93123456', downloaded_at_ms: 1792000000000 }), desktop)
})

test('a table that is not there has no edition, size or date, and still carries the credit', () => {
  const absent = { installed: false, edition: '', sizeBytes: 0, downloadedAtMs: 0, attribution: cityTableAttribution }
  assert.deepEqual(cityTableFromProtocol({ installed: false, sizeBytes: '0', downloadedAtMs: '0', attribution: 'IP Geolocation by DB-IP' }), absent)
  assert.deepEqual(cityTableFromProtocol({}), absent)
  assert.deepEqual(cityTableFromProtocol(null), absent)
  // Whatever the protobuf defaults say, an absent table is not described.
  assert.deepEqual(cityTableFromProtocol({ installed: false, edition: '2026-10', sizeBytes: '5' }), absent)
  assert.equal(cityTableStatus(absent, 'en'), null)
})

test('the status says the edition, size and day as the locale writes them', () => {
  const state = cityTableFromProtocol({ installed: true, edition: '2026-10', sizeBytes: '93123456', downloadedAtMs: String(Date.UTC(2026, 9, 21, 12)) })
  assert.deepEqual(cityTableStatus(state, 'en-US'), { edition: 'October 2026', size: '93 MB', date: 'Oct 21, 2026' })
  const french = cityTableStatus(state, 'fr')
  assert.equal(french.edition, 'octobre 2026')
  assert.match(french.size, /^93\s?Mo$/)
})

test('editions and sizes that are odd are shown plainly', () => {
  assert.equal(formatEdition('2026-13', 'en'), '2026-13')
  assert.equal(formatEdition('latest', 'en'), 'latest')
  assert.equal(formatEdition('', 'en'), '')
  assert.equal(formatSize(86_400_000, 'en'), '86 MB')
  assert.equal(formatSize(450_000, 'en'), '450 kB')
  assert.equal(formatSize(31, 'en'), '31 byte')
})

test('the city estimate is read from either client’s GetNodeInfo', () => {
  const louisville = { city: 'Louisville', region: 'Kentucky', countryCode: 'US', latitude: 38.2542, longitude: -85.7594, source: 'stun', attribution: 'IP Geolocation by DB-IP' }
  assert.deepEqual(cityEstimateFromNodeInfo({ peerId: 'x', cityEstimate: { ...louisville, attributionUrl: 'https://db-ip.com' } }), louisville)
  assert.deepEqual(cityEstimateFromNodeInfo({ city_estimate: { city: 'Louisville', region: 'Kentucky', country_code: 'us', latitude: '38.2542', longitude: -85.7594, source: 'stun' } }), louisville)
})

test('a node the table does not place has no estimate', () => {
  assert.equal(cityEstimateFromNodeInfo({ peerId: 'x' }), null)
  assert.equal(cityEstimateFromNodeInfo({ cityEstimate: null }), null)
  // The desktop's client fills an absent message with nothing; the
  // browser's leaves it out.
  assert.equal(cityEstimateFromNodeInfo({ cityEstimate: { city: '', region: '', countryCode: '', latitude: 0, longitude: 0 } }), null)
  assert.equal(cityEstimateFromNodeInfo(undefined), null)
})

test('a place is named from the city to the country, each part once', () => {
  assert.equal(placeName({ city: 'Louisville', region: 'Kentucky', countryCode: 'US' }), 'Louisville, Kentucky, US')
  assert.equal(placeName({ city: '', region: 'Kentucky', countryCode: 'US' }), 'Kentucky, US')
  assert.equal(placeName({ city: 'Singapore', region: 'Singapore', countryCode: 'SG' }), 'Singapore, SG')
  assert.equal(placeName({ city: '', region: '', countryCode: 'NZ' }), 'NZ')
})

test('an owner’s pin is seen in either client’s GetNodeInfo', () => {
  assert.equal(ownerPinned({ ownerLocation: { latitude: 37.27, longitude: -79.94, share: 'LOCATION_SHARE_CITY' } }), true)
  assert.equal(ownerPinned({ owner_location: { latitude: 37.27, longitude: -79.94, share: 3 } }), true)
  // The desktop's client gives an absent message as null; the browser's
  // leaves it out.
  assert.equal(ownerPinned({ ownerLocation: null }), false)
  assert.equal(ownerPinned({}), false)
  assert.equal(ownerPinned(undefined), false)
})
