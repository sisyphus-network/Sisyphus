import assert from 'node:assert/strict'
import { test } from 'node:test'
import { matchesSettingsSearch, normalizeSettingsSearch } from '../src/renderer/src/lib/settings-search.ts'

test('settings search handles accents, Hebrew marks, Arabic marks and Cyrillic', () => {
  for (const [query, label] of [['reseau', 'Réseau'], ['מיקום', 'מִיקוּם'], ['الموقع', 'الْمَوْقِع'], ['СЕТЬ', 'Сеть']]) {
    assert.equal(matchesSettingsSearch(query, [label]), true)
  }
})
test('all query words must match, regardless of order or language', () => {
  assert.equal(matchesSettingsSearch('GPU זיכרון', ['זיכרון', 'GPU memory']), true)
  assert.equal(matchesSettingsSearch('memory GPU', ['GPU memory']), true)
  assert.equal(matchesSettingsSearch('GPU Docker', ['GPU memory']), false)
})
test('empty search restores everything and punctuation is normalized', () => {
  assert.equal(matchesSettingsSearch('  ', []), true)
  assert.equal(normalizeSettingsSearch('NAT-PMP / STUN'), 'nat pmp stun')
  assert.equal(matchesSettingsSearch('nat pmp', ['NAT-PMP']), true)
})
