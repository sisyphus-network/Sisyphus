import { contextBridge, ipcRenderer } from 'electron'
import { createRpcStream } from './rpc-stream'

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
    return createRpcStream(ipcRenderer, method, request, callback, onError, onEnd)
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
