export function getMessageDirection(text: string, fallback: 'ltr' | 'rtl'): 'ltr' | 'rtl' {
  // Detect the prose language, excluding code and URLs that skew mixed replies.
  const prose = text.replace(/```[\s\S]*?(?:```|$)|`[^`]*`|https?:\/\/\S+/g, '')
  const firstLetter = prose.match(/\p{L}/u)?.[0]
  if (!firstLetter) return fallback
  return /[\p{Script=Hebrew}\p{Script=Arabic}]/u.test(firstLetter) ? 'rtl' : 'ltr'
}
