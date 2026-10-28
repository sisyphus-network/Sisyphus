import { countSettingChanges } from '@/lib/settings-save'
import { SettingsSaveAction } from '@/components/ui/settings-save-dock'
import { SettingsIcon } from '@/components/ui/settings-icon'
import { SettingsSectionSkeleton } from '@/components/ui/settings-section-skeleton'
import { useEffect, useMemo, useRef, useState } from 'react'
import { Check, CircleAlert, Cpu, Globe2, Gpu, Layers3, LoaderCircle, RefreshCw, ServerCog, Share2, Timer } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { CardDescription, CardTitle } from '@/components/ui/card'
import { NodeSettingsSection } from '@/components/node-settings-section'
import { FloatingLabelInput } from '@/components/ui/floating-label-input'
import { LiquidSwitch } from '@/components/ui/liquid-switch'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { getNodeApi } from '@/lib/node-api'
import { sharedStateDomainCode } from '@/lib/shared-state-domain'
import { SharedStateDomain } from '@sisyphus/client'
import { toast } from 'sonner'
import { RestartNodeButton } from '@/components/restart-node-button'
import type { getMessages } from '@/i18n/messages'

type Messages = ReturnType<typeof getMessages>
type Lending = { enabled: boolean; ollamaUrl: string; only: string[]; gpuLayers: number; threads: number; context: number; keepAlive: string }
type LendingState = { saved?: Lending; running?: Lending; restartNeeded?: boolean; setByFlags?: boolean }
type LoadedModel = { name: string; onCardPercent: number }
const emptyLending: Lending = { enabled: false, ollamaUrl: 'http://127.0.0.1:11434', only: [], gpuLayers: -1, threads: 0, context: 0, keepAlive: '' }
type SharedLendingEvent = {
  domain: number | string
  error?: string
  snapshot?: boolean
  state?: { case?: string; value?: LendingState } | string
  modelLending?: LendingState
}

// withDefaults fills in what the node leaves out of lending never set up: it
// says an empty address, which would blank the field, so the local Ollama's
// address stands in for it.
function withDefaults(saved?: Partial<Lending>): Lending {
  const lending = { ...emptyLending, ...saved }
  if (!lending.ollamaUrl?.trim()) lending.ollamaUrl = emptyLending.ollamaUrl
  return lending
}

function canonicalLending(value: Lending) {
  return { ...value, only: JSON.stringify([...new Set(value.only)].sort()) }
}

export function ModelLendingSettings({ messages, embedded = false }: { messages: Messages; embedded?: boolean }) {
  const labels = messages.modelSettings
  const [form, setForm] = useState<Lending>(emptyLending)
  const [state, setState] = useState<LendingState>()
  const [available, setAvailable] = useState<string[]>([])
  const [loaded, setLoaded] = useState<LoadedModel[]>([])
  const [version, setVersion] = useState('')
  const [loading, setLoading] = useState(true)
  const [inspecting, setInspecting] = useState(false)
  const [saving, setSaving] = useState(false)
  const [supported, setSupported] = useState(true)
  const [loads, setLoads] = useState(0)
  const stateRef = useRef(state)
  stateRef.current = state

  useEffect(() => {
    let active = true
    let cancel: (() => void) | undefined
    let retry: number | undefined
    let attempt = 0
    const loadFallback = async () => {
      if (!active || stateRef.current) return
      try {
        const result = await getNodeApi().call<LendingState>('getModelLending')
        if (!active) return
        stateRef.current = result
        setState(result)
        setForm(withDefaults(result.saved))
        setSupported(true)
        if (result.saved?.enabled) void inspect(result.saved.ollamaUrl, false)
      } catch {
        if (active) setSupported(false)
      } finally {
        if (active) setLoading(false)
      }
    }
    const disconnected = () => {
      cancel = undefined
      void loadFallback()
      if (active) retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
    }
    const connect = () => {
      if (!active) return
      cancel = getNodeApi().stream<SharedLendingEvent>('watchSharedState', {
        domains: [SharedStateDomain.MODEL_LENDING],
      }, (event) => {
        if (sharedStateDomainCode(event.domain) !== 'modelLending') return
        if (!event.snapshot) attempt = 0
        if (event.error) {
          setSupported(false)
          setLoading(false)
          return
        }
        const next = event.state && typeof event.state === 'object' && event.state.case === 'modelLending'
          ? event.state.value
          : event.modelLending
        if (!next) return
        const previous = stateRef.current
        const saved = withDefaults(next.saved)
        setState(next)
        stateRef.current = next
        setForm((current) => previous && countSettingChanges(canonicalLending(current), canonicalLending(withDefaults(previous.saved))) > 0
          ? current
          : saved)
        setSupported(true)
        setLoading(false)
        if (saved.enabled) void inspect(saved.ollamaUrl, false)
        else { setAvailable([]); setLoaded([]); setVersion('') }
      }, disconnected, disconnected)
    }
    connect()
    return () => {
      active = false
      if (retry !== undefined) window.clearTimeout(retry)
      cancel?.()
    }
  }, [loads])

  const selected = useMemo(() => new Set(form.only), [form.only])
  const changeCount = state ? countSettingChanges(canonicalLending(form), canonicalLending(withDefaults(state.saved))) : 0
  const dirty = changeCount > 0
  const remoteEndpoint = useMemo(() => {
    try {
      const endpoint = new URL(form.ollamaUrl.trim())
      if (endpoint.protocol !== 'http:' && endpoint.protocol !== 'https:') return ''
      const host = endpoint.hostname.replace(/^\[|\]$/g, '').toLowerCase()
      const loopback = host === 'localhost' || host.endsWith('.localhost') || host === '::1' || host.startsWith('127.')
      return loopback ? '' : endpoint.protocol === 'http:' ? 'http' : 'https'
    } catch { return '' }
  }, [form.ollamaUrl])
  function change<K extends keyof Lending>(key: K, value: Lending[K]) { setForm((current) => ({ ...current, [key]: value })) }

  async function inspect(url = form.ollamaUrl, notify = true) {
    setInspecting(true)
    try {
      const result = await getNodeApi().call<{ version?: string; error?: string; models?: string[]; loaded?: LoadedModel[] }>('inspectOllama', { url, only: [] })
      if (result.error) throw new Error(result.error)
      setAvailable(result.models ?? [])
      setLoaded(result.loaded ?? [])
      setVersion(result.version ?? '')
    } catch (error) {
      setAvailable([]); setLoaded([]); setVersion('')
      if (notify) toast.error(labels.inspectFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally { setInspecting(false) }
  }

  function toggleModel(name: string, checked: boolean) {
    const current = form.only.length ? form.only : available
    const next = checked ? [...new Set([...current, name])] : current.filter((item) => item !== name)
    // An empty allow-list means “all models” in the node protocol. If this
    // is the only available model, turning lending off is the way to offer none.
    if (!checked && next.length === 0) return
    change('only', next.length === available.length ? [] : next)
  }

  async function save() {
    setSaving(true)
    try {
      const result = await getNodeApi().call<LendingState>('setModelLending', { lending: form })
      const saved = withDefaults(result.saved)
      setState(result)
      stateRef.current = result
      setForm(saved)
      toast.success(labels.lendingSaved)
      // Saved without checking first: check now, so a wrong address shows
      // before the node is restarted with it.
      if (saved.enabled) void inspect(saved.ollamaUrl)
    } catch (error) {
      toast.error(labels.saveFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally { setSaving(false) }
  }

  if (loading) return embedded
    ? <SettingsSectionSkeleton label={messages.connecting} rows={3} />
    : <NodeSettingsSection title={labels.lendingTitle} description={labels.lendingDescription} icon={Share2}><SettingsSectionSkeleton label={messages.connecting} rows={3} /></NodeSettingsSection>
  if (!supported) return null

  const header = <div className="flex items-start gap-3">
      <div className="grid size-9 shrink-0 place-items-center rounded-xl bg-[var(--app-wash)]"><Cpu className="size-4 text-muted-foreground" /></div>
      <div className="min-w-0 flex-1"><CardTitle className="text-sm">{labels.lendingTitle}</CardTitle><CardDescription className="mt-1 text-xs">{labels.lendingDescription} {labels.lendingLiveNote}</CardDescription></div>
      {state?.restartNeeded && <Badge variant="outline" className="shrink-0">{labels.restartNeeded}</Badge>}
    </div>
  const controls = <div className="space-y-5">
      {state?.setByFlags && <div role="status" className="rounded-xl border border-amber-500/30 bg-amber-500/5 p-3 text-xs leading-relaxed text-amber-800 dark:text-amber-200">{labels.setByFlags}</div>}
      <div className="flex items-center gap-3 rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)]/45 p-3 text-sm">
        <SettingsIcon icon={Share2} /><LiquidSwitch checked={form.enabled} disabled={saving} onChange={(enabled) => change('enabled', enabled)} label={labels.lendingEnabled} />
        <span className="min-w-0 flex-1">{labels.lendingEnabled}</span>
        <Badge variant={form.enabled ? 'secondary' : 'outline'}>{form.enabled ? messages.accessUi.on : messages.accessUi.off}</Badge>
      </div>
      <div className="node-lending-fields grid min-w-0 gap-4 sm:grid-cols-[minmax(0,1fr)_auto] sm:items-end">
        <FloatingLabelInput label={labels.ollamaUrl} icon={<SettingsIcon icon={Globe2} compact />} dir="ltr" disabled={inspecting || saving} value={form.ollamaUrl} onChange={(event) => change('ollamaUrl', event.target.value)} placeholder="http://127.0.0.1:11434" containerClassName="min-w-0" />
        <Button type="button" variant="outline" disabled={inspecting || !form.ollamaUrl.trim()} onClick={() => void inspect()} className="gap-2">{inspecting ? <LoaderCircle className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}{inspecting ? labels.inspecting : labels.inspect}</Button>
      </div>
      {remoteEndpoint && <div role="note" className={`flex items-start gap-2.5 rounded-xl border p-3 text-xs leading-relaxed ${remoteEndpoint === 'http' ? 'border-amber-500/30 bg-amber-500/5 text-amber-800 dark:text-amber-200' : 'border-[var(--app-line)] bg-[var(--app-wash)]/45 text-muted-foreground'}`}>
        {remoteEndpoint === 'http' ? <CircleAlert className="mt-0.5 size-4 shrink-0" /> : <Globe2 className="mt-0.5 size-4 shrink-0" />}
        <span>{remoteEndpoint === 'http' ? labels.remoteOllamaHttpNote : labels.remoteOllamaNote}</span>
      </div>}
      {(version || available.length > 0 || loaded.length > 0) && <div role="status" className="rounded-xl border border-emerald-500/25 bg-emerald-500/5 p-3">
        <div className="flex items-center gap-2 text-xs font-medium text-emerald-700 dark:text-emerald-300"><Check className="size-4" />{version || labels.modelsOffered}: {available.length}</div>
        {available.length > 0 ? <><p className="mt-3 text-xs text-muted-foreground">{labels.allModelsHint}</p><div className="mt-2 grid gap-2 sm:grid-cols-2 xl:grid-cols-3">{available.map((name) => <label key={name} className="flex min-w-0 cursor-pointer items-center gap-2 rounded-lg border border-[var(--app-line)] bg-background/70 px-3 py-2 text-xs"><input type="checkbox" checked={form.only.length === 0 || selected.has(name)} onChange={(event) => toggleModel(name, event.target.checked)} className="size-3.5 accent-[var(--app-accent)]" /><span className="truncate">{name}</span></label>)}</div></> : <p className="mt-2 text-xs text-muted-foreground">{labels.inspectEmpty}</p>}
        {loaded.length > 0 && <div className="mt-3 flex flex-wrap items-center gap-2 text-xs"><span className="font-medium">{labels.loadedModels}</span>{loaded.map((item) => <Badge key={item.name} variant="secondary">{item.name}{item.onCardPercent ? ` · ${item.onCardPercent}% GPU` : ''}</Badge>)}</div>}
      </div>}
      <div className="node-lending-fields grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <FloatingLabelInput label={labels.gpuLayers} icon={<SettingsIcon icon={Gpu} compact />} type="number" min={-1} dir="ltr" disabled={saving} value={form.gpuLayers} onChange={(event) => change('gpuLayers', Number(event.target.value))} />
        <FloatingLabelInput label={labels.threads} icon={<SettingsIcon icon={Cpu} compact />} type="number" min={0} dir="ltr" disabled={saving} value={form.threads} onChange={(event) => change('threads', Number(event.target.value))} />
        <FloatingLabelInput label={labels.context} icon={<SettingsIcon icon={Layers3} compact />} type="number" min={0} dir="ltr" disabled={saving} value={form.context} onChange={(event) => change('context', Number(event.target.value))} />
        <FloatingLabelInput label={labels.keepAlive} icon={<SettingsIcon icon={Timer} compact />} dir="ltr" placeholder="10m" disabled={saving} value={form.keepAlive} onChange={(event) => change('keepAlive', event.target.value)} />
      </div>
      <Collapsible className="rounded-2xl border border-[var(--app-line)] bg-[var(--app-wash)]/35 p-1">
        <CollapsibleTrigger title={labels.ollamaOptions} icon={ServerCog} />
        <CollapsibleContent><div className="px-1 pb-2">
        <dl className="mt-3 grid gap-x-4 gap-y-2 text-xs sm:grid-cols-2">
          <dt className="text-muted-foreground">{labels.ollamaLoadedLimit}</dt><dd className="font-mono">OLLAMA_MAX_LOADED_MODELS</dd>
          <dt className="text-muted-foreground">{labels.ollamaParallel}</dt><dd className="font-mono">OLLAMA_NUM_PARALLEL</dd>
          <dt className="text-muted-foreground">{labels.ollamaGpuOverhead}</dt><dd className="font-mono">OLLAMA_GPU_OVERHEAD</dd>
          <dt className="text-muted-foreground">{labels.ollamaGpuDevices}</dt><dd className="font-mono">CUDA_VISIBLE_DEVICES / ROCR_VISIBLE_DEVICES</dd>
          <dt className="text-muted-foreground">{labels.ollamaKeepAlive}</dt><dd className="font-mono">OLLAMA_KEEP_ALIVE</dd>
          <dt className="text-muted-foreground">{labels.ollamaHost}</dt><dd className="font-mono">OLLAMA_HOST</dd>
        </dl>
        <p className="mt-3 text-xs leading-relaxed text-muted-foreground">{labels.ollamaRestartNote}</p>
        <ul className="mt-2 space-y-1 text-xs leading-relaxed text-muted-foreground"><li>{labels.ollamaWindows}</li><li>{labels.ollamaMac}</li><li>{labels.ollamaLinux}</li></ul>
        </div></CollapsibleContent>
      </Collapsible>
      <div className="flex flex-col-reverse items-stretch justify-between gap-3 border-t border-[var(--app-line)] pt-4 sm:flex-row sm:items-center">
        <p className="text-xs text-muted-foreground">{state?.restartNeeded ? labels.restartNeeded : state?.running?.enabled ? labels.lendingRunning : labels.lendingStopped}</p>
        <div className="flex flex-wrap gap-2">{state?.restartNeeded && <RestartNodeButton messages={messages} onRestarted={() => setLoads((n) => n + 1)} />}<SettingsSaveAction onRevert={() => { if (state) setForm(withDefaults(state.saved)) }} dirty={dirty} changeCount={changeCount} disabled={!dirty || saving} onSave={save}>{saving && <LoaderCircle className="size-4 animate-spin" />}{labels.saveLending}</SettingsSaveAction></div>
      </div>
  </div>

  return embedded
    ? <section className="space-y-5">{header}{controls}</section>
    : <NodeSettingsSection title={labels.lendingTitle} description={`${labels.lendingDescription} ${labels.lendingLiveNote}`} icon={Share2} action={state?.restartNeeded ? <Badge variant="outline">{labels.restartNeeded}</Badge> : undefined}>{controls}</NodeSettingsSection>
}
