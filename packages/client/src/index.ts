import { Code, ConnectError, createClient } from '@connectrpc/connect'
import { MethodKind } from '@bufbuild/protobuf'
import { createConnectTransport } from '@connectrpc/connect-web'
import { NodeService } from './gen/sisyphus/node/v1/node_connect.js'

export { NodeService } from './gen/sisyphus/node/v1/node_connect.js'
export * from './gen/sisyphus/node/v1/node_pb.js'
export type { Client } from '@connectrpc/connect'

type NodeMethods = typeof NodeService.methods
export type UnaryNodeMethod = {
  [Name in keyof NodeMethods]: NodeMethods[Name]['kind'] extends MethodKind.Unary ? Name : never
}[keyof NodeMethods]
export type ServerStreamingNodeMethod = {
  [Name in keyof NodeMethods]: NodeMethods[Name]['kind'] extends MethodKind.ServerStreaming ? Name : never
}[keyof NodeMethods]

export type NodeServiceClient = ReturnType<typeof createNodeServiceClient>

export type BrowserClientOptions = {
  /** Base URL of the node's web API. Defaults to the current page's origin. */
  baseUrl?: string
  /** Supplies a short-lived credential without persisting it in the UI bundle. */
  getAuthorization?: () => string | Promise<string>
  fetch?: typeof globalThis.fetch
}

export type BrowserSessionOptions = {
  baseUrl?: string
  fetch?: typeof globalThis.fetch
}

export type PairedBrowser = { id: string; name: string; createdAt: string }

async function webControl<T>(path: string, init: RequestInit = {}, options: BrowserSessionOptions = {}): Promise<T> {
  const baseUrl = options.baseUrl ?? globalThis.location?.origin
  if (!baseUrl) throw new Error('A node API base URL is required outside a browser.')
  const response = await (options.fetch ?? globalThis.fetch)(new URL(path, baseUrl), {
    ...init,
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', ...init.headers },
  })
  if (!response.ok) {
    const detail = await response.text().catch(() => '')
    throw new Error(detail || `Node request failed (HTTP ${response.status}).`)
  }
  if (response.status === 204) return undefined as T
  return await response.json() as T
}

export async function createBrowserPairing(options?: BrowserSessionOptions) {
  return webControl<{ code: string; expiresAt: string; pairUrl: string }>('/pairing', { method: 'POST', body: '{}' }, options)
}

export async function pairBrowser(code: string, name: string, options?: BrowserSessionOptions) {
  await webControl<void>('/pair', { method: 'POST', body: JSON.stringify({ code, name }) }, options)
}

export async function listPairedBrowsers(options?: BrowserSessionOptions) {
  return webControl<{ devices: PairedBrowser[] }>('/devices', { method: 'GET' }, options)
}

export async function revokePairedBrowser(id: string, options?: BrowserSessionOptions) {
  await webControl<void>(`/devices/${encodeURIComponent(id)}`, { method: 'DELETE' }, options)
}

/** Creates a typed client directly from the NodeService protobuf contract. */
export function createNodeServiceClient(options: BrowserClientOptions = {}) {
  const baseUrl = options.baseUrl ?? globalThis.location?.origin
  if (!baseUrl) throw new Error('A node API base URL is required outside a browser.')

  const fetcher = options.fetch ?? globalThis.fetch
  const transport = createConnectTransport({
    baseUrl: baseUrl.replace(/\/$/, ''),
    useBinaryFormat: false,
    fetch: async (input, init) => {
      const headers = new Headers(init?.headers)
      if (options.getAuthorization) {
        const authorization = await options.getAuthorization()
        if (authorization) headers.set('Authorization', authorization)
      }
      return fetcher(input, { ...init, headers, credentials: 'same-origin' })
    },
  })

  return createClient(NodeService, transport)
}

/** Enrolls this browser tab; the API token is exchanged for an HttpOnly cookie. */
export async function startBrowserSession(apiToken: string, options: BrowserSessionOptions = {}) {
  const baseUrl = options.baseUrl ?? globalThis.location?.origin
  if (!baseUrl) throw new Error('A node API base URL is required outside a browser.')
  if (!apiToken.trim()) throw new Error('Enter the node API token.')
  const response = await (options.fetch ?? globalThis.fetch)(new URL('/session', baseUrl), {
    method: 'POST',
    headers: { Authorization: `Bearer ${apiToken.trim()}` },
    credentials: 'same-origin',
  })
  if (!response.ok) {
    const detail = await response.text().catch(() => '')
    throw new Error(detail || `Could not connect this browser (HTTP ${response.status}).`)
  }
}

/** Ends the server-managed browser session without exposing its cookie to JS. */
export async function endBrowserSession(options: BrowserSessionOptions = {}) {
  const baseUrl = options.baseUrl ?? globalThis.location?.origin
  if (!baseUrl) throw new Error('A node API base URL is required outside a browser.')
  const response = await (options.fetch ?? globalThis.fetch)(new URL('/session', baseUrl), {
    method: 'DELETE',
    credentials: 'same-origin',
  })
  if (!response.ok) throw new Error(`Could not end the browser session (HTTP ${response.status}).`)
}

export function isAuthenticationError(error: unknown): boolean {
  if (error instanceof ConnectError) return error.code === Code.Unauthenticated || error.code === Code.PermissionDenied
  if (error instanceof Error) return /unauthenticated|unauthorized|HTTP 401|HTTP 403/i.test(error.message)
  return false
}

export type ClientContext =
  | { kind: 'electron'; bridge: unknown }
  | { kind: 'browser'; baseUrl: string }

export type SisyphusClient =
  | { context: 'electron'; bridge: unknown }
  | { context: 'browser'; baseUrl: string; node: NodeServiceClient }

/** Detects the host environment; it never guesses a remote node address. */
export function detectClientContext(): ClientContext {
  if (typeof window !== 'undefined' && 'sisyphus' in window) {
    const bridge = (window as Window & { sisyphus?: unknown }).sisyphus
    if (bridge) return { kind: 'electron', bridge }
  }
  if (typeof window === 'undefined') {
    throw new Error('Client context is unavailable outside Electron or a browser.')
  }
  return { kind: 'browser', baseUrl: window.location.origin }
}

/** Selects the host adapter once, keeping environment checks out of UI components. */
export function createSisyphusClient(options: Omit<BrowserClientOptions, 'baseUrl'> = {}): SisyphusClient {
  const context = detectClientContext()
  if (context.kind === 'electron') return { context: 'electron', bridge: context.bridge }
  return {
    context: 'browser',
    baseUrl: context.baseUrl,
    node: createNodeServiceClient({ ...options, baseUrl: context.baseUrl }),
  }
}
