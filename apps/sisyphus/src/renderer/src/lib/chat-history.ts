export type StoredCall = { name: string; arguments: string }
export type Activity = { id: string; tool: string; arguments: string; result?: string; jobId?: string; state: 'working' | 'complete' | 'failed' | 'interrupted' }
export type AttachedFile = { name: string; cid: string; image: boolean; private?: boolean }
export type ChatItem = { id: string; role: 'user' | 'assistant' | 'tool'; content: string; tool?: string; calls?: StoredCall[]; activities?: Activity[]; attachments?: AttachedFile[] }
export type StoredChatMessage = { role: string; content: string; tool?: string; calls?: StoredCall[] }

function newId() {
  return globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`
}

function readAttachments(content: string): { content: string; attachments: AttachedFile[] } {
  const marker = '\n\n[Sisyphus attachments]\n'
  const index = content.lastIndexOf(marker)
  if (index < 0) return { content, attachments: [] }
  const attachments = content.slice(index + marker.length).split('\n').flatMap((line) => {
    const match = /^- (.*) \| cid:([^|]+) \| image:(true|false)(?: \| private:(true|false))?$/.exec(line)
    if (!match) return []
    try {
      const name: unknown = JSON.parse(match[1])
      return typeof name === 'string' ? [{ name, cid: match[2], image: match[3] === 'true', ...(match[4] ? { private: match[4] === 'true' } : {}) }] : []
    } catch { return [] }
  })
  return attachments.length ? { content: content.slice(0, index), attachments } : { content, attachments: [] }
}

export function loadMessages(messages: StoredChatMessage[]): ChatItem[] {
  const output: ChatItem[] = []
  for (const stored of messages) {
    if (stored.role === 'user') {
      output.push({ id: newId(), role: 'user', ...readAttachments(stored.content) })
    } else if (stored.role === 'assistant') {
      const activities: Activity[] = (stored.calls ?? []).map((call) => ({ id: newId(), tool: call.name, arguments: call.arguments, state: 'working' }))
      output.push({ id: newId(), role: 'assistant', content: stored.content, calls: stored.calls ?? [], activities })
    } else if (stored.role === 'tool') {
      const assistant = [...output].reverse().find((item) => item.role === 'assistant' && item.activities?.some((activity) => activity.tool === stored.tool && activity.result === undefined))
      // The planner executes calls in order; repeated tool names must not swap results.
      const activity = assistant?.activities?.find((item) => item.tool === stored.tool && item.result === undefined)
      if (!activity) {
        output.push({ id: newId(), role: 'tool', content: stored.content, tool: stored.tool })
        continue
      }
      activity.result = stored.content
      activity.state = 'complete'
      try {
        const result = JSON.parse(stored.content)
        if (result && typeof result === 'object') {
          if (typeof result.error === 'string' && result.error) activity.state = 'failed'
          if (['run_job', 'get_job'].includes(activity.tool) && typeof result.job_id === 'string' && result.job_id) activity.jobId = result.job_id
        }
      } catch { /* Non-JSON tool results are still valid history. */ }
      if (!activity.jobId && activity.tool === 'get_job') {
        try {
          const args = JSON.parse(activity.arguments)
          if (typeof args?.job_id === 'string' && args.job_id) activity.jobId = args.job_id
        } catch { /* Preserve malformed arguments for inspection. */ }
      }
    }
  }
  for (const item of output) {
    for (const activity of item.activities ?? []) if (activity.state === 'working') activity.state = 'interrupted'
  }
  return output
}
