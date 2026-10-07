import { useMemo, useState, type ReactNode, type FormEvent } from 'react'
import { Activity, ArrowUpRight, Cpu, Globe2, HardDrive, Link2, RefreshCw, Server, Signal, type LucideIcon } from 'lucide-react'
import type { NodeSnapshot, Peer } from '../../../preload'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Separator } from '@/components/ui/separator'
import { Input } from '@/components/ui/input'
import { formatMessage, type getMessages } from '@/i18n/messages'
import { getNodeApi, isDesktopApp } from '@/lib/node-api'
import { toast } from 'sonner'

type Messages = ReturnType<typeof getMessages>
function shortId(id: string) { return id.length < 22 ? `${id.slice(0, 12)}…${id.slice(-8)}` : id }
function peerIsConnected(peer: Peer) { return peer.connectionState === 2 || (typeof peer.connectionState === 'string' && peer.connectionState.endsWith('_CONNECTED')) }

export function NodeDetailsPage({ snapshot, messages }: { snapshot: NodeSnapshot; messages: Messages }) {
  const [refreshing, setRefreshing] = useState(false)
  const [peerAddress, setPeerAddress] = useState('')
  const [peerBusy, setPeerBusy] = useState(false)
  const desktopApp = isDesktopApp()
  const connectedPeers = useMemo(() => snapshot.peers.filter(peerIsConnected).length, [snapshot.peers])
  const statusLabel = snapshot.status === 'connected' ? messages.connected : snapshot.status === 'connecting' ? messages.connecting : messages.offline
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
  async function toggleComputeTrust(peer: Peer) {
    setPeerBusy(true)
    try {
      await getNodeApi().setPeerComputeTrust(peer.peerId, !peer.trustedForCompute)
      toast.success(peer.trustedForCompute ? messages.computeTrustRemoved : messages.trustedForCompute)
    } catch (error) {
      toast.error(messages.trustForCompute, { description: error instanceof Error ? error.message : String(error) })
    }
    finally { setPeerBusy(false) }
  }

  return <>
    <section className="flex items-end justify-between gap-4 py-8 max-[600px]:items-start max-[600px]:flex-col"><div><div className="text-[10px] font-semibold tracking-[0.14em] text-muted-foreground">{messages.machineStatus}</div><h1 className="mt-2 text-3xl font-semibold tracking-tight">{messages.nodeOverview}</h1><p className="mt-2 text-sm text-muted-foreground">{messages.liveDescription}</p></div>{desktopApp && <Button variant="outline" disabled={refreshing} onClick={() => void reconnect()} className="gap-2"><RefreshCw className={`size-4 ${refreshing ? 'animate-spin' : ''}`} />{refreshing ? messages.connecting : messages.reconnect}</Button>}</section>
    {snapshot.status !== 'connected' && <div role="status" className={`mb-4 flex items-start gap-3 rounded-xl border px-4 py-3 ${snapshot.status === 'connecting' ? 'border-amber-500/20 bg-amber-500/5' : 'border-destructive/20 bg-destructive/5'}`}><Activity className={`mt-0.5 size-4 shrink-0 ${snapshot.status === 'connecting' ? 'text-amber-600' : 'text-destructive'}`} /><div><div className="text-sm font-medium">{snapshot.status === 'connecting' ? messages.connectingDaemon : messages.daemonUnavailable}</div><p className="mt-1 text-xs leading-relaxed text-muted-foreground">{snapshot.error ?? formatMessage(messages.waitingForDaemon, { endpoint: snapshot.endpoint })}</p></div></div>}
    <section aria-label={messages.nodeOverview} className="grid grid-cols-3 gap-3 max-[900px]:grid-cols-2 max-[600px]:grid-cols-1">
      <MetricCard label={messages.nodeIdentity} icon={Server} value={snapshot.info ? shortId(snapshot.info.peerId) : '—'} detail={snapshot.info ? `${messages.daemonVersion} v${snapshot.info.daemonVersion}` : messages.waitingHandshake} mono />
      <MetricCard label={messages.connectedPeers} icon={Globe2} value={<>{connectedPeers}<span className="text-lg font-normal text-muted-foreground"> / {snapshot.peers.length}</span></>} detail={messages.activeKnownPeers} />
      <MetricCard label={messages.peerRevision} icon={Signal} value={snapshot.revision} detail={snapshot.lastUpdated ? formatMessage(messages.updated, { time: new Date(snapshot.lastUpdated).toLocaleTimeString() }) : messages.listeningSnapshot} mono className="max-[900px]:col-span-2 max-[600px]:col-span-1" />
    </section>
    <Card className="mt-4"><CardHeader className="flex min-h-16 flex-row items-center justify-between border-b border-[var(--app-line)] py-3"><div className="flex items-center gap-3"><div className="grid size-8 place-items-center rounded-lg bg-[var(--app-wash)]"><HardDrive className="size-4 text-muted-foreground" /></div><div><div className="text-[9px] font-semibold tracking-[0.14em] text-muted-foreground">{messages.daemon}</div><CardTitle className="mt-0.5 text-sm">{messages.nodeDetails}</CardTitle></div></div><Badge variant="secondary" className="gap-1.5 capitalize"><span className={`size-1.5 rounded-full ${snapshot.status === 'connected' ? 'bg-emerald-500' : snapshot.status === 'connecting' ? 'bg-amber-500' : 'bg-muted-foreground'}`} />{statusLabel}</Badge></CardHeader><CardContent className="grid grid-cols-2 gap-x-8 max-[600px]:grid-cols-1"><Detail label={messages.peerId} value={snapshot.info?.peerId ?? '—'} code wide /><Detail label={messages.daemonVersion} value={snapshot.info?.daemonVersion ?? '—'} /><Detail label={messages.geoCountry} value={snapshot.info?.countryCode || messages.geoUnavailable} /><Detail label={messages.listenAddresses} wide>{snapshot.info?.listenAddresses.length ? <div className="flex flex-col gap-1.5">{snapshot.info.listenAddresses.slice(0, 3).map((address) => <code key={address} dir="ltr" className="break-all text-xs">{address}</code>)}{snapshot.info.listenAddresses.length > 3 && <span className="text-xs text-muted-foreground">{formatMessage(messages.moreAddresses, { count: snapshot.info.listenAddresses.length - 3 })}</span>}</div> : <span className="text-xs text-muted-foreground">{messages.noAddresses}</span>}</Detail></CardContent></Card>
    <Card className="mt-4"><CardHeader className="flex min-h-16 flex-row items-center justify-between py-3"><div className="flex items-center gap-3"><div className="grid size-8 place-items-center rounded-lg bg-[var(--app-wash)]"><Cpu className="size-4 text-muted-foreground" /></div><div><div className="text-[9px] font-semibold tracking-[0.14em] text-muted-foreground">{messages.p2pNetwork}</div><CardTitle className="mt-0.5 text-sm">{messages.knownPeers} <span className="ms-1 font-mono text-xs text-muted-foreground">{snapshot.peers.length}</span></CardTitle></div></div><div className="flex items-center gap-1.5 text-xs text-muted-foreground"><span className={`size-1.5 rounded-full ${snapshot.status === 'connected' ? 'animate-pulse bg-emerald-500' : 'bg-muted-foreground'}`} />{snapshot.status === 'connected' ? messages.streaming : messages.streamPaused}</div></CardHeader><Separator /><div className="px-5 py-4"><form onSubmit={(event) => void connectPeer(event)} className="flex gap-2 max-[600px]:flex-col"><Input dir="ltr" value={peerAddress} onChange={(event) => setPeerAddress(event.target.value)} placeholder="/ip4/host/tcp/port/p2p/…" aria-label={messages.peerMultiaddress} disabled={peerBusy || snapshot.status !== 'connected'} /><Button type="submit" disabled={peerBusy || snapshot.status !== 'connected' || !peerAddress.trim()} className="gap-2"><Link2 className="size-4" />{peerBusy ? messages.connecting : messages.connectPeer}</Button></form><p className="mt-2 text-xs text-muted-foreground">{messages.peerConnectionHint}</p></div><Separator />{snapshot.peers.length === 0 ? <div className="flex min-h-32 flex-col items-center justify-center px-6 py-8 text-center"><div className="mb-3 grid size-10 place-items-center rounded-full border border-[var(--app-line)] bg-[var(--app-wash)]"><Globe2 className="size-4 text-muted-foreground" /></div><div className="text-sm font-medium">{snapshot.status === 'connected' ? messages.noPeersYet : messages.peerListHere}</div><p className="mt-1 max-w-md text-xs leading-relaxed text-muted-foreground">{snapshot.status === 'connected' ? messages.onlineListening : messages.connectDaemon}</p></div> : <div className="px-5">{snapshot.peers.map((peer) => <PeerRow key={peer.peerId} peer={peer} messages={messages} busy={peerBusy} onToggleTrust={() => void toggleComputeTrust(peer)} />)}</div>}</Card>
    {/* <footer className="flex items-center justify-between gap-4 px-1 pt-5 text-[9px] font-medium tracking-[0.1em] text-muted-foreground max-[600px]:flex-col max-[600px]:items-start"></footer> */}
  </>
}

function MetricCard({ label, icon: Icon, value, detail, mono = false, className = '' }: { label: string; icon: LucideIcon; value: ReactNode; detail: string; mono?: boolean; className?: string }) {
  return <Card className={`min-h-[132px] ${className}`}><CardContent className="flex h-full flex-col justify-between"><div className="flex items-center justify-between"><span className="text-[9px] font-semibold tracking-[0.13em] text-muted-foreground">{label}</span><Icon className="size-4 text-muted-foreground/70" /></div><div className={`mt-5 truncate text-[25px] font-medium leading-none tracking-tight ${mono ? 'font-mono text-[17px]' : ''}`}>{value}</div><div className="mt-2 truncate text-[11px] text-muted-foreground">{detail}</div></CardContent></Card>
}
function Detail({ label, value, code = false, wide = false, children }: { label: string; value?: string; code?: boolean; wide?: boolean; children?: ReactNode }) { return <div className={`min-w-0 border-b border-[var(--app-line)] py-3 last:border-0 ${wide ? 'col-span-2 max-[600px]:col-span-1' : ''}`}><div className="mb-1.5 text-[9px] font-semibold tracking-[0.12em] text-muted-foreground">{label}</div>{children ?? <div dir={code ? 'ltr' : undefined} className={`${code ? 'break-all font-mono text-xs' : 'text-xs'} text-foreground/85`}>{value}</div>}</div> }
function PeerRow({ peer, messages, busy, onToggleTrust }: { peer: Peer; messages: Messages; busy: boolean; onToggleTrust: () => void }) { const connected = peerIsConnected(peer); return <article className="flex min-h-[68px] flex-wrap items-center gap-3 border-b border-[var(--app-line)] py-2 last:border-0"><div className="grid size-8 shrink-0 place-items-center rounded-lg border border-[var(--app-line)] bg-[var(--app-wash)]"><span className={`size-2 rotate-45 rounded-[2px] border ${connected ? 'border-emerald-500 bg-emerald-500' : 'border-muted-foreground'}`} /></div><div className="flex min-w-0 flex-1 flex-col gap-1"><code dir="ltr" className="truncate text-xs">{peer.peerId}</code><span dir="ltr" className="truncate font-mono text-[10px] text-muted-foreground">{peer.knownAddresses[0] ?? messages.noKnownAddress}</span></div><Badge variant={connected ? 'secondary' : 'outline'} className={`gap-1.5 ${connected ? 'text-emerald-700 dark:text-emerald-300' : 'text-muted-foreground'}`}><span className={`size-1.5 rounded-full ${connected ? 'bg-emerald-500' : 'bg-muted-foreground'}`} />{connected ? messages.connected : messages.known}</Badge>{peer.worksForThisNode ? <Badge variant="secondary">{messages.worksForYou}</Badge> : null}{peer.thisNodeWorksFor ? <Badge variant="secondary">{messages.youWorkForIt}</Badge> : null}<Button size="sm" variant={peer.trustedForCompute ? 'secondary' : 'outline'} disabled={busy} onClick={onToggleTrust}>{peer.trustedForCompute ? messages.trustedForCompute : messages.trustForCompute}</Button></article> }
