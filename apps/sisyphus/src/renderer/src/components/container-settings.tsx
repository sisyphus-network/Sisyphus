import { countSettingChanges } from '@/lib/settings-save'
import { SettingsSaveAction } from '@/components/ui/settings-save-dock'
import { SettingsSectionSkeleton } from '@/components/ui/settings-section-skeleton'
import { SettingsIcon } from '@/components/ui/settings-icon'
import { useEffect, useState } from 'react'
import { CircleAlert, CircleCheck, Container, ExternalLink, LoaderCircle, MemoryStick, Lock, RefreshCw, Save, TriangleAlert, Wifi } from 'lucide-react'
import type { ContainerSettings, ContainerState } from '../../../preload'
import { getNodeApi } from '@/lib/node-api'
import { LiquidSwitch } from '@/components/ui/liquid-switch'
import { Button } from '@/components/ui/button'
import { NodeSettingsSection } from '@/components/node-settings-section'
import { FloatingLabelInput } from '@/components/ui/floating-label-input'
import { formatMessage, type getMessages } from '@/i18n/messages'
import { toast } from 'sonner'
import { RestartNodeButton } from '@/components/restart-node-button'

type Messages = ReturnType<typeof getMessages>

const maxTmpMb = 65536

/**
 * Whether the node this app starts runs container work for the pool, and
 * how confined that work is. Docker must answer before it can be turned on.
 */
export function ContainerSettingsCard({ messages }: { messages: Messages }) {
  const labels = messages.containerSettings
  const [state, setState] = useState<ContainerState>()
  const [form, setForm] = useState<ContainerSettings>({ enabled: false, network: false, readOnly: true, tmpMb: 1024 })
  const [tmpText, setTmpText] = useState('1024')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [checking, setChecking] = useState(false)

  const [loads, setLoads] = useState(0)
  useEffect(() => {
    let active = true
    void getNodeApi().getContainers().then((result) => {
      if (!active) return
      setState(result)
      setForm(result.saved)
      setTmpText(String(result.saved.tmpMb))
    }).catch((error) => {
      if (active) toast.error(labels.loadFailed, { description: error instanceof Error ? error.message : String(error) })
    }).finally(() => { if (active) setLoading(false) })
    return () => { active = false }
  }, [labels.loadFailed, loads])

  // Asks whether Docker answers now, keeping what the owner has changed.
  async function checkAgain() {
    setChecking(true)
    try { setState(await getNodeApi().getContainers()) }
    catch (error) { toast.error(labels.loadFailed, { description: error instanceof Error ? error.message : String(error) }) }
    finally { setChecking(false) }
  }

  const tmpMb = /^\d+$/.test(tmpText) ? Number(tmpText) : NaN
  const tmpValid = !form.readOnly || (Number.isInteger(tmpMb) && tmpMb >= 1 && tmpMb <= maxTmpMb)
  const next: ContainerSettings = { ...form, tmpMb: !form.readOnly ? state?.saved.tmpMb ?? 1024 : Number.isInteger(tmpMb) && tmpMb >= 1 && tmpMb <= maxTmpMb ? tmpMb : state?.saved.tmpMb ?? 1024 }
  const changeCount = state ? countSettingChanges({ ...form, tmpMb: form.readOnly ? tmpMb : state.saved.tmpMb }, state.saved) : 0
  const dirty = changeCount > 0
  const editable = Boolean(state?.managed) && !saving
  const docker = state?.docker ?? null
  const dockerReady = docker?.state === 'ready'
  // Turning it off never waits on Docker; turning it on does.
  const canSwitch = editable && (form.enabled || dockerReady)

  async function save() {
    if (!state?.managed || !dirty || !tmpValid) return
    setSaving(true)
    try {
      const result = await getNodeApi().saveContainers(next)
      setState(result)
      setForm(result.saved)
      setTmpText(String(result.saved.tmpMb))
      toast.success(labels.saved)
    } catch (error) {
      toast.error(labels.saveFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally { setSaving(false) }
  }

  const option = (id: string, Icon: typeof Wifi, title: string, description: string, checked: boolean, onChange: (value: boolean) => void) =>
    <div className="settings-row settings-row--divider flex-wrap">
      <div className="flex min-w-0 flex-1 items-start gap-3">
        <SettingsIcon icon={Icon} />
        <div className="min-w-0"><div id={id} className="text-sm font-medium">{title}</div><p className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{description}</p></div>
      </div>
      <LiquidSwitch checked={checked} disabled={!editable || !form.enabled} label={title} onChange={onChange} />
    </div>

  return <NodeSettingsSection title={labels.title} description={labels.description} icon={Container} action={<LiquidSwitch checked={form.enabled} disabled={loading || !canSwitch} label={labels.title} onChange={(enabled) => setForm((current) => ({ ...current, enabled }))} />}>
            {loading ? <SettingsSectionSkeleton label={messages.connecting} rows={3} /> : <>
        {state && !state.managed && <div role="note" className="mt-4 flex items-start gap-2.5 rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)]/45 p-3 text-sm leading-relaxed text-muted-foreground"><CircleAlert className="mt-0.5 size-4 shrink-0" />{labels.notManaged}</div>}
        {state?.unavailable && <div role="alert" className="mt-4 flex items-start gap-2.5 rounded-xl border border-destructive/40 p-3 text-sm leading-relaxed text-destructive"><TriangleAlert className="mt-0.5 size-4 shrink-0" />{labels.unavailable}</div>}
        <p className="mt-3 text-xs leading-relaxed text-muted-foreground">{labels.dockerNote}</p>
        {state?.managed && docker && <div className="mt-3 flex flex-wrap items-center gap-3">
          {docker.state === 'ready'
            ? <p role="status" className="flex min-w-0 flex-1 items-start gap-2 text-sm text-muted-foreground"><CircleCheck className="mt-0.5 size-4 shrink-0 text-emerald-600" />{formatMessage(labels.dockerReady, { version: docker.version })}</p>
            : <p role="alert" className="flex min-w-0 flex-1 items-start gap-2 text-sm text-amber-700 dark:text-amber-400"><TriangleAlert className="mt-0.5 size-4 shrink-0" />{docker.state === 'missing' ? labels.dockerMissing : labels.dockerStopped}</p>}
          {docker.state !== 'ready' && <div className="flex flex-wrap gap-2">
            <Button type="button" variant="outline" size="sm" onClick={() => void getNodeApi().openDockerInstall()} className="gap-2"><ExternalLink className="size-4" />{labels.getDocker}</Button>
            <Button type="button" variant="outline" size="sm" disabled={checking} onClick={() => void checkAgain()} className="gap-2">{checking ? <LoaderCircle className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}{labels.checkAgain}</Button>
          </div>}
        </div>}
        {form.enabled && <p role="note" className="mt-3 flex items-start gap-2 text-xs leading-relaxed text-amber-700 dark:text-amber-400"><TriangleAlert className="mt-0.5 size-3.5 shrink-0" />{labels.trustWarning}</p>}
        {option('containers-network', Wifi, labels.networkTitle, labels.networkDescription, form.network, (network) => setForm((current) => ({ ...current, network })))}
        {option('containers-read-only', Lock, labels.readOnlyTitle, labels.readOnlyDescription, form.readOnly, (readOnly) => setForm((current) => ({ ...current, readOnly })))}
        <div className="settings-row settings-row--divider flex-wrap">
          <div className="flex min-w-0 flex-1 items-start gap-3">
            <SettingsIcon icon={MemoryStick} />
            <div className="min-w-0"><label htmlFor="containers-tmp-mb" className="text-sm font-medium">{labels.tmpTitle}</label><p className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">{labels.tmpDescription}</p></div>
          </div>
          <FloatingLabelInput label={labels.tmpTitle} icon={<SettingsIcon icon={MemoryStick} compact />} containerClassName="w-full sm:w-56" id="containers-tmp-mb" dir="ltr" inputMode="numeric" autoComplete="off" value={tmpText} disabled={!editable || !form.enabled || !form.readOnly} onChange={(event) => setTmpText(event.target.value.trim())} errorMessage={!tmpValid ? formatMessage(labels.tmpInvalid, { max: maxTmpMb }) : undefined} className="font-mono" />
        </div>
        {state?.managed && <div className="mt-3 flex flex-col-reverse justify-between gap-3 border-t border-[var(--app-line)] pt-4 sm:flex-row sm:items-center">
          <p role="status" className="text-xs leading-relaxed text-muted-foreground">{state.restartNeeded ? labels.restartRequired : labels.restartHint}</p>
          <div className="flex flex-wrap gap-2">{state.restartNeeded && <RestartNodeButton messages={messages} onRestarted={() => setLoads((n) => n + 1)} />}<SettingsSaveAction onRevert={() => { if (state) { setForm(state.saved); setTmpText(String(state.saved.tmpMb)) } }} dirty={dirty} changeCount={changeCount} disabled={!dirty || saving || !tmpValid} onSave={save}><Save className="size-4" />{saving ? <LoaderCircle className="size-4 animate-spin" /> : null}{labels.save}</SettingsSaveAction></div>
        </div>}
      </>}
  </NodeSettingsSection>
}
