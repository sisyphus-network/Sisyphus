import { execFile } from 'node:child_process'
import { mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from 'node:fs'
import { randomUUID } from 'node:crypto'
import { dirname } from 'node:path'

// Running containers lets whoever may submit jobs to the pool run an image of
// their choosing on this machine, so it is off until the owner turns it on.
// When on, a task gets no network and a read-only root with a /tmp of its
// own unless the owner says otherwise: transcoding and molecular dynamics
// write only to /output and /tmp, so they need neither.
export type ContainerSettings = { enabled: boolean; network: boolean; readOnly: boolean; tmpMb: number }

export const defaultContainerTmpMb = 1024
export const maxContainerTmpMb = 65536
export const defaultContainerSettings: ContainerSettings = { enabled: false, network: false, readOnly: true, tmpMb: defaultContainerTmpMb }

/** Where Docker can be downloaded, for a machine that does not have it. */
export const dockerInstallUrl = 'https://docs.docker.com/get-started/get-docker/'

function validTmpMb(value: unknown): value is number {
  return typeof value === 'number' && Number.isInteger(value) && value >= 1 && value <= maxContainerTmpMb
}

export function normalizeContainerSettings(value: unknown): ContainerSettings {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return { ...defaultContainerSettings }
  const settings = value as Record<string, unknown>
  return {
    enabled: settings.enabled === true,
    network: settings.network === true,
    // Only a saved false loosens it: a damaged file keeps the safer default.
    readOnly: settings.readOnly !== false,
    tmpMb: validTmpMb(settings.tmpMb) ? settings.tmpMb : defaultContainerTmpMb,
  }
}

export function validateContainerSettings(value: unknown): ContainerSettings {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid container settings.')
  const candidate = value as Record<string, unknown>
  if (typeof candidate.enabled !== 'boolean' || typeof candidate.network !== 'boolean' || typeof candidate.readOnly !== 'boolean') {
    throw new Error('Invalid container settings.')
  }
  if (!validTmpMb(candidate.tmpMb)) throw new Error(`The size of /tmp must be a whole number of megabytes from 1 to ${maxContainerTmpMb}.`)
  return { enabled: candidate.enabled, network: candidate.network, readOnly: candidate.readOnly, tmpMb: candidate.tmpMb }
}

export function loadContainerSettings(path: string): ContainerSettings {
  try {
    return normalizeContainerSettings(JSON.parse(readFileSync(path, 'utf8')))
  } catch {
    return { ...defaultContainerSettings }
  }
}

export function saveContainerSettings(path: string, value: unknown): ContainerSettings {
  const settings = validateContainerSettings(value)
  const temporaryPath = `${path}.${randomUUID()}.tmp`
  try {
    mkdirSync(dirname(path), { recursive: true })
    writeFileSync(temporaryPath, `${JSON.stringify(settings, null, 2)}\n`, { encoding: 'utf8', mode: 0o600 })
    renameSync(temporaryPath, path)
  } catch (error) {
    try { unlinkSync(temporaryPath) } catch { /* No temporary file to clean up. */ }
    throw error
  }
  return settings
}

/** The daemon flags that run the pool's containers as the owner chose, or none. */
export function containerFlags(settings: ContainerSettings): string[] {
  if (!settings.enabled) return []
  const flags = ['--containers']
  if (settings.readOnly) flags.push('--containers-read-only', '--containers-tmp-mb', String(settings.tmpMb))
  if (settings.network) flags.push('--containers-network')
  return flags
}

export type DockerStatus =
  | { state: 'ready'; version: string }
  | { state: 'missing' }
  | { state: 'stopped'; detail: string }

export type DockerCheck = () => Promise<DockerStatus>

type Run = (command: string, args: string[], timeoutMs: number) => Promise<{ stdout: string; stderr: string; error: (Error & { code?: unknown; killed?: boolean }) | null }>

const runCommand: Run = (command, args, timeoutMs) => new Promise((resolve) => {
  execFile(command, args, { timeout: timeoutMs, windowsHide: true, encoding: 'utf8' }, (error, stdout, stderr) => {
    resolve({ stdout: String(stdout ?? ''), stderr: String(stderr ?? ''), error })
  })
})

/**
 * Whether Docker answers, asked the way the daemon asks before it runs
 * containers. Docker that is not installed and Docker that is installed but
 * not running are told apart, since each asks the owner for something else.
 */
export async function checkDocker({ command = 'docker', timeoutMs = 5000, run = runCommand }: { command?: string; timeoutMs?: number; run?: Run } = {}): Promise<DockerStatus> {
  try {
    const { stdout, stderr, error } = await run(command, ['version', '--format', '{{.Server.Version}}'], timeoutMs)
    if (error?.code === 'ENOENT') return { state: 'missing' }
    const version = stdout.trim()
    if (!error && version) return { state: 'ready', version }
    const detail = error?.killed ? `Docker did not answer within ${Math.round(timeoutMs / 1000)} seconds.` : (stderr.trim() || error?.message || 'Docker did not answer.')
    return { state: 'stopped', detail: detail.split('\n')[0].slice(0, 300) }
  } catch (error) {
    return { state: 'stopped', detail: error instanceof Error ? error.message : String(error) }
  }
}

/**
 * A stand-in for Docker that end-to-end tests set with SISYPHUS_DOCKER_CHECK,
 * as "ready:<version>", "missing" or "stopped", so they do not depend on
 * whether the machine running them has Docker.
 */
export function dockerCheckFromEnvironment(value: string | undefined): DockerCheck | null {
  if (!value) return null
  if (value === 'missing') return async () => ({ state: 'missing' })
  if (value === 'stopped') return async () => ({ state: 'stopped', detail: 'Cannot connect to the Docker daemon.' })
  if (value.startsWith('ready:') && value.length > 'ready:'.length) {
    const version = value.slice('ready:'.length)
    return async () => ({ state: 'ready', version })
  }
  return null
}

export type ContainerState = {
  managed: boolean
  saved: ContainerSettings
  running: ContainerSettings
  restartNeeded: boolean
  /** Containers were on, but Docker did not answer when the node started, so it started without them. */
  unavailable: boolean
  docker: DockerStatus | null
}

export function sameContainerSettings(a: ContainerSettings, b: ContainerSettings): boolean {
  // How containers would run changes nothing while none are run.
  if (!a.enabled && !b.enabled) return true
  return containerFlags(a).join(' ') === containerFlags(b).join(' ')
}

export function containerState(value: Omit<ContainerState, 'restartNeeded'>): ContainerState {
  return { ...value, restartNeeded: value.managed && !sameContainerSettings(value.saved, value.running) }
}

/**
 * The flags the app's node starts with, and what it runs with: saved
 * containers that Docker cannot run are left off, so that the node still
 * starts, and the owner is told why.
 */
export async function containerLaunch(settings: ContainerSettings, check: DockerCheck): Promise<{ flags: string[]; running: ContainerSettings; unavailable: boolean; docker: DockerStatus | null }> {
  if (!settings.enabled) return { flags: [], running: settings, unavailable: false, docker: null }
  const docker = await check()
  if (docker.state !== 'ready') return { flags: [], running: { ...settings, enabled: false }, unavailable: true, docker }
  return { flags: containerFlags(settings), running: settings, unavailable: false, docker }
}
