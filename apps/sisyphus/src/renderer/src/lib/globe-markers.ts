export type PeerCountry = { peerId: string; countryCode?: string }
export type PeerLocation = { peerId: string; countryCode?: string; lat: number; lng: number }

export function locatePeers(peers: PeerCountry[], centroids: ReadonlyMap<string, { lat: number; lng: number }>): PeerLocation[] {
  return peers.flatMap((peer) => {
    const countryCode = peer.countryCode?.toUpperCase()
    const centroid = countryCode ? centroids.get(countryCode) : undefined
    return centroid ? [{ peerId: peer.peerId, countryCode: peer.countryCode, ...centroid }] : []
  }).sort((a, b) => a.peerId.localeCompare(b.peerId))
}

// The globe animates points when its points prop changes. Keep this key limited
// to fields that affect marker identity or position, not other peer snapshot data.
export function peerLocationsKey(locations: PeerLocation[]): string {
  return JSON.stringify(locations.map(({ peerId, countryCode }) => [peerId, countryCode]))
}
