import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createIpcLogEntry, sanitizeIpcLogValue } from '../src/main/ipc-logger.ts'

test('redacts credential-like fields recursively and sensitive URL query values', () => {
  const value = sanitizeIpcLogValue({
    apiKey: 'key-value',
    nodeApiToken: 'node-token',
    nested: { authorization: 'Bearer token', pairing_code: '012345', ok: 'https://host.test/?token=abc&mode=x' },
  })
  assert.deepEqual(value, {
    apiKey: '[REDACTED]',
    nodeApiToken: '[REDACTED]',
    nested: { authorization: '[REDACTED]', pairing_code: '[REDACTED]', ok: 'https://host.test/?token=[REDACTED]&mode=x' },
  })
})

test('bounds strings, arrays, objects and binary data', () => {
  assert.match(sanitizeIpcLogValue('x'.repeat(4_100)), /truncated 100 chars/)
  assert.equal(sanitizeIpcLogValue(new Uint8Array(12)), '[Uint8Array: 12 bytes]')
  assert.equal(sanitizeIpcLogValue(new ArrayBuffer(8)), '[ArrayBuffer: 8 bytes]')
  const items = sanitizeIpcLogValue(Array.from({ length: 52 }, (_, index) => index))
  assert.ok(Array.isArray(items))
  assert.equal(items.length, 51)
  const fields = sanitizeIpcLogValue(Object.fromEntries(Array.from({ length: 82 }, (_, i) => [`k${i}`, i])))
  assert.ok(fields && typeof fields === 'object')
  assert.equal(Object.keys(fields).length, 81)
})

test('returns structured, sanitized IPC log entries', () => {
  const entry = createIpcLogEntry({ phase: 'response', channel: 'node:rpc', timestamp: 'now', request: { password: 'secret' }, response: { ok: true }, durationMs: 12 })
  assert.deepEqual(entry.request, { password: '[REDACTED]' })
  assert.deepEqual(entry.response, { ok: true })
  assert.equal(entry.durationMs, 12)
})
