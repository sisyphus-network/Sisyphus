import type { SisyphusBridge } from './index'

declare global {
  interface Window {
    sisyphus: SisyphusBridge
  }
}

export {}
