export function validTaskTimeout(value: number): boolean {
  return Number.isInteger(value) && value >= 0 && value <= 0xffffffff
}

export function formatJobResult(bytes: Uint8Array): string | null {
  if (!bytes.byteLength) return ''
  try {
    const text = new TextDecoder('utf-8', { fatal: true }).decode(bytes)
    if (/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/.test(text)) return null
    try { return JSON.stringify(JSON.parse(text), null, 2) } catch { return text }
  } catch { return null }
}
