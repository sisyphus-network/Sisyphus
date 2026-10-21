import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { cityEstimateFromProtocol, connectionOf, escapeHtml, hostOf, nodeDetails, peerDetails, poolWorkers, precisionOf, renderDetailsHtml } from '../src/renderer/src/lib/globe-details.ts'

const locales = ['en', 'he', 'ar', 'ru', 'pl', 'es', 'fr']
const dictionaries = Object.fromEntries(locales.map((locale) => [locale, JSON.parse(readFileSync(new URL(`../src/renderer/src/i18n/messages/${locale}.json`, import.meta.url), 'utf8'))]))
const labels = dictionaries.en.globeDetails
const ltr = { direction: 'ltr', locale: 'en' }

// What a ListWorkers reply looks like from the Electron client: 64-bit
// numbers as strings.
const rig = {
  peerId: '12D3KooWRig', name: 'rig', hostname: 'rig.lan', os: 'linux', arch: 'amd64', cpuModel: 'AMD Ryzen 9 7950X', cpuCores: 32,
  memoryBytes: String(64 * 1024 ** 3), gpus: [{ name: 'RTX 4050', memoryBytes: String(6 * 1024 ** 3) }], models: ['qwen3:8b', 'llama3.2'],
  taskSlots: 4, runningTasks: 1, sharedLocation: { latitude: 37.3, longitude: -79.9 },
}

test('a worker is read with everything its machine says, from either client', () => {
  const workers = poolWorkers([rig, { peerId: 'bare' }, null, { name: 'no id' }])
  assert.deepEqual([...workers.keys()], ['12D3KooWRig', 'bare'])
  const worker = workers.get('12D3KooWRig')
  assert.equal(worker.memoryBytes, 64 * 1024 ** 3)
  assert.deepEqual(worker.gpus, [{ name: 'RTX 4050', memoryBytes: 6 * 1024 ** 3 }])
  assert.deepEqual(worker.place, { lat: 37.3, lng: -79.9 })
  assert.deepEqual(workers.get('bare'), { peerId: 'bare', name: '', hostname: '', os: '', arch: '', cpuModel: '', cpuCores: 0, memoryBytes: 0, gpus: [], models: [], taskSlots: 0, runningTasks: 0, place: undefined })
  for (const nonsense of [undefined, null, {}, 'x']) assert.equal(poolWorkers(nonsense).size, 0)
})

test('the peer card has the full peer ID, its state, its addresses and IPs, its country and how work flows', () => {
  const peerId = '12D3KooWQmZ8f6bK2r9hJwXaYtLp3NcVdE4sRuT1oPqA7xyz'
  const details = peerDetails(peerId, {
    peerId, connectionState: 'PEER_CONNECTION_STATE_CONNECTED', countryCode: 'il', trustedForCompute: true, worksForThisNode: true,
    knownAddresses: ['/ip4/192.0.2.7/tcp/7700', '/ip4/192.0.2.7/udp/7700/quic-v1', '/ip4/192.0.2.7/tcp/7700', '/ip6/2001:db8::7/tcp/7700', '/dns4/peer.example.org/tcp/443'],
  })
  assert.equal(details.kind, 'peer')
  assert.equal(details.connection, 'connected')
  assert.equal(details.countryCode, 'IL')
  assert.deepEqual(details.addresses, ['/ip4/192.0.2.7/tcp/7700', '/ip4/192.0.2.7/udp/7700/quic-v1', '/ip6/2001:db8::7/tcp/7700', '/dns4/peer.example.org/tcp/443'])
  assert.deepEqual(details.ips, ['192.0.2.7', '2001:db8::7', 'peer.example.org'])
  // A daemon that does not tell the two sides of trust apart gives work
  // both ways to a trusted peer.
  assert.deepEqual(details.roles, ['trusted', 'worksForThisNode', 'givesWork', 'takesWork'])

  const html = renderDetailsHtml(details, labels, ltr)
  assert.ok(html.includes(peerId), 'the peer ID is cut short')
  for (const text of ['Connected', 'Known addresses', '/ip4/192.0.2.7/udp/7700/quic-v1', 'IP addresses', '2001:db8::7', 'Trusted for compute', 'Working for this node now', 'This node gives it work', 'This node takes its work', 'Click to keep this open']) {
    assert.ok(html.includes(text), `the card does not say ${text}`)
  }
  assert.ok(!html.includes('Pool worker'))
})

test('trust is told apart from work given and taken, and an untrusted peer says so', () => {
  assert.deepEqual(peerDetails('p', { peerId: 'p', trustedForCompute: true, givesWork: false, takesWork: true, thisNodeWorksFor: true }).roles, ['trusted', 'thisNodeWorksFor', 'takesWork'])
  assert.deepEqual(peerDetails('p', { peerId: 'p', trustedForCompute: false }).roles, ['untrusted'])
  assert.equal(connectionOf(2), 'connected')
  assert.equal(connectionOf(1), 'disconnected')
  assert.equal(connectionOf('PEER_CONNECTION_STATE_DISCONNECTED'), 'disconnected')
  assert.equal(connectionOf(0), 'unknown')
  assert.equal(connectionOf(undefined), 'unknown')
})

test('a peer with many addresses shows six, and says how many more', () => {
  const knownAddresses = Array.from({ length: 10 }, (_, index) => `/ip4/198.51.100.${index + 1}/tcp/7700`)
  const html = renderDetailsHtml(peerDetails('p', { peerId: 'p', knownAddresses }), labels, ltr)
  assert.ok(html.includes('198.51.100.6/tcp'))
  assert.ok(!html.includes('198.51.100.7/tcp'))
  // Both lists, the addresses and their IPs, are cut at six.
  assert.equal(html.split('+4 more').length - 1, 2)
  assert.ok(renderDetailsHtml(peerDetails('p', { peerId: 'p', knownAddresses }), labels, { ...ltr, maxAddresses: 20 }).includes('198.51.100.10/tcp'))
})

test('a pool worker says what its machine has and where it shares it is', () => {
  const worker = poolWorkers([rig]).get('12D3KooWRig')
  const details = peerDetails('12D3KooWRig', undefined, worker)
  assert.equal(details.kind, 'worker')
  assert.equal(details.connection, undefined, 'a worker known only from ListWorkers has no connection state to show')
  assert.deepEqual(details.place, { lat: 37.3, lng: -79.9, precision: 'city' })
  const html = renderDetailsHtml(details, labels, ltr)
  for (const text of ['rig', 'Pool worker', 'rig.lan', 'linux / amd64', 'AMD Ryzen 9 7950X', '32 cores', '64 GB', 'RTX 4050 · 6 GB', 'qwen3:8b', 'llama3.2', '1 of 4 running', '37.3, -79.9', 'shared at about city precision']) {
    assert.ok(html.includes(text), `the card does not say ${text}: ${html}`)
  }
  // Nothing about trust, from a worker this node has no peer record of.
  assert.ok(!html.includes('Compute'))
})

test('how precisely a place was shared is read from its numbers', () => {
  assert.equal(precisionOf({ lat: 37.3, lng: -79.9 }), 'city')
  assert.equal(precisionOf({ lat: 0, lng: 0 }), 'city')
  assert.equal(precisionOf({ lat: 37.27, lng: -79.94 }), 'about-1km')
  assert.equal(precisionOf({ lat: 37.271, lng: -79.9414 }), undefined)
  const exact = renderDetailsHtml(peerDetails('w', undefined, { ...poolWorkers([rig]).get('12D3KooWRig'), place: { lat: 37.271, lng: -79.9414 } }), labels, ltr)
  assert.ok(exact.includes('37.271, -79.9414'))
  assert.ok(!exact.includes('precision'), 'a place given precisely is said to be rounded')
  const near = renderDetailsHtml(peerDetails('w', undefined, poolWorkers([{ peerId: 'w', sharedLocation: { latitude: 37.27, longitude: -79.94 } }]).get('w')), labels, ltr)
  assert.ok(near.includes('shared at about 1 km precision'))
})

test('this node’s card has its ID, version, listen addresses, country and source, city and pin', () => {
  const details = nodeDetails({
    peerId: '12D3KooWSelf', daemonVersion: 'v0.42.0', listenAddresses: ['/ip4/127.0.0.1/tcp/7700', '/ip4/127.0.0.1/tcp/7700'], countryCode: 'us', countrySource: 'pool',
    cityEstimate: cityEstimateFromProtocol({ city: 'Roanoke', region: 'Virginia', countryCode: 'us', source: 'stun', attribution: 'IP Geolocation by DB-IP' }),
    pin: { lat: 37.271, lng: -79.9414, share: 'about-1km' },
  })
  assert.deepEqual(details.addresses, ['/ip4/127.0.0.1/tcp/7700'])
  const html = renderDetailsHtml(details, labels, ltr)
  for (const text of ['This node', '12D3KooWSelf', 'v0.42.0', 'Listen addresses', '/ip4/127.0.0.1/tcp/7700', 'US', 'as the pool sees it', 'Roanoke, Virginia, US', 'IP Geolocation by DB-IP', '37.271, -79.9414', 'shared at about 1 km']) {
    assert.ok(html.includes(text), `the card does not say ${text}`)
  }
  assert.ok(!html.includes('Connection'), 'this node has no connection to itself')
  assert.equal(cityEstimateFromProtocol(undefined), undefined)
  assert.equal(cityEstimateFromProtocol({ countryCode: 'US' }), undefined, 'a city estimate with no city or region places nothing')
})

test('what is not known is left out, not shown empty', () => {
  const html = renderDetailsHtml(nodeDetails({ peerId: 'self' }), labels, ltr)
  for (const label of ['Daemon version', 'Listen addresses', 'IP addresses', 'Country', 'City estimate', 'Pin', 'Processor', 'Memory']) {
    assert.ok(!html.includes(`<dt>${label}</dt>`), `${label} is shown with nothing in it`)
  }
  assert.ok(html.includes('<dt>Peer ID</dt>'))
  const peer = renderDetailsHtml(peerDetails('p', { peerId: 'p', connectionState: 1, knownAddresses: [], countryCode: '' }), labels, ltr)
  assert.ok(!peer.includes('Known addresses'))
  assert.ok(!peer.includes('<dt>Country</dt>'))
  assert.ok(peer.includes('Not connected'))
  assert.ok(peer.includes('<dt>Compute</dt>'))
})

test('everything a machine says of itself is escaped', () => {
  const attack = '<script>alert(1)</script>'
  const worker = poolWorkers([{ peerId: `p"><img src=x onerror=alert(1)>`, name: attack, hostname: `${attack}.lan`, os: '<b>', arch: '"', cpuModel: '<i>cpu', gpus: [{ name: '<svg onload=alert(1)>' }], models: ['<iframe>'] }])
  const [peerId] = worker.keys()
  const html = renderDetailsHtml(peerDetails(peerId, { peerId, knownAddresses: ['/dns4/<x>/tcp/1'], countryCode: '<u>' }, worker.get(peerId)), labels, ltr)
  for (const tag of ['<script', '<img', '<b>', '<i>', '<svg', '<iframe', '<x>', '<u>']) assert.ok(!html.includes(tag), `${tag} reached the card unescaped`)
  assert.ok(html.includes('&lt;script&gt;alert(1)&lt;/script&gt;.lan'))
  assert.ok(html.includes('p&quot;&gt;&lt;img'))
  // A translation is text too.
  assert.ok(renderDetailsHtml(peerDetails('p'), { ...labels, peer: '<b>Peer</b>' }, ltr).includes('&lt;b&gt;Peer&lt;/b&gt;'))
  assert.equal(escapeHtml(`&<>"'`), '&amp;&lt;&gt;&quot;&#39;')
})

test('a right-to-left card keeps IDs, addresses and coordinates left to right', () => {
  const he = dictionaries.he.globeDetails
  const html = renderDetailsHtml(peerDetails('12D3KooWPeer', { peerId: '12D3KooWPeer', knownAddresses: ['/ip4/192.0.2.1/tcp/7700'], countryCode: 'IL', trustedForCompute: true }, poolWorkers([rig]).get('12D3KooWRig')), he, { direction: 'rtl', locale: 'he' })
  assert.ok(html.startsWith('<div class="globe-card" dir="rtl">'))
  for (const value of ['12D3KooWPeer', '/ip4/192.0.2.1/tcp/7700', '192.0.2.1', 'IL', '37.3, -79.9', 'linux / amd64']) {
    assert.ok(html.includes(`<bdi dir="ltr">${value}</bdi>`), `${value} is not isolated left to right`)
  }
  // A name its owner chose is isolated in its own direction.
  assert.ok(html.includes('<bdi>rig.lan</bdi>'))
  assert.ok(html.includes(he.trusted))
  assert.ok(!html.includes('Trusted for compute'))
  // A direction is one of two words, whatever is passed.
  assert.ok(renderDetailsHtml(peerDetails('p'), labels, { direction: '"><script>', locale: 'en' }).startsWith('<div class="globe-card" dir="ltr">'))
})

test('hosts are read from multiaddrs and plain addresses alike', () => {
  assert.equal(hostOf('/ip4/10.0.0.1/tcp/1'), '10.0.0.1')
  assert.equal(hostOf('/ip6/::1/tcp/1'), '::1')
  assert.equal(hostOf('/dns/example.org/tcp/1'), 'example.org')
  assert.equal(hostOf('/p2p-circuit/p2p/12D3'), undefined)
  assert.equal(hostOf('192.0.2.1:7700'), '192.0.2.1')
  assert.equal(hostOf('[2001:db8::1]:7700'), '2001:db8::1')
})

test('every locale has every card label, with the same placeholders', () => {
  const keys = Object.keys(labels).sort()
  for (const locale of locales) {
    const card = dictionaries[locale].globeDetails
    assert.deepEqual(Object.keys(card).sort(), keys, locale)
    for (const key of keys) {
      assert.ok(typeof card[key] === 'string' && card[key].trim() !== '', `${locale}.globeDetails.${key}`)
      assert.deepEqual(card[key].match(/\{\w+\}/g)?.sort() ?? [], labels[key].match(/\{\w+\}/g)?.sort() ?? [], `${locale}.globeDetails.${key} placeholders`)
    }
    if (locale !== 'en') assert.notEqual(card.copyPeerId, labels.copyPeerId, `${locale} is not translated`)
  }
})
