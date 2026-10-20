type Snapshot = { status: string }
type SnapshotSource<T extends Snapshot> = {
  getSnapshot: () => Promise<T>
  onSnapshot: (callback: (snapshot: T) => void) => () => void
}

/** Resolve only after a connected snapshot; reject if the deadline expires. */
export function waitForConnectedSnapshot<T extends Snapshot>(source: SnapshotSource<T>, timeoutMs: number, timeoutMessage: string): Promise<void> {
  return new Promise((resolve, reject) => {
    let settled = false
    let unsubscribe = () => {}
    let timer: ReturnType<typeof setTimeout> | undefined
    const finish = (error?: Error) => {
      if (settled) return
      settled = true
      if (timer !== undefined) clearTimeout(timer)
      unsubscribe()
      if (error) reject(error)
      else resolve()
    }
    const onSnapshot = (snapshot: T) => {
      if (snapshot.status === 'connected') finish()
    }

    // Subscribe before fetching the current state so a fast reconnect cannot
    // land in the gap between the initial read and the event listener.
    unsubscribe = source.onSnapshot(onSnapshot)
    if (settled) {
      unsubscribe()
      return
    }
    timer = setTimeout(() => finish(new Error(timeoutMessage)), timeoutMs)
    void source.getSnapshot().then(onSnapshot).catch(() => {})
  })
}
