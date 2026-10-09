import { Buffer } from 'node:buffer'

export const MAX_FILE_DOWNLOAD_BYTES = 256 * 1024 * 1024

type DownloadStream = {
  on: (event: string, callback: (...args: any[]) => void) => unknown
  cancel: () => void
}

export function collectDownload(stream: DownloadStream, limit = MAX_FILE_DOWNLOAD_BYTES) {
  let settled = false
  let chunks: Buffer[] = []
  let total = 0
  let rejectDownload: (error: Error) => void
  const fail = (error: Error, cancel: boolean) => {
    if (settled) return
    settled = true
    chunks = []
    rejectDownload(error)
    if (cancel) stream.cancel()
  }
  const promise = new Promise<Uint8Array>((resolve, reject) => {
    rejectDownload = reject
    stream.on('data', (response: { data: Uint8Array }) => {
      if (settled) return
      if (response.data.byteLength > limit - total) {
        fail(new Error('The file exceeds this desktop client’s 256 MiB download limit.'), true)
        return
      }
      total += response.data.byteLength
      chunks.push(Buffer.from(response.data))
    })
    stream.on('error', (error: Error) => fail(error, false))
    stream.on('end', () => {
      if (settled) return
      settled = true
      const data = Buffer.concat(chunks, total)
      chunks = []
      resolve(data)
    })
  })
  return { promise, cancel: () => fail(new Error('The file download was cancelled.'), true) }
}
