// What the globe's hover card says about a point: everything this node knows
// of it, gathered from the node's state, its pool's list of workers and its
// own GetNodeInfo, and the card itself as HTML. The card is put into the page
// as markup, so every piece of text in it is escaped here.

import { sharedPlaces, type Place } from './globe-markers.ts'

export type PeerFacts = {
  peerId: string
  connectionState?: number | string
  knownAddresses?: string[]
  trustedForCompute?: boolean
  worksForThisNode?: boolean
  thisNodeWorksFor?: boolean
  givesWork?: boolean
  takesWork?: boolean
  countryCode?: string
}

export type WorkerFacts = {
  peerId: string
  name: string
  hostname: string
  os: string
  arch: string
  cpuModel: string
  cpuCores: number
  memoryBytes: number
  gpus: { name: string; memoryBytes: number }[]
  models: string[]
  taskSlots: number
  runningTasks: number
  place?: Place
}

export type CityEstimate = { city: string; region: string; countryCode: string; source: string; attribution: string }
export type PinShare = 'exact' | 'about-1km' | 'city' | 'country' | 'nothing'

export type NodeFacts = {
  peerId: string
  daemonVersion?: string
  listenAddresses?: string[]
  countryCode?: string
  countrySource?: string
  cityEstimate?: CityEstimate
  pin?: { lat: number; lng: number; share: PinShare }
}

export type Role = 'trusted' | 'untrusted' | 'worksForThisNode' | 'thisNodeWorksFor' | 'givesWork' | 'takesWork'
export type Precision = 'city' | 'about-1km'

export type GlobeDetails = {
  kind: 'node' | 'peer' | 'worker'
  peerId: string
  connection?: 'connected' | 'disconnected' | 'unknown'
  version?: string
  // Every address, once each, in the order the node gave them, and the
  // hosts in them.
  addresses: string[]
  ips: string[]
  countryCode?: string
  countrySource?: string
  city?: CityEstimate
  pin?: NodeFacts['pin']
  roles: Role[]
  worker?: WorkerFacts
  place?: Place & { precision?: Precision }
}

const text = (value: unknown) => typeof value === 'string' ? value : ''
const count = (value: unknown) => {
  const n = Number(value ?? 0)
  return Number.isFinite(n) && n > 0 ? n : 0
}
const strings = (value: unknown) => Array.isArray(value) ? value.filter((item): item is string => typeof item === 'string' && item !== '') : []

// poolWorkers reads the node API's list of workers, by peer ID, with the
// place each shares, if any. Either client's shapes are taken: numbers as
// numbers or as the strings 64-bit fields arrive as.
export function poolWorkers(workers: unknown): Map<string, WorkerFacts> {
  const result = new Map<string, WorkerFacts>()
  if (!Array.isArray(workers)) return result
  const places = sharedPlaces(workers)
  for (const raw of workers) {
    const worker = (raw ?? {}) as Record<string, unknown>
    const peerId = text(worker.peerId)
    if (!peerId) continue
    const gpus = Array.isArray(worker.gpus)
      ? worker.gpus.flatMap((gpu) => {
        const { name, memoryBytes } = (gpu ?? {}) as { name?: unknown; memoryBytes?: unknown }
        return text(name) || count(memoryBytes) ? [{ name: text(name), memoryBytes: count(memoryBytes) }] : []
      })
      : []
    result.set(peerId, {
      peerId,
      name: text(worker.name),
      hostname: text(worker.hostname),
      os: text(worker.os),
      arch: text(worker.arch),
      cpuModel: text(worker.cpuModel),
      cpuCores: count(worker.cpuCores),
      memoryBytes: count(worker.memoryBytes),
      gpus,
      models: strings(worker.models),
      taskSlots: count(worker.taskSlots),
      runningTasks: count(worker.runningTasks),
      place: places.get(peerId),
    })
  }
  return result
}

// cityEstimateFromProtocol reads GetNodeInfo's city_estimate, which is left
// out where the node has none.
export function cityEstimateFromProtocol(value: unknown): CityEstimate | undefined {
  if (!value || typeof value !== 'object') return undefined
  const estimate = value as Record<string, unknown>
  const city = { city: text(estimate.city), region: text(estimate.region), countryCode: text(estimate.countryCode).toUpperCase(), source: text(estimate.source), attribution: text(estimate.attribution) }
  return city.city || city.region ? city : undefined
}

export function connectionOf(state: unknown): 'connected' | 'disconnected' | 'unknown' {
  if (state === 2 || (typeof state === 'string' && state.endsWith('_CONNECTED') && !state.endsWith('_DISCONNECTED'))) return 'connected'
  if (state === 1 || (typeof state === 'string' && state.endsWith('_DISCONNECTED'))) return 'disconnected'
  return 'unknown'
}

// hostOf gives the IP address or DNS name a multiaddr reaches, or a plain
// address's host.
export function hostOf(address: string): string | undefined {
  const multi = address.match(/^\/(?:ip4|ip6|dns|dns4|dns6)\/([^/]+)/)
  if (multi) return multi[1]
  if (address.startsWith('/')) return undefined
  const bracketed = address.match(/^\[([^\]]+)\](?::\d+)?$/)
  if (bracketed) return bracketed[1]
  const hostPort = address.match(/^([^:/\s]+)(?::\d+)?$/)
  return hostPort ? hostPort[1] : undefined
}

function addressesOf(list: string[] | undefined) {
  const addresses = [...new Set(strings(list).map((address) => address.trim()).filter(Boolean))]
  const ips = [...new Set(addresses.map(hostOf).filter((host): host is string => Boolean(host)))]
  return { addresses, ips }
}

// precisionOf says how precisely a shared place was rounded, if its
// numbers show it: to 0.1 degree for a city, or to 0.01, about a kilometre.
// A place given more precisely says nothing of how it was shared.
export function precisionOf(place: Place): Precision | undefined {
  const roundedTo = (step: number) => [place.lat, place.lng].every((value) => Math.abs(value / step - Math.round(value / step)) < 1e-6)
  if (roundedTo(0.1)) return 'city'
  if (roundedTo(0.01)) return 'about-1km'
  return undefined
}

export function nodeDetails(node: NodeFacts, worker?: WorkerFacts): GlobeDetails {
  return {
    kind: 'node',
    peerId: node.peerId,
    version: node.daemonVersion || undefined,
    ...addressesOf(node.listenAddresses),
    countryCode: node.countryCode?.toUpperCase() || undefined,
    countrySource: node.countrySource || undefined,
    city: node.cityEstimate,
    pin: node.pin,
    roles: [],
    worker,
  }
}

export function peerDetails(peerId: string, peer?: PeerFacts, worker?: WorkerFacts): GlobeDetails {
  const roles: Role[] = []
  if (peer) {
    roles.push(peer.trustedForCompute ? 'trusted' : 'untrusted')
    if (peer.worksForThisNode) roles.push('worksForThisNode')
    if (peer.thisNodeWorksFor) roles.push('thisNodeWorksFor')
    // A daemon from before the two sides of trust were told apart gives
    // only the one.
    if (peer.givesWork ?? peer.trustedForCompute) roles.push('givesWork')
    if (peer.takesWork ?? peer.trustedForCompute) roles.push('takesWork')
  }
  return {
    kind: worker ? 'worker' : 'peer',
    peerId,
    connection: peer ? connectionOf(peer.connectionState) : undefined,
    ...addressesOf(peer?.knownAddresses),
    countryCode: peer?.countryCode?.toUpperCase() || undefined,
    roles,
    worker,
    place: worker?.place ? { ...worker.place, precision: precisionOf(worker.place) } : undefined,
  }
}

export type GlobeDetailLabels = {
  thisNode: string
  peer: string
  worker: string
  peerId: string
  version: string
  listenAddresses: string
  knownAddresses: string
  ips: string
  more: string
  connection: string
  connected: string
  disconnected: string
  unknown: string
  country: string
  sourceInterface: string
  sourcePool: string
  sourceGateway: string
  sourceStun: string
  sourceLookup: string
  city: string
  pin: string
  shareExact: string
  shareAbout1km: string
  shareCity: string
  shareCountry: string
  shareNothing: string
  compute: string
  trusted: string
  untrusted: string
  worksForThisNode: string
  thisNodeWorksFor: string
  givesWork: string
  takesWork: string
  name: string
  hostname: string
  system: string
  processor: string
  cores: string
  memory: string
  gpus: string
  models: string
  taskSlots: string
  slotsValue: string
  sharedPlace: string
  placeCity: string
  placeAbout1km: string
  clickToPin: string
}

const escapes: Record<string, string> = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }
export function escapeHtml(value: unknown): string {
  return String(value ?? '').replace(/[&<>"']/g, (character) => escapes[character])
}

function fill(template: string, values: Record<string, string | number>): string {
  return template.replace(/\{(\w+)\}/g, (_, key: string) => String(values[key] ?? `{${key}}`))
}

function bytes(value: number, locale: string): string {
  const units = ['byte', 'kilobyte', 'megabyte', 'gigabyte', 'terabyte']
  const power = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1)
  return new Intl.NumberFormat(locale, { style: 'unit', unit: units[power], unitDisplay: 'short', maximumFractionDigits: power ? 1 : 0 }).format(value / 1024 ** power)
}

const coordinates = (place: Place) => `${Number(place.lat.toFixed(5))}, ${Number(place.lng.toFixed(5))}`

export type RenderOptions = { direction: 'ltr' | 'rtl'; locale: string; maxAddresses?: number; pinned?: boolean }

// renderDetailsHtml writes the card. Text in the app's language follows its
// direction; peer IDs, addresses and coordinates are always left to right,
// isolated so that a right-to-left card does not reorder their parts, and
// names the owner of a machine chose are isolated in whichever direction
// they are written.
export function renderDetailsHtml(details: GlobeDetails, labels: GlobeDetailLabels, { direction, locale, maxAddresses = 6, pinned = false }: RenderOptions): string {
  const rows: string[] = []
  const row = (label: string, value: string) => { if (value) rows.push(`<dt>${escapeHtml(label)}</dt><dd>${value}</dd>`) }
  const ltr = (value: string) => `<bdi dir="ltr">${escapeHtml(value)}</bdi>`
  const auto = (value: string) => `<bdi>${escapeHtml(value)}</bdi>`
  const plain = (value: string) => escapeHtml(value)
  const list = (values: string[], render: (value: string) => string) => {
    if (!values.length) return ''
    const shown = values.slice(0, maxAddresses).map((value) => `<li>${render(value)}</li>`)
    const more = values.length - maxAddresses
    if (more > 0) shown.push(`<li class="globe-card__more">${escapeHtml(fill(labels.more, { count: more }))}</li>`)
    return `<ul>${shown.join('')}</ul>`
  }

  const { worker } = details
  const kindLabel = details.kind === 'node' ? labels.thisNode : details.kind === 'worker' ? labels.worker : labels.peer
  const title = worker?.name ? `${auto(worker.name)} <span class="globe-card__kind">· ${escapeHtml(kindLabel)}</span>` : escapeHtml(kindLabel)

  row(labels.peerId, `<code class="globe-card__id">${ltr(details.peerId)}</code>`)
  if (details.connection) row(labels.connection, plain(labels[details.connection]))
  if (details.version) row(labels.version, ltr(details.version))
  row(details.kind === 'node' ? labels.listenAddresses : labels.knownAddresses, list(details.addresses, ltr))
  row(labels.ips, list(details.ips, ltr))
  if (details.countryCode) {
    const sources: Record<string, string> = { interface: labels.sourceInterface, pool: labels.sourcePool, gateway: labels.sourceGateway, stun: labels.sourceStun, lookup: labels.sourceLookup }
    const source = details.countrySource ? sources[details.countrySource] : undefined
    row(labels.country, `${ltr(details.countryCode)}${source ? ` · ${plain(source)}` : ''}`)
  }
  if (details.city) {
    const place = [details.city.city, details.city.region, details.city.countryCode].filter(Boolean).join(', ')
    row(labels.city, `${auto(place)}${details.city.attribution ? `<small class="globe-card__note">${auto(details.city.attribution)}</small>` : ''}`)
  }
  if (details.pin) {
    const shares: Record<PinShare, string> = { exact: labels.shareExact, 'about-1km': labels.shareAbout1km, city: labels.shareCity, country: labels.shareCountry, nothing: labels.shareNothing }
    row(labels.pin, `${ltr(coordinates(details.pin))} · ${plain(shares[details.pin.share] ?? '')}`)
  }
  if (details.roles.length) {
    const phrases: Record<Role, string> = { trusted: labels.trusted, untrusted: labels.untrusted, worksForThisNode: labels.worksForThisNode, thisNodeWorksFor: labels.thisNodeWorksFor, givesWork: labels.givesWork, takesWork: labels.takesWork }
    row(labels.compute, `<ul>${details.roles.map((role) => `<li>${plain(phrases[role])}</li>`).join('')}</ul>`)
  }
  if (worker) {
    if (worker.name) row(labels.name, auto(worker.name))
    if (worker.hostname) row(labels.hostname, auto(worker.hostname))
    const system = [worker.os, worker.arch].filter(Boolean).join(' / ')
    if (system) row(labels.system, ltr(system))
    const cores = worker.cpuCores ? fill(labels.cores, { count: worker.cpuCores }) : ''
    if (worker.cpuModel || cores) row(labels.processor, [worker.cpuModel ? auto(worker.cpuModel) : '', plain(cores)].filter(Boolean).join(' · '))
    if (worker.memoryBytes) row(labels.memory, plain(bytes(worker.memoryBytes, locale)))
    row(labels.gpus, list(worker.gpus.map((gpu) => [gpu.name, gpu.memoryBytes ? bytes(gpu.memoryBytes, locale) : ''].filter(Boolean).join(' · ')), auto))
    row(labels.models, list(worker.models, auto))
    if (worker.taskSlots) row(labels.taskSlots, plain(fill(labels.slotsValue, { running: worker.runningTasks, total: worker.taskSlots })))
  }
  if (details.place) {
    const precision = details.place.precision === 'city' ? labels.placeCity : details.place.precision === 'about-1km' ? labels.placeAbout1km : ''
    row(labels.sharedPlace, `${ltr(coordinates(details.place))}${precision ? ` · ${plain(precision)}` : ''}`)
  }

  return `<div class="globe-card" dir="${direction === 'rtl' ? 'rtl' : 'ltr'}">`
    + `<div class="globe-card__title">${title}</div>`
    + `<dl class="globe-card__rows">${rows.join('')}</dl>`
    + (pinned ? '' : `<div class="globe-card__hint">${escapeHtml(labels.clickToPin)}</div>`)
    + '</div>'
}
