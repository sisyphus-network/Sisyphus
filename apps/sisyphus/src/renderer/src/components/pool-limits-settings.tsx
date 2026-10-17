import { useEffect, useId, useRef, useState } from 'react'
import { Boxes, CircleAlert, Gauge, HardDrive, ListChecks, LoaderCircle, Save, Timer, Upload } from 'lucide-react'
import type { PoolLimits, PoolLimitsState } from '../../../preload'
import { getNodeApi } from '@/lib/node-api'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { byteUnits, bytesForForm, countForForm, daemonErrorMessage, durationForForm, durationUnits, maxPoolLimitCount, noPoolLimits, parseAmount, samePoolLimits, type ByteUnit, type DurationUnit, type ParsedAmount } from '../../../shared/pool-limits.ts'
import { formatMessage } from '@/i18n/messages'
import { toast } from 'sonner'
import type { getMessages } from '@/i18n/messages'

type Messages = ReturnType<typeof getMessages>
type Labels = Messages['poolLimitsSettings']
type Form = {
  jobs: string
  tasks: string
  timeout: string
  timeoutUnit: DurationUnit
  store: string
  storeUnit: ByteUnit
  upload: string
  uploadUnit: ByteUnit
}
type Field = 'jobs' | 'tasks' | 'timeout' | 'store' | 'upload'

function formFromLimits(limits: PoolLimits): Form {
  const timeout = durationForForm(limits.maxTaskTimeoutSeconds)
  const store = bytesForForm(limits.maxStoreBytesPerClient)
  const upload = bytesForForm(limits.maxUploadBytesPerAttempt)
  return {
    jobs: countForForm(limits.maxJobsPerClient), tasks: countForForm(limits.maxTasksPerClient),
    timeout: timeout.text, timeoutUnit: timeout.unit, store: store.text, storeUnit: store.unit, upload: upload.text, uploadUnit: upload.unit,
  }
}

function parseForm(form: Form): { limits?: PoolLimits; fields: Record<Field, ParsedAmount> } {
  const fields: Record<Field, ParsedAmount> = {
    jobs: parseAmount(form.jobs, 1n, BigInt(maxPoolLimitCount)),
    tasks: parseAmount(form.tasks, 1n, BigInt(maxPoolLimitCount)),
    timeout: parseAmount(form.timeout, durationUnits[form.timeoutUnit]),
    store: parseAmount(form.store, byteUnits[form.storeUnit]),
    upload: parseAmount(form.upload, byteUnits[form.uploadUnit]),
  }
  const { jobs, tasks, timeout, store, upload } = fields
  if (!jobs.ok || !tasks.ok || !timeout.ok || !store.ok || !upload.ok) return { fields }
  return {
    fields,
    limits: {
      maxJobsPerClient: Number(jobs.value), maxTasksPerClient: Number(tasks.value), maxTaskTimeoutSeconds: String(timeout.value),
      maxStoreBytesPerClient: String(store.value), maxUploadBytesPerAttempt: String(upload.value),
    },
  }
}

function describeLimit(labels: Labels, field: Field, limits: PoolLimits): string {
  if (field === 'jobs' || field === 'tasks') {
    const value = field === 'jobs' ? limits.maxJobsPerClient : limits.maxTasksPerClient
    return value === 0 ? labels.noLimit : String(value)
  }
  if (field === 'timeout') {
    const { text, unit } = durationForForm(limits.maxTaskTimeoutSeconds)
    return text === '' ? labels.noLimit : `${text} ${labels.units[unit]}`
  }
  const { text, unit } = bytesForForm(field === 'store' ? limits.maxStoreBytesPerClient : limits.maxUploadBytesPerAttempt)
  return text === '' ? labels.noLimit : `${text} ${labels.units[unit]}`
}

export function PoolLimitsSettingsCard({ messages }: { messages: Messages }) {
  const labels = messages.poolLimitsSettings
  const idPrefix = useId()
  const [state, setState] = useState<PoolLimitsState>()
  const [form, setForm] = useState<Form>(() => formFromLimits(noPoolLimits))
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const hasLoadedSettings = useRef(false)

  useEffect(() => {
    let active = true
    const api = getNodeApi()
    const refresh = () => {
      void api.getPoolLimits().then((result) => {
        if (!active) return
        setState(result)
        if (!hasLoadedSettings.current) {
          hasLoadedSettings.current = true
          setForm(formFromLimits(result.saved))
        }
      }).catch((error) => {
        if (active) toast.error(labels.loadFailed, { description: daemonErrorMessage(error) })
      }).finally(() => { if (active) setLoading(false) })
    }
    let wasConnected: boolean | undefined
    const unsubscribe = api.onSnapshot((snapshot) => {
      const connected = snapshot.status === 'connected'
      if (connected !== wasConnected) {
        wasConnected = connected
        refresh()
      }
    })
    refresh()
    return () => { active = false; unsubscribe() }
  }, [labels.loadFailed])

  const parsed = parseForm(form)
  const valid = parsed.limits !== undefined
  const dirty = Boolean(state && (!parsed.limits || !samePoolLimits(parsed.limits, state.saved)))
  const restartNeeded = state?.restartNeeded === true
  const editable = !loading && state?.configurable === true

  async function save() {
    if (!state?.configurable || !dirty || !parsed.limits) return
    setSaving(true)
    try {
      const next = await getNodeApi().savePoolLimits(parsed.limits)
      setState(next)
      setForm(formFromLimits(next.saved))
      toast.success(labels.saved)
    } catch (error) {
      toast.error(labels.saveFailed, { description: daemonErrorMessage(error) })
    } finally { setSaving(false) }
  }

  const fieldError = (field: Field): string | undefined => {
    const result = parsed.fields[field]
    if (result.ok) return undefined
    if (field === 'jobs' || field === 'tasks') return labels.invalidCount
    if (result.reason === 'tooLarge') return labels.tooLarge
    return field === 'timeout' ? labels.invalidDuration : labels.invalidBytes
  }

  const row = (field: Field, Icon: typeof Boxes, title: string, description: string, unit?: { value: string; options: readonly string[]; onChange: (value: string) => void }) => {
    const inputId = `${idPrefix}-${field}`
    const error = fieldError(field)
    const errorId = `${inputId}-error`
    const value = form[field]
    const runningDiffers = state && describeLimit(labels, field, state.saved) !== describeLimit(labels, field, state.running)
    return <div key={field} className="settings-row settings-row--divider flex-wrap">
      <div className="flex min-w-0 flex-1 items-start gap-3">
        <span className="grid size-10 shrink-0 place-items-center rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)] text-[var(--app-muted)]"><Icon className="size-[18px]" /></span>
        <div className="min-w-0">
          <label htmlFor={inputId} className="text-sm font-medium">{title}</label>
          <p className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{description}</p>
          {runningDiffers && <p className="mt-1 text-xs text-muted-foreground">{formatMessage(labels.inEffect, { value: describeLimit(labels, field, state.running) })}</p>}
          {error && <p id={errorId} role="alert" className="mt-1 text-xs text-destructive">{error}</p>}
        </div>
      </div>
      <div className="flex w-full max-w-sm items-center gap-2 sm:w-auto">
        <Input id={inputId} inputMode={unit ? 'decimal' : 'numeric'} autoComplete="off" spellCheck={false} maxLength={32} value={value} disabled={!editable}
          onChange={(event) => setForm((current) => ({ ...current, [field]: event.target.value }))}
          placeholder={labels.noLimit} aria-invalid={Boolean(error)} aria-describedby={error ? errorId : undefined}
          className="w-full min-w-0 text-start tabular-nums sm:w-36" />
        {unit && <select value={unit.value} disabled={!editable} onChange={(event) => unit.onChange(event.target.value)} aria-label={formatMessage(labels.unitFor, { field: title })}
          className="h-9 shrink-0 rounded-lg border border-[var(--app-line)] bg-background px-3 text-sm">
          {unit.options.map((option) => <option key={option} value={option}>{labels.units[option as keyof Labels['units']]}</option>)}
        </select>}
      </div>
    </div>
  }

  return <Card className="settings-card min-w-0">
    <CardContent className="p-4 sm:p-5">
      <div className="flex items-start gap-3">
        <div className="settings-row__icon"><Gauge className="size-4" /></div>
        <div className="min-w-0 flex-1"><h3 className="text-sm font-semibold">{labels.title}</h3><p className="mt-1 text-sm leading-relaxed text-muted-foreground">{labels.description}</p><p className="mt-1 text-xs leading-relaxed text-muted-foreground">{labels.unitsNote}</p></div>
      </div>
      {loading ? <div role="status" className="flex items-center gap-2 py-7 text-sm text-muted-foreground"><LoaderCircle className="size-4 animate-spin" />{messages.connecting}</div> : <>
        {state && !state.configurable && <div role="note" className="mt-4 flex items-start gap-2.5 rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)]/45 p-3 text-sm leading-relaxed text-muted-foreground"><CircleAlert className="mt-0.5 size-4 shrink-0" />{labels.operatorManaged}</div>}
        {state?.configurable && !state.coordinator && <div role="note" className="mt-4 flex items-start gap-2.5 rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)]/45 p-3 text-sm leading-relaxed text-muted-foreground"><CircleAlert className="mt-0.5 size-4 shrink-0" />{labels.notCoordinator}</div>}
        {row('jobs', Boxes, labels.jobsTitle, labels.jobsDescription)}
        {row('tasks', ListChecks, labels.tasksTitle, labels.tasksDescription)}
        {row('timeout', Timer, labels.timeoutTitle, labels.timeoutDescription, { value: form.timeoutUnit, options: Object.keys(durationUnits), onChange: (value) => setForm((current) => ({ ...current, timeoutUnit: value as DurationUnit })) })}
        {row('store', HardDrive, labels.storeTitle, labels.storeDescription, { value: form.storeUnit, options: Object.keys(byteUnits), onChange: (value) => setForm((current) => ({ ...current, storeUnit: value as ByteUnit })) })}
        {row('upload', Upload, labels.uploadTitle, labels.uploadDescription, { value: form.uploadUnit, options: Object.keys(byteUnits), onChange: (value) => setForm((current) => ({ ...current, uploadUnit: value as ByteUnit })) })}
        {state?.configurable && <div className="mt-3 flex flex-col-reverse justify-between gap-3 border-t border-[var(--app-line)] pt-4 sm:flex-row sm:items-center">
          <p role="status" className="text-xs leading-relaxed text-muted-foreground">{restartNeeded ? (state.managed ? labels.restartRequired : labels.restartDaemonRequired) : (state.managed ? labels.restartHint : labels.restartDaemonHint)}</p>
          <Button type="button" disabled={!dirty || saving || !valid} onClick={() => void save()} className="gap-2"><Save className="size-4" />{saving ? <LoaderCircle className="size-4 animate-spin" /> : null}{labels.save}</Button>
        </div>}
      </>}
    </CardContent>
  </Card>
}
