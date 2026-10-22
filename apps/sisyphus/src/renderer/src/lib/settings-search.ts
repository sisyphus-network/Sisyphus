export function normalizeSettingsSearch(text: string): string {
  return text.normalize('NFKD').replace(/\p{M}/gu, '').toLocaleLowerCase().replace(/[^\p{L}\p{N}]+/gu, ' ').trim()
}

export function matchesSettingsSearch(query: string, terms: readonly string[]): boolean {
  const tokens = normalizeSettingsSearch(query).split(/\s+/).filter(Boolean)
  const haystack = normalizeSettingsSearch(terms.join(' '))
  return tokens.every((token) => haystack.includes(token))
}
