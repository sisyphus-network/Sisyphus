export function createReconnectBackoff(minimumMs: number, maximumMs: number) {
  if (!Number.isFinite(minimumMs) || minimumMs <= 0) throw new RangeError('The minimum reconnect delay must be positive.')
  if (!Number.isFinite(maximumMs) || maximumMs < minimumMs) throw new RangeError('The maximum reconnect delay must be at least the minimum.')

  let delay = minimumMs
  return {
    next() {
      const current = delay
      delay = Math.min(delay * 2, maximumMs)
      return current
    },
    reset() { delay = minimumMs },
  }
}
