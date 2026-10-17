import { mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from 'node:fs'
import { randomUUID } from 'node:crypto'
import { dirname } from 'node:path'

export type LocationDiscoverySettings = { locateGateway: boolean; locateCountry: boolean }

export const defaultLocationDiscoverySettings: LocationDiscoverySettings = {
  locateGateway: false,
  locateCountry: false,
}

export function normalizeLocationDiscoverySettings(value: unknown): LocationDiscoverySettings {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return { ...defaultLocationDiscoverySettings }
  const settings = value as Record<string, unknown>
  return {
    locateGateway: settings.locateGateway === true,
    locateCountry: settings.locateCountry === true,
  }
}

export function effectiveLocationDiscoverySettings(settings: LocationDiscoverySettings, gatewaySupported: boolean): LocationDiscoverySettings {
  return { locateGateway: settings.locateGateway && gatewaySupported, locateCountry: settings.locateCountry }
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
  if (typeof candidate.locateGateway !== 'boolean' || typeof candidate.locateCountry !== 'boolean') {
    throw new Error('Invalid location discovery settings.')
  }
  const settings = normalizeLocationDiscoverySettings(candidate)
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
