import { execFile } from 'node:child_process'
import { join } from 'node:path'
import { Menu, Tray, nativeImage, type NativeImage } from 'electron'
import { trayIconFile, trayMenu, trayStatusLabel, type NodeStatus, type TrayLabels } from './tray-policy'

/** Whether a Linux desktop runs a StatusNotifier host that can show the icon. */
export function statusNotifierWatcherRunning(): Promise<boolean> {
  return new Promise((resolve) => {
    execFile('dbus-send', ['--session', '--print-reply', '--dest=org.freedesktop.DBus', '/org/freedesktop/DBus', 'org.freedesktop.DBus.NameHasOwner', 'string:org.kde.StatusNotifierWatcher'], { timeout: 2000 }, (error, stdout) => {
      resolve(!error && /boolean true/.test(stdout))
    })
  })
}

/**
 * The notification-area icon for the node this app started. Its picture and
 * menu follow the node state the window shows, and change only when it does.
 */
export class AppTray {
  private readonly tray: Tray
  private readonly images: Record<NodeStatus, NativeImage>
  private labels: TrayLabels
  private status: NodeStatus
  private readonly onOpen: () => void
  private readonly onQuit: () => void

  private constructor(images: Record<NodeStatus, NativeImage>, options: { labels: TrayLabels; status: NodeStatus; onOpen: () => void; onQuit: () => void }) {
    this.images = images
    this.tray = new Tray(images[options.status])
    this.labels = options.labels
    this.status = options.status
    this.onOpen = options.onOpen
    this.onQuit = options.onQuit
    this.tray.on('click', () => this.onOpen())
    this.render()
  }

  /** Returns null when an icon cannot be loaded, so the app keeps closing with its window. */
  static create(options: { iconDirectory: string; labels: TrayLabels; status: NodeStatus; onOpen: () => void; onQuit: () => void }): AppTray | null {
    const statuses: NodeStatus[] = ['connected', 'connecting', 'disconnected']
    const images = Object.fromEntries(statuses.map((status) => [status, nativeImage.createFromPath(join(options.iconDirectory, trayIconFile(process.platform, status)))])) as Record<NodeStatus, NativeImage>
    if (statuses.some((status) => images[status].isEmpty())) return null
    return new AppTray(images, options)
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
    this.tray.setImage(this.images[this.status])
    this.tray.setToolTip(`Sisyphus · ${trayStatusLabel(this.labels, this.status)}`)
    this.tray.setContextMenu(Menu.buildFromTemplate(trayMenu(this.labels, this.status).map((item) => {
      if (item.kind === 'separator') return { type: 'separator' as const }
      if (item.kind === 'status') return { label: item.label, enabled: false }
      return { label: item.label, click: item.action === 'open' ? this.onOpen : this.onQuit }
    })))
  }
}
