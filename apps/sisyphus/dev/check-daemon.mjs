// Checks that a running sisyphusd answers the local API the way the desktop
// client expects, using the same proto file and gRPC library the client
// does. Not part of the app.
//
//   node dev/check-daemon.mjs [address] [token-file]
//
// Prints what it finds and exits non-zero if the daemon does not answer.
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import * as grpc from '@grpc/grpc-js'
import * as protoLoader from '@grpc/proto-loader'

const address = process.argv[2] ?? process.env.SISYPHUS_API_ADDRESS ?? '127.0.0.1:50051'
const tokenFile = process.argv[3]
const proto = fileURLToPath(new URL('../../../proto/sisyphus/node/v1/node.proto', import.meta.url))
const definition = protoLoader.loadSync(proto, { keepCase: false, longs: String, enums: String, defaults: true, oneofs: true })
const { NodeService } = grpc.loadPackageDefinition(definition).sisyphus.node.v1
const client = new NodeService(address, grpc.credentials.createInsecure())

const call = (method, request, metadata = new grpc.Metadata()) =>
  new Promise((resolve, reject) => client[method](request, metadata, (error, value) => (error ? reject(error) : resolve(value))))

const authorization = new grpc.Metadata()
if (tokenFile) authorization.set('authorization', `Bearer ${readFileSync(tokenFile, 'utf8').trim()}`)

try {
  const info = await call('getNodeInfo', {})
  console.log('node', info.peerId, 'version', info.daemonVersion, 'listening on', info.listenAddresses.join(', ') || '(nothing)')

  const first = await new Promise((resolve, reject) => {
    const stream = client.watchPeers({})
    stream.on('data', (update) => { resolve(update); stream.cancel() })
    stream.on('error', (error) => { if (error.code !== grpc.status.CANCELLED) reject(error) })
  })
  console.log(`peers at revision ${first.revision}:`)
  for (const peer of first.peers) {
    console.log(' ', peer.peerId, peer.connectionState, peer.trustedForCompute ? 'trusted for compute' : 'not trusted')
  }

  // Changing trust for a peer and changing it back, when a token is given.
  const target = first.peers.find((peer) => peer.trustedForCompute)
  if (tokenFile && target) {
    try {
      await call('setPeerComputeTrust', { peerId: target.peerId, trusted: true })
      console.log('WRONG: a change was accepted without the token')
      process.exitCode = 1
    } catch (error) {
      console.log('without the token, a change is refused:', error.details)
    }
    const kept = await call('setPeerComputeTrust', { peerId: target.peerId, trusted: true }, authorization)
    console.log('with the token, it is accepted: trusted =', kept.trusted)
  }
} catch (error) {
  console.error('the daemon did not answer as expected:', error.details ?? error.message)
  process.exitCode = 1
} finally {
  client.close()
}
