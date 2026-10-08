import { useEffect, useRef, useState, type FormEvent } from 'react'
import { useSearchParams } from 'react-router-dom'
import { Boxes, Download, FileUp, RefreshCw, Trash2, Users, X } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { toast } from 'sonner'
import { EmptyState, PageHeading } from '@/components/ui/page-layout'
import { getNodeApi, isDesktopApp } from '@/lib/node-api'
import { type getMessages } from '@/i18n/messages'
import { getSelectedJobId, includeLinkedJob } from '@/lib/job-navigation'

type Messages = ReturnType<typeof getMessages>
type Job = { jobId: string; workload: string; mode: string | number; state: string | number; progress: number; error: string; private?: boolean; outputBlobs?: string[]; tasks: { index: number; state: string | number; workerName: string; error: string }[]; result: Uint8Array; createdAtMs: string | number }
type StoredFile = { cid: string; name: string; sizeBytes: string | number; private: boolean; storedAtMs: string | number }
type Member = { peerId: string; role: string | number; joinedAtMs: string | number }
const prettyBytes = (value: string | number) => { const bytes = Number(value); if (!bytes) return '0 B'; const unit = Math.min(3, Math.floor(Math.log(bytes) / Math.log(1024))); return `${(bytes / 1024 ** unit).toFixed(unit ? 1 : 0)} ${['B', 'KB', 'MB', 'GB'][unit]}` }

export function OperationsPage({ messages }: { messages: Messages }) {
  const labels = messages.operationText
  const stateLabels: Record<string, string> = { unspecified: labels.unspecified, pending: labels.pending, running: labels.running, succeeded: labels.succeeded, failed: labels.failed, cancelled: labels.cancelled }
  const modeLabels: Record<string, string> = { unspecified: labels.unspecified, distributed: messages.distributed, full_worker: messages.fullWorker }
  const roleLabels: Record<string, string> = { unspecified: labels.unspecified, worker: labels.worker, client: labels.client }
  const stateName = (state: string | number) => stateLabels[typeof state === 'number' ? ['unspecified', 'pending', 'running', 'succeeded', 'failed', 'cancelled'][state] ?? '' : state.replace(/^JOB_STATE_/, '').toLowerCase()] ?? labels.unknown
  const modeName = (mode: string | number) => modeLabels[typeof mode === 'number' ? ['unspecified', 'distributed', 'full_worker'][mode] ?? '' : mode.replace(/^JOB_MODE_/, '').toLowerCase()] ?? labels.unknown
  const roleName = (role: string | number) => roleLabels[typeof role === 'number' ? ['unspecified', 'worker', 'client'][role] ?? '' : role.replace(/^POOL_ROLE_/, '').toLowerCase()] ?? labels.unknown
  const eventLabels: Record<string, string> = { submitted: labels.submitted, resumed: labels.resumed, 'task-started': labels.taskStarted, 'task-succeeded': labels.taskSucceeded, 'task-failed': labels.taskFailed, 'task-lost': labels.taskLost, 'task-timed-out': labels.taskTimedOut, log: labels.log, succeeded: labels.succeeded, failed: labels.failed, cancelled: labels.cancelled }
  const desktop = isDesktopApp()
  const [searchParams] = useSearchParams()
  const selectedJobId = getSelectedJobId(searchParams.toString())
  const [linkedJob, setLinkedJob] = useState<{ id: string; job?: Job; error?: string } | null>(null)
  const [linkRetry, setLinkRetry] = useState(0)
  const scrolledJobId = useRef('')
  const [tab, setTab] = useState<'jobs' | 'files' | 'pool'>('jobs')
  const [jobs, setJobs] = useState<Job[]>([])
  const [jobEvents, setJobEvents] = useState<Record<string, { kind: string; text: string; workerName: string }[]>>({})
  const [files, setFiles] = useState<StoredFile[]>([])
  const [members, setMembers] = useState<Member[]>([])
  const [workload, setWorkload] = useState('primes')
  const [params, setParams] = useState('{"from":0,"to":100000}')
  const [mode, setMode] = useState('JOB_MODE_DISTRIBUTED')
  const [maxTasks, setMaxTasks] = useState(0)
  const [minMemory, setMinMemory] = useState(0)
  const [minGpus, setMinGpus] = useState(0)
  const [jobPrivate, setJobPrivate] = useState(false)
  const [busy, setBusy] = useState(false)
  const [privateFile, setPrivateFile] = useState(false)
  const [invitation, setInvitation] = useState('')
  const [invitationExpiry, setInvitationExpiry] = useState('')
  const [address, setAddress] = useState('')
  const [joinToken, setJoinToken] = useState('')

  const displayedJobs = includeLinkedJob(jobs, linkedJob?.job, selectedJobId)
  const hasSelectedJob = displayedJobs.some((job) => job.jobId === selectedJobId)
  const selectedError = linkedJob?.id === selectedJobId ? linkedJob.error : undefined

  useEffect(() => {
    if (!selectedJobId || !desktop) return
    let cancelled = false
    setTab('jobs')
    setLinkedJob({ id: selectedJobId })
    void getNodeApi().call<{ job?: Job }>('getJob', { jobId: selectedJobId }).then((result) => {
      if (cancelled) return
      if (!result.job || result.job.jobId !== selectedJobId) throw new Error(labels.jobNotFound)
      setLinkedJob({ id: selectedJobId, job: result.job })
    }).catch((error: unknown) => {
      if (!cancelled) setLinkedJob({ id: selectedJobId, error: error instanceof Error ? error.message : String(error) })
    })
    return () => { cancelled = true }
  }, [selectedJobId, desktop, linkRetry, labels.jobNotFound])

  useEffect(() => {
    if (!selectedJobId) { scrolledJobId.current = ''; return }
    if (tab !== 'jobs' || !hasSelectedJob || scrolledJobId.current === selectedJobId) return
    const frame = requestAnimationFrame(() => {
      const card = document.getElementById(`job-${selectedJobId}`)
      if (!card) return
      card.scrollIntoView({ block: 'nearest', behavior: 'instant' })
      card.focus({ preventScroll: true })
      scrolledJobId.current = selectedJobId
    })
    return () => cancelAnimationFrame(frame)
  }, [selectedJobId, hasSelectedJob, tab])

  async function refresh() {
    const api = getNodeApi()
    const results = await Promise.allSettled([
      api.call<{ jobs?: Job[] }>('listJobs'),
      api.call<{ files?: StoredFile[] }>('listFiles'),
      api.call<{ members?: Member[] }>('listMembers'),
    ])
    if (results[1].status === 'fulfilled') setFiles((results[1].value.files ?? []).map((file) => ({ cid: file.cid ?? '', name: file.name || file.cid || '', sizeBytes: file.sizeBytes ?? 0, private: file.private ?? false, storedAtMs: file.storedAtMs ?? 0 })))
    if (results[2].status === 'fulfilled') setMembers(results[2].value.members ?? [])
    if (results[0].status === 'fulfilled') setJobs((results[0].value.jobs ?? []).map((job) => ({ ...job, state: typeof job.state === 'string' ? job.state : ({ 1: 'JOB_STATE_PENDING', 2: 'JOB_STATE_RUNNING', 3: 'JOB_STATE_SUCCEEDED', 4: 'JOB_STATE_FAILED', 5: 'JOB_STATE_CANCELLED' }[job.state] ?? 'JOB_STATE_UNSPECIFIED'), mode: typeof job.mode === 'string' ? job.mode : ({ 1: 'JOB_MODE_DISTRIBUTED', 2: 'JOB_MODE_FULL_WORKER' }[job.mode] ?? 'JOB_MODE_UNSPECIFIED') })))
  }
  useEffect(() => {
    void refresh()
    if (!desktop) return
    const stop = getNodeApi().stream<{ jobs?: Job[] }>('watchJobs', {}, (result) => setJobs((result.jobs ?? []).map((job) => ({ ...job, state: typeof job.state === 'string' ? job.state : ({ 1: 'JOB_STATE_PENDING', 2: 'JOB_STATE_RUNNING', 3: 'JOB_STATE_SUCCEEDED', 4: 'JOB_STATE_FAILED', 5: 'JOB_STATE_CANCELLED' }[job.state] ?? 'JOB_STATE_UNSPECIFIED'), mode: typeof job.mode === 'string' ? job.mode : ({ 1: 'JOB_MODE_DISTRIBUTED', 2: 'JOB_MODE_FULL_WORKER' }[job.mode] ?? 'JOB_MODE_UNSPECIFIED') }))), () => {})
    return stop
  }, [])

  async function submitJob(event: FormEvent) {
    event.preventDefault(); setBusy(true)
    try {
      const parsed = JSON.parse(params)
      const result = await getNodeApi().call<{ job?: Job }>('submitJob', { workload, params: new TextEncoder().encode(JSON.stringify(parsed)), mode: mode === 'JOB_MODE_FULL_WORKER' ? 2 : 1, maxTasks, minMemoryBytes: minMemory, minGpus, private: jobPrivate })
      if (result.job) setJobs((current) => [result.job!, ...current.filter((job) => job.jobId !== result.job!.jobId)])
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
  const liveJobIds = displayedJobs.filter((job) => ['JOB_STATE_PENDING', 'JOB_STATE_RUNNING', 1, 2].includes(job.state)).map((job) => job.jobId).join(',')
  useEffect(() => {
    const ids = liveJobIds ? liveJobIds.split(',') : []
    const stops = ids.map((jobId) => getNodeApi().stream<{ kind: string; text: string; workerName: string }>('watchJobEvents', { jobId }, (event) => setJobEvents((current) => ({ ...current, [jobId]: [...(current[jobId] ?? []).slice(-19), event] })), () => {}))
    return () => stops.forEach((stop) => stop())
  }, [liveJobIds])
  async function uploadFile(file?: File) {
    if (!file) return
    if (file.size > 256 * 1024 * 1024) { toast.error(labels.fileTooLarge, { description: labels.fileSizeLimit }); return }
    setBusy(true)
    try {
      const result = await getNodeApi().storeFile({ name: file.name, data: new Uint8Array(await file.arrayBuffer()), private: privateFile }) as unknown as StoredFile
      setFiles((current) => [result, ...current.filter((item) => item.cid !== result.cid)])
      toast.success(labels.fileStored, { description: result.cid })
    } catch (error) { toast.error(labels.storeFailed, { description: error instanceof Error ? error.message : String(error) }) }
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
  async function downloadOutput(cid: string, jobId: string, index: number) {
    try {
      const data = await getNodeApi().fetchFile(cid)
      const bytes = new Uint8Array(data.byteLength); bytes.set(data)
      const url = URL.createObjectURL(new Blob([bytes.buffer]))
      const link = document.createElement('a'); link.href = url; link.download = `${jobId}-output-${index + 1}`; link.click(); URL.revokeObjectURL(url)
    } catch (error) { toast.error(labels.fetchOutputFailed, { description: error instanceof Error ? error.message : String(error) }) }
  }
  async function createInvitation(role: string) {
    try { const result = await getNodeApi().call<{ invitation?: string; expiresAtMs?: string | number }>('createInvitation', { role: role === 'POOL_ROLE_CLIENT' ? 2 : 1, ttlSeconds: 3600 }); setInvitation(result.invitation ?? ''); if (result.expiresAtMs) setInvitationExpiry(new Date(Number(result.expiresAtMs)).toLocaleString(document.documentElement.lang)) }
    catch (error) { toast.error(labels.inviteFailed, { description: error instanceof Error ? error.message : String(error) }) }
  }
  async function joinPool(event: FormEvent) {
    event.preventDefault()
    try { await getNodeApi().call('joinPool', { address, invitation: joinToken }); setAddress(''); setJoinToken(''); await refresh(); toast.success(labels.poolJoined) }
    catch (error) { toast.error(labels.joinFailed, { description: error instanceof Error ? error.message : String(error) }) }
  }

  return <div className="space-y-5 pb-7">
    <PageHeading title={messages.computeOperations} description={messages.operationsDescription} actions={<Button variant="outline" onClick={() => void refresh()} className="gap-2"><RefreshCw className="size-4" />{messages.refresh}</Button>} />
    {!desktop && <p role="status" className="rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)] px-4 py-3 text-sm text-muted-foreground">{messages.browserOperationsReadOnly}</p>}
    {desktop && tab === 'jobs' && selectedJobId && !hasSelectedJob && <EmptyState icon={Boxes} loading={!selectedError} title={selectedError ? labels.loadJobFailed : labels.openingJob} description={selectedError} action={selectedError ? <Button variant="outline" onClick={() => setLinkRetry((value) => value + 1)}>{messages.refresh}</Button> : undefined} />}
    <div role="tablist" className="flex gap-2 border-b border-[var(--app-line)]">{(['jobs', 'files', 'pool'] as const).map((item) => <button key={item} role="tab" aria-selected={tab === item} onClick={() => setTab(item)} className={`border-b-2 px-3 py-2 text-sm ${tab === item ? 'border-[var(--app-accent)] font-medium' : 'border-transparent text-muted-foreground'}`}>{messages[item]}</button>)}</div>
    {tab === 'jobs' && <div className="space-y-4"><Card><CardHeader className="border-b border-[var(--app-line)]"><CardTitle className="flex items-center gap-2 text-sm"><Boxes className="size-4" />{messages.submitJob}</CardTitle></CardHeader><CardContent><form onSubmit={(event) => void submitJob(event)} className="grid gap-3 pt-4 sm:grid-cols-2"><label className="space-y-1 text-xs">{messages.workload}<Input value={workload} onChange={(event) => setWorkload(event.target.value)} placeholder={labels.workloadExample} /></label><label className="space-y-1 text-xs">{messages.schedulingMode}<select value={mode} onChange={(event) => setMode(event.target.value)} className="h-9 w-full rounded-lg border border-[var(--app-line)] bg-background px-3"><option value="JOB_MODE_DISTRIBUTED">{messages.distributed}</option><option value="JOB_MODE_FULL_WORKER">{messages.fullWorker}</option></select></label><label className="space-y-1 text-xs sm:col-span-2">{messages.parametersJson}<textarea value={params} onChange={(event) => setParams(event.target.value)} className="min-h-20 w-full rounded-lg border border-[var(--app-line)] bg-background p-3 font-mono text-xs" /></label><label className="space-y-1 text-xs">{messages.taskCount}<Input type="number" min="0" value={maxTasks} onChange={(event) => setMaxTasks(Number(event.target.value))} /></label><label className="space-y-1 text-xs">{messages.minimumMemoryBytes}<Input type="number" min="0" value={minMemory} onChange={(event) => setMinMemory(Number(event.target.value))} /></label><label className="space-y-1 text-xs">{messages.minimumGpus}<Input type="number" min="0" value={minGpus} onChange={(event) => setMinGpus(Number(event.target.value))} /></label><label className="flex items-center gap-2 pt-5 text-xs"><input type="checkbox" checked={jobPrivate} onChange={(event) => setJobPrivate(event.target.checked)} />{messages.privateJob}</label><div className="sm:col-span-2"><Button type="submit" disabled={busy || !workload.trim()}>{messages.submit}</Button><p className="mt-2 text-[10px] text-muted-foreground">{messages.privateJobWarning}</p></div></form></CardContent></Card><div className="space-y-3">{displayedJobs.map((job) => <Card key={job.jobId} id={`job-${job.jobId}`} tabIndex={-1} aria-label={`${messages.jobId}: ${job.jobId}`} className={job.jobId === selectedJobId ? 'scroll-mt-4 ring-1 ring-[var(--app-accent)] focus-visible:outline-2 focus-visible:outline-[var(--app-accent)]' : undefined}><CardContent className="flex flex-wrap items-start gap-3 py-4"><div className="min-w-0 flex-1"><div className="flex flex-wrap items-center gap-2"><span className="font-medium">{job.workload}</span><Badge variant="outline" className="capitalize">{stateName(job.state)}</Badge><Badge variant="secondary">{modeName(job.mode)}</Badge>{job.private && <Badge variant="outline">{messages.private}</Badge>}</div><code className="mt-1 block truncate text-[10px] text-muted-foreground">{job.jobId}</code><div className="mt-3 h-1.5 overflow-hidden rounded-full bg-[var(--app-wash)]"><div className="h-full bg-[var(--app-accent)]" style={{ width: `${Math.max(0, Math.min(100, job.progress * 100))}%` }} /></div><div className="mt-2 text-[10px] text-muted-foreground">{job.tasks?.length ?? 0} · {labels.tasks}{job.error ? ` · ${job.error}` : ''}</div>{jobEvents[job.jobId]?.length ? <div className="mt-3 max-h-28 space-y-1 overflow-auto rounded-lg bg-[var(--app-wash)] p-2">{jobEvents[job.jobId].map((item, index) => <p key={index} className="text-[10px] text-muted-foreground"><span className="font-medium text-foreground">{eventLabels[item.kind] ?? labels.unknown}</span>{item.workerName ? ` · ${item.workerName}` : ''}{item.text ? ` · ${item.text}` : ''}</p>)}</div> : null}{job.outputBlobs?.length ? <div className="mt-2 flex flex-wrap gap-2">{job.outputBlobs.map((cid, index) => <Button key={cid} type="button" size="sm" variant="outline" onClick={() => void downloadOutput(cid, job.jobId, index)}><Download className="me-1 size-3" />{messages.output} {index + 1}</Button>)}</div> : null}</div>{['JOB_STATE_PENDING', 'JOB_STATE_RUNNING', 1, 2].includes(job.state) && <Button size="sm" variant="outline" onClick={() => void cancelJob(job.jobId)}>{messages.cancel}</Button>}</CardContent></Card>)}{displayedJobs.length === 0 && !selectedJobId && <EmptyState icon={Boxes} title={messages.noJobs} />}</div></div>}
    {tab === 'files' && <div className="space-y-4"><Card><CardHeader className="border-b border-[var(--app-line)]"><CardTitle className="flex items-center gap-2 text-sm"><FileUp className="size-4" />{messages.storeFile}</CardTitle></CardHeader><CardContent><div className="flex flex-wrap items-center gap-3 pt-4"><label className="flex items-center gap-2 text-xs"><input type="checkbox" checked={privateFile} onChange={(event) => setPrivateFile(event.target.checked)} />{messages.privateFile}</label><label className="cursor-pointer"><input className="sr-only" type="file" disabled={busy} onChange={(event) => { void uploadFile(event.target.files?.[0]); event.currentTarget.value = '' }} /><span className="inline-flex h-8 items-center gap-2 rounded-lg bg-[var(--app-accent)] px-3 text-xs text-[var(--app-accent-ink)]"><FileUp className="size-4" />{messages.chooseFile}</span></label></div></CardContent></Card><Card><CardContent className="divide-y divide-[var(--app-line)]">{files.map((file) => <div key={file.cid} className="flex flex-wrap items-center gap-3 py-3"><div className="min-w-0 flex-1"><div className="truncate text-sm font-medium">{file.name || file.cid}</div><code className="block truncate text-[10px] text-muted-foreground">{file.cid}</code></div><span className="text-xs text-muted-foreground">{prettyBytes(file.sizeBytes)}</span>{file.private && <Badge variant="secondary">{messages.private}</Badge>}<Button size="icon" variant="ghost" aria-label={`${labels.download} ${file.name}`} onClick={() => void downloadFile(file)}><Download className="size-4" /></Button><Button size="icon" variant="ghost" aria-label={`${messages.remove} ${file.name}`} onClick={() => void removeStoredFile(file.cid)}><Trash2 className="size-4" /></Button></div>)}{files.length === 0 && <EmptyState icon={FileUp} title={messages.noFiles} />}</CardContent></Card></div>}
    {tab === 'pool' && <div className="space-y-4"><Card><CardHeader className="border-b border-[var(--app-line)]"><CardTitle className="flex items-center gap-2 text-sm"><Users className="size-4" />{messages.inviteNode}</CardTitle></CardHeader><CardContent><div className="flex flex-wrap gap-2 pt-4"><Button variant="outline" onClick={() => void createInvitation('POOL_ROLE_WORKER')}>{messages.inviteWorker}</Button><Button variant="outline" onClick={() => void createInvitation('POOL_ROLE_CLIENT')}>{messages.inviteClient}</Button></div>{invitation && <div className="mt-4 rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)] p-3"><div className="mb-2 flex items-center justify-between text-xs font-medium">{messages.singleUseInvitation}<Button size="icon" variant="ghost" className="size-7" aria-label={labels.dismissInvitation} onClick={() => { setInvitation(''); setInvitationExpiry('') }}><X className="size-3.5" /></Button></div><code dir="ltr" className="block break-all text-xs">{invitation}</code><p className="mt-2 text-[10px] text-muted-foreground">{invitationExpiry ? `${invitationExpiry}. ` : ''}{messages.shareInvitationHint}</p></div>}<form onSubmit={(event) => void joinPool(event)} className="mt-4 grid gap-2 border-t border-[var(--app-line)] pt-4 sm:grid-cols-2"><Input dir="ltr" value={address} onChange={(event) => setAddress(event.target.value)} placeholder={messages.coordinatorAddress} /><Input dir="ltr" value={joinToken} onChange={(event) => setJoinToken(event.target.value)} placeholder={messages.invitationToken} /><Button className="sm:col-span-2" disabled={!address.trim() || !joinToken.trim()}>{messages.joinPool}</Button></form></CardContent></Card><Card><CardHeader className="border-b border-[var(--app-line)]"><CardTitle className="text-sm">{messages.poolMembers} · {members.length}</CardTitle></CardHeader><CardContent className="divide-y divide-[var(--app-line)]">{members.map((member) => <div key={member.peerId} className="flex flex-wrap items-center gap-3 py-3"><code dir="ltr" className="min-w-0 flex-1 truncate text-xs">{member.peerId}</code><Badge variant="outline" className="capitalize">{roleName(member.role)}</Badge><Button size="sm" variant="outline" onClick={async () => { try { await getNodeApi().call('removeMember', { peerId: member.peerId }); await refresh() } catch (error) { toast.error(labels.removeMemberFailed, { description: error instanceof Error ? error.message : String(error) }) } }}>{messages.remove}</Button></div>)}{members.length === 0 && <EmptyState icon={Users} title={messages.noPoolMembers} />}</CardContent></Card></div>}
  </div>
}
