import type { NodeSnapshot, SisyphusBridge } from '../../../preload'

type SnapshotApi = Pick<SisyphusBridge, 'getSnapshot' | 'onSnapshot'>

/** Wait until the restarted node is confirmed connected, or fail after the timeout. */
export function waitForConnectedSnapshot(api: SnapshotApi, timeoutMs: number, timeoutMessage: string): Promise<void> {
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
    const onSnapshot = (snapshot: NodeSnapshot) => {
      if (snapshot.status === 'connected') finish()
    }

    // Subscribe before fetching the current state so a fast reconnect cannot
    // land in the gap between the initial read and the event listener.
    unsubscribe = api.onSnapshot(onSnapshot)
    if (settled) unsubscribe()
    if (settled) return
    timer = setTimeout(() => finish(new Error(timeoutMessage)), timeoutMs)
    void api.getSnapshot().then(onSnapshot).catch(() => {})
  })
}
