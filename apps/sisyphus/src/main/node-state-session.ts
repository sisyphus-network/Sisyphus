type RpcError = Error & { code?: number }

type StateStream<T> = {
  on(event: 'data', listener: (value: T) => void): unknown
  on(event: 'error', listener: (error: RpcError) => void): unknown
  on(event: 'end', listener: () => void): unknown
  cancel(): void
}

type StateClient<TInfo, TState, TPeers> = {
  getNodeInfo(request: object, options: { deadline: number }, callback: (error: RpcError | null, info?: TInfo) => void): void
  watchNodeState(request: object): StateStream<TState>
  watchPeers(request: object): StateStream<TPeers>
}

export function startNodeStateSession<TInfo, TState, TPeers>(options: {
  client: StateClient<TInfo, TState, TPeers>
  isCurrent: () => boolean
  unimplementedCode: number
  onReady: (info: TInfo) => void
  onUpdate: (update: TState | TPeers, legacy: boolean) => void
  onError: (error: RpcError) => void
}): () => void {
  let stopped = false
  let stateStream: StateStream<TState> | null = null
  let legacyStream: StateStream<TPeers> | null = null
  const current = () => !stopped && options.isCurrent()

  const fail = (error: RpcError) => {
    if (current()) options.onError(error)
  }

  const startLegacyStream = () => {
    if (!current()) return
    try {
      const stream = options.client.watchPeers({})
      legacyStream = stream
      stream.on('data', (update) => {
        if (current() && legacyStream === stream) options.onUpdate(update, true)
      })
      stream.on('error', (error) => {
        if (legacyStream === stream) fail(error)
      })
      stream.on('end', () => {
        if (legacyStream === stream) fail(new Error('The peer stream ended.'))
      })
    } catch (error) {
      fail(error instanceof Error ? error as RpcError : new Error(String(error)))
    }
  }

  try {
    options.client.getNodeInfo({}, { deadline: Date.now() + 5_000 }, (error, info) => {
      if (!current()) return
      if (error || !info) {
        options.onError(error ?? new Error('The daemon returned no node information.'))
        return
      }

      options.onReady(info)
      if (!current()) return

      try {
        const stream = options.client.watchNodeState({})
        stateStream = stream
        stream.on('data', (update) => {
          if (current() && stateStream === stream) options.onUpdate(update, false)
        })
        stream.on('error', (streamError) => {
          if (!current() || stateStream !== stream) return
          if (streamError.code === options.unimplementedCode) {
            stateStream = null
            stream.cancel()
            startLegacyStream()
            return
          }
          fail(streamError)
        })
        stream.on('end', () => {
          if (stateStream === stream) fail(new Error('The node state stream ended.'))
        })
      } catch (streamError) {
        fail(streamError instanceof Error ? streamError as RpcError : new Error(String(streamError)))
      }
    })
  } catch (error) {
    fail(error instanceof Error ? error as RpcError : new Error(String(error)))
  }

  return () => {
    if (stopped) return
    stopped = true
    const state = stateStream
    const peers = legacyStream
    stateStream = null
    legacyStream = null
    state?.cancel()
    peers?.cancel()
  }
}
