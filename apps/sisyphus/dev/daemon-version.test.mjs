import assert from 'node:assert/strict'
import { test } from 'node:test'
import { displayDaemonVersion } from '../src/renderer/src/lib/daemon-version.ts'

test('only semantic daemon versions receive a v prefix', () => {
  for (const [input, expected] of [
    ['dev', 'dev'],
    ['test-a1b2c3', 'test-a1b2c3'],
    ['v1.2.3', 'v1.2.3'],
    ['1.2.3', 'v1.2.3'],
    ['1.2.3-rc.1', 'v1.2.3-rc.1'],
    ['1.2.3+build.4', 'v1.2.3+build.4'],
  ]) assert.equal(displayDaemonVersion(input), expected, input)
})
