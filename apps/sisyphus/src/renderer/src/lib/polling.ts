type Schedule = (callback: () => void, delay: number) => () => void

const scheduleTimer: Schedule = (callback, delay) => {
  const timer = setTimeout(callback, delay)
  return () => clearTimeout(timer)
}

// Schedule the next refresh only after the previous request has settled.
export function startPolling(task: () => Promise<void>, delay = 5_000, schedule: Schedule = scheduleTimer): () => void {
  let active = true
  let cancelTimer: (() => void) | undefined
  const run = async () => {
    if (!active) return
    try { await task() } catch { /* The next refresh can recover. */ }
    if (active) cancelTimer = schedule(() => { void run() }, delay)
  }
  void run()
  return () => { active = false; cancelTimer?.() }
}
