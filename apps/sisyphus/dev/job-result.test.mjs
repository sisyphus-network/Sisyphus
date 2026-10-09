import assert from 'node:assert/strict'
import { test } from 'node:test'
import { formatJobResult, validTaskTimeout } from '../src/renderer/src/lib/job-result.ts'

test('timeout accepts the protobuf uint32 range, including zero for default', () => {
  for (const value of [0, 1, 60, 0xffffffff]) assert.equal(validTaskTimeout(value), true)
  for (const value of [-1, 0.5, NaN, Infinity, 0x100000000]) assert.equal(validTaskTimeout(value), false)
})
test('JSON results are readable and plain multilingual text is preserved', () => {
  const encode = (value) => new TextEncoder().encode(value)
  assert.equal(formatJobResult(encode('{"count":25}')), '{\n  "count": 25\n}')
  assert.equal(formatJobResult(encode('שלום\nhello')), 'שלום\nhello')
  assert.equal(formatJobResult(encode('0')), '0')
})
test('binary and malformed UTF-8 are not rendered as corrupt text', () => {
  assert.equal(formatJobResult(new Uint8Array([0, 1, 2])), null)
  assert.equal(formatJobResult(new Uint8Array([0xff])), null)
  assert.equal(formatJobResult(new Uint8Array()), '')
})
