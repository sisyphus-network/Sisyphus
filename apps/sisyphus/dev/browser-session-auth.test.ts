import assert from 'node:assert/strict'
import { mkdtemp, rm, writeFile } from 'node:fs/promises'
import { createServer } from 'node:http'
import type { Server } from 'node:http'
import type { AddressInfo } from 'node:net'
import { EventEmitter } from 'node:events'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'
import type { Connect, ViteDevServer } from 'vite'
import { nodeBrowserApiPlugin } from './node-browser-api.ts'

function listen(server: Server) {
  return new Promise<number>((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', () => {
      server.off('error', reject)
      resolve((server.address() as AddressInfo).port)
    })
  })
}

function close(server: Server) {
  return new Promise<void>((resolve) => server.close(() => resolve()))
}

test('the dev proxy forwards browser credentials without adding the node token', async () => {
  const received: { authorization?: string, cookie?: string }[] = []
  const upstream = createServer((request, response) => {
    received.push({ authorization: request.headers.authorization, cookie: request.headers.cookie })
    const authorized = request.headers.authorization === 'Bearer user-supplied-token'
    response.writeHead(authorized ? 200 : 401, { 'Content-Type': 'application/json' })
    const payload = request.url === '/pairing' && authorized
      ? { code: '12345678', expiresAt: '2026-10-15T12:00:00Z', pairUrl: 'https://node.example.test:50052/#pair=12345678' }
      : { message: authorized ? 'authorized' : 'unauthorized' }
    response.end(JSON.stringify(payload))
  })
  const upstreamPort = await listen(upstream)
  const previousTokenFile = process.env.SISYPHUS_API_TOKEN_FILE
  const previousWebAddress = process.env.SISYPHUS_WEB_API_ADDRESS
  const tempDir = await mkdtemp(join(tmpdir(), 'sisyphus-browser-auth-'))
  const tokenFile = join(tempDir, 'api.token')
  await writeFile(tokenFile, 'node-secret')
  process.env.SISYPHUS_API_TOKEN_FILE = tokenFile
  process.env.SISYPHUS_WEB_API_ADDRESS = `127.0.0.1:${upstreamPort}`

  let middleware: Connect.NextHandleFunction | undefined
  const preview = createServer((request, response) => middleware!(request, response, () => {
    response.writeHead(404)
    response.end()
  }))
  const lifecycle = new EventEmitter()
  const protoPath = fileURLToPath(new URL('../../../proto/sisyphus/node/v1/node.proto', import.meta.url))
  const plugin = nodeBrowserApiPlugin(protoPath)
  // The plugin's hook is a plain function, and the test hands it only the two
  // server parts it touches.
  ;(plugin.configureServer as (server: ViteDevServer) => void)({
    middlewares: { use(handler: Connect.NextHandleFunction) { middleware = handler } }, httpServer: lifecycle,
  } as unknown as ViteDevServer)

  try {
    const previewPort = await listen(preview)
    const url = `http://127.0.0.1:${previewPort}/sisyphus.node.v1.NodeService/GetNodeInfo`
    const denied = await fetch(url, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Origin: `http://127.0.0.1:${previewPort}`,
        Cookie: 'sisyphus_web_session=revoked.id',
      },
      body: '{}',
    })
    assert.equal(denied.status, 401)
    assert.deepEqual(received[0], { authorization: undefined, cookie: 'sisyphus_web_session=revoked.id' })

    const explicitlyAuthorized = await fetch(url, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Origin: `http://127.0.0.1:${previewPort}`,
        Authorization: 'Bearer user-supplied-token',
      },
      body: '{}',
    })
    assert.equal(explicitlyAuthorized.status, 200)
    assert.equal(received[1].authorization, 'Bearer user-supplied-token')

    const pairing = await fetch(`http://127.0.0.1:${previewPort}/pairing`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Origin: `http://127.0.0.1:${previewPort}`,
        Authorization: 'Bearer user-supplied-token',
      },
      body: '{}',
    })
    assert.equal(pairing.status, 200)
    assert.equal((await pairing.json()).pairUrl, 'https://node.example.test:50052/#pair=12345678')
  } finally {
    lifecycle.emit('close')
    await close(preview)
    await close(upstream)
    await rm(tempDir, { recursive: true, force: true })
    if (previousTokenFile === undefined) delete process.env.SISYPHUS_API_TOKEN_FILE
    else process.env.SISYPHUS_API_TOKEN_FILE = previousTokenFile
    if (previousWebAddress === undefined) delete process.env.SISYPHUS_WEB_API_ADDRESS
    else process.env.SISYPHUS_WEB_API_ADDRESS = previousWebAddress
  }
})
