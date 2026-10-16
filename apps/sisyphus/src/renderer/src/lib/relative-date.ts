export function formatRelativeDate(value: string, now: number, locale: string) {
  const timestamp = Date.parse(value)
  if (!Number.isFinite(timestamp)) return value
  const elapsedSeconds = (timestamp - now) / 1000
  const units: Array<[Intl.RelativeTimeFormatUnit, number]> = [
    ['year', 31_536_000], ['month', 2_592_000], ['week', 604_800],
    ['day', 86_400], ['hour', 3_600], ['minute', 60], ['second', 1],
  ]
  const [unit, secondsPerUnit] = units.find(([, seconds]) => Math.abs(elapsedSeconds) >= seconds) ?? ['second', 1]
  try { return new Intl.RelativeTimeFormat(locale || undefined, { numeric: 'auto' }).format(Math.round(elapsedSeconds / secondsPerUnit), unit) }
  catch { return new Date(timestamp).toLocaleString() }
}
