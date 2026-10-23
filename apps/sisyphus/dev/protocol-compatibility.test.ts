import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import protobuf from 'protobufjs'
import protoLoader from '@grpc/proto-loader'
import grpc from '@grpc/grpc-js'
import type { MethodDefinition } from '@grpc/proto-loader'
import { firstUploadMessage } from '../src/main/file-upload.ts'

// The definition is loaded from node.proto at run time, so its messages have
// no static types: each is a plain record here, and the service and its
// client are typed by what grpc-js gives any definition.
type Message = Record<string, unknown>
type NodeServiceClient = grpc.Client & Record<string, (...args: unknown[]) => unknown>

const source = readFileSync(new URL('../../../proto/sisyphus/node/v1/node.proto', import.meta.url), 'utf8')
const definition = protoLoader.fromJSON(protobuf.parse(source).root.toJSON(), { keepCase: false, longs: String, enums: String, defaults: true, oneofs: true })
const service = definition['sisyphus.node.v1.NodeService'] as Record<string, MethodDefinition<Message, Message>>
const rpcDeadlineMs = 30_000
test('parser and proto-loader resolve the same protobuf installation', () => {
  const require = createRequire(import.meta.url)
  const loaderRequire = createRequire(require.resolve('@grpc/proto-loader'))
  assert.equal(loaderRequire.resolve('protobufjs'), require.resolve('protobufjs'))
})
test('all node methods construct descriptors and roundtrip default messages', () => {
  for (const method of Object.values(service)) {
    assert.ok(method.path.startsWith('/sisyphus.node.v1.NodeService/'))
    assert.doesNotThrow(() => method.requestDeserialize(method.requestSerialize({})))
    assert.doesNotThrow(() => method.responseDeserialize(method.responseSerialize({})))
  }
})
test('jobs, files and events preserve bytes, enums and uint64 values', () => {
  const job = service.SubmitJob.requestDeserialize(service.SubmitJob.requestSerialize({ workload: 'primes', params: Buffer.from([0, 255]), mode: 2, taskTimeoutSeconds: 60, minMemoryBytes: '18446744073709551615' }))
  assert.equal(job.mode, 'JOB_MODE_FULL_WORKER')
  assert.equal(job.taskTimeoutSeconds, 60)
  assert.equal(job.minMemoryBytes, '18446744073709551615')
  assert.deepEqual(job.params, Buffer.from([0, 255]))
  const file = service.StoreFile.requestDeserialize(service.StoreFile.requestSerialize({ name: 'input', data: Buffer.from([128, 255]), private: true, chatAttachment: true }))
  assert.equal(file.private, true)
  assert.equal(file.chatAttachment, true)
  assert.deepEqual(file.data, Buffer.from([128, 255]))
  const event = service.WatchJobEvents.responseDeserialize(service.WatchJobEvents.responseSerialize({ seq: '9007199254740993', taskIndex: -1, text: 'שלום' }))
  assert.equal(event.seq, '9007199254740993')
  assert.equal(event.taskIndex, -1)
  assert.equal(event.text, 'שלום')
})
test('joined-pool events preserve enum names and whether a live worker count exists', () => {
  const method = service.GetJoinedPool
  const workerOnly = method.responseDeserialize(method.responseSerialize({
    coordinatorId: 'coordinator-id', coordinatorName: 'rig', state: 'JOINED_POOL_STATE_DISCONNECTED', roles: ['POOL_ROLE_WORKER'],
  }))
  assert.equal(workerOnly.state, 'JOINED_POOL_STATE_DISCONNECTED')
  assert.deepEqual(workerOnly.roles, ['POOL_ROLE_WORKER'])
  assert.equal(Object.hasOwn(workerOnly, 'workerCount'), false)
  const client = method.responseDeserialize(method.responseSerialize({
    state: 'JOINED_POOL_STATE_CONNECTED', roles: ['POOL_ROLE_WORKER', 'POOL_ROLE_CLIENT'], workerCount: 0,
  }))
  assert.equal(client.workerCount, 0)
  assert.deepEqual(client.roles, ['POOL_ROLE_WORKER', 'POOL_ROLE_CLIENT'])
})
test('grpc-js serves a unary call and a token stream with this definition', async () => {
  const server = new grpc.Server()
  let receivedAttachments: unknown
  const uploads: Message[][] = []
  server.addService(service, {
    getNodeInfo: (_: unknown, callback: grpc.sendUnaryData<Message>) => callback(null, { peerId: 'test-peer', countryCode: 'NL', countrySource: 'pool' }),
    ask: (call: grpc.ServerWritableStream<Message, Message>) => { receivedAttachments = call.request.attachmentCids; call.write({ chatId: 'chat', kind: 'text', text: 'שלום' }); call.write({ chatId: 'chat', kind: 'done' }); call.end() },
    storeFile: (call: grpc.ServerReadableStream<Message, Message>, callback: grpc.sendUnaryData<Message>) => {
      const chunks: Message[] = []
      call.on('data', (chunk: Message) => chunks.push(chunk))
      call.on('end', () => { uploads.push(chunks); callback(null, { cid: 'test-cid' }) })
    },
  })
  const port = await new Promise<number>((resolve, reject) => server.bindAsync('127.0.0.1:0', grpc.ServerCredentials.createInsecure(), (error, port) => error ? reject(error) : resolve(port)))
  const loaded = grpc.loadPackageDefinition(definition) as unknown as { sisyphus: { node: { v1: { NodeService: grpc.ServiceClientConstructor } } } }
  const client = new loaded.sisyphus.node.v1.NodeService(`127.0.0.1:${port}`, grpc.credentials.createInsecure()) as NodeServiceClient
  try {
    const info = await new Promise<Message>((resolve, reject) => client.getNodeInfo({}, { deadline: Date.now() + rpcDeadlineMs }, (error: grpc.ServiceError | null, response: Message) => error ? reject(error) : resolve(response)))
    assert.equal(info.peerId, 'test-peer')
    assert.equal(info.countryCode, 'NL')
    assert.equal(info.countrySource, 'pool')
    const events = await new Promise<Message[]>((resolve, reject) => {
      const events: Message[] = []
      const stream = client.ask({ text: 'test', attachmentCids: ['uploaded-one', 'uploaded-two'] }, { deadline: Date.now() + rpcDeadlineMs }) as grpc.ClientReadableStream<Message>
      stream.on('data', (event: Message) => events.push(event))
      stream.on('error', reject)
      stream.on('end', () => resolve(events))
    })
    assert.deepEqual(events.map((event) => event.kind), ['text', 'done'])
    assert.equal(events[0].text, 'שלום')
    assert.deepEqual(receivedAttachments, ['uploaded-one', 'uploaded-two'])
    for (const data of [Buffer.alloc(0), Buffer.from('draft')]) {
      await new Promise((resolve, reject) => {
        const upload = client.storeFile({ deadline: Date.now() + rpcDeadlineMs }, (error: grpc.ServiceError | null, response: Message) => error ? reject(error) : resolve(response)) as grpc.ClientWritableStream<Message>
        upload.end(firstUploadMessage({ name: 'draft.txt', private: true, chatAttachment: true }, data))
      })
      const sent = uploads.at(-1)
      assert.ok(sent, 'nothing was uploaded')
      const [first] = sent
      assert.equal(first.private, true)
      assert.equal(first.chatAttachment, true)
      assert.deepEqual(first.data, data)
    }
  } finally { client.close(); server.forceShutdown() }
})
