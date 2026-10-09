import { contextBridge, ipcRenderer } from 'electron'

export type Peer = {
  peerId: string
  connectionState: number | string
  knownAddresses: string[]
  trustedForCompute: boolean
  // Which way work is flowing now. A daemon from before these existed
  // leaves them out.
  worksForThisNode?: boolean
  thisNodeWorksFor?: boolean
  // The two sides of this node's trust in the peer.
  givesWork?: boolean
  takesWork?: boolean
  countryCode?: string
}

export type NodeSnapshot = {
  status: 'connecting' | 'connected' | 'disconnected'
  endpoint: string
  info: {
    peerId: string
    daemonVersion: string
    listenAddresses: string[]
    countryCode: string
  } | null
  peers: Peer[]
  revision: string
  lastUpdated: string | null
  error: string | null
}

export type NodeRpcMethod =
  | 'getBootstrapPeers' | 'setBootstrapPeers' | 'listWorkers' | 'listPeers' | 'getNodeInfo'
  | 'submitJob' | 'getJob' | 'listJobs' | 'cancelJob'
  | 'getModelConfig' | 'setModelConfig' | 'listProviders' | 'listModels' | 'removeModel'
  | 'listChats' | 'getChat' | 'deleteChat'
  | 'storeFile' | 'fetchFile' | 'listFiles' | 'removeFile'
  | 'createInvitation' | 'listMembers' | 'removeMember' | 'joinPool'

export type NodeStreamMethod = 'watchJobs' | 'watchJobEvents' | 'ask' | 'pullModel'

const api = {
  getSnapshot: (): Promise<NodeSnapshot> => ipcRenderer.invoke('node:get-snapshot'),
  reconnect: (): Promise<NodeSnapshot> => ipcRenderer.invoke('node:reconnect'),
  connectPeer: (address: string): Promise<string> => ipcRenderer.invoke('node:connect-peer', address),
  setPeerComputeTrust: (peerId: string, trusted: boolean): Promise<void> => ipcRenderer.invoke('node:set-peer-compute-trust', { peerId, trusted }),
  setPeerComputePermissions: (peerId: string, givesWork: boolean, takesWork: boolean): Promise<void> => ipcRenderer.invoke('node:set-peer-compute-permissions', { peerId, givesWork, takesWork }),
  call: <T = Record<string, unknown>>(method: NodeRpcMethod, request: Record<string, unknown> = {}): Promise<T> => ipcRenderer.invoke('node:rpc', { method, request }),
  stream: <T = Record<string, unknown>>(method: NodeStreamMethod, request: Record<string, unknown>, callback: (event: T) => void, onError?: (error: string) => void, onEnd?: () => void) => {
    const id = crypto.randomUUID()
    const channel = `node:rpc-stream:${id}`
    const listener = (_event: Electron.IpcRendererEvent, payload: { data?: T; error?: string; end?: boolean }) => {
      if (payload.data !== undefined) callback(payload.data)
      if (payload.error) onError?.(payload.error)
      if (payload.end) { ipcRenderer.removeListener(channel, listener); onEnd?.() }
    }
    ipcRenderer.on(channel, listener)
    void ipcRenderer.invoke('node:rpc-stream-start', { id, method, request }).catch((error: unknown) => {
      ipcRenderer.removeListener(channel, listener)
      onError?.(error instanceof Error ? error.message : String(error))
    })
    return () => {
      ipcRenderer.removeListener(channel, listener)
      void ipcRenderer.invoke('node:rpc-stream-stop', id)
    }
  },
  storeFile: (file: { name: string; data: Uint8Array; private: boolean }): Promise<Record<string, unknown>> => ipcRenderer.invoke('node:store-file', file),
  fetchFile: (cid: string): Promise<Uint8Array> => ipcRenderer.invoke('node:fetch-file', cid),
  onSnapshot: (callback: (snapshot: NodeSnapshot) => void) => {
    const listener = (_event: Electron.IpcRendererEvent, snapshot: NodeSnapshot) => callback(snapshot)
    ipcRenderer.on('node:snapshot', listener)
    return () => { ipcRenderer.removeListener('node:snapshot', listener) }
  },
}

contextBridge.exposeInMainWorld('sisyphus', api)

export type SisyphusBridge = typeof api
