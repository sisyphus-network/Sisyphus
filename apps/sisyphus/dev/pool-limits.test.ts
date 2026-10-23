import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { byteUnits, bytesForForm, countForForm, daemonErrorMessage, durationForForm, durationUnits, maxPoolLimitCount, noPoolLimits, normalizePoolLimits, parseAmount, samePoolLimits, validatePoolLimits } from '../src/shared/pool-limits.ts'

const huge = '18446744073709551615'

test('limits from the daemon keep 64-bit values exactly, whether bigint or string', () => {
  assert.deepEqual(normalizePoolLimits({
    maxJobsPerClient: 3, maxTasksPerClient: 40, maxTaskTimeoutSeconds: 9223372036n,
    maxStoreBytesPerClient: 9223372036854775807n, maxUploadBytesPerAttempt: huge,
  }), {
    maxJobsPerClient: 3, maxTasksPerClient: 40, maxTaskTimeoutSeconds: '9223372036',
    maxStoreBytesPerClient: '9223372036854775807', maxUploadBytesPerAttempt: huge,
  })
  assert.deepEqual(normalizePoolLimits(undefined), noPoolLimits)
  assert.deepEqual(normalizePoolLimits({ maxJobsPerClient: -1, maxTasksPerClient: 2 ** 31, maxTaskTimeoutSeconds: '-5', maxStoreBytesPerClient: '18446744073709551616', maxUploadBytesPerAttempt: 1.5 }), noPoolLimits)
})

test('limits a renderer saves are checked strictly', () => {
  const limits = { maxJobsPerClient: maxPoolLimitCount, maxTasksPerClient: 0, maxTaskTimeoutSeconds: '3600', maxStoreBytesPerClient: '9223372036854775807', maxUploadBytesPerAttempt: '0' }
  assert.deepEqual(validatePoolLimits({ ...limits, extra: true }), limits)
  for (const bad of [
    null, [], {},
    { ...limits, maxJobsPerClient: maxPoolLimitCount + 1 },
    { ...limits, maxTasksPerClient: -1 },
    { ...limits, maxTasksPerClient: '4' },
    { ...limits, maxTaskTimeoutSeconds: 60 },
    { ...limits, maxStoreBytesPerClient: '1e9' },
    { ...limits, maxUploadBytesPerAttempt: '18446744073709551616' },
  ]) assert.throws(() => validatePoolLimits(bad), /Invalid pool limits/)
  assert.equal(samePoolLimits(limits, { ...limits }), true)
  assert.equal(samePoolLimits(limits, { ...limits, maxUploadBytesPerAttempt: '1' }), false)
})

test('typed amounts become whole base units with BigInt, or say why not', () => {
  assert.deepEqual(parseAmount('', byteUnits.GB), { ok: true, value: 0n })
  assert.deepEqual(parseAmount('  ', 1n), { ok: true, value: 0n })
  assert.deepEqual(parseAmount('1.5', byteUnits.GB), { ok: true, value: 1_500_000_000n })
  assert.deepEqual(parseAmount('1,5', byteUnits.GB), { ok: true, value: 1_500_000_000n })
  assert.deepEqual(parseAmount('.25', durationUnits.hours), { ok: true, value: 900n })
  assert.deepEqual(parseAmount('9223372.036854775807', byteUnits.TB), { ok: true, value: 9223372036854775807n })
  assert.deepEqual(parseAmount('2.0', 1n, BigInt(maxPoolLimitCount)), { ok: true, value: 2n })
  assert.deepEqual(parseAmount('2.5', 1n), { ok: false, reason: 'fraction' })
  assert.deepEqual(parseAmount('0.0000001', byteUnits.MB), { ok: false, reason: 'fraction' })
  assert.deepEqual(parseAmount('1.01', durationUnits.minutes), { ok: false, reason: 'fraction' })
  for (const text of ['-1', '+1', '1e3', '1 000', '1.000.000', '.', 'abc', '0x10']) assert.deepEqual(parseAmount(text, 1n), { ok: false, reason: 'invalid' }, text)
  assert.deepEqual(parseAmount('2147483648', 1n, BigInt(maxPoolLimitCount)), { ok: false, reason: 'tooLarge' })
  assert.deepEqual(parseAmount('20000000', byteUnits.TB), { ok: false, reason: 'tooLarge' })
})

test('saved limits are shown in the form exactly and read back unchanged', () => {
  assert.deepEqual(bytesForForm('0'), { text: '', unit: 'GB' })
  assert.deepEqual(bytesForForm('1048576'), { text: '1.048576', unit: 'MB' })
  assert.deepEqual(bytesForForm('500'), { text: '0.0005', unit: 'MB' })
  assert.deepEqual(bytesForForm('2500000000'), { text: '2.5', unit: 'GB' })
  assert.deepEqual(bytesForForm('9223372036854775807'), { text: '9223372.036854775807', unit: 'TB' })
  assert.deepEqual(durationForForm('0'), { text: '', unit: 'hours' })
  assert.deepEqual(durationForForm('7200'), { text: '2', unit: 'hours' })
  assert.deepEqual(durationForForm('5400'), { text: '90', unit: 'minutes' })
  assert.deepEqual(durationForForm('90'), { text: '90', unit: 'seconds' })
  assert.equal(countForForm(0), '')
  assert.equal(countForForm(12), '12')
  for (const bytes of ['1', '999999', '1048576', '123456789012345', '9223372036854775807', huge]) {
    const { text, unit } = bytesForForm(bytes)
    assert.deepEqual(parseAmount(text, byteUnits[unit]), { ok: true, value: BigInt(bytes) }, bytes)
  }
  for (const seconds of ['1', '59', '60', '3599', '3600', '86400', '9223372036']) {
    const { text, unit } = durationForForm(seconds)
    assert.deepEqual(parseAmount(text, durationUnits[unit]), { ok: true, value: BigInt(seconds) }, seconds)
  }
})

test('a daemon refusal is shown in its own words', () => {
  assert.equal(daemonErrorMessage(new Error("Error invoking remote method 'node:save-pool-limits': Error: 3 INVALID_ARGUMENT: max_jobs_per_client may be at most 2147483647")), 'max_jobs_per_client may be at most 2147483647')
  assert.equal(daemonErrorMessage(new Error('[invalid_argument] max_task_timeout_seconds may be at most 9223372036')), 'max_task_timeout_seconds may be at most 9223372036')
  assert.equal(daemonErrorMessage('plain'), 'plain')
})

type Messages = { [key: string]: string | Messages }

test('every locale has every pool limits label', () => {
  const locales = ['en', 'he', 'ar', 'ru', 'pl', 'es', 'fr']
  const read = (locale: string): Messages => JSON.parse(readFileSync(new URL(`../src/renderer/src/i18n/messages/${locale}.json`, import.meta.url), 'utf8')).poolLimitsSettings
  const flatten = (value: Messages, prefix = ''): [string, string][] => Object.entries(value).flatMap(([key, item]) => typeof item === 'object' ? flatten(item, `${prefix}${key}.`) : [[`${prefix}${key}`, item]])
  const keys = flatten(read('en')).map(([key]) => key).sort()
  for (const locale of locales) {
    const entries = flatten(read(locale))
    assert.deepEqual(entries.map(([key]) => key).sort(), keys, locale)
    for (const [key, text] of entries) assert.ok(typeof text === 'string' && text.trim() !== '', `${locale}.poolLimitsSettings.${key}`)
    for (const key of ['inEffect', 'unitFor']) assert.match(Object.fromEntries(entries)[key], /\{(value|field)\}/, `${locale}.${key}`)
  }
})
