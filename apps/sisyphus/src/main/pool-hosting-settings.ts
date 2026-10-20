import { mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from 'node:fs'
import { randomUUID } from 'node:crypto'
import { isIPv4 } from 'node:net'
import { networkInterfaces } from 'node:os'
import { dirname } from 'node:path'
import { allInterfaces } from '../shared/pool-hosting.ts'

// Hosting lets other machines reach the pool port of the node the app
// starts. The address is one of this machine's own, or every interface at
// once only when the owner picks that on purpose: a laptop may also be on a
// VPN or a guest network it should not serve.
export type PoolHostingSettings = { enabled: boolean; address: string; port: number }
export type HostAddress = { address: string; interface: string; lan: boolean }

export const defaultPoolHostingPort = 7700
export const defaultPoolHostingSettings: PoolHostingSettings = { enabled: false, address: '', port: defaultPoolHostingPort }

type Interfaces = ReturnType<typeof networkInterfaces>

// Private ranges (RFC 1918) are what a home or office network hands out.
export function isLanAddress(address: string): boolean {
  const [a, b] = address.split('.').map(Number)
  return a === 10 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168)
}

/** This machine's IPv4 addresses other machines could reach, LAN ones first. */
export function hostAddresses(interfaces: Interfaces = networkInterfaces()): HostAddress[] {
  const found: HostAddress[] = []
  for (const [name, entries] of Object.entries(interfaces)) {
    for (const entry of entries ?? []) {
      if (entry.family !== 'IPv4' || entry.internal || entry.address.startsWith('169.254.')) continue
      found.push({ address: entry.address, interface: name, lan: isLanAddress(entry.address) })
    }
  }
  return found.sort((a, b) => Number(b.lan) - Number(a.lan))
}

function validPort(value: unknown): value is number {
  return typeof value === 'number' && Number.isInteger(value) && value >= 1 && value <= 65535
}

function validAddress(value: unknown): value is string {
  return typeof value === 'string' && isIPv4(value) && !value.startsWith('127.')
}

export function normalizePoolHostingSettings(value: unknown): PoolHostingSettings {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return { ...defaultPoolHostingSettings }
  const settings = value as Record<string, unknown>
  const address = validAddress(settings.address) ? settings.address : ''
  return {
    enabled: settings.enabled === true && address !== '',
    address,
    port: validPort(settings.port) ? settings.port : defaultPoolHostingPort,
  }
}

export function validatePoolHostingSettings(value: unknown): PoolHostingSettings {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid pool hosting settings.')
  const candidate = value as Record<string, unknown>
  if (typeof candidate.enabled !== 'boolean') throw new Error('Invalid pool hosting settings.')
  if (!validPort(candidate.port)) throw new Error('The pool port must be a whole number from 1 to 65535.')
  if (candidate.address !== '' && !validAddress(candidate.address)) throw new Error('Choose one of this computer’s addresses, or all interfaces.')
  if (candidate.enabled && candidate.address === '') throw new Error('Choose the address other machines should use to reach this pool.')
  return { enabled: candidate.enabled, address: candidate.address as string, port: candidate.port }
}

export function loadPoolHostingSettings(path: string): PoolHostingSettings {
  try {
    return normalizePoolHostingSettings(JSON.parse(readFileSync(path, 'utf8')))
  } catch {
    return { ...defaultPoolHostingSettings }
  }
}

export function savePoolHostingSettings(path: string, value: unknown): PoolHostingSettings {
  const settings = validatePoolHostingSettings(value)
  const temporaryPath = `${path}.${randomUUID()}.tmp`
  try {
    mkdirSync(dirname(path), { recursive: true })
    writeFileSync(temporaryPath, `${JSON.stringify(settings, null, 2)}\n`, { encoding: 'utf8', mode: 0o600 })
    renameSync(temporaryPath, path)
  } catch (error) {
    try { unlinkSync(temporaryPath) } catch { /* No temporary file to clean up. */ }
    throw error
  }
  return settings
}

export type HostingListen = { listen: string } | { unavailable: true }

/**
 * Where the app's node should take workers, or why it cannot host as saved.
 * An address that is no longer this machine's (another Wi-Fi network, a VPN
 * that is down) keeps the node on loopback rather than binding elsewhere.
 */
export function hostingListen(settings: PoolHostingSettings, addresses: HostAddress[]): HostingListen | null {
  if (!settings.enabled) return null
  if (settings.address !== allInterfaces && !addresses.some((entry) => entry.address === settings.address)) {
    return { unavailable: true }
  }
  return { listen: `${settings.address}:${settings.port}` }
}

export type PoolHostingState = {
  managed: boolean
  saved: PoolHostingSettings
  running: PoolHostingSettings
  restartNeeded: boolean
  /** Hosting was on, but its address was not this computer's when the node started. */
  unavailable: boolean
  addresses: HostAddress[]
  platform: string
}

export function samePoolHostingSettings(a: PoolHostingSettings, b: PoolHostingSettings): boolean {
  // The address and port of a pool not hosted change nothing the node does.
  if (!a.enabled && !b.enabled) return true
  return a.enabled === b.enabled && a.port === b.port && a.address === b.address
}

export function poolHostingState(value: Omit<PoolHostingState, 'restartNeeded'>): PoolHostingState {
  return { ...value, restartNeeded: value.managed && !samePoolHostingSettings(value.saved, value.running) }
}
