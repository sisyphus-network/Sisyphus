import assert from 'node:assert/strict'
import { test } from 'node:test'
import { WindowStreams } from '../src/main/window-streams.ts'
import { isTrustedRendererUrl } from '../src/main/renderer-policy.ts'

const stream = () => ({ cancellations: 0, cancel() { this.cancellations++ } })

test('closing a window cancels only that window’s streams', () => {
  const registry = new WindowStreams(), a = stream(), b = stream()
  registry.add(1, 'same-id', a); registry.add(2, 'same-id', b)
  registry.clear(1)
  assert.equal(a.cancellations, 1)
  assert.equal(b.cancellations, 0)
  assert.equal(registry.has(2, 'same-id', b), true)
  registry.clear(1)
  assert.equal(a.cancellations, 1)
})

test('one window cannot stop another window’s stream', () => {
  const registry = new WindowStreams(), a = stream()
  registry.add(1, 'id', a)
  registry.stop(2, 'id')
  assert.equal(a.cancellations, 0)
  registry.stop(1, 'id')
  assert.equal(a.cancellations, 1)
})

test('late events from a replaced stream cannot release its replacement', () => {
  const registry = new WindowStreams(), a = stream(), b = stream()
  registry.add(1, 'id', a); registry.add(1, 'id', b)
  assert.equal(a.cancellations, 1)
  registry.release(1, 'id', a)
  assert.equal(registry.has(1, 'id', b), true)
})

test('connection teardown cancels all owners exactly once', () => {
  const registry = new WindowStreams(), a = stream(), b = stream()
  registry.add(1, 'a', a); registry.add(2, 'b', b)
  registry.clearAll(); registry.clearAll()
  assert.equal(a.cancellations, 1)
  assert.equal(b.cancellations, 1)
})

test('normally completed streams are not cancelled during later teardown', () => {
  const registry = new WindowStreams(), a = stream()
  registry.add(1, 'id', a); registry.release(1, 'id', a); registry.clearAll()
  assert.equal(a.cancellations, 0)
})

test('development allows router paths only at the configured origin', () => {
  const expected = 'http://localhost:5173/'
  assert.equal(isTrustedRendererUrl('http://localhost:5173/operations?job=1', expected, true), true)
  for (const candidate of ['http://localhost:5174/', 'https://localhost:5173/', 'http://localhost:5173.attacker.invalid/', 'https://attacker.invalid/', 'file:///tmp/index.html', 'not a URL']) {
    assert.equal(isTrustedRendererUrl(candidate, expected, true), false)
  }
})

test('production allows only the packaged renderer, including hash routes', () => {
  for (const expected of ['file:///opt/Sisyphus/renderer/index.html', 'file:///C:/Sisyphus/renderer/index.html']) {
    assert.equal(isTrustedRendererUrl(expected + '#/settings', expected, false), true)
    assert.equal(isTrustedRendererUrl(expected + '?file=other', expected, false), false)
    assert.equal(isTrustedRendererUrl(expected.replace('index.html', 'other.html'), expected, false), false)
  }
  assert.equal(isTrustedRendererUrl('https://attacker.invalid/', 'file:///app/index.html', false), false)
})
