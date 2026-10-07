/// <reference types="vite/client" />

import type { SisyphusBridge } from '../../preload'

declare global {
  interface Window {
    sisyphus: SisyphusBridge
  }
}

export {}
