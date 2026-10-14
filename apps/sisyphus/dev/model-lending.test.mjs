import assert from 'node:assert/strict'
import test from 'node:test'
import { isModelLendingDirty } from '../src/renderer/src/lib/model-lending.ts'

const emptyLending = {
  enabled: false,
  ollamaUrl: 'http://127.0.0.1:11434',
  only: [],
  gpuLayers: -1,
  threads: 0,
  context: 0,
  keepAlive: '',
}

test('Protobuf-ES message metadata does not make unchanged lending settings dirty', () => {
  const saved = { $typeName: 'sisyphus.node.v1.ModelLending', ...emptyLending }
  const form = { ...emptyLending, ...saved }

  assert.equal(isModelLendingDirty(saved, form), false)
})

test('lending settings become dirty when a saved field changes', () => {
  const saved = { $typeName: 'sisyphus.node.v1.ModelLending', ...emptyLending }

  assert.equal(isModelLendingDirty(saved, { ...emptyLending, enabled: true }), true)
  assert.equal(isModelLendingDirty(saved, { ...emptyLending, only: ['llama3.1:8b'] }), true)
})

test('lending settings are not dirty until saved settings have loaded', () => {
  assert.equal(isModelLendingDirty(undefined, { ...emptyLending, enabled: true }), false)
})
