export type JobEvent = {
  seq?: string | number
  atMs?: string | number
  kind: string
  text: string
  workerName: string
  taskIndex?: number
}

// Sequence numbers are uint64, so do not round them through Number.
export function eventSequence(event: JobEvent): bigint {
  try { return BigInt(event.seq ?? 0) } catch { return 0n }
}

export function appendJobEvent(events: JobEvent[], event: JobEvent): JobEvent[] {
  const seq = eventSequence(event)
  const previous = events.length ? eventSequence(events[events.length - 1]) : 0n
  if (seq > 0n && seq <= previous) return events
  return [...events.slice(-499), event]
}
