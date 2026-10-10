// Real daemon acceptance, separate from the fast desktop unit suite.
import assert from 'node:assert/strict'
import { mkdtemp, readFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { createServer } from 'node:net'
import grpc from '@grpc/grpc-js'
import protoLoader from '@grpc/proto-loader'
import { DesktopDaemon, portIsUnused } from '../src/main/daemon-process.ts'

const executable = resolve(process.argv[2] ?? '../../bin/sisyphusd')
const directory = await mkdtemp(join(tmpdir(), 'sisyphus-desktop-lifecycle-'))
const reservation = createServer()
await new Promise((accept) => reservation.listen(0, '127.0.0.1', accept))
const endpoint = `127.0.0.1:${reservation.address().port}`
await new Promise((accept) => reservation.close(accept))
const diagnostics = []
const daemon = new DesktopDaemon({ executable, endpoint, dataDir: directory, enabled: true, onError: (error) => diagnostics.push(error) })
const proto = resolve('../../proto/sisyphus/node/v1/node.proto')
const definition = protoLoader.loadSync(proto, { keepCase: false, longs: String, enums: String, defaults: true })
const service = grpc.loadPackageDefinition(definition).sisyphus.node.v1.NodeService
const client = new service(endpoint, grpc.credentials.createInsecure())
try {
  await daemon.ensureStarted()
  await new Promise((accept, reject) => client.waitForReady(Date.now() + 15000, (error) => error ? reject(error) : accept()))
  const getInfo = () => new Promise((accept, reject) => client.getNodeInfo({}, { deadline: Date.now() + 5000 }, (error, value) => error ? reject(error) : accept(value)))
  const info = await getInfo()
  assert.ok(info.peerId)
  assert.ok((await readFile(join(directory, 'api.token'), 'utf8')).trim())
  // A second desktop attaching to the same live node cannot acquire ownership.
  const other = new DesktopDaemon({ executable, endpoint, dataDir: directory, enabled: true, onError: (error) => diagnostics.push(error) })
  await other.ensureStarted()
  await other.stop()
  assert.equal((await getInfo()).peerId, info.peerId)
  await daemon.stop()
  assert.equal(await portIsUnused(endpoint), true)
  assert.deepEqual(diagnostics, [])
  console.log('Real daemon startup, existing-node reuse and owned-process shutdown passed.')
} finally {
  client.close()
  await daemon.stop()
  await rm(directory, { recursive: true, force: true })
}
