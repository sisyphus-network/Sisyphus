import assert from 'node:assert/strict'
import test from 'node:test'
import { locatePeers, peerLocationsKey, sharedPlaces } from '../src/renderer/src/lib/globe-markers.ts'

const centroids = new Map([
  ['US', { lat: 37.0902, lng: -95.7129 }],
  ['IL', { lat: 31.0461, lng: 34.8516 }],
])

test('unrelated peer snapshot changes keep the globe marker key stable', () => {
  // Snapshots carry more than the globe reads; what else changes must not
  // move a marker.
  const before = [
    { peerId: 'peer-b', countryCode: 'us', connectionState: 1, knownAddresses: [] },
    { peerId: 'peer-a', countryCode: 'IL', connectionState: 2, knownAddresses: ['/ip4/192.0.2.1'] },
  ]
  const after = [
    { peerId: 'peer-a', countryCode: 'IL', connectionState: 0, knownAddresses: [] },
    { peerId: 'peer-b', countryCode: 'US', connectionState: 2, knownAddresses: ['/ip4/198.51.100.1'] },
  ]
  const first = locatePeers(before, centroids)
  const next = locatePeers(after, centroids)

  assert.deepEqual(next, first)
  assert.equal(peerLocationsKey(next), peerLocationsKey(first))
})

test('a peer moving to a different known country changes the globe marker key', () => {
  const before = locatePeers([{ peerId: 'peer-a', countryCode: 'US' }], centroids)
  const after = locatePeers([{ peerId: 'peer-a', countryCode: 'IL' }], centroids)

  assert.notEqual(peerLocationsKey(after), peerLocationsKey(before))
})

test('peers without a known country do not create globe markers', () => {
  const locations = locatePeers([
    { peerId: 'peer-a', countryCode: 'ZZ' },
    { peerId: 'peer-b' },
  ], centroids)

  assert.deepEqual(locations, [])
  assert.equal(peerLocationsKey(locations), '[]')
})

test('a member that shares a place is placed there, not in the middle of its country', () => {
  const shared = sharedPlaces([
    { peerId: 'peer-a', sharedLocation: { latitude: 37.3, longitude: -79.9 } },
    { peerId: 'peer-b' },
    { peerId: 'peer-c', sharedLocation: { latitude: 48.9, longitude: 2.4 } },
  ])
  const locations = locatePeers([{ peerId: 'peer-a', countryCode: 'US' }, { peerId: 'peer-b', countryCode: 'IL' }], centroids, shared)
  assert.deepEqual(locations, [
    { peerId: 'peer-a', countryCode: 'US', lat: 37.3, lng: -79.9, shared: true },
    { peerId: 'peer-b', countryCode: 'IL', lat: 31.0461, lng: 34.8516 },
    // Known only as a worker, by the place its owner shares.
    { peerId: 'peer-c', countryCode: undefined, lat: 48.9, lng: 2.4, shared: true },
  ])
  // Moving the shared place moves the marker.
  const moved = locatePeers([{ peerId: 'peer-a', countryCode: 'US' }], centroids, sharedPlaces([{ peerId: 'peer-a', sharedLocation: { latitude: 37.4, longitude: -79.9 } }]))
  assert.notEqual(peerLocationsKey(moved), peerLocationsKey(locatePeers([{ peerId: 'peer-a', countryCode: 'US' }], centroids, shared)))
})

test('shared places are read from either client, and nonsense is left out', () => {
  // Proto3 leaves out zeros: a place on the equator at Greenwich is still a place.
  assert.deepEqual([...sharedPlaces([{ peerId: 'p', sharedLocation: {} }])], [['p', { lat: 0, lng: 0 }]])
  assert.deepEqual([...sharedPlaces([{ peerId: 'p', sharedLocation: { latitude: '51.5', longitude: '-0.1' } }])], [['p', { lat: 51.5, lng: -0.1 }]])
  for (const workers of [undefined, null, {}, [null], [{ sharedLocation: { latitude: 1, longitude: 1 } }], [{ peerId: 'p', sharedLocation: { latitude: 91, longitude: 0 } }], [{ peerId: 'p', sharedLocation: { latitude: 'x', longitude: 0 } }]]) {
    assert.equal(sharedPlaces(workers).size, 0, JSON.stringify(workers))
  }
})
