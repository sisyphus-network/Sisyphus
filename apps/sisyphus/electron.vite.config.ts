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
      fs: {
        // @sisyphus/client is a local file: package outside the renderer root.
        allow: [resolve(__dirname, '../..')],
      },
    },
    resolve: {
      preserveSymlinks: true,
      alias: {
        '@': resolve(__dirname, 'src/renderer/src'),
      },
      // The local SDK is a file: dependency whose source lives outside this
      // package. Its runtime deps resolve from the Electron app installation,
      // through each package's exports map: Protobuf-ES v2 is imported by
      // subpath (@bufbuild/protobuf/codegenv2), which an alias to the package
      // directory would bypass.
      dedupe: ['@bufbuild/protobuf', '@connectrpc/connect', '@connectrpc/connect-web'],
    },
  },
})
