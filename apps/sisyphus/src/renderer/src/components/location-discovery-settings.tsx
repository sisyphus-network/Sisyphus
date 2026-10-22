import { countSettingChanges } from '@/lib/settings-save'
import { SettingsSaveAction } from '@/components/ui/settings-save-dock'
import { SettingsSectionSkeleton } from '@/components/ui/settings-section-skeleton'
import { SettingsIcon } from '@/components/ui/settings-icon'
import { useEffect, useRef, useState } from 'react'
import { CircleAlert, Globe2, LoaderCircle, LocateFixed, Radar, Router, Save } from 'lucide-react'
import type { LocationDiscoverySettings, LocationDiscoveryState } from '../../../preload'
import { getNodeApi } from '@/lib/node-api'
import { LiquidSwitch } from '@/components/ui/liquid-switch'
import { Button } from '@/components/ui/button'
import { NodeSettingsSection } from '@/components/node-settings-section'
import { FloatingLabelInput } from '@/components/ui/floating-label-input'
import { normalizeStunServer } from '../../../shared/stun-server.ts'
import { toast } from 'sonner'
import { RestartNodeButton } from '@/components/restart-node-button'
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

  const normalizedStun = (value: string) => { try { return normalizeStunServer(value) } catch { return value } }
  const changeCount = state ? countSettingChanges({ ...form, stunServer: normalizedStun(form.stunServer) }, { ...state.saved, stunServer: normalizedStun(state.saved.stunServer) }) : 0
  const dirty = changeCount > 0
  const restartNeeded = state?.restartNeeded === true
  const stunServerValid = (() => { try { normalizeStunServer(form.stunServer); return true } catch { return false } })()
  function change(key: 'locateGateway' | 'locateCountry', value: boolean) {
    setForm((current) => ({ ...current, [key]: value }))
  }
  async function save() {
    if (!state?.configurable || !dirty || !stunServerValid) return
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
        <SettingsIcon icon={Icon} />
        <div className="min-w-0"><div className="text-sm font-medium">{title}</div><p className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{description}</p>{unsupported && <p className="mt-1 text-xs text-muted-foreground">{labels.gatewayUnavailable}</p>}</div>
      </div>
      <LiquidSwitch checked={checked} disabled={loading || !state?.configurable || unsupported} label={title} onChange={(value) => change(settingKey, value)} />
    </div>
  }

  return <NodeSettingsSection title={labels.title} description={labels.description} icon={Radar}>
            {loading ? <SettingsSectionSkeleton label={messages.connecting} rows={3} /> : <>
        {state && !state.configurable && <div role="note" className="mt-4 flex items-start gap-2.5 rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)]/45 p-3 text-sm leading-relaxed text-muted-foreground"><CircleAlert className="mt-0.5 size-4 shrink-0" />{labels.operatorManaged}</div>}
        {row('gateway', form.locateGateway, Router, state?.gatewaySupported === false)}
        {row('lookup', form.locateCountry, Globe2)}
        <div className="settings-row settings-row--divider flex-wrap">
          <div className="flex min-w-0 flex-1 items-start gap-3">
            <SettingsIcon icon={LocateFixed} />
            <div className="min-w-0"><label htmlFor="location-stun-server" className="text-sm font-medium">{labels.stunTitle}</label><p className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{labels.stunDescription}</p></div>
          </div>
          <FloatingLabelInput label={labels.stunTitle} icon={<SettingsIcon icon={LocateFixed} compact />} containerClassName="w-full sm:max-w-sm" id="location-stun-server" dir="ltr" autoComplete="off" spellCheck={false} maxLength={260} value={form.stunServer} disabled={loading || !state?.configurable} onChange={(event) => setForm((current) => ({ ...current, stunServer: event.target.value }))} placeholder={labels.stunPlaceholder} aria-label={labels.stunTitle} errorMessage={!stunServerValid ? labels.stunInvalid : undefined} className="font-mono" />
        </div>
        {state?.configurable && <div className="mt-3 flex flex-col-reverse justify-between gap-3 border-t border-[var(--app-line)] pt-4 sm:flex-row sm:items-center">
          <p role="status" className="text-xs leading-relaxed text-muted-foreground">{restartNeeded ? (state.managed ? labels.restartRequired : labels.restartDaemonRequired) : (state.managed ? labels.restartHint : labels.restartDaemonHint)}</p>
          <div className="flex flex-wrap gap-2">{restartNeeded && state.managed && <RestartNodeButton messages={messages} />}<SettingsSaveAction onRevert={() => { if (state) setForm(state.saved) }} dirty={dirty} changeCount={changeCount} disabled={!dirty || saving || !stunServerValid} onSave={save}><Save className="size-4" />{saving ? <LoaderCircle className="size-4 animate-spin" /> : null}{labels.save}</SettingsSaveAction></div>
        </div>}
      </>}
  </NodeSettingsSection>
}
