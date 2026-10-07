import type { NodeSnapshot, SisyphusBridge } from '../../../preload'

function browserBridge(): SisyphusBridge {
  return {
    getSnapshot: async () => {
      const response = await fetch('/api/node/snapshot', { cache: 'no-store' })
      if (!response.ok) throw new Error(`Node API returned ${response.status}`)
      return response.json() as Promise<NodeSnapshot>
    },
    reconnect: async () => {
      throw new Error('Reconnect is only available in the desktop app.')
    },
    connectPeer: async () => { throw new Error('Peer connections are only available in the desktop app.') },
    setPeerComputeTrust: async () => { throw new Error('Peer trust settings are only available in the desktop app.') },
    setPeerComputePermissions: async () => { throw new Error('Peer trust settings are only available in the desktop app.') },
    onSnapshot: (callback) => {
      const events = new EventSource('/api/node/events')
      events.onmessage = (event) => {
        try { callback(JSON.parse(event.data) as NodeSnapshot) } catch { /* Ignore malformed dev-stream messages. */ }
      }
      return () => { events.close() }
    },
  }
}

export function getNodeApi(): SisyphusBridge {
  if (typeof window !== 'undefined' && window.sisyphus) return window.sisyphus
  return browserBridge()
}

export function isDesktopApp() {
  return typeof window !== 'undefined' && Boolean(window.sisyphus)
}
