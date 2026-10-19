type SessionOptions<Snapshot, State, Peers> = {
  getSnapshot: () => Promise<Snapshot>
  watchNodeState: (signal: AbortSignal) => AsyncIterable<State>
  watchPeers: (signal: AbortSignal) => AsyncIterable<Peers>
  hasNodeDetails: (state: State) => boolean
  isUnimplemented: (error: unknown) => boolean
  applyNodeState: (snapshot: Snapshot, state: State) => Snapshot
  applyPeers: (snapshot: Snapshot, peers: Peers) => Snapshot
  onSnapshot: (snapshot: Snapshot) => void
  onError: (snapshot: Snapshot | undefined, error: unknown) => Snapshot
  retryDelayMs?: number
  maxRetryDelayMs?: number
  wait?: (ms: number, signal: AbortSignal) => Promise<void>
}

function waitForRetry(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve()
    const finish = () => {
      clearTimeout(timer)
      signal.removeEventListener('abort', finish)
      resolve()
    }
    const timer = setTimeout(finish, ms)
    signal.addEventListener('abort', finish, { once: true })
  })
}

/** Starts a browser's snapshot-baseline + live-stream session, retrying only after stream failure. */
export function startBrowserNodeStateSession<Snapshot, State, Peers>(options: SessionOptions<Snapshot, State, Peers>): () => void {
  const controller = new AbortController()
  const wait = options.wait ?? waitForRetry
  const initialRetryDelayMs = options.retryDelayMs ?? 1_500
  const maxRetryDelayMs = options.maxRetryDelayMs ?? 30_000
  if (!Number.isFinite(initialRetryDelayMs) || initialRetryDelayMs <= 0) throw new RangeError('The initial browser stream retry delay must be positive.')
  if (!Number.isFinite(maxRetryDelayMs) || maxRetryDelayMs < initialRetryDelayMs) throw new RangeError('The maximum browser stream retry delay must be at least the initial delay.')
  let retryDelayMs = initialRetryDelayMs
  let generation = 0

  const run = async () => {
    while (!controller.signal.aborted) {
      const currentGeneration = ++generation
      let latest: Snapshot | undefined
      try {
        latest = await options.getSnapshot()
        if (controller.signal.aborted || currentGeneration !== generation) break
        options.onSnapshot(latest)

        try {
          for await (const state of options.watchNodeState(controller.signal)) {
            if (controller.signal.aborted || currentGeneration !== generation) break
            if (!options.hasNodeDetails(state)) throw new Error('The node state stream returned no node details.')
            latest = options.applyNodeState(latest!, state)
            retryDelayMs = initialRetryDelayMs
            options.onSnapshot(latest)
          }
          if (!controller.signal.aborted) throw new Error('The node state stream ended.')
        } catch (error) {
          if (controller.signal.aborted) break
          if (!options.isUnimplemented(error)) throw error
          for await (const peers of options.watchPeers(controller.signal)) {
            if (controller.signal.aborted || currentGeneration !== generation) break
            latest = options.applyPeers(latest!, peers)
            retryDelayMs = initialRetryDelayMs
            options.onSnapshot(latest)
          }
          if (!controller.signal.aborted) throw new Error('The peer stream ended.')
        }
      } catch (error) {
        if (controller.signal.aborted) break
        options.onSnapshot(options.onError(latest, error))
      }
      await wait(retryDelayMs, controller.signal)
      retryDelayMs = Math.min(retryDelayMs * 2, maxRetryDelayMs)
    }
  }

  void run()
  return () => {
    generation += 1
    controller.abort()
  }
}
