import test from 'node:test'
import assert from 'node:assert/strict'
import { parseImagesResult } from '../src/renderer/src/lib/images-result.ts'

const json = (value: unknown) => new TextEncoder().encode(JSON.stringify(value))

test('an images result is summed up for the page', () => {
  const summary = parseImagesResult(json({
    width: 1600, height: 1600, format: 'webp', quality: 85, keep_metadata: false, made: 1, failed: 1, tasks: 2,
    pictures: [
      { index: 1, error: 'libvips could not read or save the picture; its log says why', log: 'bafylog' },
      { index: 0, picture: 'bafywebp', name: 'image-000.webp', format: 'webp', width: 1600, height: 1067, bytes: 204811 },
    ],
    files: { 'image-000.webp': 'bafywebp' }, archive: 'bafyzip',
  }))
  assert.deepEqual(summary, {
    made: 1, failed: 1,
    pictures: [
      { index: 0, name: 'image-000.webp', format: 'webp', width: 1600, height: 1067, bytes: 204811, error: '' },
      { index: 1, name: '', format: '', width: Number.NaN, height: Number.NaN, bytes: Number.NaN, error: 'libvips could not read or save the picture; its log says why' },
    ],
  })
})

test('what is not an images result is shown raw instead', () => {
  for (const result of [undefined, new Uint8Array(), new TextEncoder().encode('not json'), json({ count: 9592 }), json({ pictures: [] }), json({ made: 'one', pictures: [] }), json(null)]) {
    assert.equal(parseImagesResult(result), null)
  }
  // A picture missing its fields still shows, in its place.
  const summary = parseImagesResult(json({ made: 0, pictures: [{ width: 'wide' }] }))
  assert.ok(summary)
  assert.equal(summary.failed, 0)
  assert.equal(summary.pictures[0].index, 0)
  assert.ok(Number.isNaN(summary.pictures[0].width))
})
