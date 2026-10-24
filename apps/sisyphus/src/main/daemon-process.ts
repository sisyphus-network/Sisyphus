import { spawn, type ChildProcess } from 'node:child_process'
import { createConnection } from 'node:net'

export function localApiAddress(endpoint: string): { host: string; port: number } | null {
  const match = /^(127\.0\.0\.1|localhost|\[::1\]):(\d+)$/.exec(endpoint)
  if (!match) return null
  const port = Number(match[2])
  return port > 0 && port <= 65535 ? { host: match[1] === '[::1]' ? '::1' : match[1], port } : null
}

/** An occupied port is never ours to take over, even if it does not speak gRPC. */
export function portIsUnused(endpoint: string): Promise<boolean> {
  const address = localApiAddress(endpoint)
  if (!address) return Promise.resolve(false)
  return new Promise((resolve) => {
    const socket = createConnection(address)
    let settled = false
    const finish = (unused: boolean) => {
      if (settled) return
      settled = true
      socket.destroy()
      resolve(unused)
    }
    socket.setTimeout(750, () => finish(false))
    socket.once('connect', () => finish(false))
    socket.once('error', (error: NodeJS.ErrnoException) => finish(error.code === 'ECONNREFUSED'))
  })
}

type Options = {
  executable: string
  endpoint: string
  webListenAddress?: string
  /** Static renderer files served by the daemon's remote HTTPS listener. */
  webUiDirectory?: string
  dataDir: string
  enabled: boolean
  locationDiscovery?: { locateGateway: boolean; locateCountry: boolean; stunServer: string }
  /** Where other machines reach this node's pool, if it hosts one for them. */
  hostingListen?: string
  /**
   * The flags that have the node run the pool's containers, worked out as it
   * starts: whether Docker answers is known only then.
   */
  containers?: () => Promise<string[]>
  unused?: (endpoint: string) => Promise<boolean>
  launch?: (executable: string, args: string[]) => ChildProcess
  onError: (message: string) => void
  shutdownGraceMs?: number
}

export const daemonShutdownGraceMs = 30000

/** Own only the process we launched. Never kill an existing independently-run node. */
export class DesktopDaemon {
  private child: ChildProcess | null = null
  private launched = false
  private attempted = false
  private stopping = false
  private pending: Promise<void> | null = null
  private readonly options: Options
  constructor(options: Options) { this.options = options }

  get ownsProcess(): boolean { return this.child !== null }
  get isStarting(): boolean { return this.pending !== null }
  /** Settings remain app-managed if our child exits unexpectedly. */
  get managesDaemon(): boolean { return this.launched || this.ownsProcess || this.isStarting }

  ensureStarted(): Promise<void> {
    if (this.pending) return this.pending
    if (this.stopping || this.attempted || !this.options.enabled || !localApiAddress(this.options.endpoint)) return Promise.resolve()
    this.attempted = true
    this.pending = this.start().finally(() => { this.pending = null })
    return this.pending
  }

  private async start(): Promise<void> {
    try {
      const unused = this.options.unused ?? portIsUnused
      if (!await unused(this.options.endpoint) || this.stopping) return
      // A hosting node is found through discovery as well as by its address;
      // being found admits no one: joining still takes an invitation.
      const args = ['run', '--api-listen', this.options.endpoint, '--data-dir', this.options.dataDir]
      if (this.options.hostingListen) {
        args.push('--listen', this.options.hostingListen)
      } else {
        const preferred = '127.0.0.1:7700'
        const listen = localApiAddress(this.options.endpoint)?.port !== 7700 && await unused(preferred) ? preferred : '127.0.0.1:0'
        args.push('--listen', listen, '--discovery', 'off')
      }
      if (this.stopping) return
      if (this.options.webListenAddress) {
        const webAddress = localApiAddress(this.options.webListenAddress)
        if (!webAddress || webAddress.host === 'localhost') {
          throw new Error('The local browser API address must use a loopback address and a valid port.')
        }
        if (!await unused(this.options.webListenAddress)) {
          throw new Error(`The local browser API address is already in use: ${this.options.webListenAddress}`)
        }
        args.push('--web-listen', this.options.webListenAddress)
        if (this.options.webUiDirectory) args.push('--web-ui-dir', this.options.webUiDirectory)
      }
      if (this.options.locationDiscovery?.locateGateway) args.push('--locate-gateway')
      if (this.options.locationDiscovery?.locateCountry) args.push('--locate-country')
      if (this.options.locationDiscovery?.stunServer) args.push('--stun-server', this.options.locationDiscovery.stunServer)
      if (this.options.containers) args.push(...await this.options.containers())
      if (this.stopping) return
      const child = (this.options.launch ?? ((executable, arguments_) => spawn(executable, arguments_, { stdio: 'ignore', windowsHide: true, shell: false })))(this.options.executable, args)
      this.launched = true
      this.child = child
      child.once('error', (error) => {
        if (!this.stopping) this.options.onError(`Could not start the local daemon: ${error.message}`)
      })
      child.once('exit', (code, signal) => {
        if (this.child === child) this.child = null
        if (!this.stopping) this.options.onError(`The desktop daemon stopped (${signal ?? code ?? 'unknown'}). Start sisyphusd manually to reconnect.`)
      })
      child.once('close', () => { if (this.child === child) this.child = null })
    } catch (error) {
      this.options.onError(`Could not start the local daemon: ${error instanceof Error ? error.message : String(error)}`)
    }
  }

  /**
   * Stops the node this app started and starts it again, with whatever of
   * its settings changed since. A node the app did not start is not ours.
   */
  async restart(update: Pick<Partial<Options>, 'hostingListen' | 'locationDiscovery' | 'containers'> = {}): Promise<void> {
    if (!this.launched) throw new Error('This app did not start the node, so it cannot restart it. Restart it where it was started.')
    await this.stop()
    Object.assign(this.options, update)
    this.stopping = false
    this.attempted = false
    await this.ensureStarted()
  }

  async stop(): Promise<void> {
    this.stopping = true
    await this.pending
    const child = this.child
    if (!child) return
    await new Promise<void>((resolve) => {
      const finish = () => {
        clearTimeout(timeout)
        child.removeListener('close', finish)
        if (this.child === child) this.child = null
        resolve()
      }
      const timeout = setTimeout(() => {
        child.kill('SIGKILL')
        finish()
      }, this.options.shutdownGraceMs ?? daemonShutdownGraceMs)
      child.once('close', finish)
      child.kill('SIGTERM')
    })
  }
}
