import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import protobuf from 'protobufjs'
import protoLoader from '@grpc/proto-loader'
import grpc from '@grpc/grpc-js'
import { firstUploadMessage } from '../src/main/file-upload.ts'

const source = readFileSync(new URL('../../../proto/sisyphus/node/v1/node.proto', import.meta.url), 'utf8')
const definition = protoLoader.fromJSON(protobuf.parse(source).root.toJSON(), { keepCase: false, longs: String, enums: String, defaults: true, oneofs: true })
const service = definition['sisyphus.node.v1.NodeService']
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
test('grpc-js serves a unary call and a token stream with this definition', async () => {
  const server = new grpc.Server()
  let receivedAttachments
  const uploads = []
  server.addService(service, {
    getNodeInfo: (_, callback) => callback(null, { peerId: 'test-peer' }),
    ask: (call) => { receivedAttachments = call.request.attachmentCids; call.write({ chatId: 'chat', kind: 'text', text: 'שלום' }); call.write({ chatId: 'chat', kind: 'done' }); call.end() },
    storeFile: (call, callback) => {
      const chunks = []
      call.on('data', (chunk) => chunks.push(chunk))
      call.on('end', () => { uploads.push(chunks); callback(null, { cid: 'test-cid' }) })
    },
  })
  const port = await new Promise((resolve, reject) => server.bindAsync('127.0.0.1:0', grpc.ServerCredentials.createInsecure(), (error, port) => error ? reject(error) : resolve(port)))
  const loaded = grpc.loadPackageDefinition(definition)
  const client = new loaded.sisyphus.node.v1.NodeService(`127.0.0.1:${port}`, grpc.credentials.createInsecure())
  try {
    const info = await new Promise((resolve, reject) => client.getNodeInfo({}, { deadline: Date.now() + 5000 }, (error, response) => error ? reject(error) : resolve(response)))
    assert.equal(info.peerId, 'test-peer')
    const events = await new Promise((resolve, reject) => {
      const events = []
      const stream = client.ask({ text: 'test', attachmentCids: ['uploaded-one', 'uploaded-two'] }, { deadline: Date.now() + 5000 })
      stream.on('data', (event) => events.push(event))
      stream.on('error', reject)
      stream.on('end', () => resolve(events))
    })
    assert.deepEqual(events.map((event) => event.kind), ['text', 'done'])
    assert.equal(events[0].text, 'שלום')
    assert.deepEqual(receivedAttachments, ['uploaded-one', 'uploaded-two'])
    for (const data of [Buffer.alloc(0), Buffer.from('draft')]) {
      await new Promise((resolve, reject) => {
        const upload = client.storeFile({ deadline: Date.now() + 5000 }, (error, response) => error ? reject(error) : resolve(response))
        upload.end(firstUploadMessage({ name: 'draft.txt', private: true, chatAttachment: true }, data))
      })
      const [first] = uploads.at(-1)
      assert.equal(first.private, true)
      assert.equal(first.chatAttachment, true)
      assert.deepEqual(first.data, data)
    }
  } finally { client.close(); server.forceShutdown() }
})
