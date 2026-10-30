import { countSettingChanges } from '@/lib/settings-save'
import { SettingsSaveAction } from '@/components/ui/settings-save-dock'
import { SettingsSectionSkeleton } from '@/components/ui/settings-section-skeleton'
import { SettingsIcon } from '@/components/ui/settings-icon'
import { useEffect, useRef, useState } from 'react'
import { CircleAlert, Globe2, LoaderCircle, LocateFixed, Radar, Router, Save } from 'lucide-react'
import type { LocationDiscoverySettings, LocationDiscoveryState } from '../../../preload'
import { getNodeApi } from '@/lib/node-api'
import { sharedStateDomainCode } from '@/lib/shared-state-domain'
import { SharedStateDomain } from '@sisyphus/client'
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
type SharedLocationDiscoveryEvent = {
  domain: number | string
  error?: string
  snapshot?: boolean
  state?: { case?: string; value?: LocationDiscoveryState } | string
  locationDiscovery?: LocationDiscoveryState
}

function normalizedSettings(settings: LocationDiscoverySettings): LocationDiscoverySettings {
  try { return { ...settings, stunServer: normalizeStunServer(settings.stunServer) } }
  catch { return settings }
}

export function LocationDiscoverySettingsCard({ messages }: { messages: Messages }) {
  const labels = messages.locationDiscoverySettings
  const [state, setState] = useState<LocationDiscoveryState>()
  const [form, setForm] = useState<LocationDiscoverySettings>(disabledSettings)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const stateRef = useRef(state)
  const formRef = useRef(form)
  stateRef.current = state
  formRef.current = form

  useEffect(() => {
    let active = true
    const api = getNodeApi()
    let cancel: (() => void) | undefined
    let retry: number | undefined
    let attempt = 0
    let receivedStreamState = false
    let fallbackRequested = false
    let retryAfterFallback = false
    let managementMetadata: Pick<LocationDiscoveryState, 'managed' | 'configurable'> | undefined
    const apply = (next: LocationDiscoveryState) => {
      if (!active) return
      const previous = stateRef.current
      const unchanged = !previous || countSettingChanges(normalizedSettings(formRef.current), normalizedSettings(previous.saved)) === 0
      const resolved = managementMetadata
        ? { ...next, ...managementMetadata }
        : { ...next, managed: false, configurable: false }
      stateRef.current = resolved
      setState(resolved)
      if (unchanged) {
        formRef.current = resolved.saved
        setForm(resolved.saved)
      }
      if (managementMetadata) setLoading(false)
    }
    const loadFallback = (preserveStreamSnapshot = false, reportFailure = true) => {
      if (!active || managementMetadata) return
      if (fallbackRequested) {
        if (preserveStreamSnapshot) retryAfterFallback = true
        return
      }
      fallbackRequested = true
      void api.getLocationDiscoverySettings().then((current) => {
        if (!active) return
        managementMetadata = { managed: current.managed, configurable: current.configurable }
        const latest = stateRef.current
        apply(preserveStreamSnapshot && receivedStreamState && latest ? latest : current)
      }).catch((error) => {
        if (active && reportFailure) toast.error(labels.loadFailed, { description: error instanceof Error ? error.message : String(error) })
      }).finally(() => {
        fallbackRequested = false
        if (active) setLoading(false)
        if (active && retryAfterFallback && !managementMetadata) {
          retryAfterFallback = false
          loadFallback(true, false)
        }
      })
    }
    const disconnected = () => {
      cancel = undefined
      loadFallback(true, false)
      if (active) retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
    }
    const connect = () => {
      if (!active) return
      cancel = getNodeApi().stream<SharedLocationDiscoveryEvent>('watchSharedState', {
        domains: [SharedStateDomain.LOCATION_DISCOVERY],
      }, (event) => {
        if (sharedStateDomainCode(event.domain) !== 'locationDiscovery') return
        if (!event.snapshot) attempt = 0
        if (event.error) {
          loadFallback(false, false)
          return
        }
        const next = event.state && typeof event.state === 'object' && event.state.case === 'locationDiscovery'
          ? event.state.value
          : event.locationDiscovery
        if (next) {
          receivedStreamState = true
          apply(next)
          // The stream omits Electron-only capability metadata. If the first
          // management read raced daemon startup and failed, a live snapshot
          // is the event that lets us safely retry it without polling.
          if (!managementMetadata) loadFallback(true, false)
        }
      }, disconnected, disconnected)
    }
    // The stream owns daemon settings; this one-shot read supplies Electron-only
    // management metadata that is intentionally absent from the protocol.
    connect()
    loadFallback(true)
    return () => {
      active = false
      if (retry !== undefined) window.clearTimeout(retry)
      cancel?.()
    }
  }, [labels.loadFailed])

  const changeCount = state ? countSettingChanges(normalizedSettings(form), normalizedSettings(state.saved)) : 0
  const dirty = changeCount > 0
  const restartNeeded = state?.restartNeeded === true
  const stunServerValid = (() => { try { normalizeStunServer(form.stunServer); return true } catch { return false } })()
  function change(key: 'locateGateway' | 'locateCountry', value: boolean) {
    const next = { ...formRef.current, [key]: value }
    formRef.current = next
    setForm(next)
  }
  async function save() {
    if (!state?.configurable || !dirty || !stunServerValid) return
    setSaving(true)
    try {
      const next = await getNodeApi().saveLocationDiscoverySettings(form)
      stateRef.current = next
      formRef.current = next.saved
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
          <FloatingLabelInput label={labels.stunTitle} icon={<SettingsIcon icon={LocateFixed} compact />} containerClassName="w-full sm:max-w-sm" id="location-stun-server" dir="ltr" autoComplete="off" spellCheck={false} maxLength={260} value={form.stunServer} disabled={loading || !state?.configurable} onChange={(event) => { const next = { ...formRef.current, stunServer: event.target.value }; formRef.current = next; setForm(next) }} placeholder={labels.stunPlaceholder} aria-label={labels.stunTitle} errorMessage={!stunServerValid ? labels.stunInvalid : undefined} className="font-mono" />
        </div>
        {state?.configurable && <div className="mt-3 flex flex-col-reverse justify-between gap-3 border-t border-[var(--app-line)] pt-4 sm:flex-row sm:items-center">
          <p role="status" className="text-xs leading-relaxed text-muted-foreground">{restartNeeded ? (state.managed ? labels.restartRequired : labels.restartDaemonRequired) : (state.managed ? labels.restartHint : labels.restartDaemonHint)}</p>
          <div className="flex flex-wrap gap-2">{restartNeeded && state.managed && <RestartNodeButton messages={messages} />}<SettingsSaveAction onRevert={() => { if (state) { formRef.current = state.saved; setForm(state.saved) } }} dirty={dirty} changeCount={changeCount} disabled={!dirty || saving || !stunServerValid} onSave={save}><Save className="size-4" />{saving ? <LoaderCircle className="size-4 animate-spin" /> : null}{labels.save}</SettingsSaveAction></div>
        </div>}
      </>}
  </NodeSettingsSection>
}
