import assert from 'node:assert/strict'
import test from 'node:test'
import { probe } from '../src/renderer/src/lib/webgl.ts'

test('a window is asked for WebGL before the globe is drawn in it', () => {
  const asked = []
  const giving = (kinds) => () => ({ getContext: (kind) => { asked.push(kind); return kinds.includes(kind) ? {} : null } })
  assert.equal(probe(giving(['webgl2'])), true)
  assert.equal(probe(giving(['webgl'])), true)
  assert.deepEqual(asked, ['webgl2', 'webgl2', 'webgl'])
  assert.equal(probe(giving([])), false)
  // A browser that throws for want of a driver has none either.
  assert.equal(probe(() => ({ getContext: () => { throw new Error('no driver') } })), false)
  assert.equal(probe(() => { throw new Error('no canvas') }), false)
})
