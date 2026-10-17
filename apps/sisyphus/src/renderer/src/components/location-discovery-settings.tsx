import { useEffect, useRef, useState } from 'react'
import { CircleAlert, Globe2, LoaderCircle, LocateFixed, Router, Save } from 'lucide-react'
import type { LocationDiscoverySettings, LocationDiscoveryState } from '../../../preload'
import { getNodeApi } from '@/lib/node-api'
import { LiquidSwitch } from '@/components/ui/liquid-switch'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { normalizeStunServer } from '../../../shared/stun-server.ts'
import { toast } from 'sonner'
import type { getMessages } from '@/i18n/messages'

type Messages = ReturnType<typeof getMessages>
const disabledSettings: LocationDiscoverySettings = { locateGateway: false, locateCountry: false, stunServer: '' }

export function LocationDiscoverySettingsCard({ messages }: { messages: Messages }) {
  const labels = messages.locationDiscoverySettings
  const [state, setState] = useState<LocationDiscoveryState>()
  const [form, setForm] = useState<LocationDiscoverySettings>(disabledSettings)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const hasLoadedSettings = useRef(false)

  useEffect(() => {
    let active = true
    const api = getNodeApi()
    const refresh = () => {
      void api.getLocationDiscoverySettings().then((result) => {
        if (!active) return
        setState(result)
        if (!hasLoadedSettings.current) {
          hasLoadedSettings.current = true
          setForm(result.saved)
        }
      }).catch((error) => {
        if (active) toast.error(labels.loadFailed, { description: error instanceof Error ? error.message : String(error) })
      }).finally(() => { if (active) setLoading(false) })
    }
    let wasConnected: boolean | undefined
    const unsubscribe = api.onSnapshot((snapshot) => {
      const connected = snapshot.status === 'connected'
      if (connected !== wasConnected) {
        wasConnected = connected
        refresh()
      }
    })
    refresh()
    return () => { active = false; unsubscribe() }
  }, [labels.loadFailed])

  const dirty = Boolean(state && (form.locateGateway !== state.saved.locateGateway || form.locateCountry !== state.saved.locateCountry || form.stunServer !== state.saved.stunServer))
  const restartNeeded = Boolean(state && ((state.gatewaySupported === true && state.saved.locateGateway !== state.running.locateGateway) || state.saved.locateCountry !== state.running.locateCountry || state.saved.stunServer !== state.running.stunServer))
  const stunServerValid = (() => { try { normalizeStunServer(form.stunServer); return true } catch { return false } })()
  function change(key: 'locateGateway' | 'locateCountry', value: boolean) {
    setForm((current) => ({ ...current, [key]: value }))
  }
  async function save() {
    if (!state?.managed || !dirty || !stunServerValid) return
    setSaving(true)
    try {
      const next = await getNodeApi().saveLocationDiscoverySettings(form)
      setState(next)
      setForm(next.saved)
      toast.success(labels.saved)
    } catch (error) {
      toast.error(labels.saveFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally { setSaving(false) }
  }

  const row = (key: 'gateway' | 'lookup', checked: boolean, Icon: typeof Router, unsupported = false) => {
    const settingKey = key === 'gateway' ? 'locateGateway' : 'locateCountry'
    const title = key === 'gateway' ? labels.gatewayTitle : labels.lookupTitle
    const description = key === 'gateway' ? labels.gatewayDescription : labels.lookupDescription
    return <div key={key} className="settings-row settings-row--divider flex-wrap">
      <div className="flex min-w-0 flex-1 items-start gap-3">
        <span className="grid size-10 shrink-0 place-items-center rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)] text-[var(--app-muted)]"><Icon className="size-[18px]" /></span>
        <div className="min-w-0"><div className="text-sm font-medium">{title}</div><p className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{description}</p>{unsupported && <p className="mt-1 text-xs text-muted-foreground">{labels.gatewayUnavailable}</p>}</div>
      </div>
      <LiquidSwitch checked={checked} disabled={loading || !state?.managed || unsupported} label={title} onChange={(value) => change(settingKey, value)} />
    </div>
  }

  return <Card className="settings-card min-w-0">
    <CardContent className="p-4 sm:p-5">
      <div className="flex items-start gap-3">
        <div className="settings-row__icon"><Globe2 className="size-4" /></div>
        <div className="min-w-0 flex-1"><h3 className="text-sm font-semibold">{labels.title}</h3><p className="mt-1 text-sm leading-relaxed text-muted-foreground">{labels.description}</p></div>
      </div>
      {loading ? <div role="status" className="flex items-center gap-2 py-7 text-sm text-muted-foreground"><LoaderCircle className="size-4 animate-spin" />{messages.connecting}</div> : <>
        {!state?.managed && <div role="note" className="mt-4 flex items-start gap-2.5 rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)]/45 p-3 text-sm leading-relaxed text-muted-foreground"><CircleAlert className="mt-0.5 size-4 shrink-0" />{labels.operatorManaged}</div>}
        {row('gateway', form.locateGateway, Router, state?.gatewaySupported === false)}
        {row('lookup', form.locateCountry, Globe2)}
        <div className="settings-row settings-row--divider flex-wrap">
          <div className="flex min-w-0 flex-1 items-start gap-3">
            <span className="grid size-10 shrink-0 place-items-center rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)] text-[var(--app-muted)]"><LocateFixed className="size-[18px]" /></span>
            <div className="min-w-0"><label htmlFor="location-stun-server" className="text-sm font-medium">{labels.stunTitle}</label><p className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{labels.stunDescription}</p></div>
          </div>
          <Input id="location-stun-server" dir="ltr" autoComplete="off" spellCheck={false} maxLength={260} value={form.stunServer} disabled={loading || !state?.managed} onChange={(event) => setForm((current) => ({ ...current, stunServer: event.target.value }))} placeholder={labels.stunPlaceholder} aria-label={labels.stunTitle} aria-invalid={!stunServerValid} className="w-full max-w-sm text-left font-mono text-sm" />
        </div>
        {!stunServerValid && <p role="alert" className="text-xs text-destructive">{labels.stunInvalid}</p>}
        {state?.managed && <div className="mt-3 flex flex-col-reverse justify-between gap-3 border-t border-[var(--app-line)] pt-4 sm:flex-row sm:items-center">
          <p role="status" className="text-xs leading-relaxed text-muted-foreground">{restartNeeded ? labels.restartRequired : labels.restartHint}</p>
          <Button type="button" disabled={!dirty || saving || !stunServerValid} onClick={() => void save()} className="gap-2"><Save className="size-4" />{saving ? <LoaderCircle className="size-4 animate-spin" /> : null}{labels.save}</Button>
        </div>}
      </>}
    </CardContent>
  </Card>
}
