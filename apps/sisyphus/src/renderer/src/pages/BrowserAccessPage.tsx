import { useCallback, useEffect, useState } from 'react'
import { Copy, Globe2, LoaderCircle, QrCode, ShieldCheck, Trash2 } from 'lucide-react'
import { QRCodeSVG } from 'qrcode.react'
import { type getMessages } from '@/i18n/messages'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { PageHeading } from '@/components/ui/page-layout'
import { getNodeApi } from '@/lib/node-api'
import { toast } from 'sonner'
import type { PairedBrowser } from '@sisyphus/client'

type Messages = ReturnType<typeof getMessages>

export function BrowserAccessPage({ messages, showHeading = true }: { messages: Messages; showHeading?: boolean }) {
  const [devices, setDevices] = useState<PairedBrowser[]>([])
  const [invitation, setInvitation] = useState<{ code: string; expiresAt: string; pairUrl: string } | null>(null)
  const [busy, setBusy] = useState(false)
  const [now, setNow] = useState(Date.now())
  const access = messages.browserAccess

  const refreshDevices = useCallback(async (reportError = false) => {
    try { setDevices((await getNodeApi().listPairedBrowsers()).devices) }
    catch (error) {
      if (reportError) toast.error(access.loadFailed, { description: error instanceof Error ? error.message : String(error) })
    }
  }, [access.loadFailed])
  useEffect(() => {
    void refreshDevices(true)
    const refreshWhenVisible = () => { if (document.visibilityState === 'visible') void refreshDevices() }
    const poll = window.setInterval(refreshWhenVisible, 5000)
    const timer = window.setInterval(() => setNow(Date.now()), 1000)
    window.addEventListener('focus', refreshWhenVisible)
    document.addEventListener('visibilitychange', refreshWhenVisible)
    return () => {
      window.clearInterval(poll)
      window.clearInterval(timer)
      window.removeEventListener('focus', refreshWhenVisible)
      document.removeEventListener('visibilitychange', refreshWhenVisible)
    }
  }, [refreshDevices])
  async function createInvitation() {
    setBusy(true)
    try { setInvitation(await getNodeApi().createBrowserPairing()); toast.success(access.codeCreated) }
    catch (error) { toast.error(access.createFailed, { description: error instanceof Error ? error.message : String(error) }) }
    finally { setBusy(false) }
  }
  async function revokeDevice(device: PairedBrowser) {
    try { await getNodeApi().revokePairedBrowser(device.id); setDevices((current) => current.filter((item) => item.id !== device.id)); toast.success(access.deviceRevoked) }
    catch (error) { toast.error(access.revokeFailed, { description: error instanceof Error ? error.message : String(error) }) }
  }
  const secondsLeft = invitation ? Math.max(0, Math.ceil((Date.parse(invitation.expiresAt) - now) / 1000)) : 0

  return <div className="browser-access-page w-full min-w-0">
    {showHeading && <PageHeading title={access.title} description={access.description} />}
    <div className="devices-layout mt-5 grid w-full min-w-0 gap-5">
      <Card className="devices-pair-card">
        <CardHeader className="flex flex-row items-center gap-3 border-b border-[var(--app-line)] py-4">
          <div className="grid size-10 place-items-center rounded-xl bg-[var(--app-wash)] text-[var(--app-accent)]"><QrCode className="size-4" /></div>
          <div><div className="text-[9px] font-semibold tracking-[0.13em] text-muted-foreground">{access.eyebrow}</div><CardTitle className="mt-0.5 text-sm">{access.createCode}</CardTitle></div>
        </CardHeader>
        <CardContent className="space-y-4 pt-4">
          <div role="note" className="rounded-xl border border-amber-500/30 bg-amber-500/5 px-3 py-2.5 text-xs leading-relaxed text-amber-800 dark:text-amber-200">{access.httpWarning}</div>
          <div className="flex flex-wrap items-center gap-2">
            <Button type="button" disabled={busy} onClick={() => void createInvitation()} className="gap-2">{busy ? <LoaderCircle className="size-4 animate-spin" /> : <QrCode className="size-4" />}{access.createCode}</Button>
          </div>
          {invitation && secondsLeft > 0 && <div className="flex flex-col items-center gap-3 rounded-2xl border border-[var(--app-line)] p-4 sm:flex-row sm:items-start">
            <div className="rounded-xl bg-white p-2"><QRCodeSVG value={invitation.pairUrl} size={144} level="M" /></div>
            <div className="min-w-0 flex-1 text-center sm:text-start">
              <p className="text-xs text-muted-foreground">{access.scanCode}</p>
              <code dir="ltr" className="mt-2 block text-2xl font-semibold tracking-[0.25em]">{invitation.code}</code>
              <p className="mt-2 text-xs text-muted-foreground">{access.expiresIn.replace('{seconds}', String(secondsLeft))}</p>
              <Button type="button" variant="ghost" size="sm" className="mt-2 gap-2" onClick={() => void copyPairingCode(invitation.code, access.codeCopied, access.copyFailed)}><Copy className="size-3.5" />{access.copyCode}</Button>
            </div>
          </div>}
          {invitation && secondsLeft === 0 && <p role="status" className="text-xs text-muted-foreground">{access.codeExpired}</p>}
        </CardContent>
      </Card>

      <section className="devices-section min-w-0" aria-labelledby="paired-devices-heading">
        <div className="mb-3 flex items-center justify-between gap-3">
          <div><div className="text-[9px] font-semibold tracking-[0.14em] text-muted-foreground">{access.eyebrow}</div><h2 id="paired-devices-heading" className="mt-1 text-base font-semibold tracking-tight">{access.devicesTitle}</h2></div>
          <span className="rounded-full border border-[var(--app-line)] bg-[var(--app-card)] px-2.5 py-1 font-mono text-[10px] text-muted-foreground">{devices.length}</span>
        </div>
        {devices.length === 0 ? <Card className="devices-empty"><CardContent className="grid min-h-44 place-items-center py-8 text-center"><div className="grid justify-items-center gap-2"><span className="grid size-11 place-items-center rounded-2xl bg-[var(--app-wash)] text-[var(--app-accent)]"><ShieldCheck className="size-5" /></span><p className="max-w-xs text-xs leading-relaxed text-muted-foreground">{access.noDevices}</p></div></CardContent></Card> :
          <div className="devices-table-wrap">
            <table className="devices-table">
              <thead><tr><th scope="col">{access.deviceColumn}</th><th scope="col">{access.pairedColumn}</th><th scope="col" className="devices-table__actions">{access.actionsColumn}</th></tr></thead>
              <tbody>{devices.map((device) => <tr key={device.id}>
                <td data-label={access.deviceColumn}><div className="flex min-w-0 items-center gap-3"><span className="grid size-9 shrink-0 place-items-center rounded-xl bg-[var(--app-wash)] text-[var(--app-accent)]"><Globe2 className="size-4" /></span><span className="min-w-0"><span className="block truncate text-sm font-semibold">{device.name}</span><span className="mt-0.5 block truncate font-mono text-[9px] text-muted-foreground">{device.id}</span></span></div></td>
                <td data-label={access.pairedColumn}><time className="text-xs text-muted-foreground" dateTime={device.createdAt}>{new Date(device.createdAt).toLocaleString()}</time></td>
                <td data-label={access.actionsColumn} className="devices-table__actions"><Button type="button" variant="outline" size="sm" className="gap-2 text-destructive hover:border-destructive/40 hover:bg-destructive/5 hover:text-destructive" aria-label={`${access.revoke} ${device.name}`} onClick={() => void revokeDevice(device)}><Trash2 className="size-3.5" /><span>{access.revoke}</span></Button></td>
              </tr>)}</tbody>
            </table>
          </div>
        }
      </section>
    </div>
  </div>
}

async function copyPairingCode(code: string, successMessage: string, failureMessage: string) {
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
    toast.success(successMessage)
  } catch { toast.error(failureMessage) }
}
