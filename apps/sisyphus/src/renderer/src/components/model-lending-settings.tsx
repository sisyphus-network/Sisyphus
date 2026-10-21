import { useEffect, useMemo, useState } from 'react'
import { Check, CircleAlert, Cpu, Globe2, LoaderCircle, MemoryStick, RefreshCw, ServerCog, SlidersHorizontal } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { getNodeApi } from '@/lib/node-api'
import { toast } from 'sonner'
import { RestartNodeButton } from '@/components/restart-node-button'
import type { getMessages } from '@/i18n/messages'

type Messages = ReturnType<typeof getMessages>
type Lending = { enabled: boolean; ollamaUrl: string; only: string[]; gpuLayers: number; threads: number; context: number; keepAlive: string }
type LendingState = { saved?: Lending; running?: Lending; restartNeeded?: boolean; setByFlags?: boolean }
type LoadedModel = { name: string; onCardPercent: number }
const emptyLending: Lending = { enabled: false, ollamaUrl: 'http://127.0.0.1:11434', only: [], gpuLayers: -1, threads: 0, context: 0, keepAlive: '' }

// withDefaults fills in what the node leaves out of lending never set up: it
// says an empty address, which would blank the field, so the local Ollama's
// address stands in for it.
function withDefaults(saved?: Partial<Lending>): Lending {
  const lending = { ...emptyLending, ...saved }
  if (!lending.ollamaUrl?.trim()) lending.ollamaUrl = emptyLending.ollamaUrl
  return lending
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

  useEffect(() => {
    let active = true
    void getNodeApi().call<LendingState>('getModelLending').then((result) => {
      if (!active) return
      const saved = withDefaults(result.saved)
      setState(result)
      setForm(saved)
      setSupported(true)
      // Lending that is on says at once what its Ollama has, without being asked.
      if (saved.enabled) void inspect(saved.ollamaUrl)
    }).catch(() => { if (active) setSupported(false) }).finally(() => { if (active) setLoading(false) })
    return () => { active = false }
  }, [loads])

  const selected = useMemo(() => new Set(form.only), [form.only])
  const dirty = Boolean(state?.saved && JSON.stringify(withDefaults(state.saved)) !== JSON.stringify(form))
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

  async function inspect(url = form.ollamaUrl) {
    setInspecting(true)
    try {
      const result = await getNodeApi().call<{ version?: string; error?: string; models?: string[]; loaded?: LoadedModel[] }>('inspectOllama', { url, only: [] })
      if (result.error) throw new Error(result.error)
      setAvailable(result.models ?? [])
      setLoaded(result.loaded ?? [])
      setVersion(result.version ?? '')
    } catch (error) {
      setAvailable([]); setLoaded([]); setVersion('')
      toast.error(labels.inspectFailed, { description: error instanceof Error ? error.message : String(error) })
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
    ? <div role="status" className="flex items-center gap-3 py-8 text-sm text-muted-foreground"><LoaderCircle className="size-4 animate-spin" />{messages.connecting}</div>
    : <Card><CardContent className="flex items-center gap-3 py-8 text-sm text-muted-foreground"><LoaderCircle className="size-4 animate-spin" />{messages.connecting}</CardContent></Card>
  if (!supported) return null

  const header = embedded
    ? <div className="flex items-start gap-3">
      <div className="grid size-9 shrink-0 place-items-center rounded-xl bg-[var(--app-wash)]"><Cpu className="size-4 text-muted-foreground" /></div>
      <div className="min-w-0 flex-1"><CardTitle className="text-sm">{labels.lendingTitle}</CardTitle><CardDescription className="mt-1 text-xs">{labels.lendingDescription} {labels.lendingLiveNote}</CardDescription></div>
      {state?.restartNeeded && <Badge variant="outline" className="shrink-0">{labels.restartNeeded}</Badge>}
    </div>
    : <CardHeader className="flex flex-row items-start gap-3 border-b border-[var(--app-line)] py-4">
      <div className="grid size-9 shrink-0 place-items-center rounded-xl bg-[var(--app-wash)]"><Cpu className="size-4 text-muted-foreground" /></div>
      <div className="min-w-0 flex-1"><CardTitle className="text-sm">{labels.lendingTitle}</CardTitle><CardDescription className="mt-1 text-xs">{labels.lendingDescription} {labels.lendingLiveNote}</CardDescription></div>
      {state?.restartNeeded && <Badge variant="outline" className="shrink-0">{labels.restartNeeded}</Badge>}
    </CardHeader>
  const controls = <div className="space-y-5">
      {state?.setByFlags && <div role="status" className="rounded-xl border border-amber-500/30 bg-amber-500/5 p-3 text-xs leading-relaxed text-amber-800 dark:text-amber-200">{labels.setByFlags}</div>}
      <label className="flex cursor-pointer items-center gap-3 rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)]/45 p-3 text-sm">
        <input type="checkbox" checked={form.enabled} onChange={(event) => change('enabled', event.target.checked)} className="size-4 accent-[var(--app-accent)]" />
        <span className="min-w-0 flex-1">{labels.lendingEnabled}</span>
        <Badge variant={form.enabled ? 'secondary' : 'outline'}>{form.enabled ? messages.connected : messages.offline}</Badge>
      </label>
      <div className="grid min-w-0 gap-4 sm:grid-cols-[minmax(0,1fr)_auto] sm:items-end">
        <label className="min-w-0 space-y-1.5 text-xs"><span>{labels.ollamaUrl}</span><Input dir="ltr" disabled={inspecting} value={form.ollamaUrl} onChange={(event) => change('ollamaUrl', event.target.value)} placeholder="http://127.0.0.1:11434" /></label>
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
      <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <label className="space-y-1.5 text-xs"><span className="flex items-center gap-1.5"><MemoryStick className="size-3.5" />{labels.gpuLayers}</span><Input type="number" min={-1} value={form.gpuLayers} onChange={(event) => change('gpuLayers', Number(event.target.value))} /></label>
        <label className="space-y-1.5 text-xs"><span className="flex items-center gap-1.5"><Cpu className="size-3.5" />{labels.threads}</span><Input type="number" min={0} value={form.threads} onChange={(event) => change('threads', Number(event.target.value))} /></label>
        <label className="space-y-1.5 text-xs"><span className="flex items-center gap-1.5"><SlidersHorizontal className="size-3.5" />{labels.context}</span><Input type="number" min={0} value={form.context} onChange={(event) => change('context', Number(event.target.value))} /></label>
        <label className="space-y-1.5 text-xs"><span className="flex items-center gap-1.5"><ServerCog className="size-3.5" />{labels.keepAlive}</span><Input dir="ltr" value={form.keepAlive} onChange={(event) => change('keepAlive', event.target.value)} placeholder="10m" /></label>
      </div>
      <details className="rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)]/35 p-3">
        <summary className="cursor-pointer text-xs font-semibold">{labels.ollamaOptions}</summary>
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
      </details>
      <div className="flex flex-col-reverse items-stretch justify-between gap-3 border-t border-[var(--app-line)] pt-4 sm:flex-row sm:items-center">
        <p className="text-xs text-muted-foreground">{state?.restartNeeded ? labels.restartNeeded : state?.running?.enabled ? labels.lendingRunning : labels.lendingStopped}</p>
        <div className="flex flex-wrap gap-2">{state?.restartNeeded && <RestartNodeButton messages={messages} onRestarted={() => setLoads((n) => n + 1)} />}<Button type="button" disabled={!dirty || saving} onClick={() => void save()} className="gap-2">{saving && <LoaderCircle className="size-4 animate-spin" />}{labels.saveLending}</Button></div>
      </div>
  </div>

  return embedded
    ? <section className="space-y-5">{header}{controls}</section>
    : <Card>{header}<CardContent className="space-y-5 pt-4">{controls}</CardContent></Card>
}
