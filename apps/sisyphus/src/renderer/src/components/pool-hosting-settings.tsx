import { countSettingChanges } from '@/lib/settings-save'
import { SettingsSaveAction } from '@/components/ui/settings-save-dock'
import { SettingsSectionSkeleton } from '@/components/ui/settings-section-skeleton'
import { SettingsIcon } from '@/components/ui/settings-icon'
import { useEffect, useState } from 'react'
import { Cable, CircleAlert, EthernetPort, LoaderCircle, Network, Save, TriangleAlert } from 'lucide-react'
import type { PoolHostingSettings, PoolHostingState } from '../../../preload'
import { getNodeApi } from '@/lib/node-api'
import { LiquidSwitch } from '@/components/ui/liquid-switch'
import { Button } from '@/components/ui/button'
import { NodeSettingsSection } from '@/components/node-settings-section'
import { FloatingLabelInput } from '@/components/ui/floating-label-input'
import { NetworkInterfacePicker } from '@/components/network-interface-picker'
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
  const changeCount = state ? countSettingChanges({ ...form, port }, formForHosting(state)) : 0
  const dirty = changeCount > 0
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

  return <NodeSettingsSection title={labels.title} description={labels.description} icon={Network} action={<LiquidSwitch checked={form.enabled} disabled={loading || !editable} label={labels.title} onChange={(enabled) => setForm((current) => ({ ...current, enabled }))} />}>
            {loading ? <SettingsSectionSkeleton label={messages.connecting} rows={2} /> : <>
        {state && !state.managed && <div role="note" className="mt-4 flex items-start gap-2.5 rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)]/45 p-3 text-sm leading-relaxed text-muted-foreground"><CircleAlert className="mt-0.5 size-4 shrink-0" />{labels.notManaged}</div>}
        {state?.unavailable && <div role="alert" className="mt-4 flex items-start gap-2.5 rounded-xl border border-destructive/40 p-3 text-sm leading-relaxed text-destructive"><TriangleAlert className="mt-0.5 size-4 shrink-0" />{formatMessage(labels.addressUnavailable, { address: state.running.address || state.saved.address })}</div>}
        <p className="mt-3 text-xs leading-relaxed text-muted-foreground">{labels.discoveryNote}</p>
        <div className="settings-row settings-row--divider flex-wrap">
          <div className="flex min-w-0 flex-1 items-start gap-3"><SettingsIcon icon={EthernetPort} /><div className="min-w-0"><label htmlFor="pool-hosting-address" className="text-sm font-medium">{labels.addressTitle}</label><p className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{labels.addressDescription}</p></div></div>
          <div className="w-full sm:max-w-sm"><NetworkInterfacePicker id="pool-hosting-address" selectionMode="single" label={labels.addressTitle} placeholder={messages.networkInterfacePicker.choose} searchPlaceholder={messages.networkInterfacePicker.search} emptyLabel={messages.networkInterfacePicker.noMatches} customLabel={labels.addressGone} doneLabel={messages.networkInterfacePicker.done} disabled={!editable} interfaces={[...choices.map((entry) => ({ name: entry.interface, address: entry.address })), { name: labels.allInterfaces, address: allInterfaces }]} value={form.address ? [form.address] : []} onChange={(update) => setForm((current) => { const previous = current.address ? [current.address] : []; const selected = typeof update === 'function' ? update(previous) : update; return { ...current, address: selected[0] ?? '' } })} /></div>
        </div>
        {form.address === allInterfaces && <p role="note" className="flex items-start gap-2 text-xs leading-relaxed text-amber-700 dark:text-amber-400"><TriangleAlert className="mt-0.5 size-3.5 shrink-0" />{labels.allInterfacesWarning}</p>}
        {addressMissing && <p role="alert" className="text-xs text-destructive">{labels.addressRequired}</p>}
        <div className="settings-row settings-row--divider flex-wrap">
          <div className="flex min-w-0 flex-1 items-start gap-3"><SettingsIcon icon={Cable} /><div className="min-w-0"><label htmlFor="pool-hosting-port" className="text-sm font-medium">{labels.portTitle}</label><p className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{labels.portDescription}</p></div></div>
          <FloatingLabelInput label={labels.portTitle} icon={<SettingsIcon icon={Cable} compact />} containerClassName="w-full sm:w-48" id="pool-hosting-port" dir="ltr" inputMode="numeric" autoComplete="off" value={portText} disabled={!editable} onChange={(event) => setPortText(event.target.value.trim())} errorMessage={!portValid ? labels.portInvalid : undefined} className="font-mono" />
        </div>
        {state?.managed && <div className="mt-3 flex flex-col-reverse justify-between gap-3 border-t border-[var(--app-line)] pt-4 sm:flex-row sm:items-center">
          <p role="status" className="text-xs leading-relaxed text-muted-foreground">{state.restartNeeded ? labels.restartRequired : labels.restartHint}</p>
          <div className="flex flex-wrap gap-2">{state.restartNeeded && <RestartNodeButton messages={messages} onRestarted={() => setLoads((n) => n + 1)} />}<SettingsSaveAction onRevert={() => { if (state) { const initial = formForHosting(state); setForm(initial); setPortText(String(initial.port)) } }} dirty={dirty} changeCount={changeCount} disabled={!dirty || saving || !portValid || addressMissing} onSave={save}><Save className="size-4" />{saving ? <LoaderCircle className="size-4 animate-spin" /> : null}{labels.save}</SettingsSaveAction></div>
        </div>}
      </>}
  </NodeSettingsSection>
}
