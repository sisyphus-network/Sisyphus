// The free city table, DB-IP's "IP to City Lite", as the daemon's
// GetCityTable, DownloadCityTable and RemoveCityTable speak of it, and the
// city it puts this node in, from GetNodeInfo's city_estimate.
//
// The desktop's gRPC client gives 64-bit numbers as strings and the
// browser's as bigints turned to strings; both name fields in camelCase.
// A snake_case name is read too, so that a reply passed on as the daemon
// wrote it is still understood.

// The credit the table's licence (CC BY 4.0) asks for wherever what it says
// is used. The daemon gives it too; these are what is shown if it does not.
export const cityTableAttribution = 'IP Geolocation by DB-IP'
export const cityTableAttributionUrl = 'https://db-ip.com'

export type CityTableState = {
  installed: boolean
  // The month DB-IP published it, as 2026-10.
  edition: string
  sizeBytes: number
  downloadedAtMs: number
  attribution: string
}

export type CityEstimate = {
  city: string
  region: string
  countryCode: string
  latitude: number
  longitude: number
  source: string
  attribution: string
}

function field(value: Record<string, unknown>, camel: string): unknown {
  if (camel in value) return value[camel]
  return value[camel.replace(/[A-Z]/g, (letter) => `_${letter.toLowerCase()}`)]
}

function text(value: unknown): string {
  return typeof value === 'string' ? value.trim() : ''
}

function count(value: unknown): number {
  const n = typeof value === 'bigint' ? Number(value) : typeof value === 'string' && value.trim() !== '' ? Number(value) : typeof value === 'number' ? value : 0
  return Number.isFinite(n) && n > 0 ? n : 0
}

export function cityTableFromProtocol(value: unknown): CityTableState {
  const state = value && typeof value === 'object' ? value as Record<string, unknown> : {}
  const installed = field(state, 'installed') === true
  return {
    installed,
    edition: installed ? text(field(state, 'edition')) : '',
    sizeBytes: installed ? count(field(state, 'sizeBytes')) : 0,
    downloadedAtMs: installed ? count(field(state, 'downloadedAtMs')) : 0,
    attribution: text(field(state, 'attribution')) || cityTableAttribution,
  }
}

// cityEstimateFromNodeInfo reads where the city table puts this node from a
// GetNodeInfo reply, or null if it puts it nowhere: there is no table, the
// node knows no public address of its own, or the owner's pin wins.
export function cityEstimateFromNodeInfo(info: unknown): CityEstimate | null {
  if (!info || typeof info !== 'object') return null
  const raw = field(info as Record<string, unknown>, 'cityEstimate')
  if (!raw || typeof raw !== 'object') return null
  const estimate = raw as Record<string, unknown>
  const city = text(field(estimate, 'city'))
  const region = text(field(estimate, 'region'))
  const countryCode = text(field(estimate, 'countryCode')).toUpperCase()
  if (!city && !region && !countryCode) return null
  return {
    city, region, countryCode,
    latitude: Number(field(estimate, 'latitude') ?? 0) || 0,
    longitude: Number(field(estimate, 'longitude') ?? 0) || 0,
    source: text(field(estimate, 'source')),
    attribution: text(field(estimate, 'attribution')) || cityTableAttribution,
  }
}

// placeName writes a place as people say it, from the most to the least
// particular: "Louisville, Kentucky, US". A region named as its city, as
// city-states are, is said once.
export function placeName(estimate: Pick<CityEstimate, 'city' | 'region' | 'countryCode'>): string {
  const parts = [estimate.city, estimate.region, estimate.countryCode].filter(Boolean)
  return parts.filter((part, index) => index === 0 || part.toLowerCase() !== parts[index - 1].toLowerCase()).join(', ')
}

// formatEdition writes the month a table was published, 2026-10, as the
// locale names it: "October 2026". Anything else is shown as given.
export function formatEdition(edition: string, locale: string): string {
  const match = edition.match(/^(\d{4})-(\d{2})$/)
  if (!match) return edition
  const month = Number(match[2])
  if (month < 1 || month > 12) return edition
  try { return new Intl.DateTimeFormat(locale || undefined, { year: 'numeric', month: 'long', timeZone: 'UTC' }).format(Date.UTC(Number(match[1]), month - 1, 1)) }
  catch { return edition }
}

// formatSize writes a size in decimal units, as the table is spoken of:
// "93 MB". A file under a megabyte, which no real table is, is given in
// kilobytes or bytes, so that it is never "0 MB".
export function formatSize(bytes: number, locale: string): string {
  const [unit, value] = bytes >= 1e6 ? ['megabyte', bytes / 1e6] : bytes >= 1e3 ? ['kilobyte', bytes / 1e3] : ['byte', bytes]
  try { return new Intl.NumberFormat(locale || undefined, { style: 'unit', unit, maximumFractionDigits: 0 }).format(value) }
  catch { return `${Math.round(value)} ${unit === 'megabyte' ? 'MB' : unit === 'kilobyte' ? 'kB' : 'B'}` }
}

export function formatDownloadDate(ms: number, locale: string): string {
  if (!ms) return ''
  try { return new Intl.DateTimeFormat(locale || undefined, { dateStyle: 'medium' }).format(ms) }
  catch { return new Date(ms).toISOString().slice(0, 10) }
}

// cityTableStatus gives what the card says of a table that is there: its
// edition, size and the day it was downloaded, each as the locale writes it.
export function cityTableStatus(state: CityTableState, locale: string): { edition: string; size: string; date: string } | null {
  if (!state.installed) return null
  return {
    edition: formatEdition(state.edition, locale),
    size: formatSize(state.sizeBytes, locale),
    date: formatDownloadDate(state.downloadedAtMs, locale),
  }
}

// ownerPinned says whether a GetNodeInfo reply carries the owner's own pin,
// which wins over the table: then the node gives no city estimate.
export function ownerPinned(info: unknown): boolean {
  if (!info || typeof info !== 'object') return false
  const location = field(info as Record<string, unknown>, 'ownerLocation')
  return Boolean(location && typeof location === 'object')
}
