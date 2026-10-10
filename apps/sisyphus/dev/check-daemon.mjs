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
    const trust = [peer.givesWork && 'gives it work', peer.takesWork && 'takes its work'].filter(Boolean).join(', ') || 'no trust either way'
    const flow = [peer.worksForThisNode && 'working for this node', peer.thisNodeWorksFor && 'this node works for it'].filter(Boolean).join(', ') || 'no work flowing'
    console.log(' ', peer.peerId, peer.connectionState, '|', trust, '|', flow)
  }

  // Setting a peer's trust to what it already is: refused without the
  // token, accepted with it, and nothing changes either way.
  const target = first.peers.find((peer) => peer.givesWork || peer.takesWork)
  if (tokenFile && target) {
    const same = { peerId: target.peerId, givesWork: target.givesWork, takesWork: target.takesWork }
    try {
      await call('setPeerComputePermissions', same)
      console.log('WRONG: a change was accepted without the token')
      process.exitCode = 1
    } catch (error) {
      console.log('without the token, a change is refused:', error.details)
    }
    const kept = await call('setPeerComputePermissions', same, authorization)
    console.log('with the token, it is accepted: gives work =', kept.givesWork, 'takes work =', kept.takesWork)
  }

  // The pool this node coordinates: its workers and its jobs.
  const { workers } = await call('listWorkers', {})
  console.log('workers:')
  for (const worker of workers) {
    console.log(' ', worker.name, worker.peerId, `${worker.runningTasks}/${worker.taskSlots} tasks`, worker.workloads.join(','))
  }
  const describe = (job) => `${job.jobId} ${job.workload} ${job.state}${job.result?.length ? ' ' + Buffer.from(job.result).toString() : ''}`
  // Jobs are read with the token, as they are changed with it: submit one
  // and watch the list until it has finished.
  if (tokenFile) {
    const { jobs } = await call('listJobs', {}, authorization)
    console.log(`jobs on record: ${jobs.length}`)
    for (const job of jobs.slice(0, 5)) console.log(' ', describe(job))
    const { job } = await call('submitJob', { workload: 'primes', params: Buffer.from('{"from":0,"to":1000000}'), maxTasks: 2 }, authorization)
    console.log('submitted', describe(job))
    const done = await new Promise((resolve, reject) => {
      const stream = client.watchJobs({}, authorization)
      stream.on('data', (update) => {
        const mine = update.jobs.find((other) => other.jobId === job.jobId)
        if (mine && (mine.state === 'JOB_STATE_SUCCEEDED' || mine.state === 'JOB_STATE_FAILED')) { resolve(mine); stream.cancel() }
      })
      stream.on('error', (error) => { if (error.code !== grpc.status.CANCELLED) reject(error) })
    })
    console.log('finished ', describe(done), 'on', [...new Set(done.tasks.map((task) => task.workerName))].join(', '))
    // What happened to it along the way. The stream ends by itself because the job is over.
    await new Promise((resolve, reject) => {
      const stream = client.watchJobEvents({ jobId: job.jobId }, authorization)
      stream.on('data', (event) => console.log('   ', event.seq, event.kind, event.taskIndex >= 0 ? `task ${event.taskIndex}` : '', event.workerName, event.text))
      stream.on('end', resolve)
      stream.on('error', reject)
    })
  }
} catch (error) {
  console.error('the daemon did not answer as expected:', error.details ?? error.message)
  process.exitCode = 1
} finally {
  client.close()
}
