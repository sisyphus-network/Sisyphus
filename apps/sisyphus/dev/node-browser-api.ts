import type { ServerResponse } from 'node:http'
import { request as httpRequest, type IncomingMessage } from 'node:http'
import { TLSSocket } from 'node:tls'
import type { Plugin } from 'vite'
import * as grpc from '@grpc/grpc-js'
import * as protoLoader from '@grpc/proto-loader'

type Peer = { peerId: string; connectionState: number | string; knownAddresses: string[] }
type NodeInfo = { peerId: string; daemonVersion: string; listenAddresses: string[] }
type Snapshot = {
  status: 'connecting' | 'connected' | 'disconnected'
  endpoint: string
  info: NodeInfo | null
  peers: Peer[]
  revision: string
  lastUpdated: string | null
  error: string | null
}
type NodeClient = {
  getNodeInfo(request: object, callback: (error: grpc.ServiceError | null, value?: NodeInfo) => void): void
  watchPeers(request: object): grpc.ClientReadableStream<{ peers?: Peer[]; revision?: string | number | { toString(): string } }>
  close(): void
}

export function nodeBrowserApiPlugin(protoPath: string): Plugin {
  const endpoint = process.env.SISYPHUS_API_ADDRESS ?? '127.0.0.1:50051'
  const webApiAddress = process.env.SISYPHUS_WEB_API_ADDRESS ?? '127.0.0.1:50052'
  const webApiURL = new URL(`http://${webApiAddress}`)
  const webApiHost = webApiURL.hostname.replace(/^\[|\]$/g, '')
  if (!(webApiHost === '::1' || /^127\./.test(webApiHost)) || webApiURL.username || webApiURL.password || webApiURL.pathname !== '/' || webApiURL.search || webApiURL.hash) {
    throw new Error('SISYPHUS_WEB_API_ADDRESS must point to a loopback-only sisyphusd web listener.')
  }
  let snapshot: Snapshot = { status: 'connecting', endpoint, info: null, peers: [], revision: '0', lastUpdated: null, error: null }
  let client: NodeClient | null = null
  let stream: grpc.ClientReadableStream<unknown> | null = null
  let retryTimer: NodeJS.Timeout | null = null
  let retryDelay = 1_000
  let generation = 0
  let stopping = false
  const subscribers = new Set<ServerResponse>()

  function isWebAPIRequest(pathname: string, method: string): boolean {
    if (pathname === '/session') return method === 'POST' || method === 'DELETE'
    if (pathname === '/pair' || pathname === '/pairing') return method === 'POST'
    if (pathname === '/devices') return method === 'GET'
    if (pathname.startsWith('/devices/')) return method === 'DELETE'
    if (pathname.startsWith('/sisyphus.node.v1.NodeService/')) return method === 'POST'
    return pathname.startsWith('/files/')
  }

  function sameDevOrigin(request: IncomingMessage): boolean {
    const host = request.headers.host
    if (!host) return false
    const expectedProtocol = request.socket instanceof TLSSocket ? 'https:' : 'http:'
    const origin = request.headers.origin
    if (origin) {
      try {
        const parsed = new URL(origin)
        return parsed.protocol === expectedProtocol && parsed.host === host
      } catch { return false }
    }
    const referer = request.headers.referer
    if (referer) {
      try {
        const parsed = new URL(referer)
        return parsed.protocol === expectedProtocol && parsed.host === host
      } catch { return false }
    }
    return true
  }

  function proxyWebAPI(request: IncomingMessage, response: ServerResponse) {
    const requestOrigin = request.socket instanceof TLSSocket ? `https://${request.headers.host}` : `http://${request.headers.host}`
    const requestURL = new URL(request.url ?? '/', requestOrigin)
    const upstreamHeaders: Record<string, string | string[] | undefined> = { ...request.headers, host: webApiURL.host }
    // The Vite preview is only a transport proxy. Authentication must come
    // from the browser's paired session or an explicitly supplied credential;
    // never silently authenticate a visitor with the node's API token.
    if (request.headers.origin) upstreamHeaders.origin = webApiURL.origin
    if (request.headers.referer) {
      try {
        const referer = new URL(request.headers.referer)
        upstreamHeaders.referer = `${webApiURL.origin}${referer.pathname}${referer.search}`
      } catch { delete upstreamHeaders.referer }
    }
    const upstream = httpRequest({
      hostname: webApiURL.hostname.replace(/^\[|\]$/g, ''),
      port: webApiURL.port || 80,
      method: request.method,
      path: `${requestURL.pathname}${requestURL.search}`,
      headers: upstreamHeaders,
    }, (upstreamResponse) => {
      response.writeHead(upstreamResponse.statusCode ?? 502, upstreamResponse.headers)
      upstreamResponse.pipe(response)
    })
    upstream.on('error', (error) => {
      if (response.headersSent) { response.destroy(error); return }
      response.statusCode = 502
      response.setHeader('Content-Type', 'text/plain; charset=utf-8')
      response.end('The local Sisyphus web API is unavailable. Start sisyphusd with --web-listen 127.0.0.1:50052.')
    })
    request.pipe(upstream)
  }

  function publish(next: Partial<Snapshot>) {
    snapshot = { ...snapshot, ...next }
    const event = `data: ${JSON.stringify(snapshot)}\n\n`
    for (const response of subscribers) {
      if (response.destroyed || response.writableEnded) subscribers.delete(response)
      else response.write(event)
    }
  }

  function closeConnection() {
    generation += 1
    stream?.cancel()
    client?.close()
    stream = null
    client = null
  }

  function scheduleReconnect(error: string, currentGeneration: number) {
    if (stopping || retryTimer || currentGeneration !== generation) return
    closeConnection()
    publish({ status: 'disconnected', error })
    retryTimer = setTimeout(() => {
      retryTimer = null
      connect()
    }, retryDelay)
    retryDelay = Math.min(retryDelay * 2, 15_000)
  }

  function connect() {
    if (stopping) return
    closeConnection()
    const currentGeneration = generation
    publish({ status: 'connecting', error: null })
    try {
      const definition = protoLoader.loadSync(protoPath, { keepCase: false, longs: String, enums: String, defaults: true, oneofs: true })
      const loaded = grpc.loadPackageDefinition(definition) as unknown as {
        sisyphus: { node: { v1: { NodeService: new (address: string, credentials: grpc.ChannelCredentials) => NodeClient } } }
      }
      const nextClient = new loaded.sisyphus.node.v1.NodeService(endpoint, grpc.credentials.createInsecure())
      client = nextClient
      nextClient.getNodeInfo({}, (error, info) => {
        if (currentGeneration !== generation) return
        if (error || !info) {
          scheduleReconnect(error?.message ?? 'The daemon returned no node information.', currentGeneration)
          return
        }
        retryDelay = 1_000
        publish({ status: 'connected', info, error: null })
        const peerStream = nextClient.watchPeers({})
        stream = peerStream
        peerStream.on('data', (update) => {
          if (currentGeneration !== generation) return
          publish({ status: 'connected', peers: update.peers ?? [], revision: update.revision?.toString() ?? '0', lastUpdated: new Date().toISOString(), error: null })
        })
        peerStream.on('error', (streamError: Error) => scheduleReconnect(streamError.message, currentGeneration))
        peerStream.on('end', () => scheduleReconnect('The peer stream ended.', currentGeneration))
      })
    } catch (error) {
      scheduleReconnect(error instanceof Error ? error.message : String(error), currentGeneration)
    }
  }

  return {
    name: 'sisyphus-node-browser-api',
    configureServer(server) {
      connect()
      server.middlewares.use((request, response, next) => {
        const pathname = new URL(request.url ?? '/', 'http://localhost').pathname
        if (isWebAPIRequest(pathname, request.method ?? 'GET')) {
          if (!sameDevOrigin(request)) {
            response.statusCode = 403
            response.end('Cross-origin requests are not allowed through the Sisyphus development proxy.')
            return
          }
          proxyWebAPI(request, response)
          return
        }
        if (pathname === '/api/node/snapshot' && request.method === 'GET') {
          response.statusCode = 200
          response.setHeader('Content-Type', 'application/json; charset=utf-8')
          response.setHeader('Cache-Control', 'no-store')
          response.end(JSON.stringify(snapshot))
          return
        }
        if (pathname === '/api/node/events' && request.method === 'GET') {
          response.statusCode = 200
          response.setHeader('Content-Type', 'text/event-stream; charset=utf-8')
          response.setHeader('Cache-Control', 'no-cache, no-transform')
          response.setHeader('Connection', 'keep-alive')
          response.flushHeaders()
          subscribers.add(response)
          response.write(`data: ${JSON.stringify(snapshot)}\n\n`)
          request.on('close', () => subscribers.delete(response))
          return
        }
        if (pathname.startsWith('/api/node/')) {
          response.statusCode = 404
          response.end('Not found')
          return
        }
        next()
      })
      server.httpServer?.once('close', () => {
        stopping = true
        if (retryTimer) clearTimeout(retryTimer)
        closeConnection()
        for (const response of subscribers) response.end()
        subscribers.clear()
      })
    },
  }
}
