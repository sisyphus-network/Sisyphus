import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { dirname } from 'node:path'

export type TrayLabels = {
  open: string
  quit: string
  connected: string
  connecting: string
  offline: string
  stillRunningTitle: string
  stillRunningBody: string
}

export type NodeStatus = 'connecting' | 'connected' | 'disconnected'

export type TrayMenuItem =
  | { kind: 'status'; label: string }
  | { kind: 'separator' }
  | { kind: 'action'; label: string; action: 'open' | 'quit' }

/**
 * Whether closing the last window should quit the app. A node this app
 * started keeps working for its pool while the window is closed, as long as
 * a tray icon is there to reopen or quit it. A node someone else started has
 * nothing of ours to keep running, and macOS keeps apps open as it always has.
 */
export function quitsWhenWindowsClose(options: { platform: NodeJS.Platform; managesDaemon: boolean; trayShown: boolean }): boolean {
  if (options.managesDaemon && options.trayShown) return false
  return options.platform !== 'darwin'
}

/**
 * Windows and macOS always have somewhere to show a tray icon. A Linux
 * desktop shows one only when a StatusNotifier host is running; without one
 * the icon would be invisible and the app could not be reopened or quit.
 */
export async function trayHostAvailable(platform: NodeJS.Platform, statusNotifierWatcherRunning: () => Promise<boolean>): Promise<boolean> {
  if (platform === 'win32' || platform === 'darwin') return true
  if (platform !== 'linux') return false
  try { return await statusNotifierWatcherRunning() } catch { return false }
}

/** The tray menu for a node state, rebuilt whenever the node state changes. */
export function trayMenu(labels: TrayLabels, status: NodeStatus): TrayMenuItem[] {
  return [
    { kind: 'status', label: trayStatusLabel(labels, status) },
    { kind: 'separator' },
    { kind: 'action', label: labels.open, action: 'open' },
    { kind: 'action', label: labels.quit, action: 'quit' },
  ]
}

export function trayStatusLabel(labels: TrayLabels, status: NodeStatus): string {
  return status === 'connected' ? labels.connected : status === 'connecting' ? labels.connecting : labels.offline
}

/**
 * The tray icon for a node state, from the folder drawn by
 * scripts/make-icons.py. A badge's shape says the state: none when
 * connected, a hollow ring while connecting, a slash while offline. macOS
 * gets a template image, which the menu bar tints for its appearance.
 * Electron takes the file's @2x twin on sharper screens by itself.
 */
export function trayIconFile(platform: NodeJS.Platform, status: NodeStatus): string {
  const state = status === 'disconnected' ? 'offline' : status
  return platform === 'darwin' ? `${state}Template.png` : `${state}.png`
}

/** The tray's labels in the window's language, or English for one it does not know. */
export function trayLabelsFor<Locale extends string>(dictionaries: Record<Locale, TrayLabels>, fallback: Locale, locale: unknown): TrayLabels {
  return typeof locale === 'string' && Object.hasOwn(dictionaries, locale) ? dictionaries[locale as Locale] : dictionaries[fallback]
}

export type TrayPreferences = { locale: string; noticeShown: boolean }

export const defaultTrayPreferences: TrayPreferences = { locale: 'en', noticeShown: false }

/**
 * The language the tray last showed and whether the app has said once that it
 * keeps running, kept so a later start can label the tray before any window
 * has loaded.
 */
export function loadTrayPreferences(path: string): TrayPreferences {
  try {
    const value: unknown = JSON.parse(readFileSync(path, 'utf8'))
    if (!value || typeof value !== 'object' || Array.isArray(value)) return { ...defaultTrayPreferences }
    const record = value as Record<string, unknown>
    return {
      locale: typeof record.locale === 'string' && record.locale ? record.locale : defaultTrayPreferences.locale,
      noticeShown: record.noticeShown === true,
    }
  } catch {
    return { ...defaultTrayPreferences }
  }
}

export function saveTrayPreferences(path: string, preferences: TrayPreferences): void {
  mkdirSync(dirname(path), { recursive: true })
  writeFileSync(path, `${JSON.stringify(preferences, null, 2)}\n`)
}
