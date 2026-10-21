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

test('a render’s frames are numbered in order, and its whole frame and archive named by kind', () => {
  const frames = Array.from({ length: 12 }, (_, i) => `cframe${i}`)
  const byFrames = json({ split: 'frames', frames: 12, images: frames, archive: 'czip', width: 64, height: 64, seconds: 3 })
  assert.equal(nameInResult(byFrames, 'cframe0'), 'images-00.png')
  assert.equal(nameInResult(byFrames, 'cframe11'), 'images-11.png')
  assert.equal(nameInResult(byFrames, 'czip'), 'archive.zip')
  assert.equal(nameInResult(json({ images: ['only'] }), 'only'), 'images-0.png')
  assert.equal(nameInResult(json({ jobs: [{ index: 2, images: ['a', 'b'] }] }), 'b'), 'jobs-2-images-1.png')
  // A list of a kind not known names nothing.
  assert.equal(nameInResult(json({ things: ['cid'] }), 'cid'), undefined)
  const byTiles = json({ split: 'tiles', frames: 1, image: 'cwhole', tiles: 2 })
  assert.equal(outputFileName({ jobId: 'abcdef0123456789', result: byTiles }, 'cwhole', 0, new Uint8Array([0x89, 0x50, 0x4e, 0x47])), 'abcdef01-image.png')
  // A frame rendered as OpenEXR is saved as one.
  const exr = new Uint8Array([0x76, 0x2f, 0x31, 0x01, 2])
  assert.equal(outputFileName({ jobId: 'abcdef0123456789', result: byFrames }, 'cframe3', 3, exr), 'abcdef01-images-03.exr')
  assert.equal(outputFileName({ jobId: 'abcdef0123456789', result: byFrames }, 'czip', 12, new Uint8Array([0x50, 0x4b, 3, 4])), 'abcdef01-archive.zip')
})

test('a transcript is saved as text and its subtitles as SRT', () => {
  const job = { jobId: 'abcdef0123456789', result: json({ text: 'bafytext', subtitles: 'bafysrt', segments: 4, seconds: 61.5, words: 140 }) }
  assert.equal(outputFileName(job, 'bafytext', 0, new Uint8Array()), 'abcdef01-text.txt')
  assert.equal(outputFileName(job, 'bafysrt', 1, new Uint8Array()), 'abcdef01-subtitles.srt')
})

test('a quantum chemistry job’s table, geometries and frequencies are saved as CSV, XYZ and text', () => {
  const job = { jobId: 'abcdef0123456789', result: json({ method: 'gfn2', job: 'ohess', structures: 12, converged: 12, lowest: 0, csv: 'bafycsv', xyz: 'bafyxyz', results: [{ index: 0, title: 'anti', geometry: 'bafyg0', frequencies: 'bafyf0' }, { index: 11, title: 'gauche', geometry: 'bafyg11', frequencies: 'bafyf11' }] }) }
  assert.equal(outputFileName(job, 'bafycsv', 0, new Uint8Array()), 'abcdef01-csv.csv')
  assert.equal(outputFileName(job, 'bafyxyz', 1, new Uint8Array()), 'abcdef01-xyz.xyz')
  assert.equal(outputFileName(job, 'bafyg11', 2, new Uint8Array()), 'abcdef01-results-11-geometry.xyz')
  assert.equal(outputFileName(job, 'bafyf0', 3, new Uint8Array()), 'abcdef01-results-0-frequencies.txt')
})

test('a scan’s text is saved as text, and its searchable PDFs as PDFs in order', () => {
  const whole = { jobId: 'abcdef0123456789', result: json({ pages: 3, text: 'bafytext', pdf: 'bafypdf', characters: 55, language: 'eng', tasks: 1 }) }
  assert.equal(outputFileName(whole, 'bafytext', 0, new Uint8Array()), 'abcdef01-text.txt')
  assert.equal(outputFileName(whole, 'bafypdf', 1, new Uint8Array()), 'abcdef01-pdf.pdf')
  const parts = { jobId: 'abcdef0123456789', result: json({ pages: 30, text: 'bafytext', pdf: '', pdfs: ['bafyp0', 'bafyp1', 'bafyp2'], archive: 'bafyzip', characters: 900, language: 'eng+heb', tasks: 3 }) }
  assert.equal(outputFileName(parts, 'bafyp0', 1, new Uint8Array()), 'abcdef01-pdfs-0.pdf')
  assert.equal(outputFileName(parts, 'bafyp2', 3, new Uint8Array()), 'abcdef01-pdfs-2.pdf')
  assert.equal(outputFileName(parts, 'bafyzip', 4, new Uint8Array()), 'abcdef01-archive.zip')
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
  assert.equal(extensionOf(new Uint8Array([0x76, 0x2f, 0x31, 0x01])), '.exr')
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
