import { useCallback, useEffect, useRef, useState } from 'react'
import { motion } from 'motion/react'
import { ArrowRight, Fingerprint, Globe, Hash, Link2, LoaderCircle, Network, ShieldAlert, Settings2 } from 'lucide-react'
import { type getMessages } from '@/i18n/messages'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { FloatingLabelInput } from '@/components/ui/floating-label-input'
import { LiquidSwitch } from '@/components/ui/liquid-switch'
import { NetworkInterfacePicker, type NetworkInterfaceAddress } from '@/components/network-interface-picker'
import { getNodeApi } from '@/lib/node-api'
import { toast } from 'sonner'

type Messages = ReturnType<typeof getMessages>
type WebAccessConfig = { enabled: boolean; listenAddress: string; listenAddresses: string[]; publicUrl: string; certificateFingerprint: string; restartRequired: boolean }

export function RemoteAccessSettings({ messages }: { messages: Messages }) {
  const labels = messages.webAccess
  const interfaceLabels = messages.networkInterfacePicker
  const [config, setConfig] = useState<WebAccessConfig>({ enabled: false, listenAddress: '', listenAddresses: [], publicUrl: '', certificateFingerprint: '', restartRequired: false })
  const [draftEnabled, setDraftEnabled] = useState(false)
  const [interfaces, setInterfaces] = useState<NetworkInterfaceAddress[]>([])
  const [hosts, setHosts] = useState<string[]>([])
  const hostsRef = useRef<string[]>([])
  const [port, setPort] = useState('50052')
  const [publicUrl, setPublicUrl] = useState('')
  const [loading, setLoading] = useState(true)
  const [loadingInterfaces, setLoadingInterfaces] = useState(false)
  const [saving, setSaving] = useState(false)

  const setSelectedHosts = useCallback((next: string[] | ((current: string[]) => string[])) => {
    const value = typeof next === 'function' ? next(hostsRef.current) : next
    hostsRef.current = value
    setHosts(value)
  }, [])

  const loadInterfaces = useCallback(async () => {
    setLoadingInterfaces(true)
    try {
      const result = await getNodeApi().call<{ interfaces: NetworkInterfaceAddress[] }>('listNetworkInterfaces')
      setInterfaces(result.interfaces ?? [])
    } catch (error) {
      toast.error(interfaceLabels.loadFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally { setLoadingInterfaces(false) }
  }, [interfaceLabels.loadFailed])

  useEffect(() => {
    let active = true
    void getNodeApi().call<WebAccessConfig>('getWebAccessConfig').then((next) => {
      if (!active) return
      const addresses = next.listenAddresses?.length ? next.listenAddresses : (next.listenAddress ? [next.listenAddress] : [])
      const parsed = addresses.map(splitAddress)
      const nextHosts = parsed.map((item) => item.host).filter(Boolean)
      hostsRef.current = nextHosts
      setHosts(nextHosts)
      setPort(parsed[0]?.port || '50052')
      setPublicUrl(next.publicUrl ?? '')
      setDraftEnabled(next.enabled)
      setConfig(next)
    }).catch((error) => {
      if (active) toast.error(labels.configLoadFailed, { description: error instanceof Error ? error.message : String(error) })
    }).finally(() => { if (active) setLoading(false) })
    return () => { active = false }
  }, [labels.configLoadFailed])
  useEffect(() => { void loadInterfaces() }, [loadInterfaces])

  const savedAddresses = config.listenAddresses?.length ? config.listenAddresses : (config.listenAddress ? [config.listenAddress] : [])
  const savedHosts = savedAddresses.map(splitAddress).map((item) => item.host)
  const selectedAddresses = isValidPort(port) ? hosts.map((host) => formatAddress(host, port)) : []
  const unchanged = draftEnabled === config.enabled && publicUrl.trim() === config.publicUrl && selectedAddresses.length === savedAddresses.length && selectedAddresses.every((address, index) => address === savedAddresses[index])
  const invalidEnabled = draftEnabled && (!hosts.length || !isValidPort(port))

  async function save() {
    setSaving(true)
    const wasEnabled = config.enabled
    try {
      const next = await getNodeApi().call<WebAccessConfig>('setWebAccessConfig', {
        enabled: draftEnabled,
        listenAddress: selectedAddresses[0] ?? '',
        listenAddresses: selectedAddresses,
        publicUrl: publicUrl.trim(),
      })
      setConfig(next)
      setDraftEnabled(next.enabled)
      const addresses = next.listenAddresses?.length ? next.listenAddresses : (next.listenAddress ? [next.listenAddress] : [])
      const parsed = addresses.map(splitAddress)
      setSelectedHosts(parsed.map((item) => item.host).filter(Boolean))
      setPort(parsed[0]?.port || '50052')
      setPublicUrl(next.publicUrl ?? '')
      if (wasEnabled !== next.enabled) await loadInterfaces()
      toast.success(labels.saved)
    } catch (error) {
      toast.error(labels.saveFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally { setSaving(false) }
  }

  if (loading) return <Card className="grid h-56 place-items-center"><LoaderCircle aria-label={labels.configLoadFailed} className="size-5 animate-spin text-[var(--app-accent)]" /></Card>

  return <Card className="w-full min-w-0 p-2 sm:p-3">
    <Collapsible defaultOpen>
      <CollapsibleTrigger
        title={labels.exposureTitle}
        description={labels.exposureDescription}
        icon={Settings2}
        rightSlot={<span className={`inline-flex h-7 items-center rounded-full border px-2.5 text-[10px] font-semibold tracking-wide ${config.enabled ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300' : 'border-rose-500/30 bg-rose-500/10 text-rose-700 dark:text-rose-300'}`} role="status">{config.enabled ? 'ON' : 'OFF'}</span>}
        className="px-3 py-3 sm:px-4"
      />
      <CollapsibleContent className="gap-0" animateSpacing={false}>

    <div className={`mt-2 flex min-h-[72px] items-center gap-3 rounded-2xl border px-4 py-3 transition-colors duration-300 ${draftEnabled ? 'border-emerald-500/35 bg-emerald-500/[0.06]' : 'border-rose-500/35 bg-rose-500/[0.045]'}`}>
      <span className={`grid size-10 shrink-0 place-items-center rounded-xl border ${draftEnabled ? 'border-emerald-500/30 text-emerald-600 dark:text-emerald-400' : 'border-rose-500/30 text-rose-600 dark:text-rose-400'}`}><Globe className="size-5" /></span>
      <motion.span key={`${config.enabled}-${draftEnabled}`} role="status" aria-live="polite" className={`inline-flex min-h-8 items-center rounded-full border px-3 text-xs font-semibold ${draftEnabled ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300' : 'border-rose-500/30 bg-rose-500/10 text-rose-700 dark:text-rose-300'}`} initial={{ opacity: 0, y: 4 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.2 }}>
        {draftEnabled !== config.enabled ? <span className="inline-flex items-center gap-1.5" dir="ltr"><span className={`rounded-full border px-1.5 py-0.5 text-[10px] ${config.enabled ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300' : 'border-rose-500/30 bg-rose-500/10 text-rose-700 dark:text-rose-300'}`}>{config.enabled ? 'ON' : 'OFF'}</span><ArrowRight className="size-3.5" />{draftEnabled ? 'ON' : 'OFF'}<span className="max-[480px]:hidden font-normal"> · {labels.save}</span></span> : draftEnabled ? labels.enabled : labels.disabled}
      </motion.span>
      <div className="ms-auto"><LiquidSwitch checked={draftEnabled} loading={saving} label={labels.exposureTitle} onChange={setDraftEnabled} /></div>
    </div>

    <motion.div initial={false} animate={{ height: config.enabled ? 'auto' : 0, opacity: config.enabled ? 1 : 0, y: config.enabled ? 0 : -5 }} transition={{ duration: 0.28, ease: [0.22, 1, 0.36, 1] }} aria-hidden={!config.enabled} className="overflow-hidden" style={{ pointerEvents: config.enabled ? 'auto' : 'none' }}>
      {config.enabled && <div className="grid min-w-0 gap-3 px-3 pb-3 pt-4 sm:grid-cols-3 sm:px-4">
        <Summary icon={Network} label={labels.listenAddress} value={savedHosts.join(', ') || '—'} />
        <Summary icon={Hash} label={interfaceLabels.port} value={savedAddresses[0] ? splitAddress(savedAddresses[0]).port : '—'} />
        <Summary icon={Link2} label={labels.publicUrl} value={config.publicUrl || '—'} />
        {config.certificateFingerprint && <div className="col-span-full flex min-w-0 items-start gap-3 rounded-xl bg-[var(--app-wash)] px-3 py-3"><Fingerprint className="mt-0.5 size-5 shrink-0 text-[var(--app-muted)]" /><div className="min-w-0"><div className="text-xs font-semibold text-muted-foreground">{labels.certificateFingerprint}</div><code dir="ltr" className="mt-1 block break-all font-mono text-[11px] leading-relaxed">{config.certificateFingerprint.match(/.{1,2}/g)?.join(':')}</code></div></div>}
      </div>}
    </motion.div>

    <motion.div initial={false} animate={{ height: config.enabled ? 0 : 'auto', opacity: config.enabled ? 0 : 1, y: config.enabled ? -6 : 0 }} transition={{ duration: 0.3, ease: [0.22, 1, 0.36, 1] }} aria-hidden={config.enabled} className="overflow-hidden" style={{ pointerEvents: config.enabled ? 'none' : 'auto' }}>
      <div className="grid min-w-0 gap-4 px-3 pb-2 pt-5 sm:px-4">
        <div className="grid min-w-0 gap-3 lg:grid-cols-[minmax(16rem,1fr)_minmax(8rem,0.32fr)_minmax(18rem,1.3fr)] lg:items-start">
          <NetworkInterfacePicker label={labels.listenAddress} placeholder={loadingInterfaces ? interfaceLabels.loading : interfaceLabels.choose} searchPlaceholder={interfaceLabels.search} emptyLabel={interfaces.length ? interfaceLabels.noMatches : interfaceLabels.none} customLabel={interfaceLabels.current} doneLabel={interfaceLabels.done} interfaces={interfaces} value={hosts} onChange={setSelectedHosts} disabled={config.enabled} />
          <FloatingLabelInput label={interfaceLabels.port} type="number" min="1" max="65535" inputMode="numeric" dir="ltr" value={port} onChange={(event) => setPort(event.target.value)} placeholder="50052" disabled={config.enabled} />
          <FloatingLabelInput label={labels.publicUrl} icon={<Link2 className="size-[18px]" />} dir="ltr" autoCapitalize="off" spellCheck={false} value={publicUrl} onChange={(event) => setPublicUrl(event.target.value)} placeholder="https://node.example:50052" disabled={config.enabled} />
        </div>
        <div role="note" className="flex items-start gap-3 rounded-2xl border border-amber-500/30 bg-amber-500/[0.055] px-4 py-3 text-xs leading-relaxed text-amber-800 dark:text-amber-200"><span className="grid size-8 shrink-0 place-items-center rounded-xl border border-amber-600/20 bg-amber-500/10"><ShieldAlert className="size-4" /></span><span><strong className="block font-semibold">HTTPS</strong><span className="mt-0.5 block">{labels.httpsNotice}</span></span></div>
      </div>
    </motion.div>
    <motion.div initial={false} animate={{ height: unchanged ? 0 : 'auto', opacity: unchanged ? 0 : 1, y: unchanged ? -4 : 0 }} transition={{ duration: 0.2 }} aria-hidden={unchanged} className="overflow-hidden" style={{ pointerEvents: unchanged ? 'none' : 'auto' }}>
      <div className="flex justify-end border-t border-[var(--app-line)] px-3 pb-2 pt-4 sm:px-4"><Button type="button" disabled={saving || unchanged || invalidEnabled} onClick={() => void save()} className="gap-2">{saving && <LoaderCircle className="size-4 animate-spin" />}{labels.save}</Button></div>
    </motion.div>
      </CollapsibleContent>
    </Collapsible>
  </Card>
}

function Summary({ icon: Icon, label, value }: { icon: typeof Network; label: string; value: string }) {
  return <div className="flex min-w-0 items-center gap-3 rounded-xl bg-[var(--app-wash)] px-3 py-3"><span className="grid size-8 shrink-0 place-items-center rounded-lg text-[var(--app-muted)]"><Icon className="size-4" /></span><div className="min-w-0"><div className="text-[11px] text-muted-foreground">{label}</div><div dir="ltr" title={value} className="truncate text-xs font-medium">{value}</div></div></div>
}

function splitAddress(value: string) {
  const text = value.trim()
  if (text.startsWith('[')) { const end = text.indexOf(']'); if (end > 0 && text[end + 1] === ':') return { host: text.slice(1, end), port: text.slice(end + 2) } }
  const split = text.lastIndexOf(':')
  return split > 0 && !text.slice(0, split).includes(':') ? { host: text.slice(0, split), port: text.slice(split + 1) } : { host: '', port: '' }
}
function formatAddress(host: string, port: string) { return `${host.includes(':') ? `[${host}]` : host}:${port.trim()}` }
function isValidPort(value: string) { const number = Number(value); return /^\d+$/.test(value) && number >= 1 && number <= 65535 }
