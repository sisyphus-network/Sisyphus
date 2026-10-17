import type { NodeSnapshot, SisyphusBridge } from '../../../preload'
import { createBrowserPairing, createSisyphusClient, isAuthenticationError, listPairedBrowsers, revokePairedBrowser, type NodeServiceClient, type SisyphusClient } from '@sisyphus/client'

let runtimeClient: SisyphusClient | undefined

function getRuntimeClient() {
  runtimeClient ??= createSisyphusClient()
  return runtimeClient
}

function normalizeProtocolValue(value: unknown): unknown {
  if (typeof value === 'bigint') return value.toString()
  if (value instanceof Uint8Array) return value
  if (Array.isArray(value)) return value.map(normalizeProtocolValue)
  if (value && typeof value === 'object') {
    // Protobuf-ES v2 marks each message with its type ($typeName) and keeps
    // fields it does not know ($unknown); the bridge hands on the data alone,
    // as the Electron bridge does.
    return Object.fromEntries(Object.entries(value).filter(([key]) => !key.startsWith('$')).map(([key, item]) => [key, normalizeProtocolValue(item)]))
  }
  return value
}

function browserBridge(node: NodeServiceClient, baseUrl: string): SisyphusBridge {
  const devPreview = import.meta.env.DEV
  let devSessionVerified = false
  const offlineSnapshot = (error: unknown): NodeSnapshot => ({
    status: 'disconnected', endpoint: baseUrl, info: null, peers: [], revision: '0', lastUpdated: null,
    error: error instanceof Error ? error.message : String(error), authRequired: isAuthenticationError(error),
  })
  const getSnapshot = async (): Promise<NodeSnapshot> => {
    if (devPreview) {
      const response = await fetch('/api/node/snapshot', { cache: 'no-store' })
      if (!response.ok) throw new Error(`Node API returned ${response.status}`)
      const current = await response.json() as NodeSnapshot
      if (current.status !== 'connected') {
        devSessionVerified = false
        return current
      }
      try {
        await node.getNodeInfo({})
        devSessionVerified = true
        return { ...current, authRequired: false, error: null }
      } catch (error) {
        devSessionVerified = false
        return { ...current, status: 'disconnected', authRequired: isAuthenticationError(error), error: error instanceof Error ? error.message : String(error) }
      }
    }
    const [info, peerList] = await Promise.all([node.getNodeInfo({}), node.listPeers({})])
    return {
      status: 'connected', endpoint: baseUrl,
      info: { peerId: info.peerId, daemonVersion: info.daemonVersion, listenAddresses: info.listenAddresses, countryCode: info.countryCode, countrySource: info.countrySource },
      peers: peerList.peers.map((peer) => ({
        peerId: peer.peerId, connectionState: peer.connectionState, knownAddresses: peer.knownAddresses,
        trustedForCompute: peer.trustedForCompute, worksForThisNode: peer.worksForThisNode,
        thisNodeWorksFor: peer.thisNodeWorksFor, givesWork: peer.givesWork, takesWork: peer.takesWork, countryCode: peer.countryCode,
      })),
      revision: String(peerList.revision), lastUpdated: new Date().toISOString(), error: null,
    }
  }

  return {
    getSnapshot,
    reconnect: getSnapshot,
    connectPeer: async (address) => (await node.connectPeer({ address })).peerId,
    setPeerComputeTrust: async (peerId, trusted) => { await node.setPeerComputeTrust({ peerId, trusted }) },
    setPeerComputePermissions: async (peerId, givesWork, takesWork) => { await node.setPeerComputePermissions({ peerId, givesWork, takesWork }) },
    call: async <T = Record<string, unknown>>(method: string, request: Record<string, unknown> = {}) => {
      const rpc = (node as unknown as Record<string, (input: Record<string, unknown>) => Promise<unknown>>)[method]
      if (!rpc) throw new Error(`The protobuf NodeService has no unary method ${method}.`)
      return normalizeProtocolValue(await rpc(request)) as T
    },
    stream: <T = Record<string, unknown>>(method: string, request: Record<string, unknown>, callback: (event: T) => void, onError?: (error: string, code?: number) => void, onEnd?: () => void) => {
      const rpc = (node as unknown as Record<string, (input: Record<string, unknown>, options: { signal: AbortSignal }) => AsyncIterable<unknown>>)[method]
      if (!rpc) throw new Error(`The protobuf NodeService has no streaming method ${method}.`)
      const controller = new AbortController()
      void (async () => {
        try {
          for await (const event of rpc(request, { signal: controller.signal })) callback(normalizeProtocolValue(event) as T)
          if (!controller.signal.aborted) onEnd?.()
        } catch (error) {
          if (!controller.signal.aborted) onError?.(error instanceof Error ? error.message : String(error))
        }
      })()
      return () => controller.abort()
    },
    storeFile: async (file) => {
      const response = await fetch(`${baseUrl}/files/${encodeURIComponent(file.name)}?private=${file.private}`, {
        method: 'PUT', body: file.data.slice().buffer as ArrayBuffer, credentials: 'same-origin',
      })
      if (!response.ok) throw new Error(await response.text() || `File upload failed (HTTP ${response.status}).`)
      return await response.json() as Record<string, unknown>
    },
    fetchFile: async (cid) => {
      const chunks: Uint8Array[] = []
      let total = 0
      for await (const part of node.fetchFile({ cid })) {
        chunks.push(part.data)
        total += part.data.byteLength
      }
      const result = new Uint8Array(total)
      let offset = 0
      for (const chunk of chunks) { result.set(chunk, offset); offset += chunk.byteLength }
      return result
    },
    createBrowserPairing,
    listPairedBrowsers,
    revokePairedBrowser,
    onSnapshot: (callback) => {
      if (!devPreview) {
        const controller = new AbortController()
        void (async () => {
          while (!controller.signal.aborted) {
            try {
              const info = await node.getNodeInfo({}, { signal: controller.signal })
              for await (const update of node.watchPeers({}, { signal: controller.signal })) {
                if (controller.signal.aborted) break
                callback({
                  status: 'connected', endpoint: baseUrl,
                  info: { peerId: info.peerId, daemonVersion: info.daemonVersion, listenAddresses: info.listenAddresses, countryCode: info.countryCode, countrySource: info.countrySource },
                  peers: update.peers.map((peer) => ({
                    peerId: peer.peerId, connectionState: peer.connectionState, knownAddresses: peer.knownAddresses,
                    trustedForCompute: peer.trustedForCompute, worksForThisNode: peer.worksForThisNode,
                    thisNodeWorksFor: peer.thisNodeWorksFor, givesWork: peer.givesWork, takesWork: peer.takesWork, countryCode: peer.countryCode,
                  })),
                  revision: String(update.revision), lastUpdated: new Date().toISOString(), error: null,
                })
              }
              if (!controller.signal.aborted) throw new Error('The node update stream ended.')
            } catch (error) {
              if (controller.signal.aborted) break
              callback(offlineSnapshot(error))
            }
            await new Promise<void>((resolve) => {
              const finish = () => { clearTimeout(timer); controller.signal.removeEventListener('abort', finish); resolve() }
              const timer = window.setTimeout(finish, 1500)
              controller.signal.addEventListener('abort', finish, { once: true })
            })
          }
        })()
        return () => controller.abort()
      }
      const events = new EventSource('/api/node/events')
      events.onmessage = (event) => {
        try {
          const next = JSON.parse(event.data) as NodeSnapshot
          callback(devSessionVerified ? { ...next, authRequired: false } : { ...next, status: 'disconnected', authRequired: true })
        } catch { /* Ignore malformed dev-stream messages. */ }
      }
      return () => { events.close() }
    },
  }
}

export function getNodeApi(): SisyphusBridge {
  const client = getRuntimeClient()
  if (client.context === 'electron') return client.bridge as SisyphusBridge
  return browserBridge(client.node, client.baseUrl)
}

export function isDesktopApp() {
  return getRuntimeClient().context === 'electron'
}
