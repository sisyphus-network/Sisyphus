import { useCallback, useEffect, useRef, useState, type Dispatch, type SetStateAction } from 'react'
import { motion, useReducedMotion } from 'motion/react'
import { ArrowRight, ArrowUpRight, Cable, Check, Copy, EthernetPort, Fingerprint, Globe, Globe2, Link2, LoaderCircle, Monitor, Plus, QrCode, Settings2, ShieldAlert, ShieldCheck, Smartphone, X } from 'lucide-react'
import { QRCodeSVG } from 'qrcode.react'
import { type getMessages } from '@/i18n/messages'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { PageHeading } from '@/components/ui/page-layout'
import { FloatingLabelInput } from '@/components/ui/floating-label-input'
import { LiquidSwitch } from '@/components/ui/liquid-switch'
import { ResponsiveActionDialog } from '@/components/ui/responsive-action-dialog'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { useDialogValue } from '@/hooks/use-dialog-value'
import { NetworkInterfacePicker, type NetworkInterfaceAddress } from '@/components/network-interface-picker'
import { getNodeApi } from '@/lib/node-api'
import { sharedStateDomainCode } from '@/lib/shared-state-domain'
import { accessDraftIsDirty } from '@/lib/access-draft'
import { SharedStateDomain } from '@sisyphus/client'
import { formatRelativeDate } from '@/lib/relative-date'
import { toast } from 'sonner'
import type { PairedBrowser } from '@sisyphus/client'

type Messages = ReturnType<typeof getMessages>
type WebAccessConfig = { enabled: boolean; listenAddress: string; listenAddresses: string[]; publicUrl: string; certificateFingerprint: string; restartRequired: boolean }
type SharedWebAccessEvent = {
  domain: number | string
  error?: string
  snapshot?: boolean
  state?: { case?: string; value?: WebAccessConfig } | string
  webAccess?: WebAccessConfig
}

export function AccessPage({ messages, showHeading = true }: { messages: Messages; showHeading?: boolean }) {
  const [devices, setDevices] = useState<PairedBrowser[]>([])
  const [revokeTarget, setRevokeTarget] = useState<PairedBrowser | null>(null)
  const [revokingDevice, setRevokingDevice] = useState(false)
  const [invitation, setInvitation] = useState<{ code: string; expiresAt: string; pairUrl: string } | null>(null)
  const [pairingOpen, setPairingOpen] = useState(false)
  const [codeCopied, setCodeCopied] = useState(false)
  const pairingBaseline = useRef<Set<string>>(new Set())
  const pairingBaselineReady = useRef(false)
  const [busy, setBusy] = useState(false)
  const [savingAccess, setSavingAccess] = useState(false)
  const [accessSettingsOpen, setAccessSettingsOpen] = useState(true)
  const accessSettingsInitialized = useRef(false)
  const [initialAccessLoaded, setInitialAccessLoaded] = useState(false)
  const [initialDevicesLoaded, setInitialDevicesLoaded] = useState(false)
  const [initialInterfacesLoaded, setInitialInterfacesLoaded] = useState(false)
  const [accessConfig, setAccessConfig] = useState<WebAccessConfig>({ enabled: false, listenAddress: '', listenAddresses: [], publicUrl: '', certificateFingerprint: '', restartRequired: false })
  const accessConfigRef = useRef(accessConfig)
  const accessConfigLoadedRef = useRef(false)
  const accessDraftDirtyRef = useRef(false)
  const [draftEnabled, setDraftEnabled] = useState(false)
  const [networkInterfaces, setNetworkInterfaces] = useState<NetworkInterfaceAddress[]>([])
  const [loadingInterfaces, setLoadingInterfaces] = useState(false)
  const [listenHosts, setListenHosts] = useState<string[]>([])
  const listenHostsRef = useRef<string[]>([])
  const [listenPort, setListenPort] = useState('50052')
  const [publicUrl, setPublicUrl] = useState('')
  const [customBrowserAddress, setCustomBrowserAddress] = useState(false)
  const [now, setNow] = useState(Date.now())
  accessConfigRef.current = accessConfig
  const savedDraftAddresses = accessConfig.listenAddresses?.length ? accessConfig.listenAddresses : (accessConfig.listenAddress ? [accessConfig.listenAddress] : [])
  const currentDraftAddresses = isValidPort(listenPort) ? listenHosts.map((host) => formatListenAddress(host, listenPort)) : []
  const savedCustomUrl = accessConfig.publicUrl.trim() && normalizeBrowserAddress(accessConfig.publicUrl) !== normalizeBrowserAddress(deriveBrowserAddress(savedDraftAddresses)) ? accessConfig.publicUrl : ''
  accessDraftDirtyRef.current = accessDraftIsDirty(
    { enabled: draftEnabled, addresses: currentDraftAddresses, publicUrl: normalizeBrowserAddress(customBrowserAddress ? publicUrl.trim() : ''), validPort: isValidPort(listenPort) },
    { enabled: accessConfig.enabled, addresses: savedDraftAddresses, publicUrl: normalizeBrowserAddress(savedCustomUrl) },
  )
  const reducedMotion = useReducedMotion()
  const access = messages.browserAccess
  const accessUi = messages.accessUi
  const labels = messages.webAccess
  const interfaceLabels = messages.networkInterfacePicker
  const { displayedValue: displayedRevokeTarget, onAfterClose: afterRevokeDialogClose } = useDialogValue(revokeTarget)

  const updateListenHosts = useCallback<Dispatch<SetStateAction<string[]>>>((update) => {
    accessDraftDirtyRef.current = true
    const next = typeof update === 'function' ? update(listenHostsRef.current) : update
    listenHostsRef.current = next
    setListenHosts(next)
  }, [])

  const replaceListenHosts = useCallback((addresses: string[]) => {
    const hosts = addresses.map((address) => splitListenAddress(address).host).filter(Boolean)
    listenHostsRef.current = hosts
    setListenHosts(hosts)
  }, [])

  const syncAccessConfig = useCallback((config: WebAccessConfig) => {
    accessConfigRef.current = config
    setAccessConfig(config)
    const savedAddresses = config.listenAddresses?.length ? config.listenAddresses : (config.listenAddress ? [config.listenAddress] : [])
    if (!accessSettingsInitialized.current) {
      setAccessSettingsOpen(!(config.enabled || savedAddresses.length > 0 || config.publicUrl.trim().length > 0))
      accessSettingsInitialized.current = true
    }
    setDraftEnabled(config.enabled)
    const parsedAddresses = savedAddresses.map(splitListenAddress)
    replaceListenHosts(savedAddresses)
    setListenPort(parsedAddresses[0]?.port || '50052')
    setPublicUrl(config.publicUrl)
    const derivedAddress = deriveBrowserAddress(savedAddresses)
    setCustomBrowserAddress(Boolean(config.publicUrl.trim() && normalizeBrowserAddress(config.publicUrl) !== normalizeBrowserAddress(derivedAddress)))
    accessDraftDirtyRef.current = false
  }, [replaceListenHosts])

  const receiveAccessConfig = useCallback((config: WebAccessConfig) => {
    if (!accessConfigLoadedRef.current || !accessDraftDirtyRef.current) syncAccessConfig(config)
    else {
      accessConfigRef.current = config
      setAccessConfig(config)
    }
    accessConfigLoadedRef.current = true
    setInitialAccessLoaded(true)
  }, [syncAccessConfig])

  useEffect(() => {
    let active = true
    const api = getNodeApi()
    let cancel: (() => void) | undefined
    let retry: number | undefined
    let attempt = 0
    let fallbackRequested = false
    const loadFallback = () => {
      if (!active || fallbackRequested) return
      fallbackRequested = true
      void api.call<WebAccessConfig>('getWebAccessConfig').then(receiveAccessConfig).catch((error) => {
        fallbackRequested = false
        if (active) toast.error(labels.configLoadFailed, { description: error instanceof Error ? error.message : String(error) })
      }).finally(() => { if (active) setInitialAccessLoaded(true) })
    }
    const disconnected = () => {
      cancel = undefined
      loadFallback()
      if (active) retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
    }
    const connect = () => {
      if (!active) return
      cancel = getNodeApi().stream<SharedWebAccessEvent>('watchSharedState', {
        domains: [SharedStateDomain.WEB_ACCESS],
      }, (event) => {
        if (sharedStateDomainCode(event.domain) !== 'webAccess') return
        if (!event.snapshot) attempt = 0
        if (event.error) {
          loadFallback()
          return
        }
        const config = event.state && typeof event.state === 'object' && event.state.case === 'webAccess'
          ? event.state.value
          : event.webAccess
        if (config) receiveAccessConfig(config)
      }, disconnected, disconnected)
    }
    connect()
    return () => {
      active = false
      if (retry !== undefined) window.clearTimeout(retry)
      cancel?.()
    }
  }, [labels.configLoadFailed, receiveAccessConfig])

  const loadNetworkInterfaces = useCallback(async () => {
    setLoadingInterfaces(true)
    try {
      const result = await getNodeApi().call<{ interfaces: NetworkInterfaceAddress[] }>('listNetworkInterfaces')
      setNetworkInterfaces(result.interfaces)
    } catch (error) {
      toast.error(interfaceLabels.loadFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally {
      setLoadingInterfaces(false)
      setInitialInterfacesLoaded(true)
    }
  }, [interfaceLabels.loadFailed])
  useEffect(() => { void loadNetworkInterfaces() }, [loadNetworkInterfaces])

  useEffect(() => {
    let active = true
    let cancel: (() => void) | undefined
    let retry: number | undefined
    let attempt = 0
    let firstUpdate = true
    const connect = () => {
      if (!active) return
      cancel = getNodeApi().stream<{ devices: { id: string; name: string; createdAtMs: number | string }[] }>(
        'watchPairedBrowsers', {}, (state) => {
          attempt = 0
          const nextDevices: PairedBrowser[] = state.devices.map((device) => ({
            id: device.id, name: device.name, createdAt: new Date(Number(device.createdAtMs)).toISOString(),
          }))
          setDevices(nextDevices)
          setInitialDevicesLoaded(true)
          if (pairingOpen && pairingBaselineReady.current && nextDevices.some((device) => !pairingBaseline.current.has(device.id))) {
            setPairingOpen(false)
            pairingBaselineReady.current = false
            setInvitation(null)
          }
          firstUpdate = false
        }, (error) => {
          cancel = undefined
          if (firstUpdate) {
            setInitialDevicesLoaded(true)
            toast.error(access.loadFailed, { description: error })
            firstUpdate = false
          }
          if (active) retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
        }, () => {
          cancel = undefined
          if (active) retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
        },
      )
    }
    connect()
    return () => {
      active = false
      if (retry !== undefined) window.clearTimeout(retry)
      cancel?.()
    }
  }, [access.loadFailed, pairingOpen])
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), invitation ? 1000 : 60_000)
    return () => window.clearInterval(timer)
  }, [invitation])
  async function createInvitation() {
    setBusy(true)
    try {
      const invitation = await getNodeApi().createBrowserPairing()
      const savedAddresses = accessConfig.listenAddresses?.length ? accessConfig.listenAddresses : (accessConfig.listenAddress ? [accessConfig.listenAddress] : [])
      const pairingOrigin = accessConfig.publicUrl.trim() || deriveBrowserAddress(savedAddresses)
      setInvitation({ ...invitation, pairUrl: `${normalizeBrowserAddress(pairingOrigin)}/#pair=${invitation.code}` })
    }
    catch (error) { toast.error(access.createFailed, { description: error instanceof Error ? error.message : String(error) }) }
    finally { setBusy(false) }
  }
  async function openPairing() {
    pairingBaseline.current = new Set(devices.map((device) => device.id))
    pairingBaselineReady.current = true
    setInvitation(null)
    setCodeCopied(false)
    setPairingOpen(true)
    setBusy(true)
    await createInvitation()
  }
  async function copyInvitationCode(code: string) {
    const copied = await copyPairingCode(code, access.copyFailed)
    setCodeCopied(copied)
    if (copied) window.setTimeout(() => setCodeCopied(false), 1600)
  }
  async function saveAccessConfig() {
    setSavingAccess(true)
    const stateChanged = draftEnabled !== accessConfig.enabled
    try {
      const selectedHosts = listenHostsRef.current
      const listenAddresses = isValidPort(listenPort) ? selectedHosts.map((host) => formatListenAddress(host, listenPort)) : []
      const config = await getNodeApi().call<WebAccessConfig>('setWebAccessConfig', {
        enabled: draftEnabled,
        listenAddress: listenAddresses[0] ?? '',
        listenAddresses,
        publicUrl: customBrowserAddress ? publicUrl.trim() : '',
      })
      syncAccessConfig(config)
      setInvitation(null)
      if (stateChanged) await loadNetworkInterfaces()
      toast.success(labels.saved)
    } catch (error) {
      toast.error(labels.saveFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally { setSavingAccess(false) }
  }
  async function revokeDevice() {
    if (!revokeTarget) return
    setRevokingDevice(true)
    try {
      await getNodeApi().revokePairedBrowser(revokeTarget.id)
      setDevices((current) => current.filter((item) => item.id !== revokeTarget.id))
      setRevokeTarget(null)
      toast.success(access.deviceRevoked)
    } catch (error) { toast.error(access.revokeFailed, { description: error instanceof Error ? error.message : String(error) }) }
    finally { setRevokingDevice(false) }
  }
  const secondsLeft = invitation ? Math.max(0, Math.ceil((Date.parse(invitation.expiresAt) - now) / 1000)) : 0
  const listenAddresses = isValidPort(listenPort) ? listenHosts.map((host) => formatListenAddress(host, listenPort)) : []
  const savedListenAddresses = accessConfig.listenAddresses?.length ? accessConfig.listenAddresses : (accessConfig.listenAddress ? [accessConfig.listenAddress] : [])
  const savedListenerDetails = savedListenAddresses.map(splitListenAddress).filter((address) => address.host)
  const derivedBrowserAddress = listenHosts.length && isValidPort(listenPort)
    ? `https://${formatListenAddress(listenHosts[0], listenPort)}`
    : ''
  const primaryInterface = networkInterfaces.find((networkInterface) => networkInterface.address === listenHosts[0])
  const publicUrlToSave = customBrowserAddress ? normalizeBrowserAddress(publicUrl) : ''
  const invalidEnabledListener = draftEnabled && (!listenHosts.length || !isValidPort(listenPort))
  const initialLoading = !initialAccessLoaded || !initialDevicesLoaded || !initialInterfacesLoaded

  return <div className="browser-access-page w-full min-w-0">
    {showHeading ? <PageHeading title={labels.title} description={labels.exposureDescription} /> : null}
    <motion.div layout="size" transition={{ layout: { duration: reducedMotion ? 0 : 0.32, ease: [0.22, 1, 0.36, 1] } }} className="mt-3 w-full min-w-0">
      {initialLoading ? <AccessPageSkeleton expanded={initialAccessLoaded && accessSettingsOpen} /> : <motion.div initial={{ opacity: reducedMotion ? 1 : 0, y: reducedMotion ? 0 : 5 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: reducedMotion ? 0 : 0.28 }} className="devices-layout w-full min-w-0 gap-6">
      <Card className="access-settings-card min-w-0 p-2">
        <Collapsible open={accessSettingsOpen} onOpenChange={setAccessSettingsOpen}>
          <CollapsibleTrigger
            title={labels.exposureTitle}
            icon={Settings2}
            rightSlot={<span className="access-status-badge access-status-badge--compact" data-enabled={accessConfig.enabled} title={accessConfig.enabled ? labels.enabled : labels.disabled}>{accessConfig.enabled ? accessUi.on : accessUi.off}</span>}
            className="px-4 py-3"
          />
          <CollapsibleContent className="gap-0">
          <div className="access-settings-content pb-3 pt-1">
          <div className="access-state-panel" data-enabled={draftEnabled} data-pending={draftEnabled !== accessConfig.enabled}>
            <span className="access-state-panel__icon"><Globe className="size-5" /></span>
            <div className="access-state-panel__copy">
              <motion.span
                key={`${accessConfig.enabled}-${draftEnabled}`}
                role="status"
                aria-live="polite"
                aria-label={draftEnabled === accessConfig.enabled
                  ? (accessConfig.enabled ? labels.enabled : labels.disabled)
                  : `${accessConfig.enabled ? labels.enabled : labels.disabled} ${document.documentElement.dir === 'rtl' ? '←' : '→'} ${draftEnabled ? labels.enabled : labels.disabled} · ${labels.save}`}
                initial={{ opacity: 0, y: 4, scale: 0.96 }}
                animate={{ opacity: 1, y: 0, scale: 1 }}
                transition={{ duration: 0.22, ease: [0.22, 1, 0.36, 1] }}
                className="access-status-badge access-status-badge--state"
                data-enabled={draftEnabled}
                data-pending={draftEnabled !== accessConfig.enabled}
              >
                {draftEnabled !== accessConfig.enabled ? <span className="access-status-badge__diff" dir={document.documentElement.dir}>
                  <span className="access-status-badge__previous" data-enabled={accessConfig.enabled}>{accessConfig.enabled ? accessUi.on : accessUi.off}</span>
                  <ArrowRight aria-hidden="true" className="size-3.5 rtl:rotate-180" />
                  <span>{draftEnabled ? accessUi.on : accessUi.off}</span>
                </span> : draftEnabled ? labels.enabled : labels.disabled}
              </motion.span>
            </div>
            <LiquidSwitch checked={draftEnabled} disabled={savingAccess} label={labels.exposureTitle} onChange={(value) => { accessDraftDirtyRef.current = true; setDraftEnabled(value) }} />
          </div>
          <motion.div
            aria-hidden={!accessConfig.enabled}
            inert={!accessConfig.enabled}
            className="mt-4 overflow-hidden"
            initial={false}
            animate={{ height: accessConfig.enabled ? 'auto' : 0, opacity: accessConfig.enabled ? 1 : 0, y: accessConfig.enabled ? 0 : -6 }}
            transition={{ duration: 0.3, ease: [0.22, 1, 0.36, 1] }}
            style={{ pointerEvents: accessConfig.enabled ? 'auto' : 'none' }}
          >
            <div className="access-compact-summary" data-enabled={accessConfig.enabled}>
              <div className="access-compact-summary__item">
                <span className="access-compact-summary__icon"><EthernetPort aria-hidden="true" className="size-4" /></span>
                <div className="min-w-0"><div className="access-compact-summary__label">{labels.listenAddress}</div><div className="access-compact-summary__value" dir="ltr">{savedListenerDetails.length ? savedListenerDetails.map((address) => address.host).join(', ') : '—'}</div></div>
              </div>
              <div className="access-compact-summary__item">
                <span className="access-compact-summary__icon"><Cable aria-hidden="true" className="size-4" /></span>
                <div className="min-w-0"><div className="access-compact-summary__label">{interfaceLabels.port}</div><div className="access-compact-summary__value" dir="ltr">{savedListenerDetails[0]?.port || '—'}</div></div>
              </div>
              <div className="access-compact-summary__item access-compact-summary__item--url">
                <span className="access-compact-summary__icon"><Link2 aria-hidden="true" className="size-4" /></span>
                <div className="min-w-0"><div className="access-compact-summary__label">{labels.publicUrl}</div><div className="access-compact-summary__value" dir="ltr">{accessConfig.publicUrl || '—'}</div></div>
              </div>
            </div>
            {accessConfig.certificateFingerprint && <div className="access-fingerprint mt-3">
              <span className="access-fingerprint__icon"><Fingerprint aria-hidden="true" className="size-5" /></span>
              <div className="min-w-0 flex-1"><div className="text-xs font-semibold text-muted-foreground">{labels.certificateFingerprint}</div><code dir="ltr" className="mt-1.5 block break-all font-mono text-xs leading-relaxed">{accessConfig.certificateFingerprint.match(/.{1,2}/g)?.join(':')}</code></div>
            </div>}
          </motion.div>
          <motion.div
            aria-hidden={accessConfig.enabled}
            inert={accessConfig.enabled}
            className="mt-5 space-y-5 overflow-hidden pt-2"
            initial={false}
            animate={{ height: accessConfig.enabled ? 0 : 'auto', opacity: accessConfig.enabled ? 0 : 1, y: accessConfig.enabled ? -8 : 0 }}
            transition={{ duration: 0.32, ease: [0.22, 1, 0.36, 1] }}
            style={{ pointerEvents: accessConfig.enabled ? 'none' : 'auto' }}
          >
          <div className="grid min-w-0 grid-cols-1 items-end gap-3 xl:grid-cols-[minmax(0,1.35fr)_minmax(9rem,0.38fr)_minmax(0,1.6fr)]">
            <NetworkInterfacePicker
              label={labels.listenAddress}
              placeholder={loadingInterfaces ? interfaceLabels.loading : interfaceLabels.choose}
              searchPlaceholder={interfaceLabels.search}
              emptyLabel={networkInterfaces.length ? interfaceLabels.noMatches : interfaceLabels.none}
              customLabel={interfaceLabels.current}
              doneLabel={interfaceLabels.done}
              interfaces={networkInterfaces}
              value={listenHosts}
              onChange={updateListenHosts}
              disabled={accessConfig.enabled}
            />
            <FloatingLabelInput containerClassName="w-full" disabled={accessConfig.enabled} label={interfaceLabels.port} icon={<Cable className="size-[18px]" />} type="number" min="1" max="65535" dir="ltr" uiDirection="ltr" inputMode="numeric" value={listenPort} onChange={(event) => { accessDraftDirtyRef.current = true; setListenPort(event.target.value) }} placeholder="50052" />
            <div className="flex min-w-0 items-center gap-3">
              {customBrowserAddress ? <FloatingLabelInput containerClassName="min-w-0 flex-1" disabled={accessConfig.enabled} label={labels.publicUrl} icon={<Link2 className="size-[18px]" />} dir="ltr" uiDirection="ltr" autoCapitalize="off" spellCheck={false} value={publicUrl} onChange={(event) => { accessDraftDirtyRef.current = true; setPublicUrl(event.target.value) }} placeholder="https://node.example.net:50052" /> : <div role="textbox" aria-readonly="true" aria-label={labels.publicUrl} aria-description={derivedBrowserAddress} dir={document.documentElement.dir} className="relative flex h-14 min-w-0 flex-1 items-center gap-3 rounded-2xl border border-[var(--app-accent)]/25 bg-[var(--app-accent)]/[0.07] px-4 text-start">
                <Link2 aria-hidden="true" className="size-[18px] shrink-0 text-[var(--app-muted)]" />
                <span className="absolute start-12 top-0 -translate-y-1/2 bg-[var(--app-card)] px-1.5 text-[11px] text-[var(--app-muted)]">{labels.publicUrl}</span>
                <span className="min-w-0"><span className="flex min-w-0 items-center gap-1 truncate text-sm font-medium text-[var(--app-ink)]"><span>{accessUi.sameAs}</span><bdi dir="auto" className="truncate">{primaryInterface?.name || labels.listenAddress}</bdi></span><span dir="ltr" className="block truncate text-start font-mono text-[10px] leading-4 text-[var(--app-muted)]">{derivedBrowserAddress || '—'}</span></span>
              </div>}
              <div className="flex shrink-0 items-center gap-2">
                <span className="flex items-center gap-1.5 text-xs font-medium text-[var(--app-muted)]"><ArrowUpRight aria-hidden="true" className="size-3.5" />{accessUi.override}</span>
                <LiquidSwitch checked={customBrowserAddress} disabled={accessConfig.enabled || savingAccess} label={labels.publicUrl} onChange={(checked) => {
                  accessDraftDirtyRef.current = true
                  if (checked && !customBrowserAddress) setPublicUrl(derivedBrowserAddress)
                  setCustomBrowserAddress(checked)
                }} />
              </div>
            </div>
          </div>
          <div role="note" className="access-security-notice">
            <span className="access-security-notice__icon"><ShieldAlert aria-hidden="true" className="size-5" /></span>
            <div className="min-w-0"><div className="text-sm font-semibold">HTTPS</div><p className="mt-0.5 text-sm leading-relaxed">{labels.httpsNotice}</p></div>
          </div>
          </motion.div>
          {(!accessConfig.enabled || draftEnabled !== accessConfig.enabled) && <div className="mt-5 flex justify-end border-t border-[var(--app-line)] pt-4"><Button type="button" size="lg" disabled={savingAccess || invalidEnabledListener || (draftEnabled === accessConfig.enabled && listenAddresses.length === savedListenAddresses.length && listenAddresses.every((address, index) => address === savedListenAddresses[index]) && publicUrlToSave === normalizeBrowserAddress(accessConfig.publicUrl))} onClick={() => void saveAccessConfig()} className="gap-2">{savingAccess && <LoaderCircle className="size-4 animate-spin" />}{labels.save}</Button></div>}
          </div>
          </CollapsibleContent>
        </Collapsible>
      </Card>
      <div className="min-w-0">
      <section className="devices-section min-w-0" aria-labelledby="paired-devices-heading">
        <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
          <div className="flex min-w-0 items-baseline gap-2"><h2 id="paired-devices-heading" className="text-xl font-semibold tracking-tight">{labels.devicesTitle}</h2><span className="text-sm font-medium tabular-nums text-[var(--app-muted)]">{devices.length}</span></div>
          <Button type="button" variant="ghost" disabled={!accessConfig.enabled} onClick={() => void openPairing()} className="devices-add-button"><span className="devices-add-button__icon"><Plus className="size-4" /></span>{labels.pairingTitle}</Button>
        </div>
        {devices.length === 0 ? <Card className="devices-empty"><CardContent className="grid min-h-44 place-items-center py-8 text-center"><div className="grid justify-items-center gap-2"><span className="grid size-11 place-items-center rounded-2xl bg-[var(--app-wash)] text-[var(--app-accent)]"><ShieldCheck className="size-5" /></span><p className="max-w-xs text-xs leading-relaxed text-muted-foreground">{access.noDevices}</p></div></CardContent></Card> :
          <ul className="paired-devices-list" aria-label={labels.devicesTitle}>{devices.map((device) => {
            const { platform, browser } = parseBrowserDeviceName(device.name)
            const DeviceIcon = /android|ios|iphone|ipad/i.test(platform) ? Smartphone : Monitor
            return <li key={device.id} className="paired-device-row">
              <span className="paired-device-icon"><DeviceIcon className="size-[18px]" /></span>
              <div className="paired-device-details">
                <div className="paired-device-title"><span className="paired-device-platform">{platform}</span>{browser && <><span aria-hidden="true" className="paired-device-separator">·</span><span className="paired-device-browser">{browser}</span></>}</div>
                <time className="paired-device-date" dateTime={device.createdAt}>{formatRelativeDate(device.createdAt, now, document.documentElement.lang)}</time>
              </div>
              <Button type="button" variant="ghost" size="icon" className="paired-device-revoke" aria-label={`${access.revoke} ${device.name}`} title={access.revoke} onClick={() => setRevokeTarget(device)}><X className="size-4" /></Button>
            </li>
          })}</ul>
        }
      </section>
      </div>
      </motion.div>}
    </motion.div>
    <ResponsiveActionDialog open={pairingOpen} onOpenChange={(open) => { setPairingOpen(open); if (!open) pairingBaselineReady.current = false }} direction={document.documentElement.dir === 'rtl' ? 'rtl' : 'ltr'} title={labels.pairingTitle} description={access.scanCode} icon={<QrCode className="size-6" />}>
      {busy ? <div role="status" className="flex min-h-40 flex-col items-center justify-center gap-3 text-sm text-muted-foreground"><LoaderCircle className="size-5 animate-spin text-[var(--app-accent)]" />{access.createCode}</div> : invitation && secondsLeft > 0 ? <div data-pairing-content className="flex flex-col items-center gap-4">
        <div className="rounded-2xl border border-[var(--app-line)] bg-white p-3"><QRCodeSVG value={invitation.pairUrl} size={160} level="M" /></div>
        <div className="min-w-0 text-center">
          <div className="flex items-center justify-center gap-2">
            <code dir="ltr" className="text-3xl font-semibold tracking-[0.25em]">{invitation.code}</code>
            <Button type="button" variant="ghost" size="icon" className="size-9 shrink-0 rounded-full" aria-label={codeCopied ? access.codeCopied : access.copyCode} title={codeCopied ? access.codeCopied : access.copyCode} onClick={() => void copyInvitationCode(invitation.code)}>
              {codeCopied ? <Check className="size-4 text-emerald-600" /> : <Copy className="size-4" />}
            </Button>
          </div>
          <p className="mt-3 text-sm text-muted-foreground">{access.expiresIn.replace('{seconds}', String(secondsLeft))}</p>
        </div>
      </div> : <div className="space-y-4 text-center">
        {invitation && <p role="status" className="text-sm text-muted-foreground">{access.codeExpired}</p>}
        <Button type="button" size="lg" disabled={busy || !accessConfig.enabled} onClick={() => void createInvitation()} className="w-full gap-2"><QrCode className="size-4" />{access.createCode}</Button>
      </div>}
      <div className="mt-5 flex justify-center border-t border-[var(--app-line)] pt-4"><Button type="button" variant="outline" size="lg" onClick={() => setPairingOpen(false)} className="h-12 min-w-0 w-full rounded-xl sm:w-auto sm:min-w-32">{messages.cancel}</Button></div>
    </ResponsiveActionDialog>
    <ResponsiveActionDialog open={Boolean(revokeTarget)} onAfterClose={afterRevokeDialogClose} direction={document.documentElement.dir === 'rtl' ? 'rtl' : 'ltr'} title={messages.revokeDeviceTitle} description={messages.revokeDeviceDescription} icon={<X className="size-6 text-destructive" />} onOpenChange={(open) => { if (!open && !revokingDevice) setRevokeTarget(null) }}>
      {displayedRevokeTarget && <p className="break-words rounded-xl bg-[var(--app-wash)] px-3 py-2 text-center text-sm font-semibold text-[var(--app-ink)]">{displayedRevokeTarget.name}</p>}
      <div className="mt-5 grid min-w-0 gap-2 sm:grid-cols-2">
        <Button type="button" variant="outline" size="lg" onClick={() => setRevokeTarget(null)} disabled={revokingDevice} className="order-2 h-12 min-w-0 w-full rounded-xl sm:order-1">{messages.cancel}</Button>
        <Button type="button" variant="destructive" size="lg" onClick={() => void revokeDevice()} disabled={revokingDevice || !revokeTarget} className="order-1 h-12 min-w-0 w-full whitespace-normal rounded-xl px-3 text-center leading-5 sm:order-2">{revokingDevice ? <LoaderCircle className="size-4 shrink-0 animate-spin" /> : <X className="size-4 shrink-0" />}{access.revoke}</Button>
      </div>
    </ResponsiveActionDialog>
  </div>
}

export const BrowserAccessPage = AccessPage

function parseBrowserDeviceName(name: string) {
  const match = /^(.+?)\s+on\s+(.+)$/i.exec(name.trim())
  return match ? { browser: match[1], platform: match[2] } : { browser: '', platform: name }
}

function AccessPageSkeleton({ expanded }: { expanded: boolean }) {
  return <div aria-busy="true" className="grid gap-6">
    <Card className="min-w-0 p-2">
      <div className="flex min-h-16 items-center gap-3 px-4 py-3">
        <Skeleton className="size-10 shrink-0 rounded-xl" />
        <Skeleton className="h-4 w-40 max-w-[45%]" />
        <Skeleton className="ms-auto h-7 w-12 shrink-0 rounded-full" />
        <Skeleton className="size-4 shrink-0" />
      </div>
      {expanded && <div className="grid gap-4 px-4 pb-4 pt-2">
        <Skeleton className="h-[72px] rounded-2xl" />
        <div className="grid gap-3 xl:grid-cols-3"><Skeleton className="h-14 rounded-xl" /><Skeleton className="h-14 rounded-xl" /><Skeleton className="h-14 rounded-xl" /></div>
        <Skeleton className="h-20 rounded-xl" />
        <div className="flex justify-end border-t border-[var(--app-line)] pt-4"><Skeleton className="h-11 w-40 rounded-xl" /></div>
      </div>}
    </Card>
    <section className="grid gap-4" aria-hidden="true">
      <div className="flex items-center justify-between"><div className="grid gap-2"><Skeleton className="h-3 w-24" /><Skeleton className="h-6 w-44" /></div><Skeleton className="h-10 w-32 rounded-xl" /></div>
      <Card className="grid min-h-44 place-items-center p-4 sm:p-5"><div className="grid justify-items-center gap-3"><Skeleton className="size-11 rounded-2xl" /><Skeleton className="h-3 w-48 max-w-full" /><Skeleton className="h-3 w-32 max-w-full" /></div></Card>
    </section>
  </div>
}

function splitListenAddress(value: string) {
  const trimmed = value.trim()
  if (trimmed.startsWith('[')) {
    const closingBracket = trimmed.indexOf(']')
    if (closingBracket > 0 && trimmed[closingBracket + 1] === ':') {
      return { host: trimmed.slice(1, closingBracket), port: trimmed.slice(closingBracket + 2) }
    }
  }
  const separator = trimmed.lastIndexOf(':')
  if (separator > 0 && !trimmed.slice(0, separator).includes(':')) {
    return { host: trimmed.slice(0, separator), port: trimmed.slice(separator + 1) }
  }
  return { host: '', port: '' }
}

function formatListenAddress(host: string, port: string) {
  const formattedHost = host.includes(':') ? `[${host}]` : host
  return `${formattedHost}:${port.trim()}`
}

function deriveBrowserAddress(addresses: string[]) {
  const primary = addresses[0] ? splitListenAddress(addresses[0]) : { host: '', port: '' }
  if (!primary.host || !isValidPort(primary.port)) return ''
  return `https://${formatListenAddress(primary.host, primary.port)}`
}

function normalizeBrowserAddress(value: string) {
  return value.trim().replace(/\/+$/, '')
}

function isValidPort(value: string) {
  const port = Number(value)
  return /^\d+$/.test(value) && port >= 1 && port <= 65535
}

async function copyPairingCode(code: string, failureMessage: string) {
  try {
    if (navigator.clipboard?.writeText) await navigator.clipboard.writeText(code)
    else {
      const field = document.createElement('textarea')
      field.value = code
      field.style.position = 'fixed'
      field.style.opacity = '0'
      document.body.appendChild(field)
      field.select()
      const copied = document.execCommand('copy')
      field.remove()
      if (!copied) throw new Error('Clipboard unavailable')
    }
    return true
  } catch { toast.error(failureMessage); return false }
}
