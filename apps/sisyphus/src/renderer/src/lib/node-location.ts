// The owner's own pin for this node, as the daemon's GetNodeLocation and
// SetNodeLocation speak of it, and the text the owner types it as.

export const locationShares = ['exact', 'about-1km', 'city', 'country', 'nothing'] as const
export type LocationShare = typeof locationShares[number]
export type NodeLocation = { latitude: number; longitude: number; share: LocationShare }

// The enum's numbers in node.proto. The desktop's gRPC client hands enums
// over by name and the browser's by number; both take a number.
const shareNumbers: Record<LocationShare, number> = { nothing: 1, country: 2, city: 3, 'about-1km': 4, exact: 5 }

export function shareFromProtocol(value: unknown): LocationShare {
  const name = typeof value === 'string' ? value.replace(/^LOCATION_SHARE_/, '').toLowerCase().replace('about_1km', 'about-1km') : undefined
  if (name && (locationShares as readonly string[]).includes(name)) return name as LocationShare
  const byNumber = locationShares.find((share) => shareNumbers[share] === value)
  return byNumber ?? 'country'
}

export function shareToProtocol(share: LocationShare): number {
  return shareNumbers[share]
}

export function locationFromProtocol(value: unknown): NodeLocation | null {
  if (!value || typeof value !== 'object') return null
  const location = value as { latitude?: unknown; longitude?: unknown; share?: unknown }
  const latitude = Number(location.latitude ?? 0)
  const longitude = Number(location.longitude ?? 0)
  if (!validCoordinates(latitude, longitude)) return null
  return { latitude, longitude, share: shareFromProtocol(location.share) }
}

export function validCoordinates(latitude: number, longitude: number): boolean {
  return Number.isFinite(latitude) && Number.isFinite(longitude) && Math.abs(latitude) <= 90 && Math.abs(longitude) <= 180
}

// parseCoordinates reads a place as people copy it from a map: "37.2710,
// -79.9414", the same with a space instead of the comma, or with the
// hemispheres written out, as in "37.2710° N, 79.9414° W". It returns null
// for anything else, or for a place off the Earth.
export function parseCoordinates(text: string): { latitude: number; longitude: number } | null {
  const number = String.raw`([+-]?\d+(?:\.\d+)?)\s*°?\s*([NSEWnsew])?`
  const match = text.trim().match(new RegExp(`^${number}\\s*(?:,|;|\\s)\\s*${number}$`))
  if (!match) return null
  const signed = (value: string, hemisphere: string | undefined, negative: string, allowed: string) => {
    if (hemisphere && !allowed.includes(hemisphere.toUpperCase())) return Number.NaN
    if (hemisphere && value.startsWith('-')) return Number.NaN
    const n = Number(value)
    return hemisphere?.toUpperCase() === negative ? -n : n
  }
  const latitude = signed(match[1], match[2], 'S', 'NS')
  const longitude = signed(match[3], match[4], 'W', 'EW')
  return validCoordinates(latitude, longitude) ? { latitude, longitude } : null
}

// formatCoordinates writes coordinates back the way parseCoordinates reads
// them, to five decimal places (about a metre), with no trailing zeros.
export function formatCoordinates(latitude: number, longitude: number): string {
  const short = (n: number) => String(Number(n.toFixed(5)))
  return `${short(latitude)}, ${short(longitude)}`
}
