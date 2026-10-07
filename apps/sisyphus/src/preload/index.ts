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

const api = {
  getSnapshot: (): Promise<NodeSnapshot> => ipcRenderer.invoke('node:get-snapshot'),
  reconnect: (): Promise<NodeSnapshot> => ipcRenderer.invoke('node:reconnect'),
  connectPeer: (address: string): Promise<string> => ipcRenderer.invoke('node:connect-peer', address),
  setPeerComputeTrust: (peerId: string, trusted: boolean): Promise<void> => ipcRenderer.invoke('node:set-peer-compute-trust', { peerId, trusted }),
  onSnapshot: (callback: (snapshot: NodeSnapshot) => void) => {
    const listener = (_event: Electron.IpcRendererEvent, snapshot: NodeSnapshot) => callback(snapshot)
    ipcRenderer.on('node:snapshot', listener)
    return () => { ipcRenderer.removeListener('node:snapshot', listener) }
  },
}

contextBridge.exposeInMainWorld('sisyphus', api)

export type SisyphusBridge = typeof api
