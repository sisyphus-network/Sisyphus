export function isTrustedRendererUrl(candidate: string, rendererUrl: string, development: boolean): boolean {
  try {
    const actual = new URL(candidate), expected = new URL(rendererUrl)
    if (development) return ['http:', 'https:'].includes(expected.protocol) && actual.origin === expected.origin
    actual.hash = ''; expected.hash = ''
    return expected.protocol === 'file:' && actual.href === expected.href
  } catch { return false }
}
