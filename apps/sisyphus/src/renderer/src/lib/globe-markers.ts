export type PeerCountry = { peerId: string; countryCode?: string }
export type Place = { lat: number; lng: number }
export type PeerLocation = { peerId: string; countryCode?: string; lat: number; lng: number; shared?: boolean }

export function peerMarkerStrength(count: number): number {
  return 1 + Math.min(0.65, Math.log2(Math.max(1, count)) * 0.18)
}

// Only group truly coincident locations: nearby shared pins must stay accurate.
export function groupCoincidentPoints<T extends { id: string; lat: number; lng: number }>(points: readonly T[]): Array<T & { members: string[] }> {
  const groups = new Map<string, T & { members: string[] }>()
  for (const point of points) {
    const key = `${point.lat === 0 ? 0 : point.lat},${point.lng === 0 ? 0 : point.lng}`
    const group = groups.get(key)
    if (group) group.members.push(point.id)
    else groups.set(key, { ...point, members: [point.id] })
  }
  return [...groups.values()]
}

// locatePeers places each peer on the globe: where its owner lets the pool
// see it is, if it shares a place, or else in the middle of its country.
// A pool member that shares a place is placed even when this node knows
// nothing else of it.
export function locatePeers(peers: PeerCountry[], centroids: ReadonlyMap<string, Place>, shared: ReadonlyMap<string, Place> = new Map()): PeerLocation[] {
  const countries = new Map(peers.map((peer) => [peer.peerId, peer.countryCode?.toUpperCase()]))
  const ids = new Set([...countries.keys(), ...shared.keys()])
  return [...ids].flatMap((peerId) => {
    const countryCode = countries.get(peerId) || undefined
    const place = shared.get(peerId)
    if (place) return [{ peerId, countryCode, lat: place.lat, lng: place.lng, shared: true }]
    const centroid = countryCode ? centroids.get(countryCode) : undefined
    return centroid ? [{ peerId, countryCode, ...centroid }] : []
  }).sort((a, b) => a.peerId.localeCompare(b.peerId))
}

// sharedPlaces reads, from the node API's list of workers, the places their
// owners share, by peer ID. Workers that share none are left out.
export function sharedPlaces(workers: unknown): Map<string, Place> {
  const places = new Map<string, Place>()
  if (!Array.isArray(workers)) return places
  for (const worker of workers) {
    const { peerId, sharedLocation } = (worker ?? {}) as { peerId?: unknown; sharedLocation?: { latitude?: unknown; longitude?: unknown } | null }
    if (typeof peerId !== 'string' || !peerId || !sharedLocation) continue
    const lat = Number(sharedLocation.latitude ?? 0)
    const lng = Number(sharedLocation.longitude ?? 0)
    if (Number.isFinite(lat) && Number.isFinite(lng) && Math.abs(lat) <= 90 && Math.abs(lng) <= 180) places.set(peerId, { lat, lng })
  }
  return places
}

// The globe animates points when its points prop changes. Keep this key limited
// to fields that affect marker identity or position, not other peer snapshot data.
export function peerLocationsKey(locations: PeerLocation[]): string {
  return JSON.stringify(locations.map(({ peerId, countryCode, lat, lng }) => [peerId, countryCode, lat, lng]))
}
