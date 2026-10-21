// Names for the files a finished job left, so that saving one gives it a
// name and a type the owner's programs know: a re-encoded video as .ts, a
// molecule's last frame as .pdb, a rendered frame as .png, a transcript as
// .txt and its subtitles as .srt, a file a container wrote by its own name.

// What a result calls a file, and the extension that kind of file has.
const kinds: Record<string, string> = {
  output: '.ts', final: '.pdb', energies: '.csv', trajectory: '.dcd', stdout: '.txt',
  image: '.png', images: '.png', archive: '.zip',
  text: '.txt', subtitles: '.srt',
}

function decode(result?: Uint8Array): unknown {
  if (!result?.byteLength) return undefined
  try { return JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(result)) } catch { return undefined }
}

// nameInResult finds cid in a job's JSON result and names it by where it
// is found: a key that is a file name already (a container's files), or a
// known key, numbered by the entry it is in when there are several. A list
// of files of a known kind, a render's frames, numbers each by its place,
// padded so that they sort in order.
export function nameInResult(result: Uint8Array | undefined, cid: string): string | undefined {
  let found: string | undefined
  const walk = (value: unknown, place: string) => {
    if (found !== undefined) return
    if (Array.isArray(value)) {
      value.forEach((item, index) => {
        const entry = item && typeof item === 'object' && 'index' in item ? Number((item as { index: unknown }).index) : index
        walk(item, `${place}${place ? '-' : ''}${Number.isFinite(entry) ? entry : index}`)
      })
      return
    }
    if (!value || typeof value !== 'object') return
    for (const [key, item] of Object.entries(value as Record<string, unknown>)) {
      if (item === cid) {
        const named = /\.[A-Za-z0-9]{1,8}$/.test(key) ? key : kinds[key] ? `${key}${kinds[key]}` : undefined
        if (named) { found = place ? `${place}-${named}` : named; return }
      }
      if (Array.isArray(item) && kinds[key] && item.includes(cid)) {
        const at = String(item.indexOf(cid)).padStart(String(item.length - 1).length, '0')
        found = `${place}${place ? '-' : ''}${key}-${at}${kinds[key]}`
        return
      }
      walk(item, Array.isArray(item) ? `${place}${place ? '-' : ''}${key}` : place)
      if (found !== undefined) return
    }
  }
  walk(decode(result), '')
  return found?.replace(/[^\w.-]+/g, '_')
}

// extensionOf guesses a file's type from its first bytes, for a file the
// result does not name.
export function extensionOf(bytes: Uint8Array): string {
  const starts = (...b: number[]) => b.every((v, i) => bytes[i] === v)
  const ascii = (at: number, s: string) => [...s].every((c, i) => bytes[at + i] === c.charCodeAt(0))
  if (bytes.length > 188 && bytes[0] === 0x47 && bytes[188] === 0x47) return '.ts'
  if (ascii(4, 'ftyp')) return '.mp4'
  if (starts(0x76, 0x2f, 0x31, 0x01)) return '.exr'
  if (starts(0x89, 0x50, 0x4e, 0x47)) return '.png'
  if (starts(0xff, 0xd8, 0xff)) return '.jpg'
  if (ascii(0, '%PDF')) return '.pdf'
  if (starts(0x1f, 0x8b)) return '.gz'
  if (starts(0x50, 0x4b, 0x03, 0x04)) return '.zip'
  if (bytes.length === 0) return '.bin'
  try {
    const text = new TextDecoder('utf-8', { fatal: true }).decode(bytes.subarray(0, 4096))
    const trimmed = text.trimStart()
    if (trimmed.startsWith('{') || trimmed.startsWith('[')) return '.json'
    return '.txt'
  } catch { return '.bin' }
}

// outputFileName is what to save the index'th output of a job as. A frame
// a render left as OpenEXR, which its result names as a picture like any
// other, is saved as what its bytes say it is.
export function outputFileName(job: { jobId: string; result?: Uint8Array }, cid: string, index: number, bytes: Uint8Array): string {
  const short = job.jobId.slice(0, 8)
  let named = nameInResult(job.result, cid)
  if (named?.endsWith('.png') && extensionOf(bytes) === '.exr') named = `${named.slice(0, -4)}.exr`
  return named ? `${short}-${named}` : `${short}-output-${index + 1}${extensionOf(bytes)}`
}

// resultFileName is what to save a job's own result as.
export function resultFileName(job: { jobId: string; result?: Uint8Array }): string {
  return `${job.jobId.slice(0, 8)}-result${decode(job.result) === undefined ? '.bin' : '.json'}`
}
