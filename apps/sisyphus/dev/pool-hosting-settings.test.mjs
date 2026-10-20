import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { defaultPoolHostingSettings, hostAddresses, hostingListen, isLanAddress, loadPoolHostingSettings, normalizePoolHostingSettings, poolHostingState, samePoolHostingSettings, savePoolHostingSettings, validatePoolHostingSettings } from '../src/main/pool-hosting-settings.ts'
import { allInterfaces, formForHosting, hostingJoinAddresses, windowsFirewallCommand } from '../src/shared/pool-hosting.ts'

const entry = (address, overrides = {}) => ({ address, family: 'IPv4', internal: false, netmask: '255.255.255.0', mac: '00:00:00:00:00:00', cidr: `${address}/24`, ...overrides })

test('hosting addresses are this machine’s reachable IPv4 ones, LAN first', () => {
  const addresses = hostAddresses({
    lo: [entry('127.0.0.1', { internal: true })],
    vpn: [entry('100.64.0.7')],
    'Wi-Fi': [entry('192.168.137.133'), entry('fe80::1', { family: 'IPv6' })],
    link: [entry('169.254.10.2')],
    office: [entry('10.1.2.3')],
    empty: undefined,
  })
  assert.deepEqual(addresses, [
    { address: '192.168.137.133', interface: 'Wi-Fi', lan: true },
    { address: '10.1.2.3', interface: 'office', lan: true },
    { address: '100.64.0.7', interface: 'vpn', lan: false },
  ])
  assert.ok(Array.isArray(hostAddresses()))
  for (const address of ['10.0.0.1', '172.16.0.1', '172.31.255.1', '192.168.0.1']) assert.equal(isLanAddress(address), true, address)
  for (const address of ['172.15.0.1', '172.32.0.1', '192.169.0.1', '8.8.8.8', '100.64.0.7']) assert.equal(isLanAddress(address), false, address)
})

test('hosting is off unless saved on with an address, and malformed preferences fall back', () => {
  const directory = mkdtempSync(join(tmpdir(), 'sisyphus-hosting-'))
  try {
    assert.deepEqual(loadPoolHostingSettings(join(directory, 'missing.json')), defaultPoolHostingSettings)
    for (const value of [null, [], 'on']) assert.deepEqual(normalizePoolHostingSettings(value), defaultPoolHostingSettings)
    assert.deepEqual(normalizePoolHostingSettings({ enabled: true, address: '', port: 7700 }), { enabled: false, address: '', port: 7700 })
    assert.deepEqual(normalizePoolHostingSettings({ enabled: true, address: '127.0.0.1', port: 0 }), { enabled: false, address: '', port: 7700 })
    assert.deepEqual(normalizePoolHostingSettings({ enabled: 'yes', address: '192.168.1.5', port: 7701 }), { enabled: false, address: '192.168.1.5', port: 7701 })
    assert.deepEqual(normalizePoolHostingSettings({ enabled: true, address: allInterfaces, port: 65535 }), { enabled: true, address: allInterfaces, port: 65535 })
  } finally { rmSync(directory, { recursive: true, force: true }) }
})

test('saving checks every field and round-trips', () => {
  const directory = mkdtempSync(join(tmpdir(), 'sisyphus-hosting-'))
  const path = join(directory, 'preferences', 'pool-hosting.json')
  try {
    for (const [value, message] of [
      [null, /Invalid pool hosting settings/],
      [{ enabled: 'yes', address: '', port: 7700 }, /Invalid pool hosting settings/],
      [{ enabled: true, address: '192.168.1.5', port: 70000 }, /from 1 to 65535/],
      [{ enabled: true, address: '192.168.1.5', port: 7700.5 }, /from 1 to 65535/],
      [{ enabled: true, address: 'example.com', port: 7700 }, /Choose one of this computer’s addresses/],
      [{ enabled: true, address: '127.0.0.1', port: 7700 }, /Choose one of this computer’s addresses/],
      [{ enabled: true, address: '', port: 7700 }, /Choose the address other machines should use/],
    ]) assert.throws(() => savePoolHostingSettings(path, value), message)
    assert.deepEqual(validatePoolHostingSettings({ enabled: false, address: '', port: 7700 }), { enabled: false, address: '', port: 7700 })
    const saved = savePoolHostingSettings(path, { enabled: true, address: '192.168.137.133', port: 7700, extra: true })
    assert.deepEqual(saved, { enabled: true, address: '192.168.137.133', port: 7700 })
    assert.deepEqual(loadPoolHostingSettings(path), saved)
    assert.deepEqual(JSON.parse(readFileSync(path, 'utf8')), saved)
    assert.throws(() => savePoolHostingSettings(join(path, 'under-a-file.json'), saved))
  } finally { rmSync(directory, { recursive: true, force: true }) }
})

test('the node listens where hosting says, and never elsewhere when that address is gone', () => {
  const addresses = [{ address: '192.168.137.133', interface: 'Wi-Fi', lan: true }]
  assert.equal(hostingListen({ enabled: false, address: '192.168.137.133', port: 7700 }, addresses), null)
  assert.deepEqual(hostingListen({ enabled: true, address: '192.168.137.133', port: 7700 }, addresses), { listen: '192.168.137.133:7700' })
  assert.deepEqual(hostingListen({ enabled: true, address: allInterfaces, port: 7701 }, []), { listen: '0.0.0.0:7701' })
  assert.deepEqual(hostingListen({ enabled: true, address: '10.0.0.4', port: 7700 }, addresses), { unavailable: true })
})

test('a restart is needed only when the hosting the node runs with would change', () => {
  const off = { enabled: false, address: '', port: 7700 }
  const on = { enabled: true, address: '192.168.137.133', port: 7700 }
  assert.equal(samePoolHostingSettings(off, { enabled: false, address: '10.0.0.4', port: 7800 }), true)
  assert.equal(samePoolHostingSettings(on, { ...on }), true)
  assert.equal(samePoolHostingSettings(on, off), false)
  assert.equal(samePoolHostingSettings(on, { ...on, port: 7701 }), false)
  assert.equal(samePoolHostingSettings(on, { ...on, address: allInterfaces }), false)
  const state = { managed: true, saved: on, running: off, unavailable: false, addresses: [], platform: 'win32' }
  assert.equal(poolHostingState(state).restartNeeded, true)
  assert.equal(poolHostingState({ ...state, managed: false }).restartNeeded, false)
  assert.equal(poolHostingState({ ...state, running: on }).restartNeeded, false)
})

test('a new form offers a LAN address, never all interfaces', () => {
  const off = { enabled: false, address: '', port: 7700 }
  assert.deepEqual(formForHosting({ saved: off, addresses: [{ address: '100.64.0.7', lan: false }, { address: '192.168.1.5', lan: true }] }), { ...off, address: '192.168.1.5' })
  assert.deepEqual(formForHosting({ saved: off, addresses: [{ address: '100.64.0.7', lan: false }] }), { ...off, address: '100.64.0.7' })
  assert.deepEqual(formForHosting({ saved: off, addresses: [] }), off)
  const saved = { enabled: true, address: allInterfaces, port: 7700 }
  assert.deepEqual(formForHosting({ saved, addresses: [{ address: '192.168.1.5', lan: true }] }), saved)
})

test('workers are told every address the hosting node takes them at', () => {
  const addresses = [{ address: '192.168.1.5', lan: true }, { address: '100.64.0.7', lan: false }]
  assert.deepEqual(hostingJoinAddresses({ running: { enabled: false, address: '192.168.1.5', port: 7700 }, addresses }), [])
  assert.deepEqual(hostingJoinAddresses({ running: { enabled: true, address: '192.168.1.5', port: 7700 }, addresses }), ['192.168.1.5:7700'])
  assert.deepEqual(hostingJoinAddresses({ running: { enabled: true, address: allInterfaces, port: 7701 }, addresses }), ['192.168.1.5:7701', '100.64.0.7:7701'])
})

test('the Windows firewall rule opens only the pool port, on private networks', () => {
  assert.equal(windowsFirewallCommand(7700), 'netsh advfirewall firewall add rule name="Sisyphus pool (TCP 7700)" dir=in action=allow protocol=TCP localport=7700 profile=private')
})
