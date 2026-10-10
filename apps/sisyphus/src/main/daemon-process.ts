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
  dataDir: string
  enabled: boolean
  unused?: (endpoint: string) => Promise<boolean>
  launch?: (executable: string, args: string[]) => ChildProcess
  onError: (message: string) => void
  shutdownGraceMs?: number
}

export const daemonShutdownGraceMs = 30000

/** Own only the process we launched. Never kill an existing independently-run node. */
export class DesktopDaemon {
  private child: ChildProcess | null = null
  private attempted = false
  private stopping = false
  private pending: Promise<void> | null = null
  private readonly options: Options
  constructor(options: Options) { this.options = options }

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
      const preferred = '127.0.0.1:7700'
      const listen = localApiAddress(this.options.endpoint)?.port !== 7700 && await unused(preferred) ? preferred : '127.0.0.1:0'
      if (this.stopping) return
      const args = ['run', '--api-listen', this.options.endpoint, '--data-dir', this.options.dataDir,
        '--listen', listen, '--discovery', 'off']
      const child = (this.options.launch ?? ((executable, arguments_) => spawn(executable, arguments_, { stdio: 'ignore', windowsHide: true, shell: false })))(this.options.executable, args)
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
