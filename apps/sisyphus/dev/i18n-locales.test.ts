import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const locales = ['en', 'he', 'ar', 'ru', 'pl', 'es', 'fr'] as const
const dictionaries = Object.fromEntries(locales.map((locale) => [
  locale,
  JSON.parse(readFileSync(new URL(`../src/renderer/src/i18n/messages/${locale}.json`, import.meta.url), 'utf8')) as unknown,
])) as Record<typeof locales[number], unknown>

function assertSameMessageShape(reference: unknown, translated: unknown, path: string): void {
  if (typeof reference === 'string') {
    assert.equal(typeof translated, 'string', `${path} must be a string`)
    assert.ok((translated as string).trim(), `${path} must not be empty`)
    return
  }

  if (Array.isArray(reference)) {
    assert.ok(Array.isArray(translated), `${path} must be an array`)
    assert.equal(translated.length, reference.length, `${path} must have the same number of entries`)
    reference.forEach((value, index) => assertSameMessageShape(value, translated[index], `${path}[${index}]`))
    return
  }

  if (reference && typeof reference === 'object') {
    assert.ok(translated && typeof translated === 'object' && !Array.isArray(translated), `${path} must be an object`)
    const referenceObject = reference as Record<string, unknown>
    const translatedObject = translated as Record<string, unknown>
    assert.deepEqual(Object.keys(translatedObject).sort(), Object.keys(referenceObject).sort(), `${path} keys`)
    for (const [key, value] of Object.entries(referenceObject)) {
      assertSameMessageShape(value, translatedObject[key], `${path}.${key}`)
    }
    return
  }

  assert.equal(translated, reference, `${path} value`)
}

test('all locale catalogs match English keys and contain no empty messages', () => {
  const english = dictionaries.en
  for (const locale of locales) {
    assertSameMessageShape(english, dictionaries[locale], locale)
  }
})
