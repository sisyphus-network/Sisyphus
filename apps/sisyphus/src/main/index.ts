import { app, BrowserWindow, ipcMain } from 'electron'
import { readFileSync, existsSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import * as grpc from '@grpc/grpc-js'
import * as protoLoader from '@grpc/proto-loader'
import * as protobuf from 'protobufjs'
import nodeProtoSource from '../../../../proto/sisyphus/node/v1/node.proto?raw'

const here = fileURLToPath(new URL('.', import.meta.url))
const nodeProtoJson = protobuf.parse(nodeProtoSource).root.toJSON()
const endpoint = process.env.SISYPHUS_API_ADDRESS ?? '127.0.0.1:50051'
// The daemon lets anything on this machine read from its local API, but only
// a holder of its token change it. The token is in the daemon's data
// directory, readable by the user who runs the daemon.
const tokenFile = process.env.SISYPHUS_API_TOKEN_FILE ?? join(daemonDataDir(), 'api.token')

// Where the daemon keeps its data unless told otherwise: ~/.sisyphus if
// that is there, as it is for nodes set up by earlier versions, and
// otherwise the place the operating system sets aside for a program's data.
function daemonDataDir(): string {
  const home = homedir()
  const old = join(home, '.sisyphus')
  if (existsSync(old)) return old
  if (process.platform === 'win32') {
    return join(process.env.LOCALAPPDATA ?? join(home, 'AppData', 'Local'), 'sisyphus')
  }
  if (process.platform === 'darwin') {
    return join(home, 'Library', 'Application Support', 'sisyphus')
  }
  return join(process.env.XDG_DATA_HOME || join(home, '.local', 'share'), 'sisyphus')
}

function authorization(): grpc.Metadata {
  const metadata = new grpc.Metadata()
  try {
    metadata.set('authorization', `Bearer ${readFileSync(tokenFile, 'utf8').trim()}`)
  } catch {
    // No token: the daemon will refuse the call and say why.
  }
  return metadata
}

type Peer = {
  peerId: string
  connectionState: number | string
  knownAddresses: string[]
  trustedForCompute: boolean
  // Which way work is flowing now. A daemon from before these existed
  // leaves them out.
  worksForThisNode?: boolean
  thisNodeWorksFor?: boolean
}

type NodeInfo = {
  peerId: string
  daemonVersion: string
  listenAddresses: string[]
  countryCode: string
}

type NodeSnapshot = {
  status: 'connecting' | 'connected' | 'disconnected'
  endpoint: string
  info: NodeInfo | null
  peers: Peer[]
  revision: string
  lastUpdated: string | null
  error: string | null
}

type GrpcNodeService = {
  getNodeInfo(request: object, callback: (error: grpc.ServiceError | null, value?: NodeInfo) => void): void
  watchPeers(request: object): grpc.ClientReadableStream<{ peers: Peer[]; revision: string | number | { toString(): string } }>
  connectPeer(request: { address: string }, metadata: grpc.Metadata, callback: (error: grpc.ServiceError | null, value?: { peerId: string }) => void): void
  setPeerComputeTrust(request: { peerId: string; trusted: boolean }, metadata: grpc.Metadata, callback: (error: grpc.ServiceError | null, value?: { trusted: boolean }) => void): void
  close(): void
}

const initialSnapshot = (): NodeSnapshot => ({
  status: 'connecting',
  endpoint,
  info: null,
  peers: [],
  revision: '0',
  lastUpdated: null,
  error: null,
})

let snapshot = initialSnapshot()
let client: GrpcNodeService | null = null
let peerStream: grpc.ClientReadableStream<unknown> | null = null
let retryTimer: NodeJS.Timeout | null = null
let retryDelayMs = 1_000
let reconnecting = false
let isQuitting = false
let connectionGeneration = 0

function publish(next: Partial<NodeSnapshot>) {
  snapshot = { ...snapshot, ...next }
  for (const window of BrowserWindow.getAllWindows()) {
    if (!window.isDestroyed()) window.webContents.send('node:snapshot', snapshot)
  }
}

function clearConnection() {
  connectionGeneration += 1
  const previousStream = peerStream
  const previousClient = client
  peerStream = null
  client = null
  previousStream?.cancel()
  previousClient?.close()
}

function scheduleReconnect(error: string, generation: number) {
  if (isQuitting || retryTimer || generation !== connectionGeneration) return
  clearConnection()
  publish({ status: 'disconnected', error })
  retryTimer = setTimeout(() => {
    retryTimer = null
    connectToNode()
  }, retryDelayMs)
  retryDelayMs = Math.min(retryDelayMs * 2, 15_000)
}

function connectToNode() {
  if (reconnecting || isQuitting) return
  reconnecting = true
  const generation = ++connectionGeneration
  publish({ status: 'connecting', error: null })

  try {
    const definition = protoLoader.fromJSON(nodeProtoJson, {
      keepCase: false,
      longs: String,
      enums: String,
      defaults: true,
      oneofs: true,
    })
    const loaded = grpc.loadPackageDefinition(definition) as unknown as {
      sisyphus: { node: { v1: { NodeService: new (address: string, credentials: grpc.ChannelCredentials) => GrpcNodeService } } }
    }
    const nextClient = new loaded.sisyphus.node.v1.NodeService(endpoint, grpc.credentials.createInsecure())
    client = nextClient

    nextClient.getNodeInfo({}, (error, info) => {
      if (generation !== connectionGeneration) return
      reconnecting = false
      if (error || !info) {
        scheduleReconnect(error?.message ?? 'The daemon returned no node information.', generation)
        return
      }

      retryDelayMs = 1_000
      publish({ status: 'connected', info, error: null })
      const stream = nextClient.watchPeers({})
      peerStream = stream
      stream.on('data', (update: { peers?: Peer[]; revision?: string | number | { toString(): string } }) => {
        if (generation !== connectionGeneration) return
        publish({
          status: 'connected',
          peers: update.peers ?? [],
          revision: update.revision?.toString() ?? '0',
          lastUpdated: new Date().toISOString(),
          error: null,
        })
      })
      stream.on('error', (streamError: Error) => scheduleReconnect(streamError.message, generation))
      stream.on('end', () => scheduleReconnect('The peer stream ended.', generation))
    })
  } catch (error) {
    reconnecting = false
    scheduleReconnect(error instanceof Error ? error.message : String(error), generation)
  }
}

function createWindow() {
  const window = new BrowserWindow({
    width: 1180,
    height: 800,
    minWidth: 900,
    minHeight: 650,
    title: 'Sisyphus',
    backgroundColor: '#10141a',
    webPreferences: {
      preload: join(here, '../preload/index.js'),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
    },
  })
  window.removeMenu()

  window.webContents.setWindowOpenHandler(() => ({ action: 'deny' }))
  if (process.env.ELECTRON_RENDERER_URL) {
    void window.loadURL(process.env.ELECTRON_RENDERER_URL)
  } else {
    void window.loadFile(join(here, '../renderer/index.html'))
  }
  window.webContents.once('did-finish-load', () => {
    window.webContents.send('node:snapshot', snapshot)
  })
}

app.whenReady().then(() => {
  ipcMain.handle('node:get-snapshot', () => snapshot)
  ipcMain.handle('node:reconnect', () => {
    if (retryTimer) clearTimeout(retryTimer)
    retryTimer = null
    reconnecting = false
    retryDelayMs = 1_000
    clearConnection()
    connectToNode()
    return snapshot
  })
  ipcMain.handle('node:connect-peer', (_event, address: unknown) => new Promise<string>((resolve, reject) => {
    if (typeof address !== 'string' || !address.trim()) return reject(new Error('Enter a peer multiaddress.'))
    if (!client) return reject(new Error('The local daemon is not connected.'))
    client.connectPeer({ address: address.trim() }, authorization(), (error, response) => {
      if (error) reject(new Error(error.message))
      else resolve(response?.peerId ?? '')
    })
  }))
  ipcMain.handle('node:set-peer-compute-trust', (_event, payload: unknown) => new Promise<void>((resolve, reject) => {
    if (!payload || typeof payload !== 'object' || !('peerId' in payload) || !('trusted' in payload)) return reject(new Error('Invalid peer trust request.'))
    const request = payload as { peerId: string; trusted: boolean }
    if (!client) return reject(new Error('The local daemon is not connected.'))
    client.setPeerComputeTrust(request, authorization(), (error) => error ? reject(new Error(error.message)) : resolve())
  }))
  createWindow()
  connectToNode()
  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0) createWindow()
  })
}).catch((error: unknown) => {
  console.error('Failed to start Sisyphus desktop:', error)
  app.quit()
})

app.on('before-quit', () => {
  isQuitting = true
  if (retryTimer) clearTimeout(retryTimer)
  clearConnection()
})

app.on('window-all-closed', () => {
  if (process.platform !== 'darwin') app.quit()
})
