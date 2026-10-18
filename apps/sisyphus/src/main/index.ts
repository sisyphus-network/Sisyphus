import { app, BrowserWindow, ipcMain, type IpcMainInvokeEvent } from 'electron'
import { readFileSync, existsSync } from 'node:fs'
import { homedir } from 'node:os'
import { isIP } from 'node:net'
import { join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { WindowStreams } from './window-streams'
import { createIpcLogEntry } from './ipc-logger'
import { DesktopDaemon } from './daemon-process'
import { effectiveLocationDiscoverySettings, loadLocationDiscoverySettings, normalizeLocationDiscoverySettings, supportsGatewayDiscovery } from './location-discovery-settings'
import { normalizePoolLimits, validatePoolLimits } from '../shared/pool-limits.ts'
import { isTrustedRendererUrl } from './renderer-policy'
import { collectDownload } from './file-download'
import { firstUploadMessage } from './file-upload'
import { startNodeStateSession } from './node-state-session'
import { createReconnectBackoff } from './reconnect-backoff'
import * as grpc from '@grpc/grpc-js'
import * as protoLoader from '@grpc/proto-loader'
import * as protobuf from 'protobufjs'
import nodeProtoSource from '../../../../proto/sisyphus/node/v1/node.proto?raw'

const here = fileURLToPath(new URL('.', import.meta.url))
const nodeProtoJson = protobuf.parse(nodeProtoSource).root.toJSON()
const endpoint = process.env.SISYPHUS_API_ADDRESS ?? '127.0.0.1:50051'
const webApiAddress = process.env.SISYPHUS_WEB_API_ADDRESS ?? '127.0.0.1:50052'
const autoStartManagedDaemon = process.env.SISYPHUS_AUTO_START_DAEMON !== '0' && !process.env.SISYPHUS_API_ADDRESS && !process.env.SISYPHUS_API_TOKEN_FILE
const locationDiscoveryPath = join(app.getPath('userData'), 'location-discovery.json')
const initialLocationDiscoverySettings = loadLocationDiscoverySettings(locationDiscoveryPath)
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
  webListenAddress: autoStartManagedDaemon ? webApiAddress : undefined,
  dataDir: daemonDataDir(),
  // A custom endpoint/token belongs to its operator, not this desktop process.
  enabled: autoStartManagedDaemon,
  locationDiscovery: effectiveLocationDiscoverySettings(initialLocationDiscoverySettings, supportsGatewayDiscovery(process.platform)),
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

function readApiToken(): string {
  return readFileSync(tokenFile, 'utf8').trim()
}

function authorization(): grpc.Metadata {
  const metadata = new grpc.Metadata()
  try {
    metadata.set('authorization', `Bearer ${readApiToken()}`)
  } catch {
    // No token: the daemon will refuse the call and say why.
  }
  return metadata
}

function validateLocationDiscoverySettings(value: unknown) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid location discovery settings.')
  const candidate = value as Record<string, unknown>
  if (typeof candidate.locateGateway !== 'boolean' || typeof candidate.locateCountry !== 'boolean' || typeof candidate.stunServer !== 'string') {
    throw new Error('Invalid location discovery settings.')
  }
  return normalizeLocationDiscoverySettings(candidate)
}

function locationDiscoveryState(value: Record<string, unknown>) {
  const saved = normalizeLocationDiscoverySettings(value.saved)
  const running = normalizeLocationDiscoverySettings(value.running)
  return {
    managed: desktopDaemon.managesDaemon,
    configurable: true,
    configured: value.configured === true,
    restartNeeded: value.restartNeeded === true,
    gatewaySupported: value.gatewaySupported === true,
    saved,
    running,
  }
}

// The daemon's database is the only place pool limits are kept, so the
// desktop reads and saves them through the API whichever daemon it is
// connected to: one it started, or a separately managed one reached with
// its endpoint and token.
function poolLimitsState(value: Record<string, unknown>) {
  return {
    managed: desktopDaemon.managesDaemon,
    configurable: true,
    configured: value.configured === true,
    restartNeeded: value.restartNeeded === true,
    coordinator: value.coordinator === true,
    saved: normalizePoolLimits(value.saved),
    running: normalizePoolLimits(value.running),
  }
}

type BrowserPairingInvitation = { code: string; expiresAt: string; pairUrl: string }
type PairedBrowser = { id: string; name: string; createdAt: string }

async function requestBrowserManagement<T>(path: string, method: 'GET' | 'POST' | 'DELETE', body?: unknown): Promise<T> {
  let base: URL
  try { base = new URL(`http://${webApiAddress}`) }
  catch { throw new Error('The local web API address is invalid.') }
  const host = base.hostname.replace(/^\[|\]$/g, '')
  if ((host !== '::1' && !(isIP(host) === 4 && host.startsWith('127.'))) || base.username || base.password || base.pathname !== '/' || base.search || base.hash) {
    throw new Error('Browser management is restricted to the node web API on this machine.')
  }
  // Do not send the node token to an unrelated process that happens to own
  // the configured loopback port. The node's own paired-device API has a
  // distinctive unauthenticated response; only send credentials after it
  // proves it is the expected service.
  let probe: Response
  try { probe = await fetch(new URL('/devices', base)) }
  catch { throw new Error('The local node browser API is not running. Restart the app-managed node and try again.') }
  const probeDetail = await probe.json().catch(() => null) as { code?: unknown; message?: unknown } | null
  if (probe.status !== 401 || probeDetail?.code !== 'Unauthorized' || probeDetail.message !== 'connect this browser before managing paired devices') {
    throw new Error('The local node browser API is unavailable at the configured address.')
  }
  let token: string
  try { token = readApiToken() }
  catch { throw new Error('Could not read the local node API token.') }
  if (!token) throw new Error('The local node API token is empty.')

  const response = await fetch(new URL(path, base), {
    method,
    headers: { Authorization: `Bearer ${token}`, ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  })
  if (!response.ok) {
    const detail = await response.text().catch(() => '')
    throw new Error(detail || `Node browser API returned HTTP ${response.status}.`)
  }
  if (response.status === 204) return undefined as T
  return await response.json() as T
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
  countrySource: string
}

type NodeSnapshot = {
  status: 'connecting' | 'connected' | 'disconnected'
  endpoint: string
  info: NodeInfo | null
  peers: Peer[]
  revision: string
  lastUpdated: string | null
  stateTransport: 'initial-daemon-fetch' | 'realtime' | null
  error: string | null
}

type GrpcNodeService = {
  getNodeInfo(request: object, options: grpc.CallOptions, callback: (error: grpc.ServiceError | null, value?: NodeInfo) => void): void
  watchPeers(request: object): grpc.ClientReadableStream<{ peers: Peer[]; revision: string | number | { toString(): string } }>
  watchNodeState(request: object): grpc.ClientReadableStream<{ info: NodeInfo; peers: Peer[]; revision: string | number | { toString(): string } }>
  connectPeer(request: { address: string }, metadata: grpc.Metadata, callback: (error: grpc.ServiceError | null, value?: { peerId: string }) => void): void
  setPeerComputeTrust(request: { peerId: string; trusted: boolean }, metadata: grpc.Metadata, callback: (error: grpc.ServiceError | null, value?: { trusted: boolean }) => void): void
  setPeerComputePermissions(request: { peerId: string; givesWork: boolean; takesWork: boolean }, metadata: grpc.Metadata, callback: (error: grpc.ServiceError | null, value?: { givesWork: boolean; takesWork: boolean }) => void): void
  storeFile(metadata: grpc.Metadata, callback: (error: grpc.ServiceError | null, response?: Record<string, unknown>) => void): grpc.ClientWritableStream<object> & { write(request: object, callback?: (error?: Error | null) => void): boolean; end(): void }
  fetchFile(request: { cid: string }, metadata: grpc.Metadata): grpc.ClientReadableStream<{ data: Uint8Array }>
  listFiles(request: object, metadata: grpc.Metadata, callback: (error: grpc.ServiceError | null, response?: { files?: { cid?: string; name?: string }[] }) => void): void
  close(): void
}
type NodeStateUpdate = { info: NodeInfo; peers: Peer[]; revision: string | number | { toString(): string } }
type PeerUpdate = { peers: Peer[]; revision: string | number | { toString(): string } }

const initialSnapshot = (): NodeSnapshot => ({
  status: 'connecting',
  endpoint,
  info: null,
  peers: [],
  revision: '0',
  lastUpdated: null,
  stateTransport: null,
  error: null,
})

let snapshot = initialSnapshot()
let client: GrpcNodeService | null = null
let nodeStateSession: (() => void) | null = null
let retryTimer: NodeJS.Timeout | null = null
const reconnectBackoff = createReconnectBackoff(1_000, 15_000)
let reconnecting = false
let isQuitting = false
let connectionGeneration = 0
const activeStreams = new WindowStreams()
const appWindows = new Set<number>()
const developmentRenderer = !app.isPackaged && Boolean(process.env.ELECTRON_RENDERER_URL)
const ipcLoggingEnabled = developmentRenderer && process.env.SISYPHUS_IPC_LOG !== '0'
const rendererUrl = developmentRenderer ? process.env.ELECTRON_RENDERER_URL! : pathToFileURL(join(here, '../renderer/index.html')).href
function handleNode(channel: string, handler: (event: IpcMainInvokeEvent, ...args: any[]) => unknown) {
  ipcMain.handle(channel, async (event, ...args) => {
    if (!appWindows.has(event.sender.id) || event.senderFrame !== event.sender.mainFrame || !event.senderFrame || !isTrustedRendererUrl(event.senderFrame.url, rendererUrl, developmentRenderer)) {
      throw new Error('This frame is not allowed to access the node bridge.')
    }
    const startedAt = performance.now()
    const log = (phase: 'request' | 'response' | 'error', details: { request?: unknown; response?: unknown; error?: unknown; durationMs?: number }) => {
      if (!ipcLoggingEnabled || event.sender.isDestroyed()) return
      event.sender.send('node:ipc-log', createIpcLogEntry({ phase, channel, timestamp: new Date().toISOString(), ...details }))
    }
    log('request', { request: args })
    try {
      const response = await handler(event, ...args)
      log('response', { response, durationMs: Math.round((performance.now() - startedAt) * 100) / 100 })
      return response
    } catch (error) {
      log('error', { error, durationMs: Math.round((performance.now() - startedAt) * 100) / 100 })
      throw error
    }
  })
}
const unaryRpcMethods = new Set([
  'getNodeInfo', 'listPeers', 'getBootstrapPeers', 'setBootstrapPeers', 'listWorkers', 'submitJob', 'getJob', 'listJobs', 'cancelJob',
  'getModelConfig', 'setModelConfig', 'listModelProfiles', 'saveModelProfile', 'deleteModelProfile', 'listProviders', 'listModels', 'removeModel', 'getWebAccessConfig', 'setWebAccessConfig', 'listNetworkInterfaces', 'getModelLending', 'setModelLending', 'inspectOllama', 'getLocationDiscoverySettings', 'setLocationDiscoverySettings', 'getPoolLimits', 'setPoolLimits', 'listChats', 'getChat', 'deleteChat',
  'listFiles', 'removeFile', 'createInvitation', 'listMembers', 'removeMember', 'joinPool',
])
const streamingRpcMethods = new Set(['watchJobs', 'watchJobEvents', 'watchPairedBrowsers', 'watchWorkers', 'ask', 'pullModel'])

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
    if (!window.isDestroyed()) {
      window.webContents.send('node:snapshot', snapshot)
      if (ipcLoggingEnabled) window.webContents.send('node:ipc-log', createIpcLogEntry({ phase: 'event', channel: 'node:snapshot', timestamp: new Date().toISOString(), response: snapshot }))
    }
  }
}

function clearConnection() {
  connectionGeneration += 1
  const previousSession = nodeStateSession
  const previousClient = client
  nodeStateSession = null
  client = null
  activeStreams.clearAll()
  previousSession?.()
  previousClient?.close()
}

function scheduleReconnect(error: string, generation: number) {
  if (isQuitting || retryTimer || generation !== connectionGeneration) return
  clearConnection()
  publish({ status: 'disconnected', stateTransport: null, error: daemonStartupError ? `${error} ${daemonStartupError}` : error })
  retryTimer = setTimeout(() => {
    retryTimer = null
    connectToNode()
  }, reconnectBackoff.next())
}

function connectToNode() {
  if (reconnecting || isQuitting) return
  reconnecting = true
  const generation = ++connectionGeneration
  publish({ status: 'connecting', stateTransport: null, error: null })

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

    nodeStateSession = startNodeStateSession<NodeInfo, NodeStateUpdate, PeerUpdate>({
      client: nextClient,
      isCurrent: () => generation === connectionGeneration,
      unimplementedCode: grpc.status.UNIMPLEMENTED,
      onReady: (info) => {
        reconnecting = false
        reconnectBackoff.reset()
        daemonStartupError = null
        publish({ status: 'connected', info, stateTransport: 'initial-daemon-fetch', error: null })
      },
      onUpdate: (update, legacy) => {
        if (legacy) {
          const peers = update as { peers?: Peer[]; revision?: string | number | { toString(): string } }
          publish({ status: 'connected', peers: peers.peers ?? [], revision: peers.revision?.toString() ?? '0', lastUpdated: new Date().toISOString(), error: null })
          return
        }
        const state = update as { info?: NodeInfo; peers?: Peer[]; revision?: string | number | { toString(): string } }
        publish({
          status: 'connected', info: state.info, peers: state.peers ?? [],
          revision: state.revision?.toString() ?? '0', lastUpdated: new Date().toISOString(), stateTransport: 'realtime', error: null,
        })
      },
      onError: (streamError) => {
        reconnecting = false
        if (streamError.code === grpc.status.UNAVAILABLE) void desktopDaemon.ensureStarted()
        scheduleReconnect(streamError.message, generation)
      },
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
    // Windows and macOS show the packaged app's icon; Linux shows this one.
    ...(process.platform === 'linux' && app.isPackaged ? { icon: join(process.resourcesPath, 'icon.png') } : {}),
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
    if (ipcLoggingEnabled) window.webContents.send('node:ipc-log', createIpcLogEntry({ phase: 'event', channel: 'node:snapshot', timestamp: new Date().toISOString(), response: snapshot }))
  })
  if (developmentRenderer) {
    window.webContents.on('before-input-event', (event, input) => {
      const isDevToolsShortcut = input.type === 'keyDown'
        && (input.key === 'F12'
          || (input.control && input.shift && input.key.toLowerCase() === 'i')
          || (input.meta && input.alt && input.key.toLowerCase() === 'i'))
      if (!isDevToolsShortcut) return
      event.preventDefault()
      if (window.webContents.isDevToolsOpened()) window.webContents.closeDevTools()
      else window.webContents.openDevTools({ mode: 'right' })
    })
  }
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
  handleNode('node:get-location-discovery-settings', async () => {
    try {
      const result = await callUnary('getLocationDiscoverySettings', {}) as Record<string, unknown>
      return locationDiscoveryState(result)
    } catch (error) {
      if (error instanceof Error && /does not implement getLocationDiscoverySettings|UNIMPLEMENTED/.test(error.message)) {
        return { managed: desktopDaemon.managesDaemon, configurable: false, configured: false, restartNeeded: false, gatewaySupported: null,
          saved: initialLocationDiscoverySettings, running: effectiveLocationDiscoverySettings(initialLocationDiscoverySettings, supportsGatewayDiscovery(process.platform)) }
      }
      throw error
    }
  })
  handleNode('node:save-location-discovery-settings', async (_event, value: unknown) => {
    const settings = validateLocationDiscoverySettings(value)
    const result = await callUnary('setLocationDiscoverySettings', { settings }) as Record<string, unknown>
    return locationDiscoveryState(result)
  })
  handleNode('node:get-pool-limits', async () => {
    try {
      return poolLimitsState(await callUnary('getPoolLimits', {}) as Record<string, unknown>)
    } catch (error) {
      if (error instanceof Error && /does not implement getPoolLimits|UNIMPLEMENTED/.test(error.message)) {
        return { managed: desktopDaemon.managesDaemon, configurable: false, configured: false, restartNeeded: false, coordinator: false, saved: normalizePoolLimits({}), running: normalizePoolLimits({}) }
      }
      throw error
    }
  })
  handleNode('node:save-pool-limits', async (_event, value: unknown) => {
    const limits = validatePoolLimits(value)
    return poolLimitsState(await callUnary('setPoolLimits', { limits }) as Record<string, unknown>)
  })
  handleNode('node:reconnect', () => {
    if (retryTimer) clearTimeout(retryTimer)
    retryTimer = null
    reconnecting = false
    reconnectBackoff.reset()
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
  handleNode('node:browser:create-pairing', () => requestBrowserManagement<BrowserPairingInvitation>('/pairing', 'POST', {}))
  handleNode('node:browser:list-paired', () => requestBrowserManagement<{ devices: PairedBrowser[] }>('/devices', 'GET'))
  handleNode('node:browser:revoke-paired', (_event, id: unknown) => {
    if (typeof id !== 'string' || !id.trim() || id.length > 128) throw new Error('Invalid paired browser ID.')
    return requestBrowserManagement<void>(`/devices/${encodeURIComponent(id)}`, 'DELETE')
  })
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
    stream.on('data', (data) => {
      if (!activeStreams.has(owner, id, stream) || event.sender.isDestroyed()) return
      event.sender.send(channel, { data })
      if (ipcLoggingEnabled) event.sender.send('node:ipc-log', createIpcLogEntry({ phase: 'event', channel, timestamp: new Date().toISOString(), response: data }))
    })
    stream.on('error', (error: grpc.ServiceError) => {
      if (!activeStreams.has(owner, id, stream)) return
      activeStreams.release(owner, id, stream)
      if (!event.sender.isDestroyed()) event.sender.send(channel, { error: error.message, errorCode: error.code })
      if (ipcLoggingEnabled && !event.sender.isDestroyed()) event.sender.send('node:ipc-log', createIpcLogEntry({ phase: 'error', channel, timestamp: new Date().toISOString(), error: { message: error.message, code: error.code } }))
    })
    stream.on('end', () => {
      if (!activeStreams.has(owner, id, stream)) return
      activeStreams.release(owner, id, stream)
      if (!event.sender.isDestroyed()) event.sender.send(channel, { end: true })
      if (ipcLoggingEnabled && !event.sender.isDestroyed()) event.sender.send('node:ipc-log', createIpcLogEntry({ phase: 'event', channel, timestamp: new Date().toISOString(), response: { end: true } }))
    })
  })
  handleNode('node:rpc-stream-stop', (event, id: unknown) => {
    if (typeof id !== 'string') return
    activeStreams.stop(event.sender.id, id)
  })
  handleNode('node:store-file', (event, payload: unknown) => new Promise((resolve, reject) => {
    if (!client) return reject(new Error('The local daemon is not connected.'))
    if (!payload || typeof payload !== 'object' || !('name' in payload) || !('data' in payload) || !('private' in payload)) return reject(new Error('Invalid file upload.'))
    const file = payload as { name: unknown; data: unknown; private: unknown; chatAttachment?: unknown }
    if (typeof file.name !== 'string' || typeof file.private !== 'boolean' || !(file.data instanceof Uint8Array)) return reject(new Error('Invalid file upload.'))
    if (file.chatAttachment !== undefined && typeof file.chatAttachment !== 'boolean') return reject(new Error('Invalid attachment upload.'))
    if (file.chatAttachment && !file.private) return reject(new Error('Chat attachments must be private.'))
    const metadata = { name: file.name, private: file.private, chatAttachment: file.chatAttachment === true }
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
        ? firstUploadMessage(metadata, chunk)
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
      stream.write(firstUploadMessage(metadata, Buffer.alloc(0)), (error?: Error | null) => {
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
