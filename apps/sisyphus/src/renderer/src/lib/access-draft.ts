export type AccessDraftValues = { enabled: boolean; addresses: string[]; publicUrl: string; validPort: boolean }

/** Compare effective values, not whether an input was ever touched. */
export function accessDraftIsDirty(draft: AccessDraftValues, saved: Omit<AccessDraftValues, 'validPort'>): boolean {
  return !draft.validPort || draft.enabled !== saved.enabled ||
    draft.publicUrl !== saved.publicUrl || draft.addresses.length !== saved.addresses.length ||
    draft.addresses.some((address, index) => address !== saved.addresses[index])
}
