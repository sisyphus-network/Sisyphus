import { app, BrowserWindow, ipcMain, type IpcMainInvokeEvent } from 'electron'
import { readFileSync, existsSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { WindowStreams } from './window-streams'
import { DesktopDaemon } from './daemon-process'
import { isTrustedRendererUrl } from './renderer-policy'
import { collectDownload } from './file-download'
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
const tokenFile = resolveTokenFile()
let daemonStartupError: string | null = null
const desktopDaemon = new DesktopDaemon({
  executable: process.env.SISYPHUS_DAEMON_PATH ?? (app.isPackaged
    ? join(process.resourcesPath, 'bin', process.platform === 'win32' ? 'sisyphusd.exe' : 'sisyphusd')
    : join(here, '../../../../bin', process.platform === 'win32' ? 'sisyphusd.exe' : 'sisyphusd')),
  endpoint,
  dataDir: daemonDataDir(),
  // A custom endpoint/token belongs to its operator, not this desktop process.
  enabled: process.env.SISYPHUS_AUTO_START_DAEMON !== '0' && !process.env.SISYPHUS_API_ADDRESS && !process.env.SISYPHUS_API_TOKEN_FILE,
  onError: (message) => { daemonStartupError = message },
})

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

function resolveTokenFile(): string {
  if (process.env.SISYPHUS_API_TOKEN_FILE) return process.env.SISYPHUS_API_TOKEN_FILE
  return join(daemonDataDir(), 'api.token')
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
  // The two sides of this node's trust in the peer.
  givesWork?: boolean
  takesWork?: boolean
  countryCode?: string
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
  getNodeInfo(request: object, options: grpc.CallOptions, callback: (error: grpc.ServiceError | null, value?: NodeInfo) => void): void
  watchPeers(request: object): grpc.ClientReadableStream<{ peers: Peer[]; revision: string | number | { toString(): string } }>
  connectPeer(request: { address: string }, metadata: grpc.Metadata, callback: (error: grpc.ServiceError | null, value?: { peerId: string }) => void): void
  setPeerComputeTrust(request: { peerId: string; trusted: boolean }, metadata: grpc.Metadata, callback: (error: grpc.ServiceError | null, value?: { trusted: boolean }) => void): void
  setPeerComputePermissions(request: { peerId: string; givesWork: boolean; takesWork: boolean }, metadata: grpc.Metadata, callback: (error: grpc.ServiceError | null, value?: { givesWork: boolean; takesWork: boolean }) => void): void
  storeFile(metadata: grpc.Metadata, callback: (error: grpc.ServiceError | null, response?: Record<string, unknown>) => void): grpc.ClientWritableStream<object> & { write(request: object, callback?: (error?: Error | null) => void): boolean; end(): void }
  fetchFile(request: { cid: string }, metadata: grpc.Metadata): grpc.ClientReadableStream<{ data: Uint8Array }>
  listFiles(request: object, metadata: grpc.Metadata, callback: (error: grpc.ServiceError | null, response?: { files?: { cid?: string; name?: string }[] }) => void): void
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
const activeStreams = new WindowStreams()
const appWindows = new Set<number>()
const developmentRenderer = !app.isPackaged && Boolean(process.env.ELECTRON_RENDERER_URL)
const rendererUrl = developmentRenderer ? process.env.ELECTRON_RENDERER_URL! : pathToFileURL(join(here, '../renderer/index.html')).href
function handleNode(channel: string, handler: (event: IpcMainInvokeEvent, ...args: any[]) => unknown) {
  ipcMain.handle(channel, (event, ...args) => {
    if (!appWindows.has(event.sender.id) || event.senderFrame !== event.sender.mainFrame || !event.senderFrame || !isTrustedRendererUrl(event.senderFrame.url, rendererUrl, developmentRenderer)) {
      throw new Error('This frame is not allowed to access the node bridge.')
    }
    return handler(event, ...args)
  })
}
const unaryRpcMethods = new Set([
  'getNodeInfo', 'listPeers', 'getBootstrapPeers', 'setBootstrapPeers', 'listWorkers', 'submitJob', 'getJob', 'listJobs', 'cancelJob',
  'getModelConfig', 'setModelConfig', 'listProviders', 'listModels', 'removeModel', 'listChats', 'getChat', 'deleteChat',
  'listFiles', 'removeFile', 'createInvitation', 'listMembers', 'removeMember', 'joinPool',
])
const streamingRpcMethods = new Set(['watchJobs', 'watchJobEvents', 'ask', 'pullModel'])

function callUnary(method: string, request: Record<string, unknown>): Promise<unknown> {
  return new Promise((resolve, reject) => {
    if (!client) return reject(new Error('The local daemon is not connected.'))
    const rpc = (client as unknown as Record<string, unknown>)[method]
    if (typeof rpc !== 'function') return reject(new Error(`The daemon does not implement ${method}.`))
    type UnaryCallback = (error: grpc.ServiceError | null, response?: unknown) => void
    const callback: UnaryCallback = (error, response) => error ? reject(new Error(error.message)) : resolve(response ?? {})
    ;(rpc as (request: Record<string, unknown>, metadata: grpc.Metadata, callback: UnaryCallback) => void).call(client, request, authorization(), callback)
  })
}

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
  activeStreams.clearAll()
  previousStream?.cancel()
  previousClient?.close()
}

function scheduleReconnect(error: string, generation: number) {
  if (isQuitting || retryTimer || generation !== connectionGeneration) return
  clearConnection()
  publish({ status: 'disconnected', error: daemonStartupError ? `${error} ${daemonStartupError}` : error })
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

    nextClient.getNodeInfo({}, { deadline: Date.now() + 5000 }, (error, info) => {
      if (generation !== connectionGeneration) return
      reconnecting = false
      if (error || !info) {
        if (error?.code === grpc.status.UNAVAILABLE) void desktopDaemon.ensureStarted()
        scheduleReconnect(error?.message ?? 'The daemon returned no node information.', generation)
        return
      }

      retryDelayMs = 1_000
      daemonStartupError = null
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
  const owner = window.webContents.id
  appWindows.add(owner)
  window.webContents.on('destroyed', () => { appWindows.delete(owner); activeStreams.clear(owner) })
  window.webContents.on('render-process-gone', () => activeStreams.clear(owner))
  window.webContents.on('did-start-navigation', (_event, _url, inPlace, mainFrame) => {
    if (mainFrame && !inPlace) activeStreams.clear(owner)
  })
  window.webContents.on('will-navigate', (event, url) => {
    if (!isTrustedRendererUrl(url, rendererUrl, developmentRenderer)) event.preventDefault()
  })

  window.webContents.setWindowOpenHandler(() => ({ action: 'deny' }))
  if (developmentRenderer) {
    void window.loadURL(rendererUrl)
  } else {
    void window.loadFile(join(here, '../renderer/index.html'))
  }
  window.webContents.once('did-finish-load', () => {
    window.webContents.send('node:snapshot', snapshot)
    if (!app.isPackaged && process.env.ELECTRON_RENDERER_URL) {
      window.webContents.openDevTools({ mode: 'right' })
    }
  })
}

app.whenReady().then(() => {
  if (!app.requestSingleInstanceLock()) {
    app.quit()
    return
  }
  app.on('second-instance', () => {
    const window = BrowserWindow.getAllWindows()[0]
    if (window?.isMinimized()) window.restore()
    window?.focus()
  })
  handleNode('node:get-snapshot', () => snapshot)
  handleNode('node:reconnect', () => {
    if (retryTimer) clearTimeout(retryTimer)
    retryTimer = null
    reconnecting = false
    retryDelayMs = 1_000
    clearConnection()
    connectToNode()
    return snapshot
  })
  handleNode('node:connect-peer', (_event, address: unknown) => new Promise<string>((resolve, reject) => {
    if (typeof address !== 'string' || !address.trim()) return reject(new Error('Enter a peer multiaddress.'))
    if (!client) return reject(new Error('The local daemon is not connected.'))
    client.connectPeer({ address: address.trim() }, authorization(), (error, response) => {
      if (error) reject(new Error(error.message))
      else resolve(response?.peerId ?? '')
    })
  }))
  handleNode('node:set-peer-compute-trust', (_event, payload: unknown) => new Promise<void>((resolve, reject) => {
    if (!payload || typeof payload !== 'object' || !('peerId' in payload) || !('trusted' in payload)) return reject(new Error('Invalid peer trust request.'))
    const request = payload as { peerId: string; trusted: boolean }
    if (!client) return reject(new Error('The local daemon is not connected.'))
    client.setPeerComputeTrust(request, authorization(), (error) => error ? reject(new Error(error.message)) : resolve())
  }))
  handleNode('node:set-peer-compute-permissions', (_event, payload: unknown) => new Promise<void>((resolve, reject) => {
    if (!payload || typeof payload !== 'object' || !('peerId' in payload) || !('givesWork' in payload) || !('takesWork' in payload)) return reject(new Error('Invalid peer trust request.'))
    const request = payload as { peerId: string; givesWork: boolean; takesWork: boolean }
    if (!client) return reject(new Error('The local daemon is not connected.'))
    client.setPeerComputePermissions(request, authorization(), (error) => error ? reject(new Error(error.message)) : resolve())
  }))
  handleNode('node:rpc', (_event, payload: unknown) => {
    if (!payload || typeof payload !== 'object' || !('method' in payload) || !('request' in payload)) throw new Error('Invalid daemon RPC request.')
    const { method, request } = payload as { method: unknown; request: unknown }
    if (typeof method !== 'string' || !unaryRpcMethods.has(method)) throw new Error('This daemon operation is not available to the desktop.')
    if (!request || typeof request !== 'object' || Array.isArray(request)) throw new Error('Invalid daemon RPC parameters.')
    return callUnary(method, request as Record<string, unknown>)
  })
  handleNode('node:rpc-stream-start', (event, payload: unknown) => {
    if (!payload || typeof payload !== 'object' || !('id' in payload) || !('method' in payload) || !('request' in payload)) throw new Error('Invalid daemon stream request.')
    const { id, method, request } = payload as { id: unknown; method: unknown; request: unknown }
    if (typeof id !== 'string' || !/^[\da-f-]{36}$/i.test(id)) throw new Error('Invalid stream identifier.')
    if (typeof method !== 'string' || !streamingRpcMethods.has(method)) throw new Error('This daemon stream is not available to the desktop.')
    if (!request || typeof request !== 'object' || Array.isArray(request)) throw new Error('Invalid daemon stream parameters.')
    if (!client) throw new Error('The local daemon is not connected.')
    const owner = event.sender.id
    const rpc = (client as unknown as Record<string, unknown>)[method]
    if (typeof rpc !== 'function') throw new Error(`The daemon does not implement ${method}.`)
    const stream = (rpc as (request: Record<string, unknown>, metadata: grpc.Metadata) => grpc.ClientReadableStream<unknown>).call(client, request as Record<string, unknown>, authorization())
    activeStreams.add(owner, id, stream)
    const channel = `node:rpc-stream:${id}`
    stream.on('data', (data) => { if (activeStreams.has(owner, id, stream) && !event.sender.isDestroyed()) event.sender.send(channel, { data }) })
    stream.on('error', (error: grpc.ServiceError) => {
      if (!activeStreams.has(owner, id, stream)) return
      activeStreams.release(owner, id, stream)
      if (!event.sender.isDestroyed()) event.sender.send(channel, { error: error.message, errorCode: error.code })
    })
    stream.on('end', () => {
      if (!activeStreams.has(owner, id, stream)) return
      activeStreams.release(owner, id, stream)
      if (!event.sender.isDestroyed()) event.sender.send(channel, { end: true })
    })
  })
  handleNode('node:rpc-stream-stop', (event, id: unknown) => {
    if (typeof id !== 'string') return
    activeStreams.stop(event.sender.id, id)
  })
  handleNode('node:store-file', (event, payload: unknown) => new Promise((resolve, reject) => {
    if (!client) return reject(new Error('The local daemon is not connected.'))
    if (!payload || typeof payload !== 'object' || !('name' in payload) || !('data' in payload) || !('private' in payload)) return reject(new Error('Invalid file upload.'))
    const file = payload as { name: unknown; data: unknown; private: unknown }
    if (typeof file.name !== 'string' || typeof file.private !== 'boolean' || !(file.data instanceof Uint8Array)) return reject(new Error('Invalid file upload.'))
    const data = file.data
    if (data.byteLength > 256 * 1024 * 1024) return reject(new Error('The file exceeds this desktop client’s 256 MiB upload limit.'))
    let settled = false
    const owner = event.sender.id
    const transferId = `upload:${crypto.randomUUID()}`
    const stream = client.storeFile(authorization(), (error, response) => {
      activeStreams.release(owner, transferId, transfer)
      if (settled) return
      settled = true
      if (error) reject(new Error(error.message))
      else resolve(response ?? {})
    })
    const transfer = { cancel: () => {
      if (settled) return
      settled = true
      reject(new Error('The file upload was cancelled.'))
      stream.cancel()
    } }
    activeStreams.add(owner, transferId, transfer)
    const chunkSize = 256 * 1024
    const writeChunk = (offset: number): void => {
      if (settled) return
      const chunk = Buffer.from(data.subarray(offset, Math.min(offset + chunkSize, data.byteLength)))
      const request = offset === 0
        ? { name: file.name, private: file.private, data: chunk }
        : { data: chunk }
      stream.write(request, (error?: Error | null) => {
        if (settled) return
        if (error) {
          activeStreams.release(owner, transferId, transfer)
          settled = true
          stream.cancel()
          reject(new Error(error.message))
          return
        }
        const nextOffset = offset + chunk.length
        if (nextOffset >= data.byteLength) stream.end()
        else writeChunk(nextOffset)
      })
    }
    // Empty files still need an initial message carrying their metadata.
    if (data.byteLength === 0) {
      stream.write({ name: file.name, private: file.private, data: Buffer.alloc(0) }, (error?: Error | null) => {
        if (settled) return
        if (error) {
          activeStreams.release(owner, transferId, transfer)
          settled = true
          stream.cancel()
          reject(new Error(error.message))
        } else stream.end()
      })
    } else writeChunk(0)
  }))
  handleNode('node:fetch-file', (event, cid: unknown) => {
    if (!client) throw new Error('The local daemon is not connected.')
    if (typeof cid !== 'string' || !cid.trim()) throw new Error('A file CID is required.')
    const stream = client.fetchFile({ cid: cid.trim() }, authorization())
    const transfer = collectDownload(stream)
    const owner = event.sender.id
    const transferId = `download:${crypto.randomUUID()}`
    activeStreams.add(owner, transferId, transfer)
    return transfer.promise.finally(() => activeStreams.release(owner, transferId, transfer))
  })
  createWindow()
  connectToNode()
  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0) createWindow()
  })
}).catch((error: unknown) => {
  console.error('Failed to start Sisyphus desktop:', error)
  app.quit()
})

let daemonStopped = false
let shutdownStarted = false
app.on('before-quit', (event) => {
  isQuitting = true
  if (retryTimer) clearTimeout(retryTimer)
  clearConnection()
  if (!daemonStopped) {
    event.preventDefault()
    if (!shutdownStarted) {
      shutdownStarted = true
      void desktopDaemon.stop().finally(() => { daemonStopped = true; app.quit() })
    }
  }
})

app.on('window-all-closed', () => {
  if (process.platform !== 'darwin') app.quit()
})
