const sensitiveField = /(?:api.?key|access.?token|authorization|password|secret|private.?key|cookie|credential|pairing.?code|token|(?:^|[_-])(?:code|otp)(?:$|[_-]))/i
const maxStringLength = 4_000
const maxArrayItems = 50
const maxObjectEntries = 80
const maxDepth = 6

export type IpcLogEntry = {
  phase: 'request' | 'response' | 'error' | 'event'
  channel: string
  timestamp: string
  durationMs?: number
  request?: unknown
  response?: unknown
  error?: unknown
}

export function sanitizeIpcLogValue(value: unknown, depth = 0, seen = new WeakSet<object>()): unknown {
  if (value === null || value === undefined || typeof value === 'boolean' || typeof value === 'number') return value
  if (typeof value === 'bigint') return value.toString()
  if (typeof value === 'string') {
    const bounded = value.length > maxStringLength ? `${value.slice(0, maxStringLength)}… [truncated ${value.length - maxStringLength} chars]` : value
    return bounded
      .replace(/([?&#](?:pair(?:ing)?[_-]?code|code|token|api[_-]?key|password)=)[^&#\s]+/gi, '$1[REDACTED]')
      .replace(/\bBearer\s+[^\s,;]+/gi, 'Bearer [REDACTED]')
  }
  if (value instanceof Uint8Array) return `[Uint8Array: ${value.byteLength} bytes]`
  if (value instanceof ArrayBuffer) return `[ArrayBuffer: ${value.byteLength} bytes]`
  if (value instanceof Error) return { name: value.name, message: sanitizeIpcLogValue(value.message) }
  if (typeof value !== 'object') return String(value)
  if (depth >= maxDepth) return '[Max depth]'
  if (seen.has(value)) return '[Circular]'
  seen.add(value)

  if (Array.isArray(value)) {
    const items = value.slice(0, maxArrayItems).map((item) => sanitizeIpcLogValue(item, depth + 1, seen))
    if (value.length > maxArrayItems) items.push(`[${value.length - maxArrayItems} more items]`)
    return items
  }

  const allEntries = Object.entries(value)
  const entries = allEntries.slice(0, maxObjectEntries).map(([key, item]) => [
    key,
    sensitiveField.test(key) ? '[REDACTED]' : sanitizeIpcLogValue(item, depth + 1, seen),
  ])
  if (allEntries.length > maxObjectEntries) entries.push(['…', '[Additional fields omitted]'])
  return Object.fromEntries(entries)
}

export function createIpcLogEntry(entry: IpcLogEntry): IpcLogEntry {
  return {
    ...entry,
    request: entry.request === undefined ? undefined : sanitizeIpcLogValue(entry.request),
    response: entry.response === undefined ? undefined : sanitizeIpcLogValue(entry.response),
    error: entry.error === undefined ? undefined : sanitizeIpcLogValue(entry.error),
  }
}
