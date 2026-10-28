import { SettingsSaveAction } from '@/components/ui/settings-save-dock'
import { SettingsSectionSkeleton } from '@/components/ui/settings-section-skeleton'
import { SettingsIcon } from '@/components/ui/settings-icon'
import { useEffect, useId, useRef, useState } from 'react'
import { Boxes, CircleAlert, Gauge, HardDrive, ListChecks, LoaderCircle, Save, Timer, Upload } from 'lucide-react'
import type { PoolLimits, PoolLimitsState } from '../../../preload'
import { getNodeApi } from '@/lib/node-api'
import { sharedStateDomainCode } from '@/lib/shared-state-domain'
import { SharedStateDomain } from '@sisyphus/client'
import { Button } from '@/components/ui/button'
import { NodeSettingsSection } from '@/components/node-settings-section'
import { FloatingLabelInput } from '@/components/ui/floating-label-input'
import { SettingsSelector } from '@/components/ui/settings-selector'
import { byteUnits, bytesForForm, countForForm, daemonErrorMessage, durationForForm, durationUnits, maxPoolLimitCount, noPoolLimits, parseAmount, samePoolLimits, type ByteUnit, type DurationUnit, type ParsedAmount } from '../../../shared/pool-limits.ts'
import { formatMessage } from '@/i18n/messages'
import { toast } from 'sonner'
import { RestartNodeButton } from '@/components/restart-node-button'
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
type SharedPoolLimitsEvent = {
  domain: number | string
  error?: string
  snapshot?: boolean
  state?: { case?: string; value?: PoolLimitsState } | string
  poolLimits?: PoolLimitsState
}

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
  const stateRef = useRef(state)
  const formRef = useRef(form)
  stateRef.current = state
  formRef.current = form

  useEffect(() => {
    let active = true
    const api = getNodeApi()
    let cancel: (() => void) | undefined
    let retry: number | undefined
    let attempt = 0
    let fallbackRequested = false
    const apply = (next: PoolLimitsState) => {
      if (!active) return
      const previous = stateRef.current
      const draft = parseForm(formRef.current).limits
      const unchanged = !previous || (draft !== undefined && samePoolLimits(draft, previous.saved))
      stateRef.current = next
      setState(next)
      if (unchanged) {
        const nextForm = formFromLimits(next.saved)
        formRef.current = nextForm
        setForm(nextForm)
      }
      setLoading(false)
    }
    const loadFallback = () => {
      if (!active || fallbackRequested) return
      fallbackRequested = true
      void api.getPoolLimits().then(apply).catch((error) => {
        if (active) toast.error(labels.loadFailed, { description: daemonErrorMessage(error) })
      }).finally(() => { if (active) setLoading(false) })
    }
    const reconnect = () => {
      cancel = undefined
      loadFallback()
      if (active) retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
    }
    const connect = () => {
      if (!active) return
      cancel = getNodeApi().stream<SharedPoolLimitsEvent>('watchSharedState', {
        domains: [SharedStateDomain.POOL_LIMITS],
      }, (event) => {
        if (sharedStateDomainCode(event.domain) !== 'poolLimits') return
        attempt = 0
        if (event.error) {
          loadFallback()
          return
        }
        const next = event.state && typeof event.state === 'object' && event.state.case === 'poolLimits'
          ? event.state.value
          : event.poolLimits
        if (next) apply(next)
      }, reconnect, reconnect)
    }
    connect()
    return () => {
      active = false
      if (retry !== undefined) window.clearTimeout(retry)
      cancel?.()
    }
  }, [labels.loadFailed])

  const parsed = parseForm(form)
  const valid = parsed.limits !== undefined
  const savedFields = state ? parseForm(formFromLimits(state.saved)).fields : undefined
  const changeCount = savedFields ? (Object.keys(parsed.fields) as Field[]).filter(key => {
    const draft = parsed.fields[key]
    const saved = savedFields[key]
    return !draft.ok || !saved.ok || draft.value !== saved.value
  }).length : 0
  const dirty = changeCount > 0
  const restartNeeded = state?.restartNeeded === true
  const editable = !loading && state?.configurable === true

  async function save() {
    if (!state?.configurable || !dirty || !parsed.limits) return
    setSaving(true)
    try {
      const next = await getNodeApi().savePoolLimits(parsed.limits)
      stateRef.current = next
      const nextForm = formFromLimits(next.saved)
      formRef.current = nextForm
      setState(next)
      setForm(nextForm)
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
    const value = form[field]
    const runningDiffers = state && describeLimit(labels, field, state.saved) !== describeLimit(labels, field, state.running)
    return <div key={field} className="settings-row settings-row--divider flex-wrap">
      <div className="flex min-w-0 flex-1 items-start gap-3">
        <SettingsIcon icon={Icon} />
        <div className="min-w-0">
          <label htmlFor={inputId} className="text-sm font-medium">{title}</label>
          <p className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{description}</p>
          {runningDiffers && <p className="mt-1 text-xs text-muted-foreground">{formatMessage(labels.inEffect, { value: describeLimit(labels, field, state.running) })}</p>}
        </div>
      </div>
      <div className="node-limit-controls grid w-full min-w-0 grid-cols-1 items-start gap-4 sm:max-w-md sm:grid-cols-[minmax(0,1fr)_12rem]">
        <FloatingLabelInput label={title} icon={<SettingsIcon icon={Icon} compact />} containerClassName="min-w-0 flex-1" id={inputId} dir="ltr" inputMode={unit ? 'decimal' : 'numeric'} autoComplete="off" spellCheck={false} maxLength={32} value={value} disabled={!editable}
          onChange={(event) => setForm((current) => ({ ...current, [field]: event.target.value }))}
          placeholder={labels.noLimit} errorMessage={error}
          className="tabular-nums" />
        {unit && <SettingsSelector value={unit.value} disabled={!editable} onChange={unit.onChange} label={messages.settingsUnit} ariaLabel={formatMessage(labels.unitFor, { field: title })} options={unit.options.map((value) => ({ value, label: labels.units[value as keyof Labels['units']], icon: Icon }))} className="w-full min-w-0" />}
      </div>
    </div>
  }

  return <NodeSettingsSection title={labels.title} description={labels.description} icon={Gauge}>
      <p className="mb-3 text-xs leading-relaxed text-muted-foreground">{labels.unitsNote}</p>
      {loading ? <SettingsSectionSkeleton label={messages.connecting} rows={5} /> : <>
        {state && !state.configurable && <div role="note" className="mt-4 flex items-start gap-2.5 rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)]/45 p-3 text-sm leading-relaxed text-muted-foreground"><CircleAlert className="mt-0.5 size-4 shrink-0" />{labels.operatorManaged}</div>}
        {state?.configurable && !state.coordinator && <div role="note" className="mt-4 flex items-start gap-2.5 rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)]/45 p-3 text-sm leading-relaxed text-muted-foreground"><CircleAlert className="mt-0.5 size-4 shrink-0" />{labels.notCoordinator}</div>}
        {row('jobs', Boxes, labels.jobsTitle, labels.jobsDescription)}
        {row('tasks', ListChecks, labels.tasksTitle, labels.tasksDescription)}
        {row('timeout', Timer, labels.timeoutTitle, labels.timeoutDescription, { value: form.timeoutUnit, options: Object.keys(durationUnits), onChange: (value) => setForm((current) => ({ ...current, timeoutUnit: value as DurationUnit })) })}
        {row('store', HardDrive, labels.storeTitle, labels.storeDescription, { value: form.storeUnit, options: Object.keys(byteUnits), onChange: (value) => setForm((current) => ({ ...current, storeUnit: value as ByteUnit })) })}
        {row('upload', Upload, labels.uploadTitle, labels.uploadDescription, { value: form.uploadUnit, options: Object.keys(byteUnits), onChange: (value) => setForm((current) => ({ ...current, uploadUnit: value as ByteUnit })) })}
        {state?.configurable && <div className="mt-3 flex flex-col-reverse justify-between gap-3 border-t border-[var(--app-line)] pt-4 sm:flex-row sm:items-center">
          <p role="status" className="text-xs leading-relaxed text-muted-foreground">{restartNeeded ? (state.managed ? labels.restartRequired : labels.restartDaemonRequired) : (state.managed ? labels.restartHint : labels.restartDaemonHint)}</p>
          <div className="flex flex-wrap gap-2">{restartNeeded && state.managed && <RestartNodeButton messages={messages} />}<SettingsSaveAction onRevert={() => { if (state) { const nextForm = formFromLimits(state.saved); formRef.current = nextForm; setForm(nextForm) } }} dirty={dirty} changeCount={changeCount} disabled={!dirty || saving || !valid} onSave={save}><Save className="size-4" />{saving ? <LoaderCircle className="size-4 animate-spin" /> : null}{labels.save}</SettingsSaveAction></div>
        </div>}
      </>}
  </NodeSettingsSection>
}
