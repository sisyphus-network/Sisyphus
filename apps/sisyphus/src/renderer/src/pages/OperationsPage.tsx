import { useEffect, useRef, useState, type FormEvent } from 'react'
import { useLocation, useSearchParams } from 'react-router-dom'
import { Boxes, ChevronDown, Play, Copy, FileUp, RefreshCw, ScrollText, Trash2, Users, X } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { toast } from 'sonner'
import { EmptyState, PageHeading } from '@/components/ui/page-layout'
import { getNodeApi } from '@/lib/node-api'
import { type getMessages } from '@/i18n/messages'
import { getSelectedJobId, includeLinkedJob } from '@/lib/job-navigation'
import { createJobSnapshotGate } from '@/lib/job-snapshot'
import { validTaskTimeout } from '@/lib/job-result'
import { JobOutputs, JobResult } from '@/components/job-result-view'
import { OperationsFileUpload } from '@/components/operations-file-upload'
import { MAX_CLIENT_FILE_UPLOAD_BYTES, fileUploadLimitLabel } from '../../../shared/file-upload-limit.ts'
import { formatMessage } from '@/i18n/messages'
import { appendJobEvent, type JobEvent } from '@/lib/job-events'
import { ResponsiveActionDialog } from '@/components/ui/responsive-action-dialog'
import { ResponsiveWorkflowDialog } from '@/components/ui/responsive-workflow-dialog'
import { useDialogValue } from '@/hooks/use-dialog-value'
import { RunPage } from '@/pages/RunPage'

type Messages = ReturnType<typeof getMessages>
type Job = { jobId: string; workload: string; mode: string | number; state: string | number; progress: number; error: string; private?: boolean; outputBlobs?: string[]; tasks: { index: number; state: string | number; workerName: string; error: string }[]; result: Uint8Array; createdAtMs: string | number }
type StoredFile = { cid: string; name: string; sizeBytes: string | number; private: boolean; storedAtMs: string | number }
type Member = { peerId: string; role: string | number; joinedAtMs: string | number }
type PoolWorker = { peerId: string; loadedModels?: { name: string; onCardPercent: number }[] }

function normalizeJobs(jobs: Job[] = []): Job[] {
  return jobs.map((job) => ({
    ...job,
    state: typeof job.state === 'string' ? job.state : ({ 1: 'JOB_STATE_PENDING', 2: 'JOB_STATE_RUNNING', 3: 'JOB_STATE_SUCCEEDED', 4: 'JOB_STATE_FAILED', 5: 'JOB_STATE_CANCELLED' }[job.state] ?? 'JOB_STATE_UNSPECIFIED'),
    mode: typeof job.mode === 'string' ? job.mode : ({ 1: 'JOB_MODE_DISTRIBUTED', 2: 'JOB_MODE_FULL_WORKER' }[job.mode] ?? 'JOB_MODE_UNSPECIFIED'),
  }))
}

function JobTimeline({ jobId, eventLabels, messages, enabled }: { jobId: string; eventLabels: Record<string, string>; messages: Messages; enabled: boolean }) {
  const [events, setEvents] = useState<JobEvent[]>([])
  const [error, setError] = useState('')
  const [ended, setEnded] = useState(false)
  const [retry, setRetry] = useState(0)
  useEffect(() => {
    if (!enabled) return
    let active = true
    setEvents([])
    setError('')
    setEnded(false)
    const stop = getNodeApi().stream<JobEvent>('watchJobEvents', { jobId, afterSeq: '0' }, (event) => {
      if (active) setEvents((current) => appendJobEvent(current, event))
    }, (message) => { if (active) setError(message) }, () => { if (active) setEnded(true) })
    return () => { active = false; stop() }
  }, [jobId, retry, enabled])
  return <div role="log" aria-label={messages.operationText.log} tabIndex={0} className="max-h-80 overflow-auto rounded-xl bg-[var(--app-wash)] p-4 focus-visible:outline-2 focus-visible:outline-[var(--app-accent)]">
    {error && <div className="mb-3 space-y-2 rounded-xl border border-destructive/20 bg-destructive/5 p-3"><p role="alert" className="break-words text-xs leading-6 text-destructive">{error}</p><Button type="button" size="sm" variant="outline" onClick={() => setRetry((current) => current + 1)}>{messages.refresh}</Button></div>}
    {!error && events.length === 0 && <p role="status" className="py-4 text-center text-xs leading-6 text-muted-foreground">{ended ? messages.operationText.noJobEvents : messages.operationText.waitingJobEvents}</p>}
    {events.map((event, index) => <div key={String(event.seq ?? index)} className="relative border-s border-[var(--app-line)] ps-5 pb-5 text-xs last:border-transparent last:pb-0">
      <span aria-hidden="true" className={`absolute -start-1 top-1.5 size-2 rounded-full ${/failed|lost|timed-out/.test(event.kind) ? 'bg-destructive' : /succeeded/.test(event.kind) ? 'bg-emerald-500' : 'bg-[var(--app-accent)]'}`} />
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 leading-5"><span className="font-medium">{eventLabels[event.kind] ?? event.kind}</span>
      {event.atMs && <time dir="ltr" className="ms-auto text-[10px] tabular-nums text-muted-foreground" dateTime={new Date(Number(event.atMs)).toISOString()}>{new Date(Number(event.atMs)).toLocaleString(document.documentElement.lang)}</time>}
      {event.taskIndex !== undefined && event.taskIndex >= 0 && <span className="rounded bg-background px-1.5 font-mono text-[10px] text-muted-foreground"><bdi>#{event.taskIndex + 1}</bdi></span>}
      {event.workerName && <bdi className="text-muted-foreground">{event.workerName}</bdi>}</div>
      {event.text && <pre dir="auto" className="mt-2 whitespace-pre-wrap break-words rounded-lg bg-background p-3 font-mono text-xs leading-6 text-muted-foreground">{event.text}</pre>}
    </div>)}
  </div>
}

export function OperationsPage({ messages }: { messages: Messages }) {
  const labels = messages.operationText
  const stateLabels: Record<string, string> = { unspecified: labels.unspecified, pending: labels.pending, running: labels.running, succeeded: labels.succeeded, failed: labels.failed, cancelled: labels.cancelled }
  const modeLabels: Record<string, string> = { unspecified: labels.unspecified, distributed: messages.distributed, full_worker: messages.fullWorker }
  const roleLabels: Record<string, string> = { unspecified: labels.unspecified, worker: labels.worker, client: labels.client }
  const stateName = (state: string | number) => stateLabels[typeof state === 'number' ? ['unspecified', 'pending', 'running', 'succeeded', 'failed', 'cancelled'][state] ?? '' : state.replace(/^JOB_STATE_/, '').toLowerCase()] ?? labels.unknown
  const modeName = (mode: string | number) => modeLabels[typeof mode === 'number' ? ['unspecified', 'distributed', 'full_worker'][mode] ?? '' : mode.replace(/^JOB_MODE_/, '').toLowerCase()] ?? labels.unknown
  const roleName = (role: string | number) => roleLabels[typeof role === 'number' ? ['unspecified', 'worker', 'client'][role] ?? '' : role.replace(/^POOL_ROLE_/, '').toLowerCase()] ?? labels.unknown
  const eventLabels: Record<string, string> = { submitted: labels.submitted, resumed: labels.resumed, 'task-started': labels.taskStarted, 'task-succeeded': labels.taskSucceeded, 'task-failed': labels.taskFailed, 'task-lost': labels.taskLost, 'task-timed-out': labels.taskTimedOut, log: labels.log, succeeded: labels.succeeded, failed: labels.failed, cancelled: labels.cancelled }
  const [searchParams, setSearchParams] = useSearchParams()
  const location = useLocation()
  const [runOpen, setRunOpen] = useState(() => !searchParams.has('job') && (searchParams.has('workload') || location.hash === '#operations-run'))
  const [expandedJobId, setExpandedJobId] = useState('')
  const [jobsOpen, setJobsOpen] = useState(true)
  const advancedFormRef = useRef<HTMLDivElement>(null)
  const [advancedOpen, setAdvancedOpen] = useState(false)
  const selectedJobId = getSelectedJobId(searchParams.toString())
  const [linkedJob, setLinkedJob] = useState<{ id: string; job?: Job; error?: string } | null>(null)
  const [linkRetry, setLinkRetry] = useState(0)
  const scrolledJobId = useRef('')
  const [jobs, setJobs] = useState<Job[]>([])
  const jobsSnapshot = useRef(createJobSnapshotGate())
  const [timelineJobId, setTimelineJobId] = useState('')
  const [files, setFiles] = useState<StoredFile[]>([])
  const [members, setMembers] = useState<Member[]>([])
  const [workers, setWorkers] = useState<PoolWorker[]>([])
  const [workload, setWorkload] = useState('primes')
  const [params, setParams] = useState('{"from":0,"to":100000}')
  const [mode, setMode] = useState('JOB_MODE_DISTRIBUTED')
  const [maxTasks, setMaxTasks] = useState(0)
  const [taskTimeout, setTaskTimeout] = useState(0)
  const [minMemory, setMinMemory] = useState(0)
  const [minGpus, setMinGpus] = useState(0)
  const [jobPrivate, setJobPrivate] = useState(false)
  const [busy, setBusy] = useState(false)
  const [privateFile, setPrivateFile] = useState(false)
  const [invitation, setInvitation] = useState('')
  const [invitationExpiry, setInvitationExpiry] = useState('')
  const [invitationTtl, setInvitationTtl] = useState(3600)
  const [inviting, setInviting] = useState(false)
  const [removeTarget, setRemoveTarget] = useState<Member | null>(null)
  const [removingMember, setRemovingMember] = useState(false)
  const { displayedValue: displayedMember, onAfterClose: afterMemberClose } = useDialogValue(removeTarget)
  const [address, setAddress] = useState('')
  const [joinToken, setJoinToken] = useState('')

  const displayedJobs = includeLinkedJob(jobs, linkedJob?.job, selectedJobId)
  const hasSelectedJob = displayedJobs.some((job) => job.jobId === selectedJobId)
  const selectedError = linkedJob?.id === selectedJobId ? linkedJob.error : undefined

  useEffect(() => {
    if (!selectedJobId) return
    setRunOpen(false)
    setExpandedJobId(selectedJobId)
    setJobsOpen(true)
    let cancelled = false
    setTimelineJobId(selectedJobId)
    setLinkedJob({ id: selectedJobId })
    void getNodeApi().call<{ job?: Job }>('getJob', { jobId: selectedJobId }).then((result) => {
      if (cancelled) return
      if (!result.job || result.job.jobId !== selectedJobId) throw new Error(labels.jobNotFound)
      setLinkedJob({ id: selectedJobId, job: result.job })
    }).catch((error: unknown) => {
      if (!cancelled) setLinkedJob({ id: selectedJobId, error: error instanceof Error ? error.message : String(error) })
    })
    return () => { cancelled = true }
  }, [selectedJobId, linkRetry, labels.jobNotFound])

  useEffect(() => {
    if (!selectedJobId) { scrolledJobId.current = ''; return }
    if (!hasSelectedJob || scrolledJobId.current === selectedJobId) return
    const frame = requestAnimationFrame(() => {
      const card = document.getElementById(`job-${selectedJobId}`)
      if (!card) return
      card.scrollIntoView({ block: 'nearest', behavior: 'instant' })
      card.focus({ preventScroll: true })
      scrolledJobId.current = selectedJobId
    })
    return () => cancelAnimationFrame(frame)
  }, [selectedJobId, hasSelectedJob])

  async function refresh() {
    const api = getNodeApi()
    const jobsSnapshotToken = jobsSnapshot.current.begin()
    const results = await Promise.allSettled([
      api.call<{ jobs?: Job[] }>('listJobs'),
      api.call<{ files?: StoredFile[] }>('listFiles'),
      api.call<{ members?: Member[] }>('listMembers'),
      api.call<{ workers?: PoolWorker[] }>('listWorkers'),
    ])
    if (results[1].status === 'fulfilled') setFiles((results[1].value.files ?? []).map((file) => ({ cid: file.cid ?? '', name: file.name || file.cid || '', sizeBytes: file.sizeBytes ?? 0, private: file.private ?? false, storedAtMs: file.storedAtMs ?? 0 })))
    if (results[2].status === 'fulfilled') setMembers(results[2].value.members ?? [])
    if (results[3].status === 'fulfilled') setWorkers(results[3].value.workers ?? [])
    if (results[0].status === 'fulfilled' && jobsSnapshot.current.accept(jobsSnapshotToken)) setJobs(normalizeJobs(results[0].value.jobs))
  }
  useEffect(() => {
    void refresh()
    const stop = getNodeApi().stream<{ jobs?: Job[] }>('watchJobs', {}, (result) => {
      jobsSnapshot.current.changed()
      setJobs(normalizeJobs(result.jobs))
    }, () => {})
    return stop
  }, [])

  async function submitJob(event: FormEvent) {
    event.preventDefault(); setBusy(true)
    try {
      const parsed = JSON.parse(params)
      if (!validTaskTimeout(taskTimeout)) throw new Error(labels.invalidTaskTimeout)
      const result = await getNodeApi().call<{ job?: Job }>('submitJob', { workload, params: new TextEncoder().encode(JSON.stringify(parsed)), mode: mode === 'JOB_MODE_FULL_WORKER' ? 2 : 1, maxTasks, taskTimeoutSeconds: taskTimeout, minMemoryBytes: minMemory, minGpus, private: jobPrivate })
      if (result.job) {
        jobsSnapshot.current.changed()
        setJobs((current) => [result.job!, ...current.filter((job) => job.jobId !== result.job!.jobId)])
        setSearchParams({ job: result.job.jobId })
        setRunOpen(false)
      }
      toast.success(labels.jobSubmitted)
    } catch (error) { toast.error(labels.submitFailed, { description: error instanceof Error ? error.message : String(error) }) }
    finally { setBusy(false) }
  }
  async function cancelJob(jobId: string) {
    try { await getNodeApi().call('cancelJob', { jobId }); await refresh() } catch (error) { toast.error(labels.cancelFailed, { description: error instanceof Error ? error.message : String(error) }) }
  }
  async function removeStoredFile(cid: string) {
    try { await getNodeApi().call('removeFile', { cid }); await refresh() } catch (error) { toast.error(labels.removeFileFailed, { description: error instanceof Error ? error.message : String(error) }) }
  }
  async function uploadFile(file?: File) {
    if (!file) return
    if (file.size > MAX_CLIENT_FILE_UPLOAD_BYTES) { const description = formatMessage(labels.fileSizeLimit, { size: fileUploadLimitLabel() }); toast.error(labels.fileTooLarge, { description }); throw new Error(description) }
    setBusy(true)
    try {
      const result = await getNodeApi().storeFile({ name: file.name, data: new Uint8Array(await file.arrayBuffer()), private: privateFile }) as unknown as StoredFile
      setFiles((current) => [result, ...current.filter((item) => item.cid !== result.cid)])
      toast.success(labels.fileStored, { description: result.cid })
    } catch (error) { toast.error(labels.storeFailed, { description: error instanceof Error ? error.message : String(error) }); throw error }
    finally { setBusy(false) }
  }
  async function downloadFile(file: StoredFile) {
    try {
      const data = await getNodeApi().fetchFile(file.cid)
      const bytes = new Uint8Array(data.byteLength); bytes.set(data)
      const url = URL.createObjectURL(new Blob([bytes.buffer]))
      const link = document.createElement('a'); link.href = url; link.download = file.name || file.cid; link.click(); URL.revokeObjectURL(url)
    } catch (error) { toast.error(labels.fetchFailed, { description: error instanceof Error ? error.message : String(error) }) }
  }
  async function createInvitation(role: string) {
    if (inviting) return
    setInviting(true)
    try { const result = await getNodeApi().call<{ invitation?: string; expiresAtMs?: string | number }>('createInvitation', { role: role === 'POOL_ROLE_CLIENT' ? 2 : 1, ttlSeconds: invitationTtl }); setInvitation(result.invitation ?? ''); setInvitationExpiry(result.expiresAtMs ? new Date(Number(result.expiresAtMs)).toLocaleString(document.documentElement.lang) : '') }
    catch (error) { toast.error(labels.inviteFailed, { description: error instanceof Error ? error.message : String(error) }) }
    finally { setInviting(false) }
  }
  async function copyInvitation() {
    try { await navigator.clipboard.writeText(invitation); toast.success(labels.invitationCopied) }
    catch (error) { toast.error(labels.copyInvitationFailed, { description: error instanceof Error ? error.message : String(error) }) }
  }
  async function removeMember() {
    if (!removeTarget || removingMember) return
    setRemovingMember(true)
    try {
      await getNodeApi().call('removeMember', { peerId: removeTarget.peerId })
      setMembers((current) => current.filter((member) => member.peerId !== removeTarget.peerId))
      setRemoveTarget(null)
    } catch (error) { toast.error(labels.removeMemberFailed, { description: error instanceof Error ? error.message : String(error) }) }
    finally { setRemovingMember(false) }
  }
  async function joinPool(event: FormEvent) {
    event.preventDefault()
    try { await getNodeApi().call('joinPool', { address, invitation: joinToken }); setAddress(''); setJoinToken(''); await refresh(); toast.success(labels.poolJoined) }
    catch (error) { toast.error(labels.joinFailed, { description: error instanceof Error ? error.message : String(error) }) }
  }

  return <div className="space-y-6 pb-8 sm:space-y-8">
    <PageHeading title={messages.computeOperations} actions={<div className="flex items-center gap-2"><Button variant="ghost" size="icon" aria-label={messages.refresh} onClick={() => void refresh()}><RefreshCw className="size-4" /></Button><Button type="button" variant="ghost" className="devices-add-button" onClick={() => { setSearchParams({}); setRunOpen(true) }}><span className="devices-add-button__icon"><Play className="size-4" /></span>{messages.run}</Button></div>} />
    <Collapsible id="operations-jobs" open={jobsOpen} onOpenChange={setJobsOpen} className="rounded-2xl border border-[color-mix(in_srgb,var(--app-line)_65%,transparent)] bg-[var(--app-card)] p-1">
      <CollapsibleTrigger title={messages.jobs} icon={Boxes} className="px-3 py-3 sm:px-4 sm:py-4" rightSlot={<span className="inline-flex min-w-7 items-center justify-center rounded-full border border-[var(--app-line)] bg-[var(--app-wash)] px-2 py-1 text-xs font-medium tabular-nums text-muted-foreground">{displayedJobs.length}</span>} />
      <CollapsibleContent animateSpacing={false}><div className="space-y-4 px-2 pb-2 pt-2 sm:px-3 sm:pb-3">
      {selectedJobId && !hasSelectedJob && <EmptyState icon={Boxes} loading={!selectedError} title={selectedError ? labels.loadJobFailed : labels.openingJob} description={selectedError} action={selectedError ? <Button variant="outline" onClick={() => setLinkRetry((value) => value + 1)}>{messages.refresh}</Button> : undefined} />}
      <div className="space-y-2">
        {displayedJobs.map((job) => {
          const expanded = expandedJobId === job.jobId
          const running = ['JOB_STATE_PENDING', 'JOB_STATE_RUNNING', 1, 2].includes(job.state)
          const progress = Math.round(Math.max(0, Math.min(100, job.progress * 100)))
          return <article key={job.jobId} id={`job-${job.jobId}`} tabIndex={-1} aria-label={`${messages.jobId}: ${job.jobId}`} className="scroll-mt-4 rounded-2xl border border-[color-mix(in_srgb,var(--app-line)_65%,transparent)] bg-[var(--app-card)] p-1 focus-visible:outline-2 focus-visible:outline-[var(--app-accent)]">
            <Collapsible open={expanded} onOpenChange={(open) => setExpandedJobId(open ? job.jobId : '')}>
            <CollapsibleTrigger aria-controls={`job-details-${job.jobId}`} className="gap-3 rounded-xl px-3 py-3 sm:px-4 sm:py-4">
              <span className="grid size-10 shrink-0 place-items-center rounded-xl bg-[var(--app-wash)] text-[var(--app-accent)]"><Boxes aria-hidden="true" className="size-[18px]" /></span>
              <span className="min-w-0 flex-1"><span className="block truncate text-sm font-medium"><bdi>{job.workload}</bdi></span><code dir="ltr" className="mt-1 block truncate text-xs text-muted-foreground">{job.jobId.slice(0, 12)}</code></span>
              {running && <span className="text-xs tabular-nums text-muted-foreground">{progress}%</span>}
              <Badge variant="outline" className="shrink-0">{stateName(job.state)}</Badge>
              <ChevronDown aria-hidden="true" className={`size-4 shrink-0 text-muted-foreground transition-transform motion-reduce:transition-none ${expanded ? 'rotate-180' : ''}`} />
            </CollapsibleTrigger>
            <CollapsibleContent animateSpacing={false}><div className="px-2 pb-2 pt-2 sm:px-3 sm:pb-3"><div id={`job-details-${job.jobId}`} className="space-y-4 rounded-xl bg-[var(--app-wash)] p-4 sm:p-5">
              <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground"><Badge variant="secondary">{modeName(job.mode)}</Badge>{job.private && <Badge variant="outline">{messages.private}</Badge>}<span>{job.tasks?.length ?? 0} · {labels.tasks}</span>{running && <Button size="sm" variant="outline" className="ms-auto" onClick={() => void cancelJob(job.jobId)}>{messages.cancel}</Button>}</div>
              <code dir="ltr" className="block break-all text-xs text-muted-foreground">{job.jobId}</code>
              {running && <div role="progressbar" aria-label={job.workload} aria-valuemin={0} aria-valuemax={100} aria-valuenow={progress} className="h-1.5 overflow-hidden rounded-full bg-background"><div className="h-full bg-[var(--app-accent)]" style={{ width: `${progress}%` }} /></div>}
              {job.error && <p role="alert" className="text-sm text-destructive">{job.error}</p>}
              <JobResult job={job} messages={messages} /><JobOutputs job={job} messages={messages} />
              <Collapsible open={timelineJobId === job.jobId} onOpenChange={(open) => setTimelineJobId(open ? job.jobId : '')} className="overflow-hidden rounded-2xl border border-[var(--app-line)] bg-[var(--app-card)]">
                <CollapsibleTrigger title={labels.log} icon={ScrollText} className="rounded-none px-3 py-3 sm:px-4" />
                <CollapsibleContent animateSpacing={false}><div className="border-t border-[var(--app-line)] p-3 sm:p-4"><JobTimeline jobId={job.jobId} eventLabels={eventLabels} messages={messages} enabled={expanded && timelineJobId === job.jobId} /></div></CollapsibleContent>
              </Collapsible>
            </div></div></CollapsibleContent>
            </Collapsible>
          </article>
        })}
        {displayedJobs.length === 0 && !selectedJobId && <EmptyState icon={Boxes} title={messages.noJobs} action={<Button variant="outline" onClick={() => { setSearchParams({}); setRunOpen(true) }}><Play className="me-2 size-4" />{messages.run}</Button>} />}
      </div>
      </div></CollapsibleContent>
    </Collapsible>
    <div className="space-y-3">
      <Collapsible id="operations-files" className="rounded-2xl border border-[color-mix(in_srgb,var(--app-line)_65%,transparent)] bg-[var(--app-card)] p-1">
        <CollapsibleTrigger title={messages.files} icon={FileUp} className="px-3 py-3 sm:px-4 sm:py-4" rightSlot={<span className="inline-flex min-w-7 items-center justify-center rounded-full border border-[var(--app-line)] bg-[var(--app-wash)] px-2 py-1 text-xs font-medium tabular-nums text-muted-foreground">{files.length}</span>} />
        <CollapsibleContent animateSpacing={false}><div className="px-2 pb-2 pt-2 sm:px-3 sm:pb-3"><OperationsFileUpload files={files} privateFile={privateFile} busy={busy} messages={messages} onPrivateChange={setPrivateFile} onUpload={uploadFile} onDownload={(file) => void downloadFile(file)} onRemove={(cid) => void removeStoredFile(cid)} /></div>
        </CollapsibleContent>
      </Collapsible>
      <Collapsible id="operations-pool" className="rounded-2xl border border-[color-mix(in_srgb,var(--app-line)_65%,transparent)] bg-[var(--app-card)] p-1">
        <CollapsibleTrigger title={messages.pool} icon={Users} className="px-3 py-3 sm:px-4 sm:py-4" rightSlot={<span className="inline-flex min-w-7 items-center justify-center rounded-full border border-[var(--app-line)] bg-[var(--app-wash)] px-2 py-1 text-xs font-medium tabular-nums text-muted-foreground">{members.length}</span>} />
        <CollapsibleContent animateSpacing={false}><div className="space-y-4 px-2 pb-2 pt-2 sm:px-3 sm:pb-3"><Card><CardHeader className="border-b border-[var(--app-line)]"><CardTitle className="flex items-center gap-2 text-sm"><Users className="size-4" />{messages.inviteNode}</CardTitle></CardHeader><CardContent><div className="flex flex-wrap items-end gap-2 pt-4"><label className="space-y-1 text-xs">{labels.invitationLifetime}<select value={invitationTtl} disabled={inviting} onChange={(event) => setInvitationTtl(Number(event.target.value))} className="block h-9 rounded-lg border border-[var(--app-line)] bg-background px-3">{[[900, 15, 'minute'], [3600, 1, 'hour'], [86400, 1, 'day'], [604800, 7, 'day']].map(([seconds, value, unit]) => <option key={seconds} value={seconds}>{new Intl.NumberFormat(document.documentElement.lang, { style: 'unit', unit: String(unit), unitDisplay: 'long' }).format(Number(value))}</option>)}</select></label><Button variant="outline" disabled={inviting} onClick={() => void createInvitation('POOL_ROLE_WORKER')}>{messages.inviteWorker}</Button><Button variant="outline" disabled={inviting} onClick={() => void createInvitation('POOL_ROLE_CLIENT')}>{messages.inviteClient}</Button></div>{invitation && <div className="mt-4 rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)] p-3"><div className="mb-2 flex items-center justify-between text-xs font-medium">{messages.singleUseInvitation}<Button size="icon" variant="ghost" className="size-7" aria-label={labels.dismissInvitation} onClick={() => { setInvitation(''); setInvitationExpiry('') }}><X className="size-3.5" /></Button></div><code dir="ltr" className="block break-all text-xs">{invitation}</code><Button type="button" variant="outline" size="sm" className="mt-2 gap-2" onClick={() => void copyInvitation()}><Copy className="size-3.5" />{labels.copyInvitation}</Button><p className="mt-2 text-[10px] text-muted-foreground">{invitationExpiry ? `${invitationExpiry}. ` : ''}{messages.shareInvitationHint}</p></div>}<form onSubmit={(event) => void joinPool(event)} className="mt-4 grid gap-2 border-t border-[var(--app-line)] pt-4 sm:grid-cols-2"><Input dir="ltr" value={address} onChange={(event) => setAddress(event.target.value)} placeholder={messages.coordinatorAddress} /><Input dir="ltr" value={joinToken} onChange={(event) => setJoinToken(event.target.value)} placeholder={messages.invitationToken} /><Button type="submit" className="sm:col-span-2" disabled={!address.trim() || !joinToken.trim()}>{messages.joinPool}</Button></form></CardContent></Card><Card><CardHeader className="border-b border-[var(--app-line)]"><CardTitle className="text-sm">{messages.poolMembers} · {members.length}</CardTitle></CardHeader><CardContent className="divide-y divide-[var(--app-line)]">{members.map((member) => { const loadedModels = workers.find((worker) => worker.peerId === member.peerId)?.loadedModels ?? []; return <div key={member.peerId} className="flex flex-wrap items-center gap-3 py-3"><div className="min-w-0 flex-1"><code dir="ltr" className="block truncate text-xs">{member.peerId}</code>{loadedModels.length > 0 && <div className="mt-1 flex flex-wrap gap-1.5" aria-label={messages.modelSettings.loadedModels}>{loadedModels.map((model) => <Badge key={model.name} variant="secondary" className="max-w-full truncate text-[10px]">{model.name}{model.onCardPercent ? ` · ${model.onCardPercent}% GPU` : ''}</Badge>)}</div>}</div><Badge variant="outline" className="capitalize">{roleName(member.role)}</Badge><Button size="sm" variant="outline" onClick={() => setRemoveTarget(member)}>{messages.remove}</Button></div> })}{members.length === 0 && <EmptyState icon={Users} title={messages.noPoolMembers} />}</CardContent></Card></div>
        </CollapsibleContent>
      </Collapsible>
    </div>
    <ResponsiveWorkflowDialog open={runOpen} direction={document.documentElement.dir === 'rtl' ? 'rtl' : 'ltr'} title={messages.run} description={messages.runPage.description} onOpenChange={setRunOpen} footer={<Button variant="outline" className="col-span-2" onClick={() => setRunOpen(false)}>{messages.cancel}</Button>}>
      <RunPage messages={messages} embedded onAdvanced={() => { setAdvancedOpen(true); advancedFormRef.current?.scrollIntoView({ block: 'nearest' }) }} />
      <div ref={advancedFormRef}><Collapsible open={advancedOpen} onOpenChange={setAdvancedOpen} className="mt-5 rounded-2xl border border-[var(--app-line)] p-1"><CollapsibleTrigger title={`${messages.submitJob} · JSON`} icon={Boxes} className="px-3 py-3" /><CollapsibleContent><Card><CardHeader className="border-b border-[var(--app-line)]"><CardTitle className="flex items-center gap-2 text-sm"><Boxes className="size-4" />{messages.submitJob}</CardTitle></CardHeader><CardContent><form onSubmit={(event) => void submitJob(event)} className="grid gap-3 pt-4 sm:grid-cols-2"><label className="space-y-1 text-xs">{messages.workload}<Input value={workload} onChange={(event) => setWorkload(event.target.value)} placeholder={labels.workloadExample} /></label><label className="space-y-1 text-xs">{messages.schedulingMode}<select value={mode} onChange={(event) => setMode(event.target.value)} className="h-9 w-full rounded-lg border border-[var(--app-line)] bg-background px-3"><option value="JOB_MODE_DISTRIBUTED">{messages.distributed}</option><option value="JOB_MODE_FULL_WORKER">{messages.fullWorker}</option></select></label><label className="space-y-1 text-xs sm:col-span-2">{messages.parametersJson}<textarea value={params} onChange={(event) => setParams(event.target.value)} className="min-h-20 w-full rounded-lg border border-[var(--app-line)] bg-background p-3 font-mono text-xs" /></label><label className="space-y-1 text-xs">{messages.taskCount}<Input type="number" min="0" value={maxTasks} onChange={(event) => setMaxTasks(Number(event.target.value))} /></label><label className="space-y-1 text-xs">{labels.taskTimeoutSeconds}<Input type="number" min="0" max="4294967295" step="1" value={taskTimeout} onChange={(event) => setTaskTimeout(Number(event.target.value))} /><span className="block text-[10px] text-muted-foreground">{labels.taskTimeoutHint}</span></label><label className="space-y-1 text-xs">{messages.minimumMemoryBytes}<Input type="number" min="0" value={minMemory} onChange={(event) => setMinMemory(Number(event.target.value))} /></label><label className="space-y-1 text-xs">{messages.minimumGpus}<Input type="number" min="0" value={minGpus} onChange={(event) => setMinGpus(Number(event.target.value))} /></label><label className="flex items-center gap-2 pt-5 text-xs"><input type="checkbox" checked={jobPrivate} onChange={(event) => setJobPrivate(event.target.checked)} />{messages.privateJob}</label><div className="sm:col-span-2"><Button type="submit" disabled={busy || !workload.trim()}>{messages.submit}</Button><p className="mt-2 text-[10px] text-muted-foreground">{messages.privateJobWarning}</p></div></form></CardContent></Card></CollapsibleContent></Collapsible></div>
    </ResponsiveWorkflowDialog>
    <ResponsiveActionDialog open={Boolean(removeTarget)} onAfterClose={afterMemberClose} direction={document.documentElement.dir === 'rtl' ? 'rtl' : 'ltr'} title={labels.removeMemberTitle} description={labels.removeMemberDescription} icon={<Trash2 className="size-6 text-destructive" />} onOpenChange={(open) => { if (!open && !removingMember) setRemoveTarget(null) }}>
      {displayedMember && <code dir="ltr" className="block break-all rounded-xl bg-[var(--app-wash)] p-3 text-xs">{displayedMember.peerId}</code>}
      <div className="mt-5 grid gap-2 sm:grid-cols-2"><Button type="button" variant="outline" disabled={removingMember} onClick={() => setRemoveTarget(null)}>{messages.cancel}</Button><Button type="button" variant="destructive" disabled={removingMember || !removeTarget} onClick={() => void removeMember()}>{removingMember ? labels.removingMember : messages.remove}</Button></div>
    </ResponsiveActionDialog>
  </div>
}
