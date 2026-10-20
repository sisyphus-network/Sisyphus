import { execFile } from 'node:child_process'
import { Menu, Tray, nativeImage } from 'electron'
import { trayMenu, trayStatusLabel, type NodeStatus, type TrayLabels } from './tray-policy'

/** Whether a Linux desktop runs a StatusNotifier host that can show the icon. */
export function statusNotifierWatcherRunning(): Promise<boolean> {
  return new Promise((resolve) => {
    execFile('dbus-send', ['--session', '--print-reply', '--dest=org.freedesktop.DBus', '/org/freedesktop/DBus', 'org.freedesktop.DBus.NameHasOwner', 'string:org.kde.StatusNotifierWatcher'], { timeout: 2000 }, (error, stdout) => {
      resolve(!error && /boolean true/.test(stdout))
    })
  })
}

/**
 * The notification-area icon for the node this app started. Its menu follows
 * the node state the window shows, and is rebuilt only when that changes.
 */
export class AppTray {
  private readonly tray: Tray
  private labels: TrayLabels
  private status: NodeStatus
  private readonly onOpen: () => void
  private readonly onQuit: () => void

  private constructor(tray: Tray, options: { labels: TrayLabels; status: NodeStatus; onOpen: () => void; onQuit: () => void }) {
    this.tray = tray
    this.labels = options.labels
    this.status = options.status
    this.onOpen = options.onOpen
    this.onQuit = options.onQuit
    tray.on('click', () => this.onOpen())
    this.render()
  }

  /** Returns null when the icon cannot be loaded, so the app keeps closing with its window. */
  static create(options: { iconPath: string; labels: TrayLabels; status: NodeStatus; onOpen: () => void; onQuit: () => void }): AppTray | null {
    const image = nativeImage.createFromPath(options.iconPath)
    if (image.isEmpty()) return null
    return new AppTray(new Tray(image.resize({ width: 16, height: 16 })), options)
  }

  setStatus(status: NodeStatus) {
    if (status === this.status) return
    this.status = status
    this.render()
  }

  setLabels(labels: TrayLabels) {
    this.labels = labels
    this.render()
  }

  private render() {
    this.tray.setToolTip(`Sisyphus · ${trayStatusLabel(this.labels, this.status)}`)
    this.tray.setContextMenu(Menu.buildFromTemplate(trayMenu(this.labels, this.status).map((item) => {
      if (item.kind === 'separator') return { type: 'separator' as const }
      if (item.kind === 'status') return { label: item.label, enabled: false }
      return { label: item.label, click: item.action === 'open' ? this.onOpen : this.onQuit }
    })))
  }
}
