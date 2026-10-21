import { useEffect, useMemo, useState, type ReactNode, type FormEvent } from 'react'
import { Check, Copy, Cpu, Globe2, HardDrive, Link2, LoaderCircle, LogOut, RefreshCw, Server, Signal, Wifi, type LucideIcon } from 'lucide-react'
import type { JoinedPool, NodeSnapshot, Peer } from '../../../preload'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Separator } from '@/components/ui/separator'
import { Input } from '@/components/ui/input'
import { formatMessage, type getMessages } from '@/i18n/messages'
import { getNodeApi, isDesktopApp } from '@/lib/node-api'
import { toast } from 'sonner'
import { Link } from 'react-router-dom'
import { EmptyState, PageHeading } from '@/components/ui/page-layout'
import { displayDaemonVersion } from '@/lib/daemon-version'
import { ResponsiveActionDialog } from '@/components/ui/responsive-action-dialog'
import { PoolHostingOverview } from '@/components/pool-hosting-overview'
import { offeredWorkloads } from '@/lib/worker-workloads'

type Messages = ReturnType<typeof getMessages>
function shortId(id: string) { return id.length > 22 ? `${id.slice(0, 12)}…${id.slice(-8)}` : id }
function peerIsConnected(peer: Peer) { return peer.connectionState === 2 || (typeof peer.connectionState === 'string' && peer.connectionState.endsWith('_CONNECTED')) }
function joinedPoolStateCode(state: JoinedPool['state']) {
  if (typeof state === 'number') return state
  return state.endsWith('_CONNECTED') ? 2 : state.endsWith('_DISCONNECTED') ? 3 : state.endsWith('_CONNECTING') ? 1 : 0
}
function hasJoinedPoolRole(pool: JoinedPool, role: 'WORKER' | 'CLIENT') {
  return pool.roles.some((value) => value === (role === 'WORKER' ? 1 : 2) || value === `POOL_ROLE_${role}`)
}
function formatBytes(value: string | number) {
  const bytes = Number(value)
  if (!Number.isFinite(bytes) || bytes <= 0) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const power = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
  return `${(bytes / 1024 ** power).toFixed(power ? 1 : 0)} ${units[power]}`
}

export function NodeDetailsPage({ snapshot, messages }: { snapshot: NodeSnapshot; messages: Messages }) {
  const [refreshing, setRefreshing] = useState(false)
  const [peerAddress, setPeerAddress] = useState('')
  const [peerBusy, setPeerBusy] = useState(false)
  const [workersState, setWorkersState] = useState<'loading' | 'ready' | 'error'>('loading')
  const [peerIdCopied, setPeerIdCopied] = useState(false)
  const [workers, setWorkers] = useState<{ peerId: string; name: string; hostname: string; os: string; arch: string; cpuCores: number; cpuModel: string; memoryBytes: string | number; taskSlots: number; runningTasks: number; workloads: string[]; models: string[]; gpus: { name: string; memoryBytes: string | number }[] }[]>([])
  const [joinedPool, setJoinedPool] = useState<JoinedPool | null>(null)
  const [joinedPoolState, setJoinedPoolState] = useState<'loading' | 'ready' | 'unavailable' | 'none'>('loading')
  const [confirmingLeave, setConfirmingLeave] = useState(false)
  const [leaving, setLeaving] = useState(false)
  const desktopApp = isDesktopApp()
  const connectedPeers = useMemo(() => snapshot.peers.filter(peerIsConnected).length, [snapshot.peers])
  const statusLabel = snapshot.status === 'connected' ? messages.connected : snapshot.status === 'connecting' ? messages.connecting : messages.offline
  useEffect(() => {
    if (snapshot.status !== 'connected') { setWorkers([]); return }
    let active = true
    let cancel: (() => void) | undefined
    let retry: number | undefined
    let attempt = 0
    setWorkersState('loading')
    const connect = () => {
      if (!active) return
      cancel = getNodeApi().stream<{ workers?: typeof workers }>('watchWorkers', {}, (result) => {
        attempt = 0
        setWorkers(result.workers ?? [])
        setWorkersState('ready')
      }, () => {
        cancel = undefined
        if (active) {
          setWorkersState('error')
          retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
        }
      }, () => {
        cancel = undefined
        if (active) retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
      })
    }
    connect()
    return () => {
      active = false
      if (retry !== undefined) window.clearTimeout(retry)
      cancel?.()
    }
  }, [snapshot.status, snapshot.endpoint])
  useEffect(() => {
    if (snapshot.status !== 'connected') { setJoinedPoolState((current) => current === 'ready' ? 'unavailable' : current); return }
    let active = true
    let cancel: (() => void) | undefined
    let retry: number | undefined
    let attempt = 0
    const connect = () => {
      if (!active) return
      cancel = getNodeApi().stream<JoinedPool>('watchJoinedPool', {}, (event) => {
        attempt = 0
        setJoinedPool(event)
        setJoinedPoolState('ready')
      }, (error, code) => {
        cancel = undefined
        if (active) {
          if (code === 9 || /has joined no other/i.test(error)) { setJoinedPool(null); setJoinedPoolState('none'); return }
          setJoinedPoolState('unavailable')
          retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
        }
      }, () => {
        cancel = undefined
        if (active) retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
      })
    }
    // Fetch the baseline before subscribing. WatchJoinedPool sends the current
    // state as its first event too, closing the gap without allowing an older
    // fetch response to overwrite a newer stream update.
    void getNodeApi().call<JoinedPool>('getJoinedPool').then((initial) => {
      if (!active) return
      setJoinedPool(initial)
      setJoinedPoolState('ready')
      connect()
    }).catch((error: unknown) => {
      if (!active) return
      // A coordinating node has joined no other pool; don't render a false
      // error card for the normal standalone-node case.
      if (/has joined no other/i.test(error instanceof Error ? error.message : String(error))) {
        setJoinedPool(null)
        setJoinedPoolState('none')
        return
      }
      setJoinedPoolState('unavailable')
      connect()
    })
    return () => { active = false; if (retry !== undefined) window.clearTimeout(retry); cancel?.() }
  }, [snapshot.status, snapshot.endpoint])
  // Leaving has the pool let go of what it kept for this node; the node then
  // forgets the pool and stops, since it was started to work for it (#509).
  async function leavePool() {
    if (leaving) return
    setLeaving(true)
    try {
      await getNodeApi().call('leavePool')
      setConfirmingLeave(false)
      toast.success(messages.nodeOverviewText.leftPool)
    } catch (error) {
      toast.error(messages.nodeOverviewText.leavePoolFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally { setLeaving(false) }
  }
  async function reconnect() {
    setRefreshing(true)
    try {
      await getNodeApi().reconnect()
      toast.success(messages.reconnectRequested)
    } catch (error) {
      toast.error(messages.reconnect, { description: error instanceof Error ? error.message : String(error) })
    } finally { setRefreshing(false) }
  }
  async function copyPeerId() {
    if (!snapshot.info?.peerId) return
    try {
      await navigator.clipboard.writeText(snapshot.info.peerId)
      setPeerIdCopied(true)
      window.setTimeout(() => setPeerIdCopied(false), 1600)
      toast.success(messages.peerIdCopied)
    } catch {
      toast.error(messages.peerIdCopyFailed)
    }
  }
  async function connectPeer(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); setPeerBusy(true)
    try {
      const peerId = await getNodeApi().connectPeer(peerAddress)
      setPeerAddress('')
      toast.success(messages.connectionRequested, { description: shortId(peerId) })
    } catch (error) {
      toast.error(messages.connectPeer, { description: error instanceof Error ? error.message : String(error) })
    }
    finally { setPeerBusy(false) }
  }
  // The two sides of trust are set separately: whether this node gives the
  // peer work, and whether it takes work from the peer.
  async function setComputePermissions(peer: Peer, givesWork: boolean, takesWork: boolean) {
    setPeerBusy(true)
    try {
      await getNodeApi().setPeerComputePermissions(peer.peerId, givesWork, takesWork)
      toast.success(messages.trustUpdated)
    } catch (error) {
      toast.error(messages.trustForCompute, { description: error instanceof Error ? error.message : String(error) })
    }
    finally { setPeerBusy(false) }
  }

  const labels = messages.nodeOverviewText
  return <div className="space-y-1 pb-7">
    <PageHeading title={messages.nodeOverview} description={messages.liveDescription} actions={desktopApp && <Button variant="outline" disabled={refreshing} onClick={() => void reconnect()} className="gap-2"><RefreshCw className={`size-4 ${refreshing ? 'animate-spin' : ''}`} />{refreshing ? messages.connecting : messages.reconnect}</Button>} />
    <section aria-label={messages.nodeOverview} className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
      <MetricCard label={messages.nodeIdentity} icon={Server} value={<span className="inline-flex min-w-0 max-w-full items-center gap-2 align-middle"><span className="min-w-0 truncate">{snapshot.info ? shortId(snapshot.info.peerId) : '—'}</span>{snapshot.info && <Button type="button" variant="ghost" size="icon" className="relative z-10 size-7 shrink-0 rounded-full border border-[var(--app-line)] bg-[var(--app-card)]/90" aria-label={peerIdCopied ? messages.peerIdCopied : messages.copyPeerId} title={peerIdCopied ? messages.peerIdCopied : messages.copyPeerId} onClick={() => void copyPeerId()}>{peerIdCopied ? <Check className="size-3.5 text-emerald-600" /> : <Copy className="size-3.5" />}</Button>}</span>} detail={snapshot.info ? `${messages.daemonVersion} ${displayDaemonVersion(snapshot.info.daemonVersion)}` : messages.waitingHandshake} mono />
      <MetricCard label={messages.connectedPeers} icon={Globe2} value={<>{connectedPeers}<span className="text-lg font-normal text-muted-foreground"> / {snapshot.peers.length}</span></>} detail={messages.activeKnownPeers} />
      <MetricCard label={messages.peerRevision} icon={Wifi} value={<span className="inline-flex items-center gap-2 text-xl"><span className={`size-2.5 rounded-full ${snapshot.status === 'connected' ? 'bg-teal-500' : snapshot.status === 'connecting' ? 'bg-amber-500' : 'bg-red-500'}`} />{statusLabel}</span>} statusTone={snapshot.status} centerValue className="sm:col-span-2 lg:col-span-1" />
    </section>
    <PoolHostingOverview messages={messages} />
    {(joinedPool || joinedPoolState === 'unavailable') && <NodeSection icon={Link2} title={labels.joinedPool} aside={joinedPool ? <Badge variant={joinedPoolState === 'unavailable' ? 'outline' : joinedPoolStateCode(joinedPool.state) === 2 ? 'secondary' : 'outline'} className={joinedPoolState === 'unavailable' ? 'text-amber-700 dark:text-amber-300' : joinedPoolStateCode(joinedPool.state) === 2 ? 'text-emerald-700 dark:text-emerald-300' : 'text-muted-foreground'}>{joinedPoolState === 'unavailable' ? labels.poolUnavailable : joinedPoolStateCode(joinedPool.state) === 2 ? labels.poolConnected : joinedPoolStateCode(joinedPool.state) === 3 ? labels.poolDisconnected : labels.poolConnecting}</Badge> : undefined}>
      {joinedPool ? <CardContent className="grid gap-5 sm:grid-cols-2">
        <Detail label={labels.poolName} value={joinedPool.coordinatorName || joinedPool.coordinatorId || labels.poolConnecting} />
        <Detail label={labels.poolAddress} code value={joinedPool.address || '—'} />
        <Detail label={labels.poolRoles} value={[hasJoinedPoolRole(joinedPool, 'WORKER') && labels.poolWorkerRole, hasJoinedPoolRole(joinedPool, 'CLIENT') && labels.poolClientRole].filter(Boolean).join(', ') || '—'} />
        {joinedPoolState === 'ready' && joinedPoolStateCode(joinedPool.state) === 2 && hasJoinedPoolRole(joinedPool, 'CLIENT') && joinedPool.workerCount !== undefined && <Detail label={labels.poolWorkers} value={String(joinedPool.workerCount)} />}
        <div className="sm:col-span-2"><Button type="button" variant="outline" className="gap-2" disabled={leaving || snapshot.status !== 'connected'} onClick={() => setConfirmingLeave(true)}><LogOut className="size-4" />{labels.leavePool}</Button></div>
      </CardContent> : <EmptyState icon={Link2} title={labels.poolUnavailable} />}
    </NodeSection>}
    <ResponsiveActionDialog open={confirmingLeave} direction={document.documentElement.dir === 'rtl' ? 'rtl' : 'ltr'} title={labels.leavePoolTitle} description={labels.leavePoolDescription} icon={<LogOut className="size-6 text-destructive" />} onOpenChange={(open) => { if (!open && !leaving) setConfirmingLeave(false) }}>
      {joinedPool && <code dir="ltr" className="block break-all rounded-xl bg-[var(--app-wash)] p-3 text-xs">{joinedPool.coordinatorName || joinedPool.coordinatorId}{joinedPool.address ? ` · ${joinedPool.address}` : ''}</code>}
      <div className="mt-5 grid gap-2 sm:grid-cols-2"><Button type="button" variant="outline" disabled={leaving} onClick={() => setConfirmingLeave(false)}>{messages.cancel}</Button><Button type="button" variant="destructive" disabled={leaving} onClick={() => void leavePool()}>{leaving ? labels.leavingPool : labels.leavePool}</Button></div>
    </ResponsiveActionDialog>
    <div className="mt-4 grid gap-4">
      <NodeSection icon={Cpu} title={labels.workers} count={workers.length} aside={<span className="text-xs text-muted-foreground">{labels.hardwareModels}</span>}>
        {workers.length === 0 ? <EmptyState icon={workersState === 'loading' ? LoaderCircle : Server} loading={workersState === 'loading'} title={workersState === 'loading' ? labels.workersLoading : workersState === 'error' ? labels.workersUnavailable : labels.noWorkers} description={workersState === 'ready' ? labels.noWorkersDescription : undefined} /> : <div className="divide-y divide-[var(--app-line)] px-5">
          {workers.map((worker) => <article key={worker.peerId} className="flex flex-wrap items-start gap-3 rounded-xl py-5 transition-colors hover:bg-[var(--app-wash)]/45">
            <div className="grid size-9 shrink-0 place-items-center rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)]"><Server className="size-4 text-muted-foreground" /></div>
            <div className="min-w-0 flex-1">
              <div className="truncate text-sm font-medium">{worker.name || worker.hostname || worker.peerId.slice(0, 14)}</div>
              <div className="mt-1 text-xs leading-5 text-muted-foreground">{worker.os} · {worker.arch} · {worker.cpuCores} {labels.cores} · {worker.cpuModel || labels.cpuUnknown}</div>
              <div className="mt-2 flex flex-wrap gap-1.5">{worker.gpus?.map((gpu, index) => <Badge key={`${worker.peerId}:gpu:${index}`} variant="secondary">{gpu.name}{Number(gpu.memoryBytes) > 0 ? ` · ${formatBytes(gpu.memoryBytes)}` : ''}</Badge>)}{worker.models?.map((model) => <Badge key={model} variant="outline">{model}</Badge>)}{offeredWorkloads(worker).map((workload) => <Badge key={workload} variant="outline" className="text-muted-foreground">{workload}</Badge>)}</div>
            </div>
            <div className="text-end text-xs leading-5 text-muted-foreground">{formatBytes(worker.memoryBytes)}<div>{worker.runningTasks}/{worker.taskSlots} {messages.operationText.tasks}</div></div>
          </article>)}
        </div>}
      </NodeSection>
      <NodeSection icon={HardDrive} title={messages.nodeDetails} aside={<Badge variant="secondary" className="gap-1.5"><span className="size-1.5 rounded-full bg-emerald-500" />{statusLabel}</Badge>}>
        {snapshot.info ? <CardContent className="grid grid-cols-2 gap-x-8 max-[600px]:grid-cols-1">
          <Detail label={messages.peerId} value={snapshot.info.peerId} code wide />
          <Detail label={messages.daemonVersion} value={snapshot.info.daemonVersion} />
          <Detail label={snapshot.info.countrySource === 'pool' ? messages.geoCountryAsSeenByPool : messages.geoCountry} value={snapshot.info.countryCode || messages.geoUnavailable}>{!snapshot.info.countryCode && isDesktopApp() ? <div className="text-xs text-foreground/85">{messages.geoUnavailable}<p className="mt-1 text-[11px] leading-relaxed text-muted-foreground">{messages.unplacedHint} <Link to="/settings/node" className="text-[var(--app-accent)] underline-offset-2 hover:underline focus-visible:outline-2 focus-visible:outline-[var(--app-accent)]">{messages.unplacedLink}</Link></p></div> : undefined}</Detail>
          <Detail label={messages.listenAddresses} wide>{snapshot.info.listenAddresses.length ? <div className="flex flex-col gap-1.5">{snapshot.info.listenAddresses.slice(0, 3).map((address) => <code key={address} dir="ltr" className="break-all text-xs">{address}</code>)}{snapshot.info.listenAddresses.length > 3 && <span className="text-xs text-muted-foreground">{formatMessage(messages.moreAddresses, { count: snapshot.info.listenAddresses.length - 3 })}</span>}</div> : <EmptyState icon={Signal} title={messages.noAddresses} />}</Detail>
        </CardContent> : <EmptyState icon={HardDrive} title={labels.noNodeDetails} description={labels.nodeDetailsPending} />}
      </NodeSection>
      <NodeSection icon={Globe2} title={messages.knownPeers} count={snapshot.peers.length} aside={<span className="flex items-center gap-1.5 text-xs text-muted-foreground"><span className="size-1.5 rounded-full bg-emerald-500" />{messages.streaming}</span>}>
        <div className="px-5 py-4">
          <form onSubmit={(event) => void connectPeer(event)} className="flex gap-2 max-[600px]:flex-col">
            <Input dir="ltr" value={peerAddress} onChange={(event) => setPeerAddress(event.target.value)} placeholder="/ip4/host/tcp/port/p2p/…" aria-label={messages.peerMultiaddress} disabled={peerBusy} />
            <Button type="submit" variant="outline" disabled={peerBusy || !peerAddress.trim()} className="gap-2"><Link2 className="size-4" />{peerBusy ? messages.connecting : messages.connectPeer}</Button>
          </form>
          <p className="mt-2 text-xs leading-5 text-muted-foreground">{messages.peerConnectionHint}</p>
        </div>
        <Separator />
        {snapshot.peers.length === 0 ? <EmptyState icon={Globe2} title={messages.noPeersYet} description={messages.onlineListening} /> : <div className="px-5">{snapshot.peers.map((peer) => <PeerRow key={peer.peerId} peer={peer} messages={messages} busy={peerBusy} onSetPermissions={(gives, takes) => void setComputePermissions(peer, gives, takes)} />)}</div>}
      </NodeSection>
    </div>
  </div>
}

function NodeSection({ icon: Icon, title, count, aside, children }: { icon: LucideIcon; title: string; count?: number; aside?: ReactNode; children: ReactNode }) {
  return <Card className="overflow-hidden rounded-2xl border-[var(--app-line)]/75 bg-[var(--app-card)]/90 shadow-sm">
    <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-3 border-b border-[var(--app-line)]/80 bg-[var(--app-wash)]/25 py-4">
    <div className="flex min-w-0 items-center gap-3"><span className="grid size-9 shrink-0 place-items-center rounded-xl border border-[var(--app-line)]/80 bg-[var(--app-card)] text-[var(--app-accent)]"><Icon className="size-[17px]" /></span><CardTitle className="text-sm font-semibold tracking-tight">{title}{count !== undefined && <span className="ms-2 inline-flex min-w-6 items-center justify-center rounded-full bg-[var(--app-wash)] px-2 py-0.5 text-[11px] font-semibold tabular-nums text-muted-foreground">{count}</span>}</CardTitle></div>{aside}
  </CardHeader>{children}</Card>
}

function MetricCard({ label, icon: Icon, value, detail, mono = false, statusTone, centerValue = false, className = '' }: { label: string; icon: LucideIcon; value: ReactNode; detail?: ReactNode; mono?: boolean; statusTone?: 'connected' | 'connecting' | 'disconnected'; centerValue?: boolean; className?: string }) {
  const tone = statusTone === 'connected' ? '54 203 163' : statusTone === 'connecting' ? '245 190 72' : statusTone === 'disconnected' ? '224 102 102' : undefined
  const gradientAngle = document.documentElement.dir === 'rtl' ? '292deg' : '68deg'
  return <Card style={tone ? { backgroundImage: `linear-gradient(${gradientAngle}, color-mix(in srgb, rgb(${tone}) 15%, var(--app-card)) 0%, color-mix(in srgb, rgb(${tone}) 7%, var(--app-card)) 48%, var(--app-card) 100%)` } : undefined} className={`relative min-h-[148px] overflow-hidden rounded-2xl border-[var(--app-line)]/75 bg-[var(--app-card)]/90 shadow-sm ${className}`}>
    <CardContent className={`relative z-[1] flex h-full flex-col justify-between gap-5 p-5 sm:p-6 ${centerValue ? 'gap-0' : ''}`}>
      <span aria-hidden="true" className="pointer-events-none absolute end-3 top-1/2 grid size-32 -translate-y-1/2 place-items-center text-[var(--app-accent)] opacity-100">
        <span className="absolute size-24 rounded-full bg-[var(--app-accent)]/[0.08] blur-2xl" />
        <Icon className={`relative size-24 ${statusTone ? 'opacity-[0.62]' : 'opacity-[0.12]'} drop-shadow-[0_0_14px_color-mix(in_srgb,var(--app-accent)_30%,transparent)]`} style={tone ? { color: `rgb(${tone})` } : undefined} strokeWidth={statusTone ? 1.55 : 1.15} />
      </span>
      <div className="relative flex items-center justify-between gap-3"><span className="text-xs font-medium text-muted-foreground">{label}</span></div>
      <div className={`${centerValue ? 'relative z-[1] grid flex-1 items-center justify-start' : 'relative z-[1] truncate'} text-[30px] font-semibold leading-none tracking-tight tabular-nums ${mono ? 'font-mono text-[16px] sm:text-[17px]' : ''}`}>{value}</div>
      {detail != null && <div className="relative z-[1] min-w-0 text-xs text-muted-foreground">{typeof detail === 'string' ? <span className="block truncate">{detail}</span> : detail}</div>}
    </CardContent>
  </Card>
}
function Detail({ label, value, code = false, wide = false, children }: { label: string; value?: string; code?: boolean; wide?: boolean; children?: ReactNode }) { return <div className={`min-w-0 border-b border-[var(--app-line)] py-3 last:border-0 ${wide ? 'col-span-2 max-[600px]:col-span-1' : ''}`}><div className="mb-1.5 text-[9px] font-semibold tracking-[0.12em] text-muted-foreground">{label}</div>{children ?? <div dir={code ? 'ltr' : undefined} className={`${code ? 'break-all font-mono text-xs' : 'text-xs'} text-foreground/85`}>{value}</div>}</div> }
function PeerRow({ peer, messages, busy, onSetPermissions }: { peer: Peer; messages: Messages; busy: boolean; onSetPermissions: (givesWork: boolean, takesWork: boolean) => void }) { const connected = peerIsConnected(peer); const gives = peer.givesWork ?? peer.trustedForCompute; const takes = peer.takesWork ?? peer.trustedForCompute; return <article className="flex min-h-[68px] flex-wrap items-center gap-3 border-b border-[var(--app-line)] py-2 last:border-0"><div className="grid size-8 shrink-0 place-items-center rounded-lg border border-[var(--app-line)] bg-[var(--app-wash)]"><span className={`size-2 rotate-45 rounded-[2px] border ${connected ? 'border-emerald-500 bg-emerald-500' : 'border-muted-foreground'}`} /></div><div className="flex min-w-0 flex-1 flex-col gap-1"><code dir="ltr" className="truncate text-xs">{peer.peerId}</code><span dir="ltr" className="truncate font-mono text-[10px] text-muted-foreground">{peer.knownAddresses[0] ?? messages.noKnownAddress}</span></div><Badge variant={connected ? 'secondary' : 'outline'} className={`gap-1.5 ${connected ? 'text-emerald-700 dark:text-emerald-300' : 'text-muted-foreground'}`}><span className={`size-1.5 rounded-full ${connected ? 'bg-emerald-500' : 'bg-muted-foreground'}`} />{connected ? messages.connected : messages.known}</Badge>{peer.worksForThisNode ? <Badge variant="secondary">{messages.worksForYou}</Badge> : null}{peer.thisNodeWorksFor ? <Badge variant="secondary">{messages.youWorkForIt}</Badge> : null}<Button size="sm" variant={gives ? 'secondary' : 'outline'} aria-pressed={gives} disabled={busy} onClick={() => onSetPermissions(!gives, takes)}>{gives ? messages.givingWork : messages.giveWork}</Button><Button size="sm" variant={takes ? 'secondary' : 'outline'} aria-pressed={takes} disabled={busy} onClick={() => onSetPermissions(gives, !takes)}>{takes ? messages.takingWork : messages.takeWork}</Button></article> }
