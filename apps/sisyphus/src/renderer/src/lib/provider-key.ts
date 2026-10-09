export function missingProviderKey(needsKey: string | number | undefined, apiKey: string, keepApiKey: boolean): boolean {
  return (needsKey === 1 || needsKey === 'SUPPORT_YES') && !apiKey.trim() && !keepApiKey
}
