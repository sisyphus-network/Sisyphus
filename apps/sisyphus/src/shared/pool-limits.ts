// Pool limits: what a coordinator lets one member of its pool use. The
// daemon stores them and is the authority on what it accepts; this module
// only carries them between the API, the bridges and the settings form
// without losing precision. 64-bit fields travel as decimal strings, since
// protobuf-es gives them as bigint and grpc-js (longs: String) as strings,
// and all byte and second arithmetic is done with BigInt.

export type PoolLimits = {
  maxJobsPerClient: number
  maxTasksPerClient: number
  maxTaskTimeoutSeconds: string
  maxStoreBytesPerClient: string
  maxUploadBytesPerAttempt: string
}

export const maxPoolLimitCount = 2147483647
const maxUint64 = 18446744073709551615n

export const noPoolLimits: PoolLimits = { maxJobsPerClient: 0, maxTasksPerClient: 0, maxTaskTimeoutSeconds: '0', maxStoreBytesPerClient: '0', maxUploadBytesPerAttempt: '0' }

export const durationUnits = { seconds: 1n, minutes: 60n, hours: 3600n } as const
export type DurationUnit = keyof typeof durationUnits
// Decimal (SI) units: 1 MB is 1,000,000 bytes.
export const byteUnits = { MB: 1_000_000n, GB: 1_000_000_000n, TB: 1_000_000_000_000n } as const
export type ByteUnit = keyof typeof byteUnits

function uint64(value: unknown): bigint | undefined {
  let parsed: bigint
  if (typeof value === 'bigint') parsed = value
  else if (typeof value === 'number' && Number.isSafeInteger(value)) parsed = BigInt(value)
  else if (typeof value === 'string' && /^\d{1,20}$/.test(value)) parsed = BigInt(value)
  else return undefined
  return parsed >= 0n && parsed <= maxUint64 ? parsed : undefined
}

function count(value: unknown): number | undefined {
  const parsed = uint64(value)
  return parsed !== undefined && parsed <= BigInt(maxPoolLimitCount) ? Number(parsed) : undefined
}

/** Limits as the daemon reported them; anything unreadable is no limit. */
export function normalizePoolLimits(value: unknown): PoolLimits {
  const candidate = value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {}
  return {
    maxJobsPerClient: count(candidate.maxJobsPerClient) ?? 0,
    maxTasksPerClient: count(candidate.maxTasksPerClient) ?? 0,
    maxTaskTimeoutSeconds: String(uint64(candidate.maxTaskTimeoutSeconds) ?? 0n),
    maxStoreBytesPerClient: String(uint64(candidate.maxStoreBytesPerClient) ?? 0n),
    maxUploadBytesPerAttempt: String(uint64(candidate.maxUploadBytesPerAttempt) ?? 0n),
  }
}

/** Limits a renderer asks to save; anything malformed is refused rather than guessed at. */
export function validatePoolLimits(value: unknown): PoolLimits {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid pool limits.')
  const candidate = value as Record<string, unknown>
  const jobs = count(candidate.maxJobsPerClient)
  const tasks = count(candidate.maxTasksPerClient)
  const timeout = typeof candidate.maxTaskTimeoutSeconds === 'string' ? uint64(candidate.maxTaskTimeoutSeconds) : undefined
  const store = typeof candidate.maxStoreBytesPerClient === 'string' ? uint64(candidate.maxStoreBytesPerClient) : undefined
  const upload = typeof candidate.maxUploadBytesPerAttempt === 'string' ? uint64(candidate.maxUploadBytesPerAttempt) : undefined
  if (typeof candidate.maxJobsPerClient !== 'number' || typeof candidate.maxTasksPerClient !== 'number' || jobs === undefined || tasks === undefined || timeout === undefined || store === undefined || upload === undefined) {
    throw new Error('Invalid pool limits.')
  }
  return { maxJobsPerClient: jobs, maxTasksPerClient: tasks, maxTaskTimeoutSeconds: String(timeout), maxStoreBytesPerClient: String(store), maxUploadBytesPerAttempt: String(upload) }
}

export function samePoolLimits(a: PoolLimits, b: PoolLimits): boolean {
  return a.maxJobsPerClient === b.maxJobsPerClient && a.maxTasksPerClient === b.maxTasksPerClient && a.maxTaskTimeoutSeconds === b.maxTaskTimeoutSeconds
    && a.maxStoreBytesPerClient === b.maxStoreBytesPerClient && a.maxUploadBytesPerAttempt === b.maxUploadBytesPerAttempt
}

export type ParsedAmount = { ok: true; value: bigint } | { ok: false; reason: 'invalid' | 'fraction' | 'tooLarge' }

/**
 * Reads what was typed into a limit field, times its unit. Empty is 0 (no
 * limit). A decimal point or comma is accepted, so long as the result is a
 * whole number of the base unit; signs, exponents and grouping are not.
 */
export function parseAmount(text: string, unit: bigint, max: bigint = maxUint64): ParsedAmount {
  const trimmed = text.trim()
  if (trimmed === '') return { ok: true, value: 0n }
  const match = /^(\d*)(?:[.,](\d*))?$/.exec(trimmed)
  if (!match || (match[1] === '' && !match[2])) return { ok: false, reason: 'invalid' }
  const fraction = (match[2] ?? '').replace(/0+$/, '')
  if (fraction.length > 30) return { ok: false, reason: 'fraction' }
  const scale = 10n ** BigInt(fraction.length)
  const scaled = BigInt((match[1] || '0') + fraction) * unit
  if (scaled % scale !== 0n) return { ok: false, reason: 'fraction' }
  const value = scaled / scale
  return value > max ? { ok: false, reason: 'tooLarge' } : { ok: true, value }
}

/** value / unit as an exact decimal, without trailing zeros. */
function exactDecimal(value: bigint, unit: bigint): string {
  const whole = value / unit
  let remainder = value % unit
  if (remainder === 0n) return String(whole)
  let digits = ''
  // Units here are powers of ten, so this ends.
  while (remainder !== 0n && digits.length < 30) {
    remainder *= 10n
    digits += String(remainder / unit)
    remainder %= unit
  }
  return `${whole}.${digits}`
}

/** A saved byte limit as the form shows it: the largest unit it is at least one of, exactly. */
export function bytesForForm(value: string): { text: string; unit: ByteUnit } {
  const bytes = uint64(value) ?? 0n
  if (bytes === 0n) return { text: '', unit: 'GB' }
  const unit: ByteUnit = bytes >= byteUnits.TB ? 'TB' : bytes >= byteUnits.GB ? 'GB' : 'MB'
  return { text: exactDecimal(bytes, byteUnits[unit]), unit }
}

/** A saved timeout as the form shows it: in hours or minutes when it is whole ones, otherwise seconds. */
export function durationForForm(value: string): { text: string; unit: DurationUnit } {
  const seconds = uint64(value) ?? 0n
  if (seconds === 0n) return { text: '', unit: 'hours' }
  const unit: DurationUnit = seconds % durationUnits.hours === 0n ? 'hours' : seconds % durationUnits.minutes === 0n ? 'minutes' : 'seconds'
  return { text: String(seconds / durationUnits[unit]), unit }
}

export function countForForm(value: number): string {
  return value === 0 ? '' : String(value)
}

/**
 * The daemon's own words from a failed call, without the transport's
 * wrapping: Electron's "Error invoking remote method …: Error: ", grpc-js's
 * "3 INVALID_ARGUMENT: " and Connect's "[invalid_argument] ".
 */
export function daemonErrorMessage(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error)
  return message
    .replace(/^Error invoking remote method '[^']*': (?:Error: )?/, '')
    .replace(/^\d+ [A-Z_]+: /, '')
    .replace(/^\[[a-z_]+\] /, '')
}
