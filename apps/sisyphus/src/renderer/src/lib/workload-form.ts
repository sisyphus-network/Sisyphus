// What the Run page knows of a workload: what ListWorkloads says of it, read
// the same whichever client asked, and its form's values, checked against the
// form and turned into the job's JSON parameters.
//
// In Electron the reply comes through proto-loader, which names enums
// (WORKLOAD_FIELD_KIND_INTEGER) and sets every field to its default; in a
// browser it is a Connect message, with enums as numbers, the unset optional
// bounds left out, and nothing of the protobuf markers once the bridge has
// normalized it. Either way the page sees the same plain workload.

export type FieldKind = 'file' | 'files' | 'integer' | 'number' | 'text' | 'choice' | 'toggle' | 'unknown'

// A field of several files holds their CIDs as a list.
export type FormValue = string | boolean | string[]
export type FormValues = Record<string, FormValue>

export type FormField = {
  name: string
  kind: FieldKind
  label: string
  help: string
  // The value the field starts with, as the daemon gave it, or undefined.
  defaultValue: unknown
  minimum?: number
  maximum?: number
  // What a number must be a multiple of, or 0 for anything.
  step: number
  choices: string[]
  required: boolean
}

export type Workload = {
  name: string
  description: string
  fields: FormField[]
  workersRunningIt: number
  composite: boolean
}

export type FieldErrorKey = 'required' | 'notANumber' | 'notWhole' | 'belowMinimum' | 'aboveMaximum' | 'offStep' | 'notAChoice'
export type FieldError = { key: FieldErrorKey; bound?: number }

const kindNumbers: Record<number, FieldKind> = { 1: 'file', 2: 'integer', 3: 'number', 4: 'text', 5: 'choice', 6: 'toggle', 7: 'files' }
const kindNames = new Set<FieldKind>(['file', 'files', 'integer', 'number', 'text', 'choice', 'toggle'])

// fieldKind reads a field's kind as a number, its enum name, or its short name.
export function fieldKind(kind: unknown): FieldKind {
  if (typeof kind === 'number') return kindNumbers[kind] ?? 'unknown'
  if (typeof kind !== 'string') return 'unknown'
  const name = kind.replace(/^WORKLOAD_FIELD_KIND_/, '').toLowerCase() as FieldKind
  return kindNames.has(name) ? name : 'unknown'
}

type Raw = Record<string, unknown>

function pick(raw: Raw, camel: string, snake: string): unknown {
  return raw[camel] !== undefined ? raw[camel] : raw[snake]
}

function text(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

function finite(value: unknown): number | undefined {
  if (value === undefined || value === null || value === '') return undefined
  const number = typeof value === 'number' ? value : typeof value === 'string' ? Number(value) : NaN
  return Number.isFinite(number) ? number : undefined
}

// bound reads an optional bound. proto-loader with oneofs names a set
// proto3 optional field in its synthetic oneof (_minimum: 'minimum'), so a
// bound whose oneof names something else, or nothing, is unset.
function bound(raw: Raw, name: string): number | undefined {
  const oneof = raw[`_${name}`]
  if (oneof !== undefined && oneof !== name) return undefined
  return finite(raw[name])
}

function parseDefault(json: unknown): unknown {
  if (typeof json !== 'string' || json.trim() === '') return undefined
  try { return JSON.parse(json) } catch { return undefined }
}

export function normalizeField(value: unknown): FormField {
  const raw = (value && typeof value === 'object' ? value : {}) as Raw
  const choices = Array.isArray(raw.choices) ? raw.choices.filter((choice): choice is string => typeof choice === 'string') : []
  return {
    name: text(raw.name),
    kind: fieldKind(raw.kind),
    label: text(raw.label),
    help: text(raw.help),
    defaultValue: parseDefault(pick(raw, 'defaultJson', 'default_json')),
    minimum: bound(raw, 'minimum'),
    maximum: bound(raw, 'maximum'),
    step: Math.max(0, finite(raw.step) ?? 0),
    choices,
    required: raw.required === true,
  }
}

export function normalizeWorkload(value: unknown): Workload {
  const raw = (value && typeof value === 'object' ? value : {}) as Raw
  const fields = Array.isArray(raw.fields) ? raw.fields.map(normalizeField).filter((field) => field.name !== '') : []
  return {
    name: text(raw.name),
    description: text(raw.description),
    fields,
    workersRunningIt: Math.max(0, Math.trunc(finite(pick(raw, 'workersRunningIt', 'workers_running_it')) ?? 0)),
    composite: raw.composite === true,
  }
}

// normalizeWorkloads reads a ListWorkloads reply, or its list of workloads.
export function normalizeWorkloads(response: unknown): Workload[] {
  const list = Array.isArray(response) ? response : (response && typeof response === 'object' ? (response as Raw).workloads : undefined)
  return Array.isArray(list) ? list.map(normalizeWorkload).filter((workload) => workload.name !== '') : []
}

// countGpuWorkers reads the worker list sent by WatchWorkers, counting each
// connected worker once when it reports at least one offered graphics card.
export function countGpuWorkers(workers: unknown): number {
  if (!Array.isArray(workers)) return 0
  return workers.filter((worker) => {
    if (!worker || typeof worker !== 'object') return false
    const gpus = (worker as Raw).gpus
    return Array.isArray(gpus) && gpus.length > 0
  }).length
}

// canRun says whether a job of the workload has anyone to run it: a worker
// that offers it, or the coordinator itself for a composite one.
export function canRun(workload: Workload): boolean {
  return workload.composite || workload.workersRunningIt > 0
}

// firstSentence is the description's first sentence, which says what the
// workload does; the rest is its parameters, for those writing JSON.
export function firstSentence(description: string): string {
  const trimmed = description.trim()
  const match = /^(.+?[.!?])(?=\s|$)/s.exec(trimmed)
  return match ? match[1] : trimmed
}

// initialValues are a form's values before anyone changes them: each
// field's default, or empty where it has none. A number is kept as the text
// its input shows, and a toggle as true or false.
export function initialValues(fields: FormField[]): FormValues {
  const values: FormValues = {}
  for (const field of fields) {
    const value = field.defaultValue
    switch (field.kind) {
      case 'toggle':
        values[field.name] = value === true
        break
      case 'integer':
      case 'number':
        values[field.name] = typeof value === 'number' && Number.isFinite(value) ? String(value) : ''
        break
      case 'choice':
        values[field.name] = typeof value === 'string' ? value : field.required && field.choices.length > 0 ? field.choices[0] : ''
        break
      case 'files':
        values[field.name] = Array.isArray(value) ? value.filter((cid): cid is string => typeof cid === 'string') : []
        break
      default:
        values[field.name] = typeof value === 'string' ? value : value === undefined || value === null ? '' : String(value)
    }
  }
  return values
}

function isEmpty(value: FormValue | undefined): boolean {
  if (Array.isArray(value)) return value.every((cid) => cid.trim() === '')
  return value === undefined || (typeof value === 'string' && value.trim() === '')
}

// A number as an input gives it: plain decimal or exponent notation, and
// nothing else that Number would read, such as hex or an empty string.
const numberPattern = /^[+-]?(\d+\.?\d*|\.\d+)(e[+-]?\d+)?$/i

function parseNumber(value: string): number | undefined {
  const trimmed = value.trim()
  if (!numberPattern.test(trimmed)) return undefined
  const number = Number(trimmed)
  return Number.isFinite(number) ? number : undefined
}

function multipleOf(value: number, step: number): boolean {
  const quotient = value / step
  return Math.abs(quotient - Math.round(quotient)) <= 1e-9 * Math.max(1, Math.abs(quotient))
}

export function validateField(field: FormField, value: FormValue | undefined): FieldError | undefined {
  if (field.kind === 'toggle') return undefined
  if (isEmpty(value)) return field.required ? { key: 'required' } : undefined
  const given = typeof value === 'string' ? value.trim() : String(value)
  switch (field.kind) {
    case 'integer':
    case 'number': {
      const number = parseNumber(given)
      if (number === undefined) return { key: 'notANumber' }
      if (field.kind === 'integer' && !Number.isInteger(number)) return { key: 'notWhole' }
      if (field.minimum !== undefined && number < field.minimum) return { key: 'belowMinimum', bound: field.minimum }
      if (field.maximum !== undefined && number > field.maximum) return { key: 'aboveMaximum', bound: field.maximum }
      if (field.step > 0 && !multipleOf(number, field.step)) return { key: 'offStep', bound: field.step }
      return undefined
    }
    case 'choice':
      return field.choices.includes(given) ? undefined : { key: 'notAChoice' }
    default:
      return undefined
  }
}

// validate checks every field, and gives the errors of those that are wrong
// by field name. A form with none is a job its workload accepts.
export function validate(fields: FormField[], values: FormValues): Record<string, FieldError> {
  const errors: Record<string, FieldError> = {}
  for (const field of fields) {
    const error = validateField(field, values[field.name])
    if (error) errors[field.name] = error
  }
  return errors
}

export function isValid(fields: FormField[], values: FormValues): boolean {
  return Object.keys(validate(fields, values)).length === 0
}

// toParams turns a form's values into the job's JSON parameters: numbers as
// numbers, toggles as true or false, several files as a list of CIDs, the
// rest as text. An optional field left
// empty is left out, so that the workload uses its own default.
export function toParams(fields: FormField[], values: FormValues): Record<string, unknown> {
  const params: Record<string, unknown> = {}
  for (const field of fields) {
    const value = values[field.name]
    if (field.kind === 'toggle') {
      params[field.name] = value === true
      continue
    }
    if (isEmpty(value)) continue
    if (Array.isArray(value)) {
      params[field.name] = value.map((cid) => cid.trim()).filter((cid) => cid !== '')
      continue
    }
    const given = typeof value === 'string' ? value.trim() : String(value)
    if (field.kind === 'integer' || field.kind === 'number') {
      const number = parseNumber(given)
      if (number !== undefined) params[field.name] = number
    } else {
      params[field.name] = given
    }
  }
  return params
}
