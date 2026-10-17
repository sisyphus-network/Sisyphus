import assert from 'node:assert/strict'
import test from 'node:test'
import { locatePeers, peerLocationsKey } from '../src/renderer/src/lib/globe-markers.ts'

const centroids = new Map([
  ['US', { lat: 37.0902, lng: -95.7129 }],
  ['IL', { lat: 31.0461, lng: 34.8516 }],
])

test('unrelated peer snapshot changes keep the globe marker key stable', () => {
  const first = locatePeers([
    { peerId: 'peer-b', countryCode: 'us', connectionState: 1, knownAddresses: [] },
    { peerId: 'peer-a', countryCode: 'IL', connectionState: 2, knownAddresses: ['/ip4/192.0.2.1'] },
  ], centroids)
  const next = locatePeers([
    { peerId: 'peer-a', countryCode: 'IL', connectionState: 0, knownAddresses: [] },
    { peerId: 'peer-b', countryCode: 'US', connectionState: 2, knownAddresses: ['/ip4/198.51.100.1'] },
  ], centroids)

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
