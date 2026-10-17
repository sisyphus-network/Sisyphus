import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { defaultLocationDiscoverySettings, effectiveLocationDiscoverySettings, loadLocationDiscoverySettings, normalizeLocationDiscoverySettings, saveLocationDiscoverySettings } from '../src/main/location-discovery-settings.ts'
import { normalizeStunServer } from '../src/shared/stun-server.ts'

test('location discovery defaults to disabled and ignores malformed preferences', () => {
  const directory = mkdtempSync(join(tmpdir(), 'sisyphus-location-'))
  try {
    assert.deepEqual(loadLocationDiscoverySettings(join(directory, 'missing.json')), defaultLocationDiscoverySettings)
    assert.deepEqual(normalizeLocationDiscoverySettings({ locateGateway: 1, locateCountry: 'yes' }), defaultLocationDiscoverySettings)
    assert.deepEqual(normalizeLocationDiscoverySettings({ locateGateway: true, locateCountry: false, stunServer: ' stun.example.org:3478 ', unexpected: true }), { locateGateway: true, locateCountry: false, stunServer: 'stun.example.org:3478' })
    assert.deepEqual(normalizeLocationDiscoverySettings({ locateGateway: true, locateCountry: false }), { locateGateway: true, locateCountry: false, stunServer: '' })
    assert.deepEqual(normalizeLocationDiscoverySettings({ locateGateway: true, locateCountry: true, stunServer: 'https://invalid.example' }), { locateGateway: true, locateCountry: true, stunServer: '' })
  } finally { rmSync(directory, { recursive: true, force: true }) }
})

test('saved preferences are strict booleans and round-trip independently', () => {
  const directory = mkdtempSync(join(tmpdir(), 'sisyphus-location-'))
  const path = join(directory, 'preferences', 'location-discovery.json')
  try {
    assert.throws(() => saveLocationDiscoverySettings(path, { locateGateway: true, locateCountry: 'yes', stunServer: '' }), /Invalid location discovery settings/)
    assert.throws(() => saveLocationDiscoverySettings(path, { locateGateway: true, locateCountry: false, stunServer: 123 }), /Invalid location discovery settings/)
    const saved = saveLocationDiscoverySettings(path, { locateGateway: true, locateCountry: false, stunServer: 'stun.example.org:3478' })
    assert.deepEqual(saved, { locateGateway: true, locateCountry: false, stunServer: 'stun.example.org:3478' })
    assert.deepEqual(loadLocationDiscoverySettings(path), saved)
    assert.deepEqual(JSON.parse(readFileSync(path, 'utf8')), saved)
  } finally { rmSync(directory, { recursive: true, force: true }) }
})

test('gateway discovery is only passed to platforms that implement the gateway lookup', () => {
  assert.deepEqual(effectiveLocationDiscoverySettings({ locateGateway: true, locateCountry: true, stunServer: 'stun.example.org' }, false), { locateGateway: false, locateCountry: true, stunServer: 'stun.example.org' })
  assert.deepEqual(effectiveLocationDiscoverySettings({ locateGateway: true, locateCountry: false, stunServer: '' }, true), { locateGateway: true, locateCountry: false, stunServer: '' })
})

test('STUN server accepts a host with an optional port and IPv6 with or without brackets', () => {
  assert.equal(normalizeStunServer(' stun.example.org:3478 '), 'stun.example.org:3478')
  assert.equal(normalizeStunServer('127.0.0.1'), '127.0.0.1')
  assert.equal(normalizeStunServer('2001:db8::1'), '2001:db8::1')
  assert.equal(normalizeStunServer('2001:db8::1:3478'), '2001:db8::1:3478')
  assert.equal(normalizeStunServer('[2001:db8::1]:3478'), '[2001:db8::1]:3478')
  assert.equal(normalizeStunServer(''), '')
  for (const server of ['https://stun.example.org', 'stun.example.org:0', 'stun.example.org:65536', '[2001:db8::zz]', 'bad host', 'host/path']) {
    assert.throws(() => normalizeStunServer(server), /STUN server as host\[:port\]/)
  }
})
