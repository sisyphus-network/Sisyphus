import { contextBridge, ipcRenderer } from 'electron'
import { createRpcStream } from './rpc-stream'

type IpcLogEntry = {
  phase: 'request' | 'response' | 'error' | 'event'
  channel: string
  timestamp: string
  durationMs?: number
  request?: unknown
  response?: unknown
  error?: unknown
}

ipcRenderer.on('node:ipc-log', (_event, entry: IpcLogEntry) => {
  const style = entry.phase === 'error' ? 'color:#ef4444;font-weight:bold' : entry.phase === 'request' ? 'color:#60a5fa;font-weight:bold' : 'color:#34d399;font-weight:bold'
  console.groupCollapsed(`%cIPC ${entry.phase.toUpperCase()}%c ${entry.channel}${entry.durationMs === undefined ? '' : ` · ${entry.durationMs}ms`}`, style, 'color:inherit')
  console.log('timestamp:', entry.timestamp)
  if (entry.request !== undefined) console.log('request:', entry.request)
  if (entry.response !== undefined) console.log('response:', entry.response)
  if (entry.error !== undefined) console.error('error:', entry.error)
  console.groupEnd()
})

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
  authRequired?: boolean
}

export type BrowserPairingInvitation = { code: string; expiresAt: string; pairUrl: string }
export type PairedBrowser = { id: string; name: string; createdAt: string }

export type NodeRpcMethod =
  | 'getBootstrapPeers' | 'setBootstrapPeers' | 'listWorkers' | 'listPeers' | 'getNodeInfo'
  | 'submitJob' | 'getJob' | 'listJobs' | 'cancelJob'
  | 'getModelConfig' | 'setModelConfig' | 'listModelProfiles' | 'saveModelProfile' | 'deleteModelProfile' | 'listProviders' | 'listModels' | 'removeModel'
  | 'listChats' | 'getChat' | 'deleteChat'
  | 'listFiles' | 'removeFile'
  | 'createInvitation' | 'listMembers' | 'removeMember' | 'joinPool'

export type NodeStreamMethod = 'watchJobs' | 'watchJobEvents' | 'ask' | 'pullModel'

const api = {
  getSnapshot: (): Promise<NodeSnapshot> => ipcRenderer.invoke('node:get-snapshot'),
  reconnect: (): Promise<NodeSnapshot> => ipcRenderer.invoke('node:reconnect'),
  connectPeer: (address: string): Promise<string> => ipcRenderer.invoke('node:connect-peer', address),
  setPeerComputeTrust: (peerId: string, trusted: boolean): Promise<void> => ipcRenderer.invoke('node:set-peer-compute-trust', { peerId, trusted }),
  setPeerComputePermissions: (peerId: string, givesWork: boolean, takesWork: boolean): Promise<void> => ipcRenderer.invoke('node:set-peer-compute-permissions', { peerId, givesWork, takesWork }),
  call: <T = Record<string, unknown>>(method: NodeRpcMethod, request: Record<string, unknown> = {}): Promise<T> => ipcRenderer.invoke('node:rpc', { method, request }),
  stream: <T = Record<string, unknown>>(method: NodeStreamMethod, request: Record<string, unknown>, callback: (event: T) => void, onError?: (error: string, code?: number) => void, onEnd?: () => void) => {
    return createRpcStream(ipcRenderer, method, request, callback, onError, onEnd)
  },
  storeFile: (file: { name: string; data: Uint8Array; private: boolean; chatAttachment?: boolean }): Promise<Record<string, unknown>> => ipcRenderer.invoke('node:store-file', file),
  fetchFile: (cid: string): Promise<Uint8Array> => ipcRenderer.invoke('node:fetch-file', cid),
  createBrowserPairing: (): Promise<BrowserPairingInvitation> => ipcRenderer.invoke('node:browser:create-pairing'),
  listPairedBrowsers: (): Promise<{ devices: PairedBrowser[] }> => ipcRenderer.invoke('node:browser:list-paired'),
  revokePairedBrowser: (id: string): Promise<void> => ipcRenderer.invoke('node:browser:revoke-paired', id),
  onSnapshot: (callback: (snapshot: NodeSnapshot) => void) => {
    const listener = (_event: Electron.IpcRendererEvent, snapshot: NodeSnapshot) => callback(snapshot)
    ipcRenderer.on('node:snapshot', listener)
    return () => { ipcRenderer.removeListener('node:snapshot', listener) }
  },
}

contextBridge.exposeInMainWorld('sisyphus', api)

export type SisyphusBridge = typeof api
