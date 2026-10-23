import test from 'node:test'
import assert from 'node:assert/strict'
import { canRun, countGpuWorkers, fieldKind, firstSentence, initialValues, isValid, normalizeWorkloads, toParams, validate } from '../src/renderer/src/lib/workload-form.ts'
import type { FormField } from '../src/renderer/src/lib/workload-form.ts'

// primes as proto-loader gives it in Electron: enum names, every default
// set, and the optional bounds named in their synthetic oneofs.
const electronPrimes = {
  workloads: [{
    name: 'primes',
    description: 'Counts the prime numbers in a range. Parameters: {"from": <integer>, "to": <integer>}.',
    fields: [
      { name: 'from', kind: 'WORKLOAD_FIELD_KIND_INTEGER', label: 'From', help: 'The first number to look at.', defaultJson: '0', minimum: 0, _minimum: 'minimum', maximum: 1e12, _maximum: 'maximum', step: 0, choices: [], required: false },
      { name: 'to', kind: 'WORKLOAD_FIELD_KIND_INTEGER', label: 'Up to', help: 'Where to stop.', defaultJson: '1000000', minimum: 0, maximum: 1e12, step: 0, choices: [], required: false },
    ],
    workersRunningIt: 2,
    composite: false,
  }],
}

// transcode as a Connect client gives it in a browser: enums as numbers, and
// an unset bound left out.
const browserTranscode = {
  workloads: [{
    name: 'transcode',
    description: 'Re-encodes a stored video as H.264 video with AAC sound, cut into stretches. Parameters: ...',
    fields: [
      { name: 'input', kind: 1, label: 'Video', help: 'The stored video.', defaultJson: '', step: 0, choices: [], required: true },
      { name: 'height', kind: 2, label: 'Height', help: '', defaultJson: '0', minimum: 0, step: 2, choices: [], required: false },
      { name: 'crf', kind: 2, label: 'Quality', help: '', defaultJson: '23', minimum: 1, maximum: 51, step: 0, choices: [], required: false },
      { name: 'preset', kind: 5, label: 'Speed', help: '', defaultJson: '"medium"', step: 0, choices: ['ultrafast', 'medium', 'veryslow'], required: false },
    ],
    workersRunningIt: 0,
    composite: false,
  }, { name: 'graph', description: 'Runs jobs as a graph.', fields: [], workersRunningIt: 0, composite: true }],
}

test('kinds read as numbers, enum names or short names', () => {
  assert.equal(fieldKind(2), 'integer')
  assert.equal(fieldKind('WORKLOAD_FIELD_KIND_TOGGLE'), 'toggle')
  assert.equal(fieldKind('choice'), 'choice')
  assert.equal(fieldKind(0), 'unknown')
  assert.equal(fieldKind('WORKLOAD_FIELD_KIND_UNSPECIFIED'), 'unknown')
  assert.equal(fieldKind(undefined), 'unknown')
})

test('a workload reads the same from proto-loader and from Connect', () => {
  const [primes] = normalizeWorkloads(electronPrimes)
  assert.equal(primes.name, 'primes')
  assert.equal(primes.workersRunningIt, 2)
  assert.deepEqual(primes.fields[0], { name: 'from', kind: 'integer', label: 'From', help: 'The first number to look at.', defaultValue: 0, minimum: 0, maximum: 1e12, step: 0, choices: [], required: false })
  const [transcode, graph] = normalizeWorkloads(browserTranscode)
  assert.equal(transcode.fields[0].kind, 'file')
  assert.equal(transcode.fields[0].defaultValue, undefined)
  assert.equal(transcode.fields[0].required, true)
  assert.equal(transcode.fields[1].minimum, 0)
  assert.equal(transcode.fields[1].maximum, undefined)
  assert.equal(transcode.fields[1].step, 2)
  assert.equal(transcode.fields[3].defaultValue, 'medium')
  assert.equal(graph.composite, true)
  assert.deepEqual(graph.fields, [])
})

test('snake_case keys, string counts and a bound named unset in its oneof are read too', () => {
  const [workload] = normalizeWorkloads([{ name: 'x', workers_running_it: '3', fields: [{ name: 'n', kind: 'number', default_json: '1.5', minimum: 0, _minimum: '', maximum: '9' }] }])
  assert.equal(workload.workersRunningIt, 3)
  assert.equal(workload.fields[0].defaultValue, 1.5)
  assert.equal(workload.fields[0].minimum, undefined)
  assert.equal(workload.fields[0].maximum, 9)
})

test('nothing usable reads as no workloads, and unnamed ones are dropped', () => {
  assert.deepEqual(normalizeWorkloads(undefined), [])
  assert.deepEqual(normalizeWorkloads({}), [])
  assert.deepEqual(normalizeWorkloads({ workloads: [{ description: 'nameless' }] }), [])
  const [workload] = normalizeWorkloads({ workloads: [{ name: 'w', fields: [{ kind: 2 }, { name: 'ok', kind: 2, defaultJson: '{not json' }] }] })
  assert.deepEqual(workload.fields.map((field) => field.name), ['ok'])
  assert.equal(workload.fields[0].defaultValue, undefined)
})

test('GPU worker counts include each worker once and follow the latest worker snapshot', () => {
  assert.equal(countGpuWorkers([
    { peerId: 'cpu-only', gpus: [] },
    { peerId: 'one-card', gpus: [{ name: 'RTX 4050' }] },
    { peerId: 'two-cards', gpus: [{ name: 'RTX 4090' }, { name: 'RTX 4090' }] },
    null,
  ]), 2)
  assert.equal(countGpuWorkers([{ peerId: 'cpu-only', gpus: [] }]), 0)
  assert.equal(countGpuWorkers(undefined), 0)
})

test('a workload can run when a worker offers it, or when it is composite', () => {
  const [primes] = normalizeWorkloads(electronPrimes)
  const [transcode, graph] = normalizeWorkloads(browserTranscode)
  assert.equal(canRun(primes), true)
  assert.equal(canRun(transcode), false)
  assert.equal(canRun(graph), true)
})

test('the first sentence of a description is what the workload does', () => {
  assert.equal(firstSentence(electronPrimes.workloads[0].description), 'Counts the prime numbers in a range.')
  assert.equal(firstSentence(browserTranscode.workloads[0].description), 'Re-encodes a stored video as H.264 video with AAC sound, cut into stretches.')
  assert.equal(firstSentence('  No full stop here '), 'No full stop here')
  assert.equal(firstSentence(''), '')
})

test('defaults become the form’s first values', () => {
  const [transcode] = normalizeWorkloads(browserTranscode)
  assert.deepEqual(initialValues(transcode.fields), { input: '', height: '0', crf: '23', preset: 'medium' })
  const [primes] = normalizeWorkloads(electronPrimes)
  assert.deepEqual(initialValues(primes.fields), { from: '0', to: '1000000' })
  const [other] = normalizeWorkloads([{ name: 'o', fields: [
    { name: 't', kind: 6, defaultJson: 'true' }, { name: 'off', kind: 6 },
    { name: 'c', kind: 5, choices: ['a', 'b'], required: true }, { name: 'free', kind: 5, choices: ['a'] },
    { name: 's', kind: 4, defaultJson: '"hello"' }, { name: 'n', kind: 3 },
  ] }])
  assert.deepEqual(initialValues(other.fields), { t: true, off: false, c: 'a', free: '', s: 'hello', n: '' })
})

test('a form sent with its defaults is valid, and gives the defaults as parameters', () => {
  const [primes] = normalizeWorkloads(electronPrimes)
  const values = initialValues(primes.fields)
  assert.deepEqual(validate(primes.fields, values), {})
  assert.deepEqual(toParams(primes.fields, values), { from: 0, to: 1000000 })
})

test('each field is checked against its form', () => {
  const [transcode] = normalizeWorkloads(browserTranscode)
  const [primes] = normalizeWorkloads(electronPrimes)
  assert.deepEqual(validate(transcode.fields, initialValues(transcode.fields)), { input: { key: 'required' } })
  assert.deepEqual(validate(transcode.fields, { input: 'bafy', height: '3', crf: '52', preset: 'fastest' }), {
    height: { key: 'offStep', bound: 2 },
    crf: { key: 'aboveMaximum', bound: 51 },
    preset: { key: 'notAChoice' },
  })
  assert.deepEqual(validate(transcode.fields, { input: 'bafy', height: '720', crf: '0', preset: 'medium' }), { crf: { key: 'belowMinimum', bound: 1 } })
  assert.deepEqual(validate(primes.fields, { from: '-1', to: '1.5' }), { from: { key: 'belowMinimum', bound: 0 }, to: { key: 'notWhole' } })
  assert.deepEqual(validate(primes.fields, { from: 'ten', to: '0x10' }), { from: { key: 'notANumber' }, to: { key: 'notANumber' } })
  assert.equal(isValid(primes.fields, { from: ' 0 ', to: '1e5' }), true)
})

test('a number field checks steps that are not whole', () => {
  const [workload] = normalizeWorkloads([{ name: 'n', fields: [{ name: 'dt', kind: 3, step: 0.1, minimum: 0 }] }])
  assert.equal(isValid(workload.fields, { dt: '0.3' }), true)
  assert.equal(isValid(workload.fields, { dt: '2.05' }), false)
  assert.equal(isValid(workload.fields, { dt: '' }), true)
})

test('parameters leave out empty optional fields, and keep toggles and choices', () => {
  const [workload] = normalizeWorkloads([{ name: 'o', fields: [
    { name: 'input', kind: 1, required: true }, { name: 'height', kind: 2 }, { name: 'label', kind: 4 },
    { name: 'gpu', kind: 6 }, { name: 'preset', kind: 5, choices: ['fast', 'slow'] }, { name: 'rate', kind: 3 },
  ] }])
  assert.deepEqual(toParams(workload.fields, { input: ' bafy ', height: '', label: '  ', gpu: false, preset: 'slow', rate: '2.5' }), { input: 'bafy', gpu: false, preset: 'slow', rate: 2.5 })
})

test('a field of several files holds a list of CIDs and sends it as one', () => {
  assert.equal(fieldKind(7), 'files')
  assert.equal(fieldKind('WORKLOAD_FIELD_KIND_FILES'), 'files')
  const fields: FormField[] = [{ name: 'inputs', kind: 'files', label: 'Molecules', help: '', defaultValue: undefined, step: 0, choices: [], required: true }]
  const values = initialValues(fields)
  assert.deepEqual(values, { inputs: [] })
  assert.deepEqual(validate(fields, values), { inputs: { key: 'required' } })
  assert.deepEqual(validate(fields, { inputs: [' ', ''] }), { inputs: { key: 'required' } })
  assert.equal(isValid(fields, { inputs: ['bafyone'] }), true)
  assert.deepEqual(toParams(fields, { inputs: [' bafyone', '', 'bafytwo '] }), { inputs: ['bafyone', 'bafytwo'] })
  // A default list is kept, and anything in it but CIDs dropped.
  assert.deepEqual(initialValues([{ ...fields[0], defaultValue: ['bafyone', 3] }]), { inputs: ['bafyone'] })
  // An optional one left empty is left out, so the workload uses its own.
  assert.deepEqual(toParams([{ ...fields[0], required: false }], { inputs: [] }), {})
})
