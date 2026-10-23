// Checks that a running sisyphusd answers the local API the way the desktop
// client expects, using the same proto file and gRPC library the client
// does. Not part of the app.
//
//   node --experimental-strip-types dev/check-daemon.ts [address] [token-file]
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
type Message = Record<string, unknown>
type NodeServiceClient = grpc.Client & {
  watchPeers(request: Message): grpc.ClientReadableStream<PeerUpdate>
  watchJobs(request: Message, metadata: grpc.Metadata): grpc.ClientReadableStream<JobsUpdate>
  watchJobEvents(request: Message, metadata: grpc.Metadata): grpc.ClientReadableStream<JobEvent>
}
type Peer = { peerId: string, givesWork: boolean, takesWork: boolean, connectionState: string, worksForThisNode: boolean, thisNodeWorksFor: boolean }
type PeerUpdate = { revision: string | number, peers: Peer[] }
type Worker = { name: string, peerId: string, runningTasks: number, taskSlots: number, workloads: string[] }
type Task = { workerName: string }
type Job = { jobId: string, workload: string, state: string, result?: Uint8Array, tasks: Task[] }
type JobsUpdate = { jobs: Job[] }
type JobEvent = { seq: string | number, kind: string, taskIndex: number, workerName: string, text: string }
type NodeServiceConstructor = grpc.ServiceClientConstructor & (new (address: string, credentials: grpc.ChannelCredentials) => NodeServiceClient)
const loaded = grpc.loadPackageDefinition(definition) as unknown as { sisyphus: { node: { v1: { NodeService: NodeServiceConstructor } } } }
const { NodeService } = loaded.sisyphus.node.v1
const client = new NodeService(address, grpc.credentials.createInsecure())

type UnaryMethod = (this: grpc.Client, request: Message, metadata: grpc.Metadata, callback: (error: grpc.ServiceError | null, response: Message) => void) => grpc.ClientUnaryCall
const call = <T extends Message>(method: string, request: Message, metadata = new grpc.Metadata()): Promise<T> =>
  new Promise((resolve, reject) => {
    const invoke = client[method as keyof NodeServiceClient] as UnaryMethod
    invoke.call(client, request, metadata, (error, value) => error ? reject(error) : resolve(value as T))
  })

const authorization = new grpc.Metadata()
if (tokenFile) authorization.set('authorization', `Bearer ${readFileSync(tokenFile, 'utf8').trim()}`)

try {
  const info = await call<{ peerId: string, daemonVersion: string, listenAddresses: string[] }>('getNodeInfo', {})
  console.log('node', info.peerId, 'version', info.daemonVersion, 'listening on', info.listenAddresses.join(', ') || '(nothing)')

  const first = await new Promise<PeerUpdate>((resolve, reject) => {
    const stream = client.watchPeers({})
    stream.on('data', (update: PeerUpdate) => { resolve(update); stream.cancel() })
    stream.on('error', (error: grpc.ServiceError) => { if (error.code !== grpc.status.CANCELLED) reject(error) })
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
      console.log('without the token, a change is refused:', error instanceof Error && 'details' in error ? error.details : error)
    }
    const kept = await call('setPeerComputePermissions', same, authorization)
    console.log('with the token, it is accepted: gives work =', kept.givesWork, 'takes work =', kept.takesWork)
  }

  // The pool this node coordinates: its workers and its jobs.
    const { workers } = await call<{ workers: Worker[] }>('listWorkers', {})
  console.log('workers:')
  for (const worker of workers) {
    console.log(' ', worker.name, worker.peerId, `${worker.runningTasks}/${worker.taskSlots} tasks`, worker.workloads.join(','))
  }
  const describe = (job: Job) => `${job.jobId} ${job.workload} ${job.state}${job.result?.length ? ' ' + Buffer.from(job.result).toString() : ''}`
  // Jobs are read with the token, as they are changed with it: submit one
  // and watch the list until it has finished.
  if (tokenFile) {
    const { jobs } = await call<{ jobs: Job[] }>('listJobs', {}, authorization)
    console.log(`jobs on record: ${jobs.length}`)
    for (const job of jobs.slice(0, 5)) console.log(' ', describe(job))
    const { job } = await call<{ job: Job }>('submitJob', { workload: 'primes', params: Buffer.from('{"from":0,"to":1000000}'), maxTasks: 2 }, authorization)
    console.log('submitted', describe(job))
    const done = await new Promise<Job>((resolve, reject) => {
      const stream = client.watchJobs({}, authorization)
      stream.on('data', (update: JobsUpdate) => {
        const mine = update.jobs.find((other: Job) => other.jobId === job.jobId)
        if (mine && (mine.state === 'JOB_STATE_SUCCEEDED' || mine.state === 'JOB_STATE_FAILED')) { resolve(mine); stream.cancel() }
      })
      stream.on('error', (error: grpc.ServiceError) => { if (error.code !== grpc.status.CANCELLED) reject(error) })
    })
    console.log('finished ', describe(done), 'on', [...new Set(done.tasks.map((task) => task.workerName))].join(', '))
    // What happened to it along the way. The stream ends by itself because the job is over.
    await new Promise<void>((resolve, reject) => {
      const stream = client.watchJobEvents({ jobId: job.jobId }, authorization)
      stream.on('data', (event: JobEvent) => console.log('   ', event.seq, event.kind, event.taskIndex >= 0 ? `task ${event.taskIndex}` : '', event.workerName, event.text))
      stream.on('end', resolve)
      stream.on('error', reject)
    })
  }
} catch (error) {
  const details = error instanceof Error && 'details' in error ? error.details : error instanceof Error ? error.message : String(error)
  console.error('the daemon did not answer as expected:', details)
  process.exitCode = 1
} finally {
  client.close()
}
