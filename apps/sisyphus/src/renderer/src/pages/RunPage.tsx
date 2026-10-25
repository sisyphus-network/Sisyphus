import { useCallback, useEffect, useMemo, useRef, useState, type DragEvent, type FormEvent, type ReactNode } from 'react'
import { NavLink, useSearchParams } from 'react-router-dom'
import { Activity, ArrowLeft, Atom, Boxes, Captions, ChevronRight, CircleAlert, Clapperboard, Cpu, Droplets, FileCheck2, FileUp, FlaskConical, Gpu, History, Images, LoaderCircle, Pencil, Play, RefreshCw, ScanText, Shapes, Sigma, ChartSpline, TrendingUp, WholeWord, Workflow, X, type LucideIcon } from 'lucide-react'
import { toast } from 'sonner'
import { Badge } from '@/components/ui/badge'
import { Button, buttonVariants } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { AnimatedStepper } from '@/components/ui/animated-stepper'
import { LiquidSwitch } from '@/components/ui/liquid-switch'
import { EmptyState, PageHeading } from '@/components/ui/page-layout'
import { JobTracker } from '@/components/assistant/job-tracker'
import { JobOutputs, JobResult, prettyBytes } from '@/components/job-result-view'
import { getNodeApi } from '@/lib/node-api'
import { getJobHref } from '@/lib/job-navigation'
import { stateName } from '@/lib/job-progress'
import { formatNumber, parseMdResult } from '@/lib/md-result'
import { canRun, countGpuWorkers, firstSentence, initialValues, normalizeWorkloads, toParams, validate, type FieldError, type FormField, type FormValue, type FormValues, type Workload } from '@/lib/workload-form'
import { formatMessage, type getMessages } from '@/i18n/messages'
import { MAX_CLIENT_FILE_UPLOAD_BYTES, fileUploadLimitLabel } from '../../../shared/file-upload-limit.ts'

type Messages = ReturnType<typeof getMessages>
type Labels = Messages['runPage']
type Job = { jobId: string; workload: string; state: string | number; error?: string; result?: Uint8Array; outputBlobs?: string[]; tasks?: { state: string | number }[] }
type StoredFile = { cid: string; name: string; sizeBytes: string | number; private: boolean }
type FileInfo = { name: string; size: number }
type Run = { jobId: string; workload: string; values: FormValues; files: Record<string, FileInfo>; startedAt: number }

const maxUploadBytes = MAX_CLIENT_FILE_UPLOAD_BYTES

// The jobs started from this page while the app is open, newest first. They
// outlive the page, so that coming back to it finds them, but not the app.
let sessionRuns: Run[] = []

const workloadIcons: Record<string, LucideIcon> = { primes: Sigma, wordcount: WholeWord, transcode: Clapperboard, md: Atom, render: Shapes, transcribe: Captions, qchem: FlaskConical, ocr: ScanText, traj: Activity, forecast: TrendingUp, forecasts: ChartSpline, prep: Droplets, images: Images }

function workloadIcon(workload: Workload): LucideIcon {
  return workloadIcons[workload.name] ?? (workload.composite ? Workflow : Cpu)
}

// workloadText is what the app calls a workload: its own words for the ones
// it knows, and the daemon's name and first sentence for any other.
function workloadText(labels: Labels, workload: Pick<Workload, 'name' | 'description'>): { title: string; description: string } {
  const known = (labels.workloads as Record<string, { title: string; description: string } | undefined>)[workload.name]
  return known ?? { title: workload.name, description: firstSentence(workload.description) }
}

function errorText(labels: Labels, error: FieldError): string {
  const bound = error.bound === undefined ? '' : new Intl.NumberFormat(document.documentElement.lang || undefined, { maximumFractionDigits: 10 }).format(error.bound)
  return formatMessage(labels.errors[error.key], { bound })
}

// MdResultView shows a molecular dynamics result as people read one: what
// was simulated, the mean final energy and its spread, and a row for each
// replica. Anything that is not an md result shows nothing here.
function MdResultView({ job, labels }: { job: Job; labels: Labels }) {
  const summary = job.workload === 'md' ? parseMdResult(job.result) : null
  if (!summary) return null
  const text = labels.mdResult
  return <section aria-label={text.tableLabel} className="space-y-3 rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)]/40 p-4">
    <p className="text-sm text-muted-foreground">{formatMessage(text.summary, { replicas: summary.replicas, atoms: formatNumber(summary.atoms, 0), picoseconds: Number.isFinite(summary.picoseconds) ? summary.picoseconds.toLocaleString(undefined, { maximumFractionDigits: 3 }) : '—' })}</p>
    <dl className="flex flex-wrap gap-x-8 gap-y-2">
      <div><dt className="text-xs text-muted-foreground">{text.meanEnergy}</dt><dd dir="ltr" className="text-lg font-semibold tabular-nums">{formatNumber(summary.meanEnergy, 1)} <span className="text-xs font-normal text-muted-foreground">{text.unit}</span></dd></div>
      <div><dt className="text-xs text-muted-foreground">{text.spread}</dt><dd dir="ltr" className="text-lg font-semibold tabular-nums">± {formatNumber(summary.spread, 1)} <span className="text-xs font-normal text-muted-foreground">{text.unit}</span></dd></div>
    </dl>
    <div className="overflow-x-auto">
      <table className="w-full min-w-[28rem] text-sm">
        <caption className="sr-only">{text.tableLabel}</caption>
        <thead><tr className="text-start text-xs text-muted-foreground">{[text.replica, text.seed, text.ranOn, text.speed, text.energy].map((heading) => <th key={heading} scope="col" className="px-2 py-1.5 text-start font-medium">{heading}</th>)}</tr></thead>
        <tbody>{summary.runs.map((run) => <tr key={run.index} className="border-t border-[var(--app-line)]">
          <th scope="row" className="px-2 py-1.5 text-start font-medium tabular-nums">{run.index + 1}</th>
          <td className="px-2 py-1.5 tabular-nums">{formatNumber(run.seed, 0)}</td>
          <td className="px-2 py-1.5">{run.platform || '—'}</td>
          <td className="px-2 py-1.5 tabular-nums">{formatNumber(run.nsPerDay, 2)}</td>
          <td dir="ltr" className="px-2 py-1.5 text-end tabular-nums">{formatNumber(run.energy, 1)}</td>
        </tr>)}</tbody>
      </table>
    </div>
  </section>
}

function WorkerBadge({ workload, labels }: { workload: Workload; labels: Labels }) {
  if (workload.composite) return <span className="run-badge"><Workflow aria-hidden="true" className="size-3.5" />{labels.coordinatorRuns}</span>
  if (workload.workersRunningIt === 0) return <span className="run-badge run-badge--idle"><CircleAlert aria-hidden="true" className="size-3.5" />{labels.noWorker}</span>
  return <span className="run-badge run-badge--ready"><Cpu aria-hidden="true" className="size-3.5" />{formatMessage(labels.workerCount, { count: workload.workersRunningIt })}</span>
}

function FileField({ field, id, labelId, describedBy, invalid, value, info, files, labels, messages, onStored, onClear, onTouched }: {
  field: FormField; id: string; labelId: string; describedBy: string; invalid: boolean; value: string; info?: FileInfo; files: StoredFile[]; labels: Labels; messages: Messages
  onStored: (cid: string, info: FileInfo) => void; onClear: () => void; onTouched: () => void
}) {
  const input = useRef<HTMLInputElement>(null)
  const [dragging, setDragging] = useState(false)
  const [storing, setStoring] = useState('')
  const usable = files.filter((file) => !file.private)
  async function store(file?: File) {
    if (!file) return
    onTouched()
    if (file.size > maxUploadBytes) { toast.error(messages.operationText.fileTooLarge, { description: formatMessage(messages.operationText.fileSizeLimit, { size: fileUploadLimitLabel() }) }); return }
    setStoring(file.name)
    try {
      const result = await getNodeApi().storeFile({ name: file.name, data: new Uint8Array(await file.arrayBuffer()), private: false }) as unknown as StoredFile
      if (!result.cid) throw new Error(messages.fileUploadMissingCid)
      onStored(result.cid, { name: file.name, size: file.size })
    } catch (error) { toast.error(messages.operationText.storeFailed, { description: error instanceof Error ? error.message : String(error) }) }
    finally { setStoring('') }
  }
  const drop = (event: DragEvent) => { event.preventDefault(); setDragging(false); void store(event.dataTransfer.files?.[0]) }
  const dragOver = (event: DragEvent) => { event.preventDefault(); event.dataTransfer.dropEffect = 'copy'; setDragging(true) }
  const picker = <input ref={input} type="file" className="sr-only" tabIndex={-1} aria-hidden="true" onChange={(event) => { void store(event.target.files?.[0]); event.currentTarget.value = '' }} />
  const shown = info ?? (value ? { name: usable.find((file) => file.cid === value)?.name ?? value, size: Number(usable.find((file) => file.cid === value)?.sizeBytes ?? 0) } : undefined)
  return <div className="space-y-2" onDragOver={dragOver} onDragLeave={() => setDragging(false)} onDrop={drop}>
    {picker}
    {storing ? <div role="status" className="run-dropzone is-busy"><LoaderCircle aria-hidden="true" className="size-5 animate-spin motion-reduce:animate-none" /><span className="text-sm">{formatMessage(labels.storing, { name: storing })}</span></div>
      : value && shown ? <div className={`run-file ${dragging ? 'is-dragging' : ''}`}>
        <span className="settings-row__icon text-[var(--app-accent)]"><FileCheck2 aria-hidden="true" className="size-4" /></span>
        <div className="min-w-0 flex-1"><div dir="auto" className="truncate text-sm font-medium">{shown.name}</div><div className="truncate text-xs text-muted-foreground">{shown.size ? `${prettyBytes(shown.size)} · ` : ''}<code dir="ltr">{value}</code></div></div>
        <Button id={id} type="button" variant="outline" size="sm" aria-labelledby={`${labelId} ${id}-replace`} aria-describedby={describedBy} onClick={() => input.current?.click()}><span id={`${id}-replace`}>{labels.replaceFile}</span></Button>
        <Button type="button" variant="ghost" size="icon-sm" aria-label={labels.removeFile} title={labels.removeFile} onClick={() => { onTouched(); onClear() }}><X className="size-4" /></Button>
      </div>
      : <button id={id} type="button" aria-labelledby={`${labelId} ${id}-zone`} aria-describedby={describedBy} aria-invalid={invalid || undefined} className={`run-dropzone ${dragging ? 'is-dragging' : ''}`} onClick={() => input.current?.click()}>
        <span className="settings-card__icon"><FileUp aria-hidden="true" className="size-5" /></span>
        <span id={`${id}-zone`} className="text-sm font-medium">{labels.dropFile}</span>
        <span className="text-xs text-muted-foreground">{formatMessage(labels.dropFileHint, { size: fileUploadLimitLabel() })}</span>
      </button>}
    {usable.length > 0 && <select aria-label={`${field.label || field.name}: ${labels.pickStored}`} value={usable.some((file) => file.cid === value) ? value : ''} onChange={(event) => { const file = usable.find((item) => item.cid === event.target.value); onTouched(); if (file) onStored(file.cid, { name: file.name || file.cid, size: Number(file.sizeBytes) }); else onClear() }} className="run-select">
      <option value="">{labels.pickStoredPlaceholder}</option>
      {usable.map((file) => <option key={file.cid} value={file.cid}>{file.name || file.cid} · {prettyBytes(file.sizeBytes)}</option>)}
    </select>}
  </div>
}

// FilesField takes several files for one field: each is stored as it is
// dropped or chosen, or picked from those already stored, and the field
// holds their CIDs in the order they were added.
function FilesField({ field, id, labelId, describedBy, invalid, value, files, labels, messages, onChange, onTouched }: {
  field: FormField; id: string; labelId: string; describedBy: string; invalid: boolean; value: string[]; files: StoredFile[]; labels: Labels; messages: Messages
  onChange: (cids: string[]) => void; onTouched: () => void
}) {
  const input = useRef<HTMLInputElement>(null)
  // The list as it is now, not as it was when an upload began: files can be
  // removed or picked, or more dropped, while one is being stored.
  const latest = useRef(value)
  latest.current = value
  const [dragging, setDragging] = useState(false)
  const [storing, setStoring] = useState('')
  const [names, setNames] = useState<Record<string, FileInfo>>({})
  const usable = files.filter((file) => !file.private)
  async function store(list?: FileList | null) {
    if (!list || list.length === 0) return
    onTouched()
    const added: string[] = []
    for (const file of Array.from(list)) {
      if (file.size > maxUploadBytes) { toast.error(messages.operationText.fileTooLarge, { description: `${file.name}: ${messages.operationText.fileSizeLimit}` }); continue }
      setStoring(file.name)
      try {
        const result = await getNodeApi().storeFile({ name: file.name, data: new Uint8Array(await file.arrayBuffer()), private: false }) as unknown as StoredFile
        if (!result.cid) throw new Error(messages.fileUploadMissingCid)
        added.push(result.cid)
        setNames((current) => ({ ...current, [result.cid]: { name: file.name, size: file.size } }))
      } catch (error) { toast.error(messages.operationText.storeFailed, { description: error instanceof Error ? error.message : String(error) }) }
    }
    setStoring('')
    const now = latest.current
    if (added.length > 0) onChange([...now, ...added.filter((cid) => !now.includes(cid))])
  }
  const nameOf = (cid: string) => names[cid] ?? (() => { const file = usable.find((item) => item.cid === cid); return file ? { name: file.name || cid, size: Number(file.sizeBytes) } : { name: cid, size: 0 } })()
  const drop = (event: DragEvent) => { event.preventDefault(); setDragging(false); void store(event.dataTransfer.files) }
  const dragOver = (event: DragEvent) => { event.preventDefault(); event.dataTransfer.dropEffect = 'copy'; setDragging(true) }
  const choosable = usable.filter((file) => !value.includes(file.cid))
  return <div className="space-y-2" onDragOver={dragOver} onDragLeave={() => setDragging(false)} onDrop={drop}>
    <input ref={input} type="file" multiple className="sr-only" tabIndex={-1} aria-hidden="true" onChange={(event) => { void store(event.target.files); event.currentTarget.value = '' }} />
    {value.length > 0 && <ul aria-label={formatMessage(labels.filesChosen, { count: value.length })} className="space-y-1.5">
      {value.map((cid) => { const shown = nameOf(cid); return <li key={cid} className="run-file">
        <span className="settings-row__icon text-[var(--app-accent)]"><FileCheck2 aria-hidden="true" className="size-4" /></span>
        <div className="min-w-0 flex-1"><div dir="auto" className="truncate text-sm font-medium">{shown.name}</div><div className="truncate text-xs text-muted-foreground">{shown.size ? `${prettyBytes(shown.size)} · ` : ''}<code dir="ltr">{cid}</code></div></div>
        <Button type="button" variant="ghost" size="icon-sm" aria-label={`${labels.removeFile}: ${shown.name}`} title={labels.removeFile} onClick={() => { onTouched(); onChange(value.filter((item) => item !== cid)) }}><X className="size-4" /></Button>
      </li> })}
    </ul>}
    {/* The button stays while files are stored, disabled, so the field's
        label keeps pointing at it; the progress is announced beside it. */}
    <button id={id} type="button" disabled={Boolean(storing)} aria-busy={storing ? true : undefined} aria-labelledby={`${labelId} ${id}-zone`} aria-describedby={describedBy} aria-invalid={invalid || undefined} className={`run-dropzone ${dragging ? 'is-dragging' : ''} ${storing ? 'is-busy' : ''}`} onClick={() => input.current?.click()}>
      <span className="settings-card__icon">{storing ? <LoaderCircle aria-hidden="true" className="size-5 animate-spin motion-reduce:animate-none" /> : <FileUp aria-hidden="true" className="size-5" />}</span>
      <span id={`${id}-zone`} className="text-sm font-medium">{storing ? formatMessage(labels.storing, { name: storing }) : value.length > 0 ? labels.addFiles : labels.dropFiles}</span>
      {!storing && <span className="text-xs text-muted-foreground">{labels.dropFileHint}</span>}
    </button>
    <span role="status" className="sr-only">{storing ? formatMessage(labels.storing, { name: storing }) : ''}</span>
    {choosable.length > 0 && <select aria-label={`${field.label || field.name}: ${labels.pickStored}`} value="" onChange={(event) => { const cid = event.target.value; if (!cid) return; onTouched(); onChange([...value, cid]) }} className="run-select">
      <option value="">{labels.pickStoredPlaceholder}</option>
      {choosable.map((file) => <option key={file.cid} value={file.cid}>{file.name || file.cid} · {prettyBytes(file.sizeBytes)}</option>)}
    </select>}
  </div>
}

function Field({ field, workload, value, error, info, files, labels, messages, onChange, onFile, onTouched }: {
  field: FormField; workload: string; value: FormValue | undefined; error?: FieldError; info?: FileInfo; files: StoredFile[]; labels: Labels; messages: Messages
  onChange: (value: FormValue) => void; onFile: (cid: string, info: FileInfo | undefined) => void; onTouched: () => void
}) {
  const id = `run-${workload}-${field.name}`
  const helpId = `${id}-help`
  const errorId = `${id}-error`
  const describedBy = [field.help ? helpId : '', error ? errorId : ''].filter(Boolean).join(' ')
  const label = field.label || field.name
  const text = typeof value === 'string' ? value : ''
  const control = (() => {
    switch (field.kind) {
      case 'file':
        return <FileField field={field} id={id} labelId={`${id}-label`} describedBy={describedBy} invalid={Boolean(error)} value={text} info={info} files={files} labels={labels} messages={messages} onStored={(cid, stored) => onFile(cid, stored)} onClear={() => onFile('', undefined)} onTouched={onTouched} />
      case 'files':
        return <FilesField field={field} id={id} labelId={`${id}-label`} describedBy={describedBy} invalid={Boolean(error)} value={Array.isArray(value) ? value : []} files={files} labels={labels} messages={messages} onChange={onChange} onTouched={onTouched} />
      case 'integer':
      case 'number':
        return <Input id={id} type="number" inputMode={field.kind === 'integer' ? 'numeric' : 'decimal'} value={text} min={field.minimum} max={field.maximum} step={field.step || (field.kind === 'integer' ? 1 : 'any')} required={field.required} aria-describedby={describedBy || undefined} aria-invalid={Boolean(error) || undefined} onChange={(event) => { onTouched(); onChange(event.target.value) }} className="h-9 max-w-xs tabular-nums" />
      case 'choice':
        return <select id={id} value={text} required={field.required} aria-describedby={describedBy || undefined} aria-invalid={Boolean(error) || undefined} onChange={(event) => { onTouched(); onChange(event.target.value) }} className="run-select max-w-xs">
          {(!field.required || !field.choices.includes(text)) && <option value="">{labels.noChoice}</option>}
          {field.choices.map((choice) => <option key={choice} value={choice}>{choice}</option>)}
        </select>
      case 'toggle':
        return <LiquidSwitch id={id} checked={value === true} label={label} describedBy={describedBy || undefined} onChange={(checked) => { onTouched(); onChange(checked) }} />
      default:
        return <Input id={id} value={text} required={field.required} aria-describedby={describedBy || undefined} aria-invalid={Boolean(error) || undefined} onChange={(event) => { onTouched(); onChange(event.target.value) }} className="h-9" />
    }
  })()
  const heading = <>
    <div className="flex flex-wrap items-baseline gap-x-2"><label id={`${id}-label`} htmlFor={id} className="text-sm font-medium"><bdi>{label}</bdi></label>{field.required && <span className="text-xs text-muted-foreground">{labels.required}</span>}</div>
    {field.help && <p id={helpId} className="mt-0.5 text-xs leading-relaxed text-muted-foreground"><bdi>{field.help}</bdi></p>}
  </>
  return <div className={`run-field ${field.kind === 'file' || field.kind === 'files' ? 'run-field--wide' : ''}`}>
    {field.kind === 'toggle' ? <div className="flex items-start justify-between gap-4"><div className="min-w-0">{heading}</div>{control}</div> : <>{heading}<div className="run-field__control">{control}</div></>}
    {error && <p id={errorId} role="alert" className="mt-1.5 flex items-center gap-1.5 text-xs text-destructive"><CircleAlert aria-hidden="true" className="size-3.5 shrink-0" />{errorText(labels, error)}</p>}
  </div>
}

function RunStatus({ job, labels }: { job?: Job; labels: Labels }) {
  if (!job) return <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><LoaderCircle aria-hidden="true" className="size-4 animate-spin motion-reduce:animate-none" />{labels.waiting}</p>
  const state = stateName(job.state)
  if (state === 'failed') return <p role="alert" className="flex items-start gap-2 rounded-xl border border-destructive/40 p-3 text-sm text-destructive"><CircleAlert aria-hidden="true" className="mt-0.5 size-4 shrink-0" /><span>{labels.jobFailed}{job.error ? `: ${job.error}` : ''}</span></p>
  if ((job.tasks?.length ?? 0) === 0 && state !== 'succeeded' && state !== 'cancelled') return <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><LoaderCircle aria-hidden="true" className="size-4 animate-spin motion-reduce:animate-none" />{labels.waiting}</p>
  return null
}

export function RunPage({ messages, embedded = false, onAdvanced }: { messages: Messages; embedded?: boolean; onAdvanced?: () => void }) {
  const labels = messages.runPage
  const ops = messages.operationText
  const [searchParams, setSearchParams] = useSearchParams()
  const selected = searchParams.get('workload') ?? ''
  const shownJobId = searchParams.get('job') ?? ''
  const [workloads, setWorkloads] = useState<Workload[] | null>(null)
  const [gpuWorkers, setGpuWorkers] = useState<number | null>(null)
  const [gpuWorkersState, setGpuWorkersState] = useState<'loading' | 'ready' | 'unavailable'>('loading')
  const [loadError, setLoadError] = useState('')
  const [loading, setLoading] = useState(false)
  const [files, setFiles] = useState<StoredFile[]>([])
  const [drafts, setDrafts] = useState<Record<string, FormValues>>({})
  const [fileInfo, setFileInfo] = useState<Record<string, Record<string, FileInfo>>>({})
  const [review, setReview] = useState(false)
  const [stepDirection, setStepDirection] = useState<1 | -1>(1)
  const [touched, setTouched] = useState<Record<string, true>>({})
  const [submitting, setSubmitting] = useState(false)
  const [runs, setRuns] = useState<Run[]>(sessionRuns)
  const [jobs, setJobs] = useState<Record<string, Job>>({})
  const headingRef = useRef<HTMLHeadingElement>(null)
  const workloadLoadSequence = useRef(0)
  // The job shown, which a link may open though it was not started here.
  const shownRef = useRef(shownJobId)
  shownRef.current = shownJobId

  // load asks the node what it can run; a quiet load, in the background,
  // keeps what is shown until it has something new.
  const load = useCallback(async (quiet = false) => {
    const sequence = ++workloadLoadSequence.current
    if (!quiet) setLoading(true)
    try {
      const next = normalizeWorkloads(await getNodeApi().call('listWorkloads'))
      if (sequence !== workloadLoadSequence.current) return
      setWorkloads(next)
      setLoadError('')
    } catch (error) { if (!quiet && sequence === workloadLoadSequence.current) setLoadError(error instanceof Error ? error.message : String(error)) }
    finally { if (sequence === workloadLoadSequence.current) setLoading(false) }
  }, [])
  const loadFiles = useCallback(async () => {
    try {
      const result = await getNodeApi().call<{ files?: StoredFile[] }>('listFiles')
      setFiles((result.files ?? []).map((file) => ({ cid: file.cid ?? '', name: file.name || file.cid || '', sizeBytes: file.sizeBytes ?? 0, private: file.private ?? false })))
    } catch { /* The stored files are only a shortcut; a drop still works. */ }
  }, [])
  // The worker stream sends its baseline and live worker changes; while it
  // is healthy there is no need to poll for workload availability.
  useEffect(() => {
    void load()
    let active = true
    let cancel: (() => void) | undefined
    let retry: number | undefined
    let attempt = 0
    const connect = () => {
      if (!active) return
      setGpuWorkersState('loading')
      cancel = getNodeApi().stream<{ workers?: unknown[] }>('watchWorkers', {}, (result) => {
        attempt = 0
        setGpuWorkers(countGpuWorkers(result.workers))
        setGpuWorkersState('ready')
        void load(true)
      }, () => {
        cancel = undefined
        if (active) {
          setGpuWorkersState('unavailable')
          retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
        }
      }, () => {
        cancel = undefined
        if (active) {
          setGpuWorkersState('unavailable')
          retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
        }
      })
    }
    connect()
    return () => {
      active = false
      workloadLoadSequence.current++
      if (retry !== undefined) window.clearTimeout(retry)
      cancel?.()
    }
  }, [load])
  useEffect(() => { if (selected) void loadFiles() }, [selected, loadFiles])
  useEffect(() => getNodeApi().stream<{ jobs?: Job[] }>('watchJobs', {}, (result) => {
    const mine = new Set([...sessionRuns.map((run) => run.jobId), shownRef.current])
    const next: Record<string, Job> = {}
    for (const job of result.jobs ?? []) if (mine.has(job.jobId)) next[job.jobId] = job
    setJobs((current) => ({ ...current, ...next }))
  }, () => {}), [])
  // A new view is announced by moving to its heading.
  useEffect(() => { if (selected || shownJobId) headingRef.current?.focus({ preventScroll: false }) }, [selected, shownJobId])

  const workload = workloads?.find((item) => item.name === selected)
  const values = useMemo(() => workload ? drafts[workload.name] ?? initialValues(workload.fields) : {}, [drafts, workload])
  const errors = useMemo(() => workload ? validate(workload.fields, values) : {}, [values, workload])
  const valid = Object.keys(errors).length === 0
  const runnable = workload ? canRun(workload) : false
  const run = runs.find((item) => item.jobId === shownJobId)

  const setValue = (name: string, value: FormValue) => {
    if (!workload) return
    setDrafts((current) => ({ ...current, [workload.name]: { ...(current[workload.name] ?? initialValues(workload.fields)), [name]: value } }))
  }
  const setFile = (name: string, cid: string, info: FileInfo | undefined) => {
    if (!workload) return
    setValue(name, cid)
    setFileInfo((current) => {
      const forWorkload = { ...(current[workload.name] ?? {}) }
      if (info) forWorkload[name] = info
      else delete forWorkload[name]
      return { ...current, [workload.name]: forWorkload }
    })
  }
  const showGallery = () => { setStepDirection(-1); setReview(false); setTouched({}); setSearchParams({}) }
  const showForm = (name: string) => { setStepDirection(1); setReview(false); setTouched({}); setSearchParams({ workload: name }) }

  async function submit(event: FormEvent) {
    event.preventDefault()
    if (!workload || !valid || !runnable || submitting) return
    if (embedded && !review) { setStepDirection(1); setReview(true); return }
    setSubmitting(true)
    try {
      const params = toParams(workload.fields, values)
      const result = await getNodeApi().call<{ job?: Job }>('submitJob', { workload: workload.name, params: new TextEncoder().encode(JSON.stringify(params)), mode: 1 })
      const jobId = result.job?.jobId
      if (!jobId) throw new Error(ops.jobNotFound)
      const started: Run = { jobId, workload: workload.name, values, files: fileInfo[workload.name] ?? {}, startedAt: Date.now() }
      sessionRuns = [started, ...sessionRuns.filter((item) => item.jobId !== jobId)].slice(0, 8)
      setRuns(sessionRuns)
      if (result.job) setJobs((current) => ({ ...current, [jobId]: result.job! }))
      setSearchParams({ workload: workload.name, job: jobId })
    } catch (error) { toast.error(labels.submitFailed, { description: error instanceof Error ? error.message : String(error) }) }
    finally { setSubmitting(false) }
  }
  // runAgain opens a job's form with the values it was started with.
  const runAgain = (item: Run) => {
    setDrafts((current) => ({ ...current, [item.workload]: item.values }))
    setFileInfo((current) => ({ ...current, [item.workload]: item.files }))
    showForm(item.workload)
  }

  const formed = (workloads ?? []).filter((item) => item.fields.length > 0)
  const other = (workloads ?? []).filter((item) => item.fields.length === 0)
  const stateLabels: Record<string, string> = { pending: ops.pending, running: ops.running, succeeded: ops.succeeded, failed: ops.failed, cancelled: ops.cancelled }

  const recent = !embedded && runs.length > 0 && <section aria-labelledby="run-recent-heading" className="space-y-3">
    <div><h2 id="run-recent-heading" className="flex items-center gap-2 text-sm font-semibold"><History aria-hidden="true" className="size-4 text-muted-foreground" />{labels.recentJobs}</h2><p className="mt-1 text-xs text-muted-foreground">{labels.recentJobsDescription}</p></div>
    <Card className="settings-card"><CardContent className="divide-y divide-[var(--app-line)] !py-1">
      {runs.slice(0, 5).map((item) => {
        const job = jobs[item.jobId]
        const state = job ? stateName(job.state) : 'pending'
        const Icon = workloadIcons[item.workload] ?? Cpu
        const title = workloadText(labels, { name: item.workload, description: '' }).title
        return <div key={item.jobId} className="flex flex-wrap items-center gap-3 py-3">
          <span className="settings-row__icon"><Icon aria-hidden="true" className="size-4" /></span>
          <div className="min-w-0 flex-1">
            <button type="button" className="run-link truncate text-sm font-medium" onClick={() => setSearchParams({ workload: item.workload, job: item.jobId })}>{title}</button>
            <div className="truncate text-xs text-muted-foreground"><time dateTime={new Date(item.startedAt).toISOString()}>{new Date(item.startedAt).toLocaleTimeString(document.documentElement.lang || undefined, { hour: '2-digit', minute: '2-digit' })}</time> · <code dir="ltr">{item.jobId.slice(0, 12)}</code></div>
          </div>
          <Badge variant="outline">{stateLabels[state] ?? ops.unknown}</Badge>
          <Button type="button" variant="outline" size="sm" className="gap-1.5" onClick={() => runAgain(item)}><Pencil aria-hidden="true" className="size-3.5" />{labels.runAgain}</Button>
        </div>
      })}
    </CardContent></Card>
  </section>

  if (selected && shownJobId) {
    const job = jobs[shownJobId]
    const name = run?.workload ?? selected
    const known = workloads?.find((item) => item.name === name)
    const Icon = known ? workloadIcon(known) : workloadIcons[name] ?? Cpu
    const text = workloadText(labels, known ?? { name, description: '' })
    return <div className={embedded ? "run-page space-y-4" : "run-page space-y-6 pb-7"}>
      <div className="pt-5"><Button type="button" variant="ghost" className="gap-2" onClick={showGallery}><ArrowLeft aria-hidden="true" className="size-4 rtl:rotate-180" />{labels.back}</Button></div>
      <Card className="settings-card"><CardContent className="space-y-4 !p-5 sm:!p-6">
        <div className="flex items-start gap-4">
          <span className="settings-card__icon"><Icon aria-hidden="true" className="size-5" /></span>
          <div className="min-w-0 flex-1"><p className="settings-card__eyebrow">{messages.jobId}</p><h1 ref={headingRef} tabIndex={-1} className="run-heading">{text.title}</h1><code dir="ltr" className="block truncate text-xs text-muted-foreground">{shownJobId}</code></div>
        </div>
        <RunStatus job={job} labels={labels} />
        <JobTracker key={shownJobId} jobId={shownJobId} messages={messages} />
        {job && <><MdResultView job={job} labels={labels} /><JobResult key={`${shownJobId}-result`} job={job} messages={messages} open={!(job.workload === 'md' && parseMdResult(job.result))} /><JobOutputs job={job} messages={messages} /></>}
        <div className="flex flex-wrap gap-2 border-t border-[var(--app-line)] pt-4">
          {run && <Button type="button" className="gap-2" onClick={() => runAgain(run)}><Pencil aria-hidden="true" className="size-4" />{labels.changeAndRun}</Button>}
          <NavLink to={getJobHref(shownJobId)} className={buttonVariants({ variant: 'outline' })}>{labels.openInOperations}</NavLink>
        </div>
      </CardContent></Card>
      {recent}
    </div>
  }

  const stepContent = (content: ReactNode) => embedded ? <AnimatedStepper
    steps={[{ id: 'workload', label: messages.workload, icon: Boxes }, { id: 'parameters', label: messages.parametersJson, icon: Pencil }, { id: 'review', label: labels.run, icon: Play }]}
    currentIndex={selected && workload ? review ? 2 : 1 : 0}
    direction={document.documentElement.dir === 'rtl' ? 'rtl' : 'ltr'}
    navigationDirection={stepDirection}
    className="min-h-[28rem]"
  ><div className="h-full overflow-y-auto py-5">{content}</div></AnimatedStepper> : content

  if (selected && workload) {
    const Icon = workloadIcon(workload)
    const text = workloadText(labels, workload)
    const visibleErrors = Object.fromEntries(Object.entries(errors).filter(([name]) => touched[name]))
    const hint = !runnable ? labels.noWorkerHint : !valid && Object.keys(visibleErrors).length === 0 ? labels.completeForm : ''
    return stepContent(<div className={embedded ? "run-page space-y-4" : "run-page space-y-6 pb-7"}>
      <div className="pt-5"><Button type="button" variant="ghost" className="gap-2" onClick={() => { if (embedded && review) { setStepDirection(-1); setReview(false) } else showGallery() }}><ArrowLeft aria-hidden="true" className="size-4 rtl:rotate-180" />{labels.back}</Button></div>
      <Card className="settings-card">
        <div className="settings-card__header flex-wrap border-b border-[var(--app-line)]">
          <span className="settings-card__icon"><Icon aria-hidden="true" className="size-5" /></span>
          <div className="min-w-0 flex-1"><p className="settings-card__eyebrow"><bdi>{workload.name}</bdi></p><h1 ref={headingRef} tabIndex={-1} className="run-heading">{text.title}</h1><p className="mt-1 text-sm leading-relaxed text-muted-foreground">{text.description}</p></div>
          <WorkerBadge workload={workload} labels={labels} />
        </div>
        <CardContent className="!pt-5">
          <form noValidate onSubmit={(event) => void submit(event)} aria-label={text.title}>
            {review && embedded ? <pre dir="ltr" className="max-h-80 overflow-auto whitespace-pre-wrap break-words rounded-xl bg-[var(--app-wash)] p-4 text-xs leading-6">{JSON.stringify(toParams(workload.fields, values), null, 2)}</pre> : <div className="run-fields">
              {workload.fields.map((field) => <Field key={field.name} field={field} workload={workload.name} value={values[field.name]} error={touched[field.name] ? errors[field.name] : undefined} info={fileInfo[workload.name]?.[field.name]} files={files} labels={labels} messages={messages} onChange={(value) => setValue(field.name, value)} onFile={(cid, info) => setFile(field.name, cid, info)} onTouched={() => setTouched((current) => current[field.name] ? current : { ...current, [field.name]: true })} />)}
            </div>}
            <div className="mt-6 flex flex-col-reverse gap-3 border-t border-[var(--app-line)] pt-5 sm:flex-row sm:items-center sm:justify-between">
              <p id="run-submit-hint" className="text-xs leading-relaxed text-muted-foreground">{hint}</p>
              <Button type="submit" size="lg" className="h-10 gap-2 px-5" disabled={!valid || !runnable || submitting} aria-describedby={hint ? 'run-submit-hint' : undefined}>{submitting ? <LoaderCircle aria-hidden="true" className="size-4 animate-spin" /> : <Play aria-hidden="true" className="size-4" />}{submitting ? labels.starting : embedded && !review ? messages.modelSettings.continue : labels.run}</Button>
            </div>
          </form>
        </CardContent>
      </Card>
      {recent}
    </div>)
  }

  return stepContent(<div className={embedded ? "run-page space-y-4" : "run-page space-y-6 pb-7"}>
    {!embedded && <PageHeading title={messages.run} description={labels.description} actions={<div className="flex flex-wrap items-center gap-2">
      <Badge variant="outline" role="status" aria-live="polite" aria-atomic="true" className="gap-1.5 px-3 py-1.5">
        {gpuWorkersState === 'loading' ? <LoaderCircle aria-hidden="true" className="size-3.5 animate-spin motion-reduce:animate-none" /> : <Gpu aria-hidden="true" className="size-4" />}
        {gpuWorkersState === 'ready' ? formatMessage(labels.gpuWorkers, { count: new Intl.NumberFormat(document.documentElement.lang || undefined).format(gpuWorkers ?? 0) }) : gpuWorkersState === 'loading' ? labels.gpuWorkersLoading : labels.gpuWorkersUnavailable}
      </Badge>
      <Button variant="outline" className="gap-2" disabled={loading} onClick={() => void load()}><RefreshCw aria-hidden="true" className={`size-4 ${loading ? 'animate-spin motion-reduce:animate-none' : ''}`} />{messages.refresh}</Button>
    </div>} />}
    {workloads === null ? (loadError ? <EmptyState icon={CircleAlert} title={labels.loadFailed} description={loadError} action={<Button variant="outline" onClick={() => void load()}>{messages.refresh}</Button>} /> : <EmptyState icon={LoaderCircle} loading title={labels.loading} />)
      : workloads.length === 0 ? <EmptyState icon={Boxes} title={labels.noWorkloads} description={labels.noWorkloadsDescription} />
      : <>
        {formed.length > 0 && <section aria-label={labels.galleryLabel}>
          <ul className="run-gallery">
            {formed.map((item) => {
              const Icon = workloadIcon(item)
              const text = workloadText(labels, item)
              return <li key={item.name} className="min-w-0"><button type="button" className="settings-category-card run-card" onClick={() => showForm(item.name)}>
                <span className="settings-category-card__icon"><Icon aria-hidden="true" className="size-5" /></span>
                <span className="settings-category-card__copy">
                  <span className="settings-category-card__title">{text.title}</span>
                  <span className="settings-category-card__description">{text.description}</span>
                  <span className="mt-1.5 flex"><WorkerBadge workload={item} labels={labels} /></span>
                </span>
                <ChevronRight aria-hidden="true" className="settings-category-card__arrow" />
              </button></li>
            })}
          </ul>
        </section>}
        {other.length > 0 && <section aria-labelledby="run-other-heading" className="space-y-3">
          <div className="flex flex-wrap items-end justify-between gap-3">
            <div><h2 id="run-other-heading" className="text-sm font-semibold">{labels.otherWorkloads}</h2><p className="mt-1 text-xs text-muted-foreground">{labels.otherWorkloadsDescription}</p></div>
            {embedded ? <Button type="button" variant="outline" size="sm" className="gap-1.5" onClick={onAdvanced}><Boxes aria-hidden="true" className="size-3.5" />{messages.submitJob} · JSON</Button> : <NavLink to="/operations#operations-jobs" className={`${buttonVariants({ variant: 'outline', size: 'sm' })} gap-1.5`}><Boxes aria-hidden="true" className="size-3.5" />{labels.openOperations}</NavLink>}
          </div>
          <Card className="settings-card"><CardContent className="divide-y divide-[var(--app-line)] !py-1">
            {other.map((item) => { const Icon = workloadIcon(item); const text = workloadText(labels, item); return <div key={item.name} className="flex flex-wrap items-center gap-3 py-3">
              <span className="settings-row__icon"><Icon aria-hidden="true" className="size-4" /></span>
              <div className="min-w-0 flex-1"><div className="text-sm font-medium"><bdi>{text.title}</bdi></div><p className="text-xs leading-relaxed text-muted-foreground"><bdi>{text.description}</bdi></p></div>
              <WorkerBadge workload={item} labels={labels} />
            </div> })}
          </CardContent></Card>
        </section>}
      </>}
    {recent}
  </div>)
}
