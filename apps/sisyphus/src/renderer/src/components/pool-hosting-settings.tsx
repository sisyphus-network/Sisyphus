import { useEffect, useState } from 'react'
import { CircleAlert, LoaderCircle, Network, Save, TriangleAlert } from 'lucide-react'
import type { PoolHostingSettings, PoolHostingState } from '../../../preload'
import { getNodeApi } from '@/lib/node-api'
import { LiquidSwitch } from '@/components/ui/liquid-switch'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { formatMessage, type getMessages } from '@/i18n/messages'
import { allInterfaces, formForHosting } from '../../../shared/pool-hosting.ts'
import { toast } from 'sonner'
import { RestartNodeButton } from '@/components/restart-node-button'

type Messages = ReturnType<typeof getMessages>

export function PoolHostingSettingsCard({ messages }: { messages: Messages }) {
  const labels = messages.poolHostingSettings
  const [state, setState] = useState<PoolHostingState>()
  const [form, setForm] = useState<PoolHostingSettings>({ enabled: false, address: '', port: 7700 })
  const [portText, setPortText] = useState('7700')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)

  const [loads, setLoads] = useState(0)
  useEffect(() => {
    let active = true
    void getNodeApi().getPoolHosting().then((result) => {
      if (!active) return
      setState(result)
      const initial = formForHosting(result)
      setForm(initial)
      setPortText(String(initial.port))
    }).catch((error) => {
      if (active) toast.error(labels.loadFailed, { description: error instanceof Error ? error.message : String(error) })
    }).finally(() => { if (active) setLoading(false) })
    return () => { active = false }
  }, [labels.loadFailed, loads])

  const port = /^\d+$/.test(portText) ? Number(portText) : NaN
  const portValid = Number.isInteger(port) && port >= 1 && port <= 65535
  const addressMissing = form.enabled && !form.address
  const dirty = Boolean(state && (form.enabled !== state.saved.enabled || form.address !== state.saved.address || port !== state.saved.port))
  const editable = Boolean(state?.managed) && !saving
  // A saved address may be one this computer no longer has; keep it listed
  // so the owner sees what was chosen rather than a silent change.
  const choices = state ? [...state.addresses] : []
  if (form.address && form.address !== allInterfaces && !choices.some((entry) => entry.address === form.address)) {
    choices.push({ address: form.address, interface: labels.addressGone, lan: false })
  }

  async function save() {
    if (!state?.managed || !dirty || !portValid || addressMissing) return
    setSaving(true)
    try {
      const next = await getNodeApi().savePoolHosting({ ...form, port })
      setState(next)
      setForm(next.saved)
      setPortText(String(next.saved.port))
      toast.success(labels.saved)
    } catch (error) {
      toast.error(labels.saveFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally { setSaving(false) }
  }

  return <Card className="settings-card min-w-0">
    <CardContent className="p-4 sm:p-5">
      <div className="flex items-start gap-3">
        <div className="settings-row__icon"><Network className="size-4" /></div>
        <div className="min-w-0 flex-1"><h3 className="text-sm font-semibold">{labels.title}</h3><p className="mt-1 text-sm leading-relaxed text-muted-foreground">{labels.description}</p></div>
        <LiquidSwitch checked={form.enabled} disabled={loading || !editable} label={labels.title} onChange={(enabled) => setForm((current) => ({ ...current, enabled }))} />
      </div>
      {loading ? <div role="status" className="flex items-center gap-2 py-7 text-sm text-muted-foreground"><LoaderCircle className="size-4 animate-spin" />{messages.connecting}</div> : <>
        {state && !state.managed && <div role="note" className="mt-4 flex items-start gap-2.5 rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)]/45 p-3 text-sm leading-relaxed text-muted-foreground"><CircleAlert className="mt-0.5 size-4 shrink-0" />{labels.notManaged}</div>}
        {state?.unavailable && <div role="alert" className="mt-4 flex items-start gap-2.5 rounded-xl border border-destructive/40 p-3 text-sm leading-relaxed text-destructive"><TriangleAlert className="mt-0.5 size-4 shrink-0" />{formatMessage(labels.addressUnavailable, { address: state.running.address || state.saved.address })}</div>}
        <p className="mt-3 text-xs leading-relaxed text-muted-foreground">{labels.discoveryNote}</p>
        <div className="settings-row settings-row--divider flex-wrap">
          <div className="min-w-0 flex-1"><label htmlFor="pool-hosting-address" className="text-sm font-medium">{labels.addressTitle}</label><p className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{labels.addressDescription}</p></div>
          <select id="pool-hosting-address" dir="ltr" value={form.address} disabled={!editable} onChange={(event) => setForm((current) => ({ ...current, address: event.target.value }))} className="h-9 w-full max-w-sm rounded-lg border border-[var(--app-line)] bg-background px-3 font-mono text-sm">
            {choices.length === 0 && <option value="">{labels.noAddresses}</option>}
            {choices.map((entry) => <option key={entry.address} value={entry.address}>{entry.address} · {entry.interface}</option>)}
            <option value={allInterfaces}>{labels.allInterfaces}</option>
          </select>
        </div>
        {form.address === allInterfaces && <p role="note" className="flex items-start gap-2 text-xs leading-relaxed text-amber-700 dark:text-amber-400"><TriangleAlert className="mt-0.5 size-3.5 shrink-0" />{labels.allInterfacesWarning}</p>}
        {addressMissing && <p role="alert" className="text-xs text-destructive">{labels.addressRequired}</p>}
        <div className="settings-row settings-row--divider flex-wrap">
          <div className="min-w-0 flex-1"><label htmlFor="pool-hosting-port" className="text-sm font-medium">{labels.portTitle}</label><p className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{labels.portDescription}</p></div>
          <Input id="pool-hosting-port" dir="ltr" inputMode="numeric" autoComplete="off" value={portText} disabled={!editable} onChange={(event) => setPortText(event.target.value.trim())} aria-invalid={!portValid} className="w-28 text-left font-mono text-sm" />
        </div>
        {!portValid && <p role="alert" className="text-xs text-destructive">{labels.portInvalid}</p>}
        {state?.managed && <div className="mt-3 flex flex-col-reverse justify-between gap-3 border-t border-[var(--app-line)] pt-4 sm:flex-row sm:items-center">
          <p role="status" className="text-xs leading-relaxed text-muted-foreground">{state.restartNeeded ? labels.restartRequired : labels.restartHint}</p>
          <div className="flex flex-wrap gap-2">{state.restartNeeded && <RestartNodeButton messages={messages} onRestarted={() => setLoads((n) => n + 1)} />}<Button type="button" disabled={!dirty || saving || !portValid || addressMissing} onClick={() => void save()} className="gap-2"><Save className="size-4" />{saving ? <LoaderCircle className="size-4 animate-spin" /> : null}{labels.save}</Button></div>
        </div>}
      </>}
    </CardContent>
  </Card>
}
