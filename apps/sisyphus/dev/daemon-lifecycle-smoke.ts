// Real daemon acceptance, separate from the fast desktop unit suite.
import assert from 'node:assert/strict'
import { mkdtemp, readFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { createServer } from 'node:net'
import grpc from '@grpc/grpc-js'
import protoLoader from '@grpc/proto-loader'
import { DesktopDaemon, portIsUnused } from '../src/main/daemon-process.ts'

type Message = Record<string, unknown>
type NodeInfo = { peerId: string, daemonVersion: string }
type NodeServiceClient = grpc.Client & {
  getNodeInfo(request: Message, metadata: grpc.Metadata, options: { deadline: number }, callback: (error: grpc.ServiceError | null, response: NodeInfo) => void): grpc.ClientUnaryCall
}
type NodeServiceConstructor = grpc.ServiceClientConstructor & (new (address: string, credentials: grpc.ChannelCredentials) => NodeServiceClient)

function listeningAddress(server: ReturnType<typeof createServer>): string {
  const address = server.address()
  assert.ok(address && typeof address === 'object', 'the temporary server is not listening on a TCP port')
  return `127.0.0.1:${address.port}`
}

const executable = resolve(process.argv[2] ?? '../../bin/sisyphusd')
const directory = await mkdtemp(join(tmpdir(), 'sisyphus-desktop-lifecycle-'))
const reservation = createServer()
await new Promise<void>((accept) => reservation.listen(0, '127.0.0.1', () => accept()))
const endpoint = listeningAddress(reservation)
await new Promise<void>((accept) => reservation.close(() => accept()))
const webReservation = createServer()
await new Promise<void>((accept) => webReservation.listen(0, '127.0.0.1', () => accept()))
const webListenAddress = listeningAddress(webReservation)
await new Promise<void>((accept) => webReservation.close(() => accept()))
const diagnostics: unknown[] = []
const daemon = new DesktopDaemon({ executable, endpoint, webListenAddress, dataDir: directory, enabled: true, onError: (error) => diagnostics.push(error) })
const proto = resolve('../../proto/sisyphus/node/v1/node.proto')
const definition = protoLoader.loadSync(proto, { keepCase: false, longs: String, enums: String, defaults: true })
const loaded = grpc.loadPackageDefinition(definition) as unknown as { sisyphus: { node: { v1: { NodeService: NodeServiceConstructor } } } }
const service = loaded.sisyphus.node.v1.NodeService
const client = new service(endpoint, grpc.credentials.createInsecure())
try {
  await daemon.ensureStarted()
  await new Promise<void>((accept, reject) => client.waitForReady(Date.now() + 15000, (error) => error ? reject(error) : accept()))
  const getInfo = () => new Promise<NodeInfo>((accept, reject) => client.getNodeInfo({}, new grpc.Metadata(), { deadline: Date.now() + 5000 }, (error: grpc.ServiceError | null, value: NodeInfo) => error ? reject(error) : accept(value)))
  const info = await getInfo()
  assert.ok(info.peerId)
  if (process.argv[3]) assert.equal(info.daemonVersion, process.argv[3])
  const token = (await readFile(join(directory, 'api.token'), 'utf8')).trim()
  assert.ok(token)
  const webBase = `http://${webListenAddress}`
  const unauthenticated = await fetch(`${webBase}/devices`)
  assert.equal(unauthenticated.status, 401)
  assert.deepEqual(await unauthenticated.json(), { code: 'Unauthorized', message: 'connect this browser before managing paired devices' })
  const devices = await fetch(`${webBase}/devices`, { headers: { Authorization: `Bearer ${token}` } })
  assert.equal(devices.status, 200)
  assert.deepEqual(await devices.json(), { devices: [] })
  // A second desktop attaching to the same live node cannot acquire ownership.
  const other = new DesktopDaemon({ executable, endpoint, dataDir: directory, enabled: true, onError: (error) => diagnostics.push(error) })
  await other.ensureStarted()
  await other.stop()
  assert.equal((await getInfo()).peerId, info.peerId)
  await daemon.stop()
  assert.equal(await portIsUnused(endpoint), true)
  assert.deepEqual(diagnostics, [])
  console.log('Real daemon startup, loopback browser API, existing-node reuse and owned-process shutdown passed.')
} finally {
  client.close()
  await daemon.stop()
  await rm(directory, { recursive: true, force: true })
}
