import { SettingsSectionSkeleton } from '@/components/ui/settings-section-skeleton'
import { SettingsIcon } from '@/components/ui/settings-icon'
import { useEffect, useState } from 'react'
import { Download, LoaderCircle, MapPinned, ShieldCheck, RefreshCw, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { getNodeApi } from '@/lib/node-api'
import { sharedStateDomainCode } from '@/lib/shared-state-domain'
import { nodeInfoFromSharedStateEvent } from '@/lib/node-shared-state'
import { SharedStateDomain } from '@sisyphus/client'
import { Button } from '@/components/ui/button'
import { NodeSettingsSection } from '@/components/node-settings-section'
import { cityEstimateFromNodeInfo, cityTableAttribution, cityTableAttributionUrl, cityTableFromProtocol, cityTableStatus, ownerPinned, placeName, type CityEstimate, type CityTableState } from '@/lib/city-table'
import { formatMessage, type getMessages } from '@/i18n/messages'

type Messages = ReturnType<typeof getMessages>

type Busy = 'download' | 'remove' | null
type SharedCityTableEvent = {
  domain: number | string
  error?: string
  snapshot?: boolean
  state?: { case?: string; value?: unknown } | string
  node?: unknown
  cityTable?: CityTableState
}

// CityTableSettingsCard lets the owner download DB-IP's free city table, by
// which the node places itself near its city from its public address, and
// update or remove it. Downloading shows db-ip.com this machine's address,
// so it happens only when the owner asks.
export function CityTableSettingsCard({ messages, locale }: { messages: Messages; locale: string }) {
  const labels = messages.cityTableSettings
  const [table, setTable] = useState<CityTableState | null>(null)
  const [estimate, setEstimate] = useState<CityEstimate | null>(null)
  const [pinned, setPinned] = useState(false)
  const [loading, setLoading] = useState(true)
  const [unavailable, setUnavailable] = useState(false)
  const [busy, setBusy] = useState<Busy>(null)

  // The node-info and city-table snapshots share the same event stream, so
  // location updates arrive without an extra RPC per table event.
  async function readEstimate() {
    try {
      const info = await getNodeApi().call('getNodeInfo')
      setEstimate(cityEstimateFromNodeInfo(info))
      setPinned(ownerPinned(info))
    } catch { setEstimate(null) }
  }

  useEffect(() => {
    let active = true
    let cancel: (() => void) | undefined
    let retry: number | undefined
    let attempt = 0
    let fallbackRequested = false
    const apply = async (value: unknown) => {
      if (!active) return
      setTable(cityTableFromProtocol(value))
      setUnavailable(false)
      setLoading(false)
    }
    const loadFallback = () => {
      if (!active || fallbackRequested) return
      fallbackRequested = true
      void getNodeApi().call('getCityTable').then(apply).catch(() => {
        if (active) { setUnavailable(true); setLoading(false) }
      })
    }
    const disconnected = () => {
      cancel = undefined
      loadFallback()
      if (active) retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
    }
    const connect = () => {
      if (!active) return
      cancel = getNodeApi().stream<SharedCityTableEvent>('watchSharedState', {
        domains: [SharedStateDomain.CITY_TABLE, SharedStateDomain.NODE],
      }, (event) => {
        if (sharedStateDomainCode(event.domain) === 'node') {
          const info = nodeInfoFromSharedStateEvent(event)
          if (info) {
            setEstimate(cityEstimateFromNodeInfo(info))
            setPinned(ownerPinned(info))
          }
          return
        }
        if (sharedStateDomainCode(event.domain) !== 'cityTable') return
        if (event.error) { loadFallback(); return }
        const next = event.state && typeof event.state === 'object' && event.state.case === 'cityTable'
          ? event.state.value
          : event.cityTable
        if (!next) return
        fallbackRequested = false
        if (event.snapshot) attempt = 0
        void apply(next)
      }, disconnected, disconnected)
    }
    connect()
    return () => {
      active = false
      if (retry !== undefined) window.clearTimeout(retry)
      cancel?.()
    }
  }, [])

  async function change(action: Exclude<Busy, null>) {
    setBusy(action)
    try {
      const result = await getNodeApi().call(action === 'download' ? 'downloadCityTable' : 'removeCityTable')
      setTable(cityTableFromProtocol(result))
      await readEstimate()
      toast.success(action === 'download' ? labels.downloadedToast : labels.removedToast)
    } catch (error) {
      toast.error(action === 'download' ? labels.downloadFailed : labels.removeFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally { setBusy(null) }
  }

  const status = table ? cityTableStatus(table, locale) : null
  const credit = table?.attribution ?? cityTableAttribution

  return <NodeSettingsSection title={labels.title} description={labels.description} icon={MapPinned}>
            {loading ? <SettingsSectionSkeleton label={messages.connecting} rows={2} controls={false} />
        : unavailable ? <p role="note" className="mt-4 text-sm text-muted-foreground">{labels.unavailable}</p>
        : <>
          <div className="settings-row settings-row--divider items-start"><SettingsIcon icon={ShieldCheck} /><div className="min-w-0 flex-1 space-y-2">
            <p className="max-w-3xl text-sm leading-relaxed text-muted-foreground">{labels.privacy}</p>
            <p className="max-w-3xl text-sm leading-relaxed text-muted-foreground">{labels.accuracy}</p>
            <p className="text-xs leading-relaxed text-muted-foreground">{labels.creditLabel} <a href={cityTableAttributionUrl} dir="ltr" onClick={(event) => { event.preventDefault(); void getNodeApi().openDbIp() }} className="text-[var(--app-accent)] underline underline-offset-2 focus-visible:outline-2 focus-visible:outline-[var(--app-accent)]">{credit}</a></p>
          </div></div>
          {status && (estimate
            ? <div className="settings-row settings-row--divider"><SettingsIcon icon={MapPinned} /><p className="min-w-0 flex-1 text-sm font-medium">{formatMessage(labels.estimate, { place: placeName(estimate) })}</p></div>
            : <div className="settings-row settings-row--divider"><SettingsIcon icon={MapPinned} /><p className="min-w-0 flex-1 text-sm leading-relaxed text-muted-foreground">{pinned ? labels.pinWins : labels.noEstimate}</p></div>)}
          <div className="mt-3 flex flex-col-reverse justify-between gap-3 border-t border-[var(--app-line)] pt-4 sm:flex-row sm:items-center">
            <p role="status" className="text-xs leading-relaxed text-muted-foreground">{busy === 'download' ? labels.downloading : status ? formatMessage(labels.downloaded, status) : labels.notDownloaded}</p>
            <div className="flex flex-wrap gap-2">
              {status && <Button type="button" variant="outline" disabled={busy !== null} onClick={() => void change('remove')} className="gap-2">{busy === 'remove' ? <LoaderCircle className="size-4 animate-spin" /> : <Trash2 className="size-4" />}{labels.remove}</Button>}
              <Button type="button" variant={status ? 'outline' : 'default'} disabled={busy !== null} aria-busy={busy === 'download'} onClick={() => void change('download')} className="gap-2">{busy === 'download' ? <LoaderCircle className="size-4 animate-spin" /> : status ? <RefreshCw className="size-4" /> : <Download className="size-4" />}{status ? labels.update : labels.download}</Button>
            </div>
          </div>
        </>}
  </NodeSettingsSection>
}
