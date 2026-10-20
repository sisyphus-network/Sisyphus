import { waitForConnectedSnapshot } from '../shared/wait-for-connected-snapshot.ts'

type Snapshot = { status: string }
type SnapshotSource<T extends Snapshot> = {
  getSnapshot: () => Promise<T>
  onSnapshot: (callback: (snapshot: T) => void) => () => void
}

/** Do not report/apply the node's new runtime settings until reconnection. */
export async function restartManagedNode<T extends Snapshot>(options: {
  restart: () => Promise<void>
  reconnect: () => void
  snapshots: SnapshotSource<T>
  timeoutMs: number
  timeoutMessage: string
  onConnected: () => void
}): Promise<void> {
  await options.restart()
  options.reconnect()
  await waitForConnectedSnapshot(options.snapshots, options.timeoutMs, options.timeoutMessage)
  options.onConnected()
}
