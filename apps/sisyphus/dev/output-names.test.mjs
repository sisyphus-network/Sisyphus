import test from 'node:test'
import assert from 'node:assert/strict'
import { extensionOf, nameInResult, outputFileName, resultFileName } from '../src/renderer/src/lib/output-names.ts'

const json = (value) => new TextEncoder().encode(JSON.stringify(value))

test('a re-encoded video is saved as a transport stream', () => {
  const job = { jobId: 'abcdef0123456789', result: json({ output: 'bafyvideo', segments: 3, bytes: 99 }) }
  assert.equal(outputFileName(job, 'bafyvideo', 0, new Uint8Array()), 'abcdef01-output.ts')
})

test('a molecule’s files are named by replica and kind', () => {
  const result = json({ replicas: 2, runs: [
    { index: 0, final: 'c0f', energies: 'c0e', trajectory: 'c0t' },
    { index: 1, final: 'c1f', energies: 'c1e', trajectory: 'c1t' },
  ] })
  assert.equal(nameInResult(result, 'c1f'), 'runs-1-final.pdb')
  assert.equal(nameInResult(result, 'c0e'), 'runs-0-energies.csv')
  assert.equal(nameInResult(result, 'c1t'), 'runs-1-trajectory.dcd')
})

test('a container’s files keep the names it gave them', () => {
  const result = json({ tasks: [{ index: 4, stdout: 'cout', files: { 'frame 0001.png': 'cpng', '../x': 'cbad' } }] })
  assert.equal(nameInResult(result, 'cpng'), 'tasks-4-frame_0001.png')
  assert.equal(nameInResult(result, 'cout'), 'tasks-4-stdout.txt')
  // A key that is no file name and no known kind names nothing.
  assert.equal(nameInResult(result, 'cbad'), undefined)
  assert.equal(nameInResult(json({ something: 'cid' }), 'cid'), undefined)
  assert.equal(nameInResult(new TextEncoder().encode('not json'), 'cid'), undefined)
  assert.equal(nameInResult(undefined, 'cid'), undefined)
})

test('an output the result does not name is typed by its first bytes', () => {
  const ts = new Uint8Array(400); ts[0] = 0x47; ts[188] = 0x47
  assert.equal(extensionOf(ts), '.ts')
  assert.equal(extensionOf(new Uint8Array([0, 0, 0, 0x18, ...new TextEncoder().encode('ftypisom')])), '.mp4')
  assert.equal(extensionOf(new Uint8Array([0x89, 0x50, 0x4e, 0x47, 1])), '.png')
  assert.equal(extensionOf(new Uint8Array([0xff, 0xd8, 0xff, 0xe0])), '.jpg')
  assert.equal(extensionOf(new TextEncoder().encode('%PDF-1.7')), '.pdf')
  assert.equal(extensionOf(new Uint8Array([0x1f, 0x8b, 8])), '.gz')
  assert.equal(extensionOf(new Uint8Array([0x50, 0x4b, 3, 4])), '.zip')
  assert.equal(extensionOf(new TextEncoder().encode('  {"the": 1}')), '.json')
  assert.equal(extensionOf(new TextEncoder().encode('the boulder 3\n')), '.txt')
  assert.equal(extensionOf(new Uint8Array([0xc3, 0x28, 0xff])), '.bin')
  assert.equal(extensionOf(new Uint8Array()), '.bin')
  assert.equal(outputFileName({ jobId: 'abcdef0123456789' }, 'cid', 1, new TextEncoder().encode('words')), 'abcdef01-output-2.txt')
})

test('a result is saved as JSON when it is JSON', () => {
  assert.equal(resultFileName({ jobId: 'abcdef0123456789', result: json({ count: 9592 }) }), 'abcdef01-result.json')
  assert.equal(resultFileName({ jobId: 'abcdef0123456789', result: new Uint8Array([0xff, 0]) }), 'abcdef01-result.bin')
})
