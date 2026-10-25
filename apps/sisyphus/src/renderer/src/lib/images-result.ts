// An images job's result, as the Run page shows it: how many pictures were
// made, and each one's name, format, size, or why it could not be made.

export type ImagesPicture = { index: number; name: string; format: string; width: number; height: number; bytes: number; error: string }
export type ImagesSummary = { made: number; failed: number; pictures: ImagesPicture[] }

const number = (value: unknown) => (typeof value === 'number' && Number.isFinite(value) ? value : Number.NaN)
const text = (value: unknown) => (typeof value === 'string' ? value : '')

// parseImagesResult reads an images job's JSON result, or returns null for
// anything that is not one, so the page can fall back to showing it raw.
export function parseImagesResult(result?: Uint8Array): ImagesSummary | null {
  if (!result?.byteLength) return null
  let value: Record<string, unknown>
  try { value = JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(result)) } catch { return null }
  if (!value || typeof value !== 'object' || !Array.isArray(value.pictures) || !Number.isInteger(value.made)) return null
  const pictures = value.pictures.map((picture: Record<string, unknown>, position: number) => ({
    index: Number.isInteger(picture?.index) ? picture.index as number : position,
    name: text(picture?.name), format: text(picture?.format), error: text(picture?.error),
    width: number(picture?.width), height: number(picture?.height), bytes: number(picture?.bytes),
  })).sort((a: ImagesPicture, b: ImagesPicture) => a.index - b.index)
  return { made: value.made as number, failed: Number.isInteger(value.failed) ? value.failed as number : 0, pictures }
}
