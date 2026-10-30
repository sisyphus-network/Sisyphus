export type ChatRevisionSummary = { chatId: string; revision?: string | number | bigint }

export function chatRevisionChange(previous: ReadonlyMap<string, string>, next: readonly ChatRevisionSummary[], selectedId: string): { revisions: Map<string, string>; action: 'none' | 'close' | 'refresh' } {
  const revisions = new Map(next.map((chat) => [chat.chatId, String(chat.revision ?? 0)]))
  if (!selectedId) return { revisions, action: 'none' }
  if (!revisions.has(selectedId)) return { revisions, action: 'close' }
  return { revisions, action: previous.has(selectedId) && previous.get(selectedId) !== revisions.get(selectedId) ? 'refresh' : 'none' }
}
