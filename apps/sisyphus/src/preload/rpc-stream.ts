type StreamPayload<T> = { data?: T; error?: string; end?: boolean }
type StreamListener<T> = (event: unknown, payload: StreamPayload<T>) => void

type StreamIpc<T> = {
  invoke: (channel: string, ...args: unknown[]) => Promise<unknown>
  on: (channel: string, listener: StreamListener<T>) => unknown
  removeListener: (channel: string, listener: StreamListener<T>) => unknown
}

// A terminal event or local cancellation releases the listener exactly once.
// In particular, a pending start rejection must not call an unmounted client.
export function createRpcStream<T>(
  ipc: StreamIpc<T>, method: string, request: Record<string, unknown>,
  callback: (event: T) => void, onError?: (error: string) => void, onEnd?: () => void,
): () => void {
  const id = crypto.randomUUID()
  const channel = `node:rpc-stream:${id}`
  let active = true
  const cleanup = () => {
    if (!active) return false
    active = false
    ipc.removeListener(channel, listener)
    return true
  }
  const listener: StreamListener<T> = (_event, payload) => {
    if (!active) return
    if (payload.error !== undefined) {
      if (cleanup()) onError?.(payload.error)
    } else if (payload.end) {
      if (cleanup()) onEnd?.()
    } else if (payload.data !== undefined) {
      callback(payload.data)
    }
  }
  ipc.on(channel, listener)
  void ipc.invoke('node:rpc-stream-start', { id, method, request }).catch((error: unknown) => {
    if (cleanup()) onError?.(error instanceof Error ? error.message : String(error))
  })
  return () => {
    if (!cleanup()) return
    // Electron may already be tearing down; cancellation is best-effort.
    void ipc.invoke('node:rpc-stream-stop', id).catch(() => {})
  }
}
