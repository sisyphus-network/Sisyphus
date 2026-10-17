import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { defaultLocationDiscoverySettings, effectiveLocationDiscoverySettings, loadLocationDiscoverySettings, normalizeLocationDiscoverySettings, saveLocationDiscoverySettings } from '../src/main/location-discovery-settings.ts'

test('location discovery defaults to disabled and ignores malformed preferences', () => {
  const directory = mkdtempSync(join(tmpdir(), 'sisyphus-location-'))
  try {
    assert.deepEqual(loadLocationDiscoverySettings(join(directory, 'missing.json')), defaultLocationDiscoverySettings)
    assert.deepEqual(normalizeLocationDiscoverySettings({ locateGateway: 1, locateCountry: 'yes' }), defaultLocationDiscoverySettings)
    assert.deepEqual(normalizeLocationDiscoverySettings({ locateGateway: true, locateCountry: false, unexpected: true }), { locateGateway: true, locateCountry: false })
  } finally { rmSync(directory, { recursive: true, force: true }) }
})

test('saved preferences are strict booleans and round-trip independently', () => {
  const directory = mkdtempSync(join(tmpdir(), 'sisyphus-location-'))
  const path = join(directory, 'preferences', 'location-discovery.json')
  try {
    assert.throws(() => saveLocationDiscoverySettings(path, { locateGateway: true, locateCountry: 'yes' }), /Invalid location discovery settings/)
    const saved = saveLocationDiscoverySettings(path, { locateGateway: true, locateCountry: false })
    assert.deepEqual(saved, { locateGateway: true, locateCountry: false })
    assert.deepEqual(loadLocationDiscoverySettings(path), saved)
    assert.deepEqual(JSON.parse(readFileSync(path, 'utf8')), saved)
  } finally { rmSync(directory, { recursive: true, force: true }) }
})

test('gateway discovery is only passed to platforms that implement the gateway lookup', () => {
  assert.deepEqual(effectiveLocationDiscoverySettings({ locateGateway: true, locateCountry: true }, false), { locateGateway: false, locateCountry: true })
  assert.deepEqual(effectiveLocationDiscoverySettings({ locateGateway: true, locateCountry: false }, true), { locateGateway: true, locateCountry: false })
})
