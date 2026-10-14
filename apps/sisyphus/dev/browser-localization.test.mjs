import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const locales = ['en', 'he', 'ar', 'ru', 'pl', 'es', 'fr']
const dictionaries = Object.fromEntries(locales.map((locale) => [locale, JSON.parse(readFileSync(new URL(`../src/renderer/src/i18n/messages/${locale}.json`, import.meta.url), 'utf8'))]))
const topLevelKeys = ['browserAuthTitle', 'browserAuthWaiting', 'browserAuthDescription', 'browserHttpWarning', 'browserAuthTokenLabel', 'browserAuthConnect', 'browserPairCodeLabel', 'browserPair', 'browserTokenAlternative']

test('all browser access locales contain every nonempty label', () => {
  const accessKeys = Object.keys(dictionaries.en.browserAccess).sort()
  for (const locale of locales) {
    const dictionary = dictionaries[locale]
    assert.deepEqual(Object.keys(dictionary.browserAccess).sort(), accessKeys, `${locale}.browserAccess`)
    for (const key of topLevelKeys) assert.ok(typeof dictionary[key] === 'string' && dictionary[key].trim() !== '', `${locale}.${key}`)
    for (const key of accessKeys) assert.ok(typeof dictionary.browserAccess[key] === 'string' && dictionary.browserAccess[key].trim() !== '', `${locale}.browserAccess.${key}`)
  }
})
