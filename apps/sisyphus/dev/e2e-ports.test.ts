import assert from 'node:assert/strict'
import { createServer } from 'node:net'
import { test } from 'node:test'
import { freePorts } from '../e2e/harness.ts'

test('the ports a test node is given are all different, and free once given', async () => {
  for (let round = 0; round < 50; round++) {
    const ports = await freePorts(3)
    assert.equal(new Set(ports).size, 3, `ports given twice: ${ports}`)
    for (const port of ports) {
      await new Promise<void>((done, failed) => {
        const server = createServer()
        server.once('error', failed)
        server.listen(port, '127.0.0.1', () => server.close(() => done()))
      })
    }
  }
})
