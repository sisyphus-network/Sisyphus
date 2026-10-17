import { mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from 'node:fs'
import { randomUUID } from 'node:crypto'
import { dirname } from 'node:path'
import { normalizeStunServer } from '../shared/stun-server.ts'

export type LocationDiscoverySettings = { locateGateway: boolean; locateCountry: boolean; stunServer: string }

export const defaultLocationDiscoverySettings: LocationDiscoverySettings = {
  locateGateway: false,
  locateCountry: false,
  stunServer: '',
}

const gatewayDiscoveryPlatforms = new Set(['linux', 'darwin', 'win32', 'freebsd', 'openbsd', 'netbsd', 'dragonfly'])

export function supportsGatewayDiscovery(platform: string): boolean {
  return gatewayDiscoveryPlatforms.has(platform)
}

function safeStunServer(value: unknown): string {
  try { return normalizeStunServer(value) } catch { return '' }
}

export function normalizeLocationDiscoverySettings(value: unknown): LocationDiscoverySettings {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return { ...defaultLocationDiscoverySettings }
  const settings = value as Record<string, unknown>
  return {
    locateGateway: settings.locateGateway === true,
    locateCountry: settings.locateCountry === true,
    stunServer: safeStunServer(settings.stunServer),
  }
}

export function effectiveLocationDiscoverySettings(settings: LocationDiscoverySettings, gatewaySupported: boolean): LocationDiscoverySettings {
  return { locateGateway: settings.locateGateway && gatewaySupported, locateCountry: settings.locateCountry, stunServer: settings.stunServer }
}

export function loadLocationDiscoverySettings(path: string): LocationDiscoverySettings {
  try {
    return normalizeLocationDiscoverySettings(JSON.parse(readFileSync(path, 'utf8')))
  } catch {
    return { ...defaultLocationDiscoverySettings }
  }
}

export function saveLocationDiscoverySettings(path: string, value: unknown): LocationDiscoverySettings {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid location discovery settings.')
  const candidate = value as Record<string, unknown>
  if (typeof candidate.locateGateway !== 'boolean' || typeof candidate.locateCountry !== 'boolean' || typeof candidate.stunServer !== 'string') {
    throw new Error('Invalid location discovery settings.')
  }
  const settings = normalizeLocationDiscoverySettings({ ...candidate, stunServer: normalizeStunServer(candidate.stunServer) })
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
