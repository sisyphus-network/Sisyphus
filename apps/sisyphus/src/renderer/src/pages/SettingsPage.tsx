import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { Download, Globe2, Languages, LoaderCircle, Palette, ServerCog, Trash2 } from 'lucide-react'
import { LanguagePicker } from '@/components/language-picker'
import { ThemeModeSwitch } from '@/components/theme-mode-switch'
import { ThemeStylePicker } from '@/components/theme-style-picker'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import type { AppLocale } from '@/i18n/locales'
import { type getMessages } from '@/i18n/messages'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import { getNodeApi } from '@/lib/node-api'
import { toast } from 'sonner'
import { PageHeading } from '@/components/ui/page-layout'
import { missingProviderKey } from '@/lib/provider-key'

type Messages = ReturnType<typeof getMessages>
export function SettingsPage({ locale, setLocale, messages, direction }: { locale: AppLocale; setLocale: (locale: AppLocale) => void; messages: Messages; direction: 'ltr' | 'rtl' }) {
  const scrollRef = useRef<HTMLDivElement>(null)
  const [providers, setProviders] = useState<{ id: string; name: string; about: string; defaultUrl: string; needsKey: string | number; fetchesModels: boolean }[]>([])
  const [provider, setProvider] = useState('')
  const [baseUrl, setBaseUrl] = useState('')
  const [model, setModel] = useState('')
  const [apiKey, setApiKey] = useState('')
  const [hasApiKey, setHasApiKey] = useState(false)
  const [models, setModels] = useState<{ name: string; label: string; sizeBytes: string | number; tools: string | number }[]>([])
  const [loadingModels, setLoadingModels] = useState(false)
  const [savingModel, setSavingModel] = useState(false)
  const [downloadModel, setDownloadModel] = useState('')
  const [pullStatus, setPullStatus] = useState('')
  const [pullProgress, setPullProgress] = useState(0)
  const [pulling, setPulling] = useState(false)
  const pullCancel = useRef<(() => void) | null>(null)
  const currentProvider = providers.find((item) => item.id === provider)
  // The node keeps a key for one service: the provider and address it was
  // saved with. Pointed anywhere else, this page must neither claim a key
  // is saved nor ask the node to reuse it.
  const [savedService, setSavedService] = useState({ provider: '', baseUrl: '' })
  const sameAddress = (a: string, b: string, fallback: string) => (a || fallback).replace(/\/+$/, '') === (b || fallback).replace(/\/+$/, '')
  const keyIsForThisService = hasApiKey && provider === savedService.provider && sameAddress(baseUrl, savedService.baseUrl, currentProvider?.defaultUrl ?? '')
  const keepApiKey = !apiKey && keyIsForThisService
  const keyMissing = missingProviderKey(currentProvider?.needsKey, apiKey, keepApiKey)
  const validateKey = () => {
    if (!keyMissing) return true
    toast.error(messages.providerKeyRequired)
    return false
  }

  useEffect(() => {
    let active = true
    Promise.all([
      getNodeApi().call<{ providers?: typeof providers }>('listProviders'),
      getNodeApi().call<{ provider?: string; baseUrl?: string; model?: string; hasApiKey?: boolean }>('getModelConfig'),
    ]).then(([providerResult, config]) => {
      if (!active) return
      const nextProviders = providerResult.providers ?? []
      setProviders(nextProviders)
      const selectedProvider = nextProviders.find((item) => item.id === config.provider) ?? nextProviders[0]
      setProvider(config.provider || selectedProvider?.id || '')
      setBaseUrl(config.baseUrl || selectedProvider?.defaultUrl || '')
      setModel(config.model ?? '')
      setHasApiKey(Boolean(config.hasApiKey))
      setSavedService({ provider: config.provider ?? '', baseUrl: config.baseUrl ?? '' })
    }).catch(() => { /* The local daemon may not be running yet. */ })
    return () => { active = false; pullCancel.current?.() }
  }, [])

  async function refreshModels() {
    if (!provider || !validateKey()) return
    setLoadingModels(true)
    try {
      const result = await getNodeApi().call<{ models?: string[]; details?: typeof models }>('listModels', { service: { provider, baseUrl, apiKey, keepApiKey } })
      const details = result.details ?? (result.models ?? []).map((name) => ({ name, label: '', sizeBytes: 0, tools: 0 }))
      setModels(details)
      if (details.some((item) => item.name === model)) return
      if (details.length && !model) setModel(details[0].name)
    } catch (error) {
      toast.error('Could not list models', { description: error instanceof Error ? error.message : String(error) })
    } finally { setLoadingModels(false) }
  }
  async function saveModel() {
    if (!validateKey()) return
    setSavingModel(true)
    try {
      const result = await getNodeApi().call<{ hasApiKey?: boolean }>('setModelConfig', { provider, baseUrl, model, apiKey, keepApiKey })
      // The node says whether a key is now saved: it drops the old one
      // when the service changed and no new one was given.
      setHasApiKey(Boolean(result.hasApiKey))
      setSavedService({ provider, baseUrl })
      setApiKey('')
      toast.success('Model configuration saved')
    } catch (error) {
      toast.error('Could not save model configuration', { description: error instanceof Error ? error.message : String(error) })
    } finally { setSavingModel(false) }
  }
  function startModelPull() {
    if (!provider || !downloadModel.trim() || pulling || !validateKey()) return
    setPullStatus('Starting download…')
    setPullProgress(0)
    setPulling(true)
    pullCancel.current = getNodeApi().stream<{ status: string; completedBytes?: string | number; totalBytes?: string | number }>('pullModel', { service: { provider, baseUrl, apiKey, keepApiKey }, model: downloadModel.trim() }, (event) => {
      setPullStatus(event.status)
      const total = Number(event.totalBytes ?? 0)
      const complete = Number(event.completedBytes ?? 0)
      if (total > 0) setPullProgress(Math.min(100, complete / total * 100))
    }, (error) => {
      setPulling(false)
      pullCancel.current = null
      toast.error('Model download failed', { description: error })
    }, () => {
      setPulling(false)
      pullCancel.current = null
      setPullProgress(100)
      setPullStatus('Download complete')
      void refreshModels()
      toast.success('Model downloaded')
    })
  }
  function cancelModelPull() {
    pullCancel.current?.()
    pullCancel.current = null
    setPulling(false)
    setPullStatus('Download cancelled')
  }
  async function removeModel(name: string) {
    if (!validateKey()) return
    try {
      await getNodeApi().call('removeModel', { service: { provider, baseUrl, apiKey, keepApiKey }, model: name })
      setModels((current) => current.filter((item) => item.name !== name))
      toast.success('Model removed')
    } catch (error) { toast.error('Could not remove model', { description: error instanceof Error ? error.message : String(error) }) }
  }
  useLayoutEffect(() => {
    if (!window.matchMedia('(min-width: 761px)').matches) return
    const scroller = scrollRef.current
    if (!scroller) return
    const key = 'sisyphus-page-scroll-positions'
    const save = () => {
      try {
        const positions: unknown = JSON.parse(window.localStorage.getItem(key) ?? '{}')
        const stored = positions && typeof positions === 'object' ? positions as Record<string, unknown> : {}
        window.localStorage.setItem(key, JSON.stringify({ ...stored, '/settings': scroller.scrollTop }))
      } catch { /* Storage may be unavailable in restricted contexts. */ }
    }
    try {
      const positions: unknown = JSON.parse(window.localStorage.getItem(key) ?? '{}')
      if (positions && typeof positions === 'object' && typeof (positions as Record<string, unknown>)['/settings'] === 'number') {
        scroller.scrollTop = (positions as Record<string, number>)['/settings']
      }
    } catch { /* Ignore malformed saved scroll positions. */ }
    scroller.addEventListener('scroll', save, { passive: true })
    return () => {
      save()
      scroller.removeEventListener('scroll', save)
    }
  }, [])

  return <div className="settings-page">
    <div className="settings-page__heading"><PageHeading title={messages.settings} description={messages.settingsDescription} /></div>
    <div ref={scrollRef} className="settings-page__scroll">
    <div className="grid w-full min-w-0 gap-4">
      <Card><CardHeader className="flex flex-row items-center gap-3 border-b border-[var(--app-line)] py-4"><div className="grid size-9 place-items-center rounded-xl bg-[var(--app-wash)]"><Globe2 className="size-4 text-muted-foreground" /></div><div><div className="text-[9px] font-semibold tracking-[0.13em] text-muted-foreground">{messages.node}</div><CardTitle className="mt-0.5 text-sm">{messages.localNodeSettings}</CardTitle></div></CardHeader><CardContent><div className="flex items-center gap-3 py-2"><div className="grid size-8 place-items-center rounded-lg border border-[var(--app-line)]"><ServerCog className="size-4 text-muted-foreground" /></div><div className="min-w-0 flex-1"><div className="text-xs font-medium">{messages.localDaemon}</div><div className="mt-1 text-[10px] text-muted-foreground">{messages.localDaemonSettingsHint}</div></div><span className="rounded-full border border-emerald-500/25 bg-emerald-500/5 px-2 py-1 text-[9px] text-emerald-700 dark:text-emerald-300">{messages.managedByDaemon}</span></div></CardContent></Card>
      <Card><CardHeader className="flex flex-row items-center gap-3 border-b border-[var(--app-line)] py-4"><div className="grid size-9 place-items-center rounded-xl bg-[var(--app-wash)]"><Languages className="size-4 text-muted-foreground" /></div><div><div className="text-[9px] font-semibold tracking-[0.13em] text-muted-foreground">{messages.language}</div><CardTitle className="mt-0.5 text-sm">{messages.languageSettings}</CardTitle></div></CardHeader><CardContent><div className="max-w-sm py-2"><LanguagePicker value={locale} label={messages.language} onChange={setLocale} /></div><p className="mt-2 text-[10px] text-muted-foreground">{messages.languageSettingsHint}</p></CardContent></Card>
      <Card><CardHeader className="flex flex-row items-center gap-3 border-b border-[var(--app-line)] py-4"><div className="grid size-9 place-items-center rounded-xl bg-[var(--app-wash)]"><ServerCog className="size-4 text-muted-foreground" /></div><div><div className="text-[9px] font-semibold tracking-[0.13em] text-muted-foreground">LOCAL AI</div><CardTitle className="mt-0.5 text-sm">Model provider</CardTitle></div><Badge variant="outline" className="ms-auto">Stored on node</Badge></CardHeader><CardContent className="space-y-4 pt-4">
        <p className="text-xs text-muted-foreground">Choose the model provider used by the planner. API keys are sent directly to the local daemon and are never displayed again.</p>
        <div className="grid gap-3 sm:grid-cols-2"><label className="space-y-1.5 text-xs"><span>Provider</span><select value={provider} onChange={(event) => { const next = providers.find((item) => item.id === event.target.value); setProvider(event.target.value); setApiKey(''); if (next) setBaseUrl(next.defaultUrl); setModels([]) }} className="h-10 w-full rounded-xl border border-[var(--app-line)] bg-background px-3 text-sm">{providers.length === 0 && <option value="">Daemon unavailable</option>}{providers.map((item) => <option value={item.id} key={item.id}>{item.name}</option>)}</select></label><label className="space-y-1.5 text-xs"><span>Service URL</span><Input dir="ltr" value={baseUrl} onChange={(event) => setBaseUrl(event.target.value)} placeholder={currentProvider?.defaultUrl || 'http://localhost:11434'} /></label></div>
        {currentProvider?.needsKey !== 2 && currentProvider?.needsKey !== 'SUPPORT_NO' && <label className="block space-y-1.5 text-xs"><span>API key {keyIsForThisService && !apiKey ? '· saved on node' : ''}</span><Input dir="ltr" type="password" autoComplete="new-password" value={apiKey} onChange={(event) => setApiKey(event.target.value)} placeholder={keyIsForThisService ? 'Leave empty to keep saved key' : 'Enter provider API key'} /></label>}
        {currentProvider?.about && <p dir="auto" className="text-xs leading-relaxed text-muted-foreground">{currentProvider.about}</p>}
        {keyMissing && <p role="status" className="text-xs text-destructive">{messages.providerKeyRequired}</p>}
        <div className="flex flex-wrap items-center gap-2"><Button type="button" variant="outline" disabled={!provider || loadingModels || keyMissing} onClick={() => void refreshModels()} className="gap-2">{loadingModels ? <LoaderCircle className="size-4 animate-spin" /> : null}Load models</Button><select aria-label="Model" value={model} onChange={(event) => setModel(event.target.value)} className="h-10 min-w-48 flex-1 rounded-xl border border-[var(--app-line)] bg-background px-3 text-sm"><option value="">Choose model</option>{models.map((item) => <option key={item.name} value={item.name} disabled={item.tools === 'SUPPORT_NO' || item.tools === 2}>{item.label || item.name}{Number(item.sizeBytes) > 0 ? ` · ${(Number(item.sizeBytes) / 1024 ** 3).toFixed(1)} GB` : ''}</option>)}</select><Button type="button" disabled={!provider || !model || savingModel || keyMissing} onClick={() => void saveModel()}>{savingModel ? 'Saving…' : 'Save model'}</Button></div>
        {models.length > 0 && <div className="space-y-1 rounded-xl border border-[var(--app-line)] px-3 py-2">{models.map((item) => <div key={item.name} className="flex items-center gap-2 py-1 text-xs"><span className="min-w-0 flex-1 truncate">{item.label || item.name}</span>{item.tools === 'SUPPORT_NO' || item.tools === 2 ? <Badge variant="outline">No tools</Badge> : null}{currentProvider?.fetchesModels && <Button type="button" size="icon" variant="ghost" className="size-7" aria-label={`Remove ${item.name}`} onClick={() => void removeModel(item.name)}><Trash2 className="size-3.5" /></Button>}</div>)}</div>}
        {currentProvider?.fetchesModels && <div className="flex flex-wrap items-center gap-2 border-t border-[var(--app-line)] pt-3"><Input value={downloadModel} onChange={(event) => setDownloadModel(event.target.value)} placeholder="Model name to download (e.g. llama3.1:8b)" className="min-w-48 flex-1" /><Button type="button" variant="outline" disabled={pulling || !downloadModel.trim() || keyMissing} onClick={startModelPull} className="gap-2"><Download className="size-4" />Download</Button>{pulling && <Button type="button" variant="ghost" onClick={cancelModelPull}>Cancel</Button>}</div>}
        {pulling && <div className="space-y-1.5"><div className="flex justify-between gap-3 text-[10px] text-muted-foreground"><span className="truncate">{pullStatus}</span><span>{Math.round(pullProgress)}%</span></div><div role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pullProgress)} className="h-2 overflow-hidden rounded-full bg-[var(--app-wash)]"><div className="h-full rounded-full bg-[var(--app-accent)] transition-[width]" style={{ width: `${pullProgress}%` }} /></div></div>}
      </CardContent></Card>
      <Card><CardHeader className="flex flex-row items-center gap-3 border-b border-[var(--app-line)] py-4"><div className="grid size-9 place-items-center rounded-xl bg-[var(--app-wash)]"><Palette className="size-4 text-muted-foreground" /></div><div><div className="text-[9px] font-semibold tracking-[0.13em] text-muted-foreground">{messages.appearance}</div><CardTitle className="mt-0.5 text-sm">{messages.theme}</CardTitle></div></CardHeader><CardContent><div className="flex flex-wrap items-center justify-between gap-5 py-2"><div><div className="text-xs font-medium">{messages.theme}</div><p className="mt-1 text-[10px] text-muted-foreground">{messages.themeSettingsHint}</p></div><ThemeModeSwitch direction={direction} labels={{ system: messages.themeSystem, light: messages.themeLight, dark: messages.themeDark, switchTheme: messages.switchTheme }} /></div><div className="mt-3 flex flex-wrap items-center justify-between gap-5 border-t border-[var(--app-line)] pt-4"><div><div className="text-xs font-medium">{messages.accent}</div><p className="mt-1 text-[10px] text-muted-foreground">{messages.accentSettingsHint}</p></div><ThemeStylePicker direction={direction} labels={{ label: messages.accent, neutral: messages.accentNeutral, blue: messages.accentBlue, pink: messages.accentPink }} /></div></CardContent></Card>
    </div>
    </div>
  </div>
}
