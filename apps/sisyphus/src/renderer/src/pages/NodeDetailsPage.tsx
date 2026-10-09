import { useEffect, useMemo, useState, type ReactNode, type FormEvent } from 'react'
import { Cpu, Globe2, HardDrive, Link2, LoaderCircle, RefreshCw, Server, Signal, type LucideIcon } from 'lucide-react'
import type { NodeSnapshot, Peer } from '../../../preload'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Separator } from '@/components/ui/separator'
import { Input } from '@/components/ui/input'
import { formatMessage, type getMessages } from '@/i18n/messages'
import { getNodeApi, isDesktopApp } from '@/lib/node-api'
import { toast } from 'sonner'
import { EmptyState, PageHeading } from '@/components/ui/page-layout'

type Messages = ReturnType<typeof getMessages>
function shortId(id: string) { return id.length > 22 ? `${id.slice(0, 12)}…${id.slice(-8)}` : id }
function peerIsConnected(peer: Peer) { return peer.connectionState === 2 || (typeof peer.connectionState === 'string' && peer.connectionState.endsWith('_CONNECTED')) }
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
  const [workers, setWorkers] = useState<{ peerId: string; name: string; hostname: string; os: string; arch: string; cpuCores: number; cpuModel: string; memoryBytes: string | number; taskSlots: number; runningTasks: number; workloads: string[]; models: string[]; gpus: { name: string; memoryBytes: string | number }[] }[]>([])
  const desktopApp = isDesktopApp()
  const connectedPeers = useMemo(() => snapshot.peers.filter(peerIsConnected).length, [snapshot.peers])
  const statusLabel = snapshot.status === 'connected' ? messages.connected : snapshot.status === 'connecting' ? messages.connecting : messages.offline
  useEffect(() => {
    if (snapshot.status !== 'connected') { setWorkers([]); return }
    let active = true
    void getNodeApi().call<{ workers?: typeof workers }>('listWorkers').then((result) => { if (active) { setWorkers(result.workers ?? []); setWorkersState('ready') } }).catch(() => { if (active) { setWorkers([]); setWorkersState('error') } })
    return () => { active = false }
  }, [snapshot.status, snapshot.revision])
  async function reconnect() {
    setRefreshing(true)
    try {
      await getNodeApi().reconnect()
      toast.success(messages.reconnectRequested)
    } catch (error) {
      toast.error(messages.reconnect, { description: error instanceof Error ? error.message : String(error) })
    } finally { setRefreshing(false) }
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
  return <div className="pb-7">
    <PageHeading title={messages.nodeOverview} description={messages.liveDescription} actions={desktopApp && <Button variant="outline" disabled={refreshing} onClick={() => void reconnect()} className="gap-2"><RefreshCw className={`size-4 ${refreshing ? 'animate-spin' : ''}`} />{refreshing ? messages.connecting : messages.reconnect}</Button>} />
    <section aria-label={messages.nodeOverview} className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
      <MetricCard label={messages.nodeIdentity} icon={Server} value={snapshot.info ? shortId(snapshot.info.peerId) : '—'} detail={snapshot.info ? `${messages.daemonVersion} v${snapshot.info.daemonVersion}` : messages.waitingHandshake} mono />
      <MetricCard label={messages.connectedPeers} icon={Globe2} value={<>{connectedPeers}<span className="text-lg font-normal text-muted-foreground"> / {snapshot.peers.length}</span></>} detail={messages.activeKnownPeers} />
      <MetricCard label={messages.peerRevision} icon={Signal} value={snapshot.revision} detail={snapshot.lastUpdated ? formatMessage(messages.updated, { time: new Date(snapshot.lastUpdated).toLocaleTimeString(document.documentElement.lang) }) : messages.listeningSnapshot} mono className="sm:col-span-2 lg:col-span-1" />
    </section>
    <div className="mt-4 grid gap-4">
      <NodeSection icon={Cpu} title={labels.workers} count={workers.length} aside={<span className="text-xs text-muted-foreground">{labels.hardwareModels}</span>}>
        {workers.length === 0 ? <EmptyState icon={workersState === 'loading' ? LoaderCircle : Server} loading={workersState === 'loading'} title={workersState === 'loading' ? labels.workersLoading : workersState === 'error' ? labels.workersUnavailable : labels.noWorkers} description={workersState === 'ready' ? labels.noWorkersDescription : undefined} /> : <div className="divide-y divide-[var(--app-line)] px-5">
          {workers.map((worker) => <article key={worker.peerId} className="flex flex-wrap items-start gap-3 py-5">
            <div className="grid size-9 shrink-0 place-items-center rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)]"><Server className="size-4 text-muted-foreground" /></div>
            <div className="min-w-0 flex-1">
              <div className="truncate text-sm font-medium">{worker.name || worker.hostname || worker.peerId.slice(0, 14)}</div>
              <div className="mt-1 text-xs leading-5 text-muted-foreground">{worker.os} · {worker.arch} · {worker.cpuCores} {labels.cores} · {worker.cpuModel || labels.cpuUnknown}</div>
              <div className="mt-2 flex flex-wrap gap-1.5">{worker.gpus?.map((gpu) => <Badge key={gpu.name} variant="secondary">{gpu.name}{Number(gpu.memoryBytes) > 0 ? ` · ${formatBytes(gpu.memoryBytes)}` : ''}</Badge>)}{worker.models?.map((model) => <Badge key={model} variant="outline">{model}</Badge>)}{worker.workloads?.map((workload) => <Badge key={workload} variant="outline" className="text-muted-foreground">{workload}</Badge>)}</div>
            </div>
            <div className="text-end text-xs leading-5 text-muted-foreground">{formatBytes(worker.memoryBytes)}<div>{worker.runningTasks}/{worker.taskSlots} {messages.operationText.tasks}</div></div>
          </article>)}
        </div>}
      </NodeSection>
      <NodeSection icon={HardDrive} title={messages.nodeDetails} aside={<Badge variant="secondary" className="gap-1.5"><span className="size-1.5 rounded-full bg-emerald-500" />{statusLabel}</Badge>}>
        {snapshot.info ? <CardContent className="grid grid-cols-2 gap-x-8 max-[600px]:grid-cols-1">
          <Detail label={messages.peerId} value={snapshot.info.peerId} code wide />
          <Detail label={messages.daemonVersion} value={snapshot.info.daemonVersion} />
          <Detail label={messages.geoCountry} value={snapshot.info.countryCode || messages.geoUnavailable} />
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
  return <Card><CardHeader className="flex flex-row flex-wrap items-center justify-between gap-3 border-b border-[var(--app-line)] py-4">
    <div className="flex min-w-0 items-center gap-3"><span className="grid size-8 shrink-0 place-items-center rounded-xl bg-[var(--app-wash)]"><Icon className="size-4 text-muted-foreground" /></span><CardTitle className="text-sm font-medium">{title}{count !== undefined && <span className="ms-2 text-xs font-normal tabular-nums text-muted-foreground">{count}</span>}</CardTitle></div>{aside}
  </CardHeader>{children}</Card>
}

function MetricCard({ label, icon: Icon, value, detail, mono = false, className = '' }: { label: string; icon: LucideIcon; value: ReactNode; detail: string; mono?: boolean; className?: string }) {
  return <Card className={`min-h-[132px] ${className}`}><CardContent className="flex h-full flex-col justify-between"><div className="flex items-center justify-between"><span className="text-[9px] font-semibold tracking-[0.13em] text-muted-foreground">{label}</span><Icon className="size-4 text-muted-foreground/70" /></div><div className={`mt-5 truncate text-[25px] font-medium leading-none tracking-tight ${mono ? 'font-mono text-[17px]' : ''}`}>{value}</div><div className="mt-2 truncate text-[11px] text-muted-foreground">{detail}</div></CardContent></Card>
}
function Detail({ label, value, code = false, wide = false, children }: { label: string; value?: string; code?: boolean; wide?: boolean; children?: ReactNode }) { return <div className={`min-w-0 border-b border-[var(--app-line)] py-3 last:border-0 ${wide ? 'col-span-2 max-[600px]:col-span-1' : ''}`}><div className="mb-1.5 text-[9px] font-semibold tracking-[0.12em] text-muted-foreground">{label}</div>{children ?? <div dir={code ? 'ltr' : undefined} className={`${code ? 'break-all font-mono text-xs' : 'text-xs'} text-foreground/85`}>{value}</div>}</div> }
function PeerRow({ peer, messages, busy, onSetPermissions }: { peer: Peer; messages: Messages; busy: boolean; onSetPermissions: (givesWork: boolean, takesWork: boolean) => void }) { const connected = peerIsConnected(peer); const gives = peer.givesWork ?? peer.trustedForCompute; const takes = peer.takesWork ?? peer.trustedForCompute; return <article className="flex min-h-[68px] flex-wrap items-center gap-3 border-b border-[var(--app-line)] py-2 last:border-0"><div className="grid size-8 shrink-0 place-items-center rounded-lg border border-[var(--app-line)] bg-[var(--app-wash)]"><span className={`size-2 rotate-45 rounded-[2px] border ${connected ? 'border-emerald-500 bg-emerald-500' : 'border-muted-foreground'}`} /></div><div className="flex min-w-0 flex-1 flex-col gap-1"><code dir="ltr" className="truncate text-xs">{peer.peerId}</code><span dir="ltr" className="truncate font-mono text-[10px] text-muted-foreground">{peer.knownAddresses[0] ?? messages.noKnownAddress}</span></div><Badge variant={connected ? 'secondary' : 'outline'} className={`gap-1.5 ${connected ? 'text-emerald-700 dark:text-emerald-300' : 'text-muted-foreground'}`}><span className={`size-1.5 rounded-full ${connected ? 'bg-emerald-500' : 'bg-muted-foreground'}`} />{connected ? messages.connected : messages.known}</Badge>{peer.worksForThisNode ? <Badge variant="secondary">{messages.worksForYou}</Badge> : null}{peer.thisNodeWorksFor ? <Badge variant="secondary">{messages.youWorkForIt}</Badge> : null}<Button size="sm" variant={gives ? 'secondary' : 'outline'} aria-pressed={gives} disabled={busy} onClick={() => onSetPermissions(!gives, takes)}>{gives ? messages.givingWork : messages.giveWork}</Button><Button size="sm" variant={takes ? 'secondary' : 'outline'} aria-pressed={takes} disabled={busy} onClick={() => onSetPermissions(gives, !takes)}>{takes ? messages.takingWork : messages.takeWork}</Button></article> }
