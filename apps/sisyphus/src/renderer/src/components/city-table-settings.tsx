import { useEffect, useState } from 'react'
import { Download, LoaderCircle, MapPinned, RefreshCw, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { getNodeApi } from '@/lib/node-api'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { cityEstimateFromNodeInfo, cityTableAttribution, cityTableAttributionUrl, cityTableFromProtocol, cityTableStatus, ownerPinned, placeName, type CityEstimate, type CityTableState } from '@/lib/city-table'
import { formatMessage, type getMessages } from '@/i18n/messages'

type Messages = ReturnType<typeof getMessages>

type Busy = 'download' | 'remove' | null

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

  // Where the table puts the node now. The node answers from the table as
  // it is when asked, so this is asked again after each change.
  async function readEstimate() {
    try {
      const info = await getNodeApi().call('getNodeInfo')
      setEstimate(cityEstimateFromNodeInfo(info))
      setPinned(ownerPinned(info))
    } catch { setEstimate(null) }
  }

  useEffect(() => {
    let active = true
    void getNodeApi().call('getCityTable').then(async (result) => {
      if (!active) return
      setTable(cityTableFromProtocol(result))
      const info = await getNodeApi().call('getNodeInfo').catch(() => null)
      if (!active) return
      setEstimate(cityEstimateFromNodeInfo(info))
      setPinned(ownerPinned(info))
    }).catch(() => { if (active) setUnavailable(true) })
      .finally(() => { if (active) setLoading(false) })
    return () => { active = false }
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

  return <Card className="settings-card min-w-0">
    <CardContent className="p-4 sm:p-5">
      <div className="flex items-start gap-3">
        <div className="settings-row__icon"><MapPinned className="size-4" /></div>
        <div className="min-w-0 flex-1"><h3 className="text-sm font-semibold">{labels.title}</h3><p className="mt-1 text-sm leading-relaxed text-muted-foreground">{labels.description}</p></div>
      </div>
      {loading ? <div role="status" className="flex items-center gap-2 py-7 text-sm text-muted-foreground"><LoaderCircle className="size-4 animate-spin" />{messages.connecting}</div>
        : unavailable ? <p role="note" className="mt-4 text-sm text-muted-foreground">{labels.unavailable}</p>
        : <>
          <div className="settings-row settings-row--divider"><div className="min-w-0 flex-1 space-y-2">
            <p className="max-w-3xl text-sm leading-relaxed text-muted-foreground">{labels.privacy}</p>
            <p className="max-w-3xl text-sm leading-relaxed text-muted-foreground">{labels.accuracy}</p>
            <p className="text-xs leading-relaxed text-muted-foreground">{labels.creditLabel} <a href={cityTableAttributionUrl} dir="ltr" onClick={(event) => { event.preventDefault(); void getNodeApi().openDbIp() }} className="text-[var(--app-accent)] underline underline-offset-2 focus-visible:outline-2 focus-visible:outline-[var(--app-accent)]">{credit}</a></p>
          </div></div>
          {status && (estimate
            ? <div className="settings-row settings-row--divider"><p className="min-w-0 flex-1 text-sm font-medium">{formatMessage(labels.estimate, { place: placeName(estimate) })}</p></div>
            : <div className="settings-row settings-row--divider"><p className="min-w-0 flex-1 text-sm leading-relaxed text-muted-foreground">{pinned ? labels.pinWins : labels.noEstimate}</p></div>)}
          <div className="mt-3 flex flex-col-reverse justify-between gap-3 border-t border-[var(--app-line)] pt-4 sm:flex-row sm:items-center">
            <p role="status" className="text-xs leading-relaxed text-muted-foreground">{busy === 'download' ? labels.downloading : status ? formatMessage(labels.downloaded, status) : labels.notDownloaded}</p>
            <div className="flex flex-wrap gap-2">
              {status && <Button type="button" variant="outline" disabled={busy !== null} onClick={() => void change('remove')} className="gap-2">{busy === 'remove' ? <LoaderCircle className="size-4 animate-spin" /> : <Trash2 className="size-4" />}{labels.remove}</Button>}
              <Button type="button" variant={status ? 'outline' : 'default'} disabled={busy !== null} aria-busy={busy === 'download'} onClick={() => void change('download')} className="gap-2">{busy === 'download' ? <LoaderCircle className="size-4 animate-spin" /> : status ? <RefreshCw className="size-4" /> : <Download className="size-4" />}{status ? labels.update : labels.download}</Button>
            </div>
          </div>
        </>}
    </CardContent>
  </Card>
}
