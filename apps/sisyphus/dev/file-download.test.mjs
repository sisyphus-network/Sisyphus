import assert from 'node:assert/strict'
import { EventEmitter } from 'node:events'
import { test } from 'node:test'
import { collectDownload, MAX_FILE_DOWNLOAD_BYTES } from '../src/main/file-download.ts'
import { WindowStreams } from '../src/main/window-streams.ts'

function source() {
  const stream = new EventEmitter()
  stream.cancellations = 0
  stream.cancel = () => { stream.cancellations++; stream.emit('error', new Error('cancelled')) }
  return stream
}

test('collects bytes in order at the exact size limit', async () => {
  const stream = source(), transfer = collectDownload(stream, 4)
  stream.emit('data', { data: new Uint8Array([1, 2]) })
  stream.emit('data', { data: new Uint8Array([3, 4]) })
  stream.emit('end')
  assert.deepEqual([...await transfer.promise], [1, 2, 3, 4])
  assert.equal(stream.cancellations, 0)
  assert.equal(MAX_FILE_DOWNLOAD_BYTES, 256 * 1024 * 1024)
})

test('rejects and cancels before buffering a chunk beyond the limit', async () => {
  const stream = source(), transfer = collectDownload(stream, 2)
  const rejection = assert.rejects(transfer.promise, /download limit/)
  stream.emit('data', { data: new Uint8Array([1, 2, 3]) })
  stream.emit('end'); transfer.cancel()
  await rejection
  assert.equal(stream.cancellations, 1)
})

test('window cleanup rejects its transfer and ignores late data and end', async () => {
  const stream = source(), transfer = collectDownload(stream), registry = new WindowStreams()
  const rejection = assert.rejects(transfer.promise, /download was cancelled/)
  registry.add(1, 'download', transfer)
  registry.clear(1)
  stream.emit('data', { data: new Uint8Array([1]) }); stream.emit('end')
  await rejection
  assert.equal(stream.cancellations, 1)
})

test('stream errors reject rather than return partial bytes', async () => {
  const stream = source(), transfer = collectDownload(stream)
  const rejection = assert.rejects(transfer.promise, /offline/)
  stream.emit('data', { data: new Uint8Array([1]) })
  stream.emit('error', new Error('offline')); stream.emit('end')
  await rejection
})

test('empty files complete and cancellation after completion is harmless', async () => {
  const stream = source(), transfer = collectDownload(stream)
  stream.emit('end')
  assert.equal((await transfer.promise).byteLength, 0)
  transfer.cancel()
  assert.equal(stream.cancellations, 0)
})
