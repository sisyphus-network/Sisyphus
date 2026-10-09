import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const locales = ['en', 'he', 'ar', 'ru', 'pl', 'es', 'fr']
const dictionaries = Object.fromEntries(locales.map((locale) => [locale, JSON.parse(readFileSync(new URL(`../src/renderer/src/i18n/messages/${locale}.json`, import.meta.url), 'utf8'))]))
test('all model settings locales contain every nonempty label', () => {
  const keys = Object.keys(dictionaries.en.modelSettings).sort()
  assert.equal(keys.length, 28)
  for (const locale of locales) {
    const labels = dictionaries[locale].modelSettings
    assert.deepEqual(Object.keys(labels).sort(), keys, locale)
    for (const [key, value] of Object.entries(labels)) assert.ok(typeof value === 'string' && value.trim(), `${locale}.${key}`)
  }
})
test('model technical example stays unchanged across translations', () => {
  for (const locale of locales) assert.ok(dictionaries[locale].modelSettings.downloadPlaceholder.includes('llama3.1:8b'))
})
