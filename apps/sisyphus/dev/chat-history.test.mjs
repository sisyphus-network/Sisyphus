import assert from 'node:assert/strict'
import { test } from 'node:test'
import { loadMessages } from '../src/renderer/src/lib/chat-history.ts'

const assistant = (calls) => ({ role: 'assistant', content: '', calls })
const call = (name = 'run_job', args = '{}') => ({ name, arguments: args })
const result = (content, tool = 'run_job') => ({ role: 'tool', tool, content })

test('a saved call without a result is interrupted, never live', () => {
  assert.equal(loadMessages([assistant([call()])])[0].activities[0].state, 'interrupted')
})
test('restores job links from the planner’s saved JSON result', () => {
  const items = loadMessages([assistant([call()]), result('{"job_id":"job-1","result":{"count":25}}')])
  assert.equal(items.length, 1)
  assert.equal(items[0].activities[0].jobId, 'job-1')
  assert.equal(items[0].activities[0].state, 'complete')
})
test('repeated calls to the same tool retain result order', () => {
  const items = loadMessages([assistant([call(), call()]), result('{"job_id":"first"}'), result('{"job_id":"second"}')])
  assert.deepEqual(items[0].activities.map((activity) => activity.jobId), ['first', 'second'])
})
test('tool failures remain failures, with a get_job link from arguments', () => {
  const activity = loadMessages([assistant([call('get_job', '{"job_id":"missing"}')]), result('{"error":"not found"}', 'get_job')])[0].activities[0]
  assert.equal(activity.state, 'failed')
  assert.equal(activity.jobId, 'missing')
})
test('unmatched tools and non-JSON results are not lost', () => {
  const items = loadMessages([result('orphan'), assistant([call()]), result('plain text')])
  assert.equal(items[0].role, 'tool')
  assert.equal(items[1].activities[0].result, 'plain text')
  assert.equal(items[1].activities[0].state, 'complete')
})
test('user attachment metadata survives history reconstruction', () => {
  const item = loadMessages([{ role: 'user', content: 'hello\n\n[Sisyphus attachments]\n- "photo.png" | cid:abc | image:true' }])[0]
  assert.equal(item.content, 'hello')
  assert.deepEqual(item.attachments, [{ name: 'photo.png', cid: 'abc', image: true }])
})
