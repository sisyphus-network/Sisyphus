export type PendingSettingsSave = { disabled: boolean; changeCount?: number; getSave: () => () => Promise<void> }

export function countSettingChanges(draft: Record<string, unknown>, saved: Record<string, unknown>): number {
  return Object.keys(draft).filter(key => !Object.is(draft[key], saved[key])).length
}

export function pendingChangeCount(entries: Iterable<PendingSettingsSave>): number {
  return [...entries].reduce((count, entry) => count + (entry.changeCount ?? 1), 0)
}

export async function savePendingSettings(entries: Iterable<PendingSettingsSave>): Promise<PromiseSettledResult<void>[]> {
  const pending = [...entries]
  if (!pending.length || pending.some(entry => entry.disabled)) return []
  // Capture each current draft before writing; callbacks may change after a save.
  const actions = pending.map(entry => entry.getSave())
  const results: PromiseSettledResult<void>[] = []
  for (const action of actions) {
    try {
      await action()
      results.push({ status: 'fulfilled', value: undefined })
    } catch (reason) {
      results.push({ status: 'rejected', reason })
    }
  }
  return results
}
