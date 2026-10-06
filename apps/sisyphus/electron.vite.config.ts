import { resolve } from 'node:path'
import { defineConfig, externalizeDepsPlugin } from 'electron-vite'
import { copyFileSync, mkdirSync } from 'node:fs'
import tailwindcss from '@tailwindcss/vite'
import { nodeBrowserApiPlugin } from './dev/node-browser-api'

const protoSource = resolve(__dirname, '../../proto/sisyphus/node/v1/node.proto')
const logoSource = resolve(__dirname, '../../assets/logo.png')
const logoTarget = resolve(__dirname, 'src/renderer/public/logo.png')
mkdirSync(resolve(__dirname, 'src/renderer/public'), { recursive: true })
copyFileSync(logoSource, logoTarget)

export default defineConfig({
  main: {
    plugins: [externalizeDepsPlugin()],
  },
  preload: {
    plugins: [externalizeDepsPlugin()],
  },
  renderer: {
    root: resolve(__dirname, 'src/renderer'),
    plugins: [tailwindcss(), nodeBrowserApiPlugin(protoSource)],
    server: {
      host: process.env.SISYPHUS_DEV_HOST ?? '127.0.0.1',
    },
    resolve: {
      alias: {
        '@': resolve(__dirname, 'src/renderer/src'),
      },
    },
  },
})
