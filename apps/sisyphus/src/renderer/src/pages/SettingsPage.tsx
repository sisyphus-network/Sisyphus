import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { Check, ChevronRight, Download, Globe2, Languages, LoaderCircle, Palette, Plus, QrCode, ServerCog, Trash2 } from 'lucide-react'
import { Link, useLocation, useParams } from 'react-router-dom'
import { LanguagePicker } from '@/components/language-picker'
import { ThemeModeSwitch } from '@/components/theme-mode-switch'
import { ThemeStylePicker } from '@/components/theme-style-picker'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import type { AppLocale } from '@/i18n/locales'
import { type getMessages } from '@/i18n/messages'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import { getNodeApi, isDesktopApp } from '@/lib/node-api'
import { toast } from 'sonner'
import { PageHeading } from '@/components/ui/page-layout'
import { missingProviderKey } from '@/lib/provider-key'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Drawer } from 'vaul'
import { RemoteAccessSettings } from '@/pages/RemoteAccessSettings'
import { ModelLendingSettings } from '@/components/model-lending-settings'
import { BrowserAccessPage } from '@/pages/BrowserAccessPage'

type Messages = ReturnType<typeof getMessages>
type SettingsCategory = 'node' | 'access' | 'language' | 'models' | 'appearance'
export function SettingsPage({ locale, setLocale, messages, direction, category: categoryOverride }: { locale: AppLocale; setLocale: (locale: AppLocale) => void; messages: Messages; direction: 'ltr' | 'rtl'; category?: string }) {
  const labels = messages.modelSettings
  const location = useLocation()
  const { category: routeCategory } = useParams()
  const category = categoryOverride ?? routeCategory
  const activeCategory = category && ['node', 'access', 'language', 'models', 'appearance'].includes(category) ? category as SettingsCategory : undefined
  const scrollRef = useRef<HTMLDivElement>(null)
  const [providers, setProviders] = useState<{ id: string; name: string; about: string; defaultUrl: string; needsKey: string | number; fetchesModels: boolean }[]>([])
  const [profiles, setProfiles] = useState<{ id: string; name: string; provider: string; baseUrl: string; model: string; hasApiKey: boolean; active: boolean }[]>([])
  const [profileOpen, setProfileOpen] = useState(false)
  const [compactViewport, setCompactViewport] = useState(false)
  const [editingProfile, setEditingProfile] = useState('')
  const [profileName, setProfileName] = useState('')
  const [provider, setProvider] = useState('')
  const [baseUrl, setBaseUrl] = useState('')
  const [model, setModel] = useState('')
  const [apiKey, setApiKey] = useState('')
  const [hasApiKey, setHasApiKey] = useState(false)
  const [models, setModels] = useState<{ name: string; label: string; sizeBytes: string | number; tools: string | number }[]>([])
  const [loadingModels, setLoadingModels] = useState(false)
  const [downloadModel, setDownloadModel] = useState('')
  const [pulling, setPulling] = useState(false)
  const [pullProgress, setPullProgress] = useState(0)
  const [pullStatus, setPullStatus] = useState('')
  const pullCancel = useRef<(() => void) | null>(null)
  const currentProvider = providers.find((item) => item.id === provider)
  const editProvider = providers.find((item) => item.id === provider)
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
      getNodeApi().call<{ profiles?: typeof profiles }>('listModelProfiles'),
    ]).then(([providerResult, config, profileResult]) => {
      if (!active) return
      const nextProviders = providerResult.providers ?? []
      setProviders(nextProviders)
      const selectedProvider = nextProviders.find((item) => item.id === config.provider) ?? nextProviders[0]
      setProvider(config.provider || selectedProvider?.id || '')
      setBaseUrl(config.baseUrl || selectedProvider?.defaultUrl || '')
      setModel(config.model ?? '')
      setHasApiKey(Boolean(config.hasApiKey))
      setSavedService({ provider: config.provider ?? '', baseUrl: config.baseUrl ?? '' })
      setProfiles(profileResult.profiles ?? [])
    }).catch(() => { /* The local daemon may not be running yet. */ })
    return () => { active = false }
  }, [])

  useEffect(() => {
    const media = window.matchMedia('(max-width: 767px)')
    const update = () => setCompactViewport(media.matches)
    update()
    media.addEventListener('change', update)
    return () => media.removeEventListener('change', update)
  }, [])

  function beginProfileEdit(profile?: typeof profiles[number]) {
    const selected = profile ? providers.find((item) => item.id === profile.provider) : providers[0]
    setEditingProfile(profile?.id ?? '')
    setProfileName(profile?.name ?? selected?.name ?? '')
    setProvider(profile?.provider ?? selected?.id ?? '')
    setBaseUrl(profile?.baseUrl ?? selected?.defaultUrl ?? '')
    setModel(profile?.model ?? '')
    setApiKey('')
    setHasApiKey(Boolean(profile?.hasApiKey))
    setSavedService({ provider: profile?.provider ?? '', baseUrl: profile?.baseUrl ?? '' })
    setProfileOpen(true)
  }

  function handleProfileOpenChange(open: boolean) {
    if (!open) {
      const activeProfile = profiles.find((item) => item.active)
      if (activeProfile) {
        setProvider(activeProfile.provider); setBaseUrl(activeProfile.baseUrl); setModel(activeProfile.model)
        setHasApiKey(activeProfile.hasApiKey); setSavedService({ provider: activeProfile.provider, baseUrl: activeProfile.baseUrl })
      }
      setApiKey('')
    }
    setProfileOpen(open)
  }

  async function saveProfile(makeActive = false) {
    if (!profileName.trim() || !provider || !validateKey()) return
    const shouldBeActive = makeActive || Boolean(profiles.find((item) => item.id === editingProfile)?.active)
    try {
      const saved = await getNodeApi().call<typeof profiles[number]>('saveModelProfile', {
        id: editingProfile || crypto.randomUUID(), name: profileName.trim(), provider, baseUrl, model, apiKey,
        keepApiKey: !apiKey && hasApiKey && provider === savedService.provider && sameAddress(baseUrl, savedService.baseUrl, editProvider?.defaultUrl ?? ''),
        active: shouldBeActive,
      })
      setProfiles((current) => [...current.filter((item) => item.id !== saved.id).map((item) => ({ ...item, active: shouldBeActive ? false : item.active })), saved])
      if (shouldBeActive) {
        setHasApiKey(saved.hasApiKey)
        setSavedService({ provider: saved.provider, baseUrl: saved.baseUrl })
        setApiKey('')
        setProvider(saved.provider); setBaseUrl(saved.baseUrl); setModel(saved.model)
      } else {
        const activeProfile = profiles.find((item) => item.active)
        if (activeProfile) {
          setProvider(activeProfile.provider); setBaseUrl(activeProfile.baseUrl); setModel(activeProfile.model)
          setHasApiKey(activeProfile.hasApiKey); setSavedService({ provider: activeProfile.provider, baseUrl: activeProfile.baseUrl })
        }
      }
      setProfileOpen(false)
      toast.success(labels.saved)
    } catch (error) { toast.error(labels.saveFailed, { description: error instanceof Error ? error.message : String(error) }) }
  }

  async function activateProfile(profile: typeof profiles[number]) {
    try {
      const saved = await getNodeApi().call<typeof profiles[number]>('saveModelProfile', { ...profile, apiKey: '', keepApiKey: profile.hasApiKey, active: true })
      setProfiles((all) => all.map((item) => ({ ...item, active: item.id === saved.id })))
      setProvider(saved.provider); setBaseUrl(saved.baseUrl); setModel(saved.model); setHasApiKey(saved.hasApiKey)
      setSavedService({ provider: saved.provider, baseUrl: saved.baseUrl })
      toast.success(labels.saved)
    } catch (error) { toast.error(labels.saveFailed, { description: error instanceof Error ? error.message : String(error) }) }
  }

  async function deleteProfile(profile: typeof profiles[number]) {
    if (profile.active) return
    try {
      await getNodeApi().call('deleteModelProfile', { id: profile.id })
      setProfiles((all) => all.filter((item) => item.id !== profile.id))
    } catch (error) { toast.error(labels.removeFailed, { description: error instanceof Error ? error.message : String(error) }) }
  }

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
      toast.error(labels.listFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally { setLoadingModels(false) }
  }

  function startModelPull(profile: typeof profiles[number]) {
    const name = downloadModel.trim()
    if (!name || pulling) return
    setPullStatus(labels.starting)
    setPullProgress(0)
    setPulling(true)
    pullCancel.current = getNodeApi().stream<{ status: string; completedBytes?: string | number; totalBytes?: string | number }>(
      'pullModel',
      { service: { provider: profile.provider, baseUrl: profile.baseUrl, apiKey: '', keepApiKey: profile.hasApiKey }, model: name },
      (event) => {
        setPullStatus(event.status)
        const total = Number(event.totalBytes ?? 0)
        const complete = Number(event.completedBytes ?? 0)
        if (total > 0) setPullProgress(Math.min(100, complete / total * 100))
      },
      (error) => {
        setPulling(false)
        pullCancel.current = null
        toast.error(labels.downloadFailed, { description: error })
      },
      () => {
        setPulling(false)
        pullCancel.current = null
        setPullProgress(100)
        setPullStatus(labels.complete)
        toast.success(labels.downloaded)
        void getNodeApi().call<{ models?: string[]; details?: typeof models }>('listModels', {
          service: { provider: profile.provider, baseUrl: profile.baseUrl, apiKey: '', keepApiKey: profile.hasApiKey },
        }).then((result) => setModels(result.details ?? (result.models ?? []).map((modelName) => ({ name: modelName, label: '', sizeBytes: 0, tools: 0 }))))
          .catch(() => { /* The download succeeded; model listing can be retried from the provider editor. */ })
      },
    )
  }

  function cancelModelPull() {
    pullCancel.current?.()
    pullCancel.current = null
    setPulling(false)
    setPullStatus(labels.cancelled)
  }
  useLayoutEffect(() => {
    if (!window.matchMedia('(min-width: 761px)').matches) return
    const scroller = scrollRef.current
    if (!scroller) return
    const key = 'sisyphus-page-scroll-positions'
    let disposed = false
    let restorePending = false
    let savedTop = 0
    const resizeObserver = new ResizeObserver(() => restore())
    const mutationObserver = new MutationObserver(() => {
      observeChildren()
      restore()
    })
    const save = () => {
      if (restorePending) return
      try {
        const positions: unknown = JSON.parse(window.localStorage.getItem(key) ?? '{}')
        const stored = positions && typeof positions === 'object' ? positions as Record<string, unknown> : {}
        window.localStorage.setItem(key, JSON.stringify({ ...stored, [location.pathname]: scroller.scrollTop }))
      } catch { /* Storage may be unavailable in restricted contexts. */ }
    }
    const finishRestore = () => {
      restorePending = false
      resizeObserver.disconnect()
      mutationObserver.disconnect()
    }
    const observeChildren = () => {
      for (const child of scroller.children) resizeObserver.observe(child)
    }
    const restore = () => {
      if (disposed || !restorePending) return
      const maxTop = Math.max(0, scroller.scrollHeight - scroller.clientHeight)
      scroller.scrollTop = Math.min(savedTop, maxTop)
      // Settings data can hydrate after the route mounts. Keep the original
      // position pending until the content is tall enough to restore it.
      if (scroller.scrollTop >= savedTop - 1) finishRestore()
    }
    try {
      const positions: unknown = JSON.parse(window.localStorage.getItem(key) ?? '{}')
      const value = positions && typeof positions === 'object' ? (positions as Record<string, unknown>)[location.pathname] : undefined
      savedTop = typeof value === 'number' && Number.isFinite(value) && value > 0 ? value : 0
    } catch { /* Ignore malformed saved scroll positions. */ }
    restorePending = savedTop > 0
    if (restorePending) {
      resizeObserver.observe(scroller)
      observeChildren()
      mutationObserver.observe(scroller, { childList: true, characterData: true, subtree: true })
    }
    restore()
    scroller.addEventListener('scroll', save, { passive: true })
    return () => {
      disposed = true
      resizeObserver.disconnect()
      mutationObserver.disconnect()
      save()
      scroller.removeEventListener('scroll', save)
    }
  }, [location.pathname])

  const profileForm = <div className="space-y-4 p-5 sm:p-6">
    <label className="block space-y-1.5 text-xs"><span>{labels.profileName}</span><Input value={profileName} onChange={(event) => setProfileName(event.target.value)} autoFocus /></label>
    <label className="block space-y-1.5 text-xs"><span>{labels.provider}</span><select value={provider} onChange={(event) => { const item = providers.find((entry) => entry.id === event.target.value); setProvider(event.target.value); setApiKey(''); setHasApiKey(false); if (item) setBaseUrl(item.defaultUrl) }} className="h-10 w-full rounded-xl border border-[var(--app-line)] bg-background px-3 text-sm">{providers.map((item) => <option value={item.id} key={item.id}>{item.name}</option>)}</select></label>
    <label className="block space-y-1.5 text-xs"><span>{labels.url}</span><Input dir="ltr" value={baseUrl} onChange={(event) => setBaseUrl(event.target.value)} placeholder={editProvider?.defaultUrl} /></label>
    {editProvider?.needsKey !== 2 && editProvider?.needsKey !== 'SUPPORT_NO' && <label className="block space-y-1.5 text-xs"><span>{labels.apiKey} {hasApiKey && !apiKey ? labels.keySaved : ''}</span><Input dir="ltr" type="password" autoComplete="new-password" value={apiKey} onChange={(event) => setApiKey(event.target.value)} placeholder={hasApiKey ? labels.keepKey : labels.enterKey} /></label>}
    {editProvider?.about && <p dir="auto" className="text-xs leading-relaxed text-muted-foreground">{editProvider.about}</p>}
    <label className="block space-y-1.5 text-xs"><span>{labels.model}</span><div className="flex gap-2"><Input aria-label={labels.model} list="available-models" value={model} onChange={(event) => setModel(event.target.value)} placeholder={labels.choose} className="min-w-0 flex-1" /><datalist id="available-models">{models.map((item) => <option key={item.name} value={item.name}>{item.label || item.name}</option>)}</datalist><Button type="button" variant="outline" disabled={!provider || loadingModels} onClick={() => void refreshModels()}>{loadingModels ? <LoaderCircle className="size-4 animate-spin" /> : labels.load}</Button></div></label>
    <div className="flex flex-col-reverse gap-2 border-t border-[var(--app-line)] pt-4 sm:flex-row sm:justify-end"><Button type="button" variant="outline" onClick={() => handleProfileOpenChange(false)}>{messages.cancel}</Button><Button type="button" disabled={!profileName.trim() || !provider} onClick={() => void saveProfile(false)}>{labels.saveProfile}</Button><Button type="button" disabled={!profileName.trim() || !provider || !model} onClick={() => void saveProfile(true)}>{labels.saveAndActivate}</Button></div>
  </div>

  const categoryMeta: Record<SettingsCategory, { title: string; description: string; icon: typeof Globe2 }> = {
    node: { title: messages.localNodeSettings, description: messages.localDaemonSettingsHint, icon: Globe2 },
    access: { title: messages.webAccess.title, description: messages.webAccess.exposureDescription, icon: QrCode },
    language: { title: messages.languageSettings, description: messages.languageSettingsHint, icon: Languages },
    models: { title: labels.title, description: labels.hint, icon: ServerCog },
    appearance: { title: messages.theme, description: messages.themeSettingsHint, icon: Palette },
  }
  const categories = (Object.keys(categoryMeta) as SettingsCategory[]).filter((item) => item !== 'access' || isDesktopApp())

  return <div className="settings-page">
    <div className="settings-page__heading"><PageHeading title={activeCategory ? categoryMeta[activeCategory].title : messages.settings} description={activeCategory ? categoryMeta[activeCategory].description : messages.settingsDescription} /></div>
    <div ref={scrollRef} className="settings-page__scroll">
    <div className="settings-page__content grid w-full min-w-0 gap-4">
      {!activeCategory ? <nav aria-label={messages.settings} className="settings-category-grid">
        {categories.map((item) => { const Icon = categoryMeta[item].icon; return <Link key={item} to={`/settings/${item}`} className="settings-category-card"><span className="settings-category-card__icon"><Icon className="size-5" /></span><span className="settings-category-card__copy"><span className="settings-category-card__title">{categoryMeta[item].title}</span><span className="settings-category-card__description">{categoryMeta[item].description}</span></span><ChevronRight className="settings-category-card__arrow" /></Link> })}
      </nav> : <div className="settings-category-content">
      {activeCategory === 'node' && <Card className="settings-card"><CardHeader className="settings-card__header"><div className="settings-card__icon"><Globe2 className="size-5" /></div><div><div className="settings-card__eyebrow">{messages.node}</div><CardTitle>{messages.localNodeSettings}</CardTitle></div></CardHeader><CardContent><div className="settings-row"><div className="settings-row__icon"><ServerCog className="size-4" /></div><div className="min-w-0 flex-1"><div className="text-sm font-medium">{messages.localDaemon}</div><div className="mt-1 text-sm text-muted-foreground">{messages.localDaemonSettingsHint}</div></div><span className="settings-status">{messages.managedByDaemon}</span></div></CardContent></Card>}
      {activeCategory === 'access' && isDesktopApp() && <><RemoteAccessSettings messages={messages} /><BrowserAccessPage messages={messages} showHeading={false} /></>}
      {activeCategory === 'language' && <Card className="settings-card settings-card--popover"><CardHeader className="settings-card__header"><div className="settings-card__icon"><Languages className="size-5" /></div><div><div className="settings-card__eyebrow">{messages.language}</div><CardTitle>{messages.languageSettings}</CardTitle></div></CardHeader><CardContent><div className="max-w-md"><LanguagePicker value={locale} label={messages.language} onChange={setLocale} /></div><p className="mt-3 text-sm text-muted-foreground">{messages.languageSettingsHint}</p></CardContent></Card>}
      {activeCategory === 'appearance' && <Card className="settings-card settings-card--popover"><CardHeader className="settings-card__header"><div className="settings-card__icon"><Palette className="size-5" /></div><div><div className="settings-card__eyebrow">{messages.appearance}</div><CardTitle>{messages.theme}</CardTitle></div></CardHeader><CardContent><div className="settings-row"><div><div className="text-sm font-medium">{messages.theme}</div><p className="mt-1 text-sm text-muted-foreground">{messages.themeSettingsHint}</p></div><ThemeModeSwitch direction={direction} labels={{ system: messages.themeSystem, light: messages.themeLight, dark: messages.themeDark, switchTheme: messages.switchTheme }} /></div><div className="settings-row settings-row--divider"><div><div className="text-sm font-medium">{messages.accent}</div><p className="mt-1 text-sm text-muted-foreground">{messages.accentSettingsHint}</p></div><div className="w-full max-w-md"><ThemeStylePicker direction={direction} labels={{ label: messages.accent, neutral: messages.accentNeutral, blue: messages.accentBlue, pink: messages.accentPink }} /></div></div></CardContent></Card>}
      {activeCategory === 'models' && <Card className="settings-card settings-card--wide"><CardHeader className="settings-card__header"><div className="settings-card__icon"><ServerCog className="size-5" /></div><div><div className="settings-card__eyebrow">{labels.localAi}</div><CardTitle>{labels.title}</CardTitle></div><Badge variant="outline" className="ms-auto">{labels.stored}</Badge></CardHeader><CardContent className="space-y-4 pt-4">
        <div className="flex flex-wrap items-center justify-between gap-3"><p className="text-xs text-muted-foreground">{labels.hint}</p><Button type="button" variant="outline" onClick={() => beginProfileEdit()} className="gap-2"><Plus className="size-4" />{labels.addProfile}</Button></div>
        <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
          {profiles.map((profile) => <Card key={profile.id} className={`min-w-0 rounded-2xl transition-colors ${profile.active ? 'border-[var(--app-accent)] ring-1 ring-[var(--app-accent)]/25' : 'hover:border-[var(--app-accent)]/40'}`}><CardContent className="space-y-3 p-4"><div className="flex items-start gap-3"><div className="grid size-10 shrink-0 place-items-center rounded-xl bg-[var(--app-wash)]"><ServerCog className="size-4 text-[var(--app-accent)]" /></div><div className="min-w-0 flex-1"><div className="truncate text-sm font-semibold">{profile.name}</div><div className="truncate text-xs text-muted-foreground">{providers.find((item) => item.id === profile.provider)?.name ?? profile.provider}</div></div>{profile.active && <Badge className="gap-1"><Check className="size-3" />{labels.active}</Badge>}</div><div className="rounded-lg bg-[var(--app-wash)]/70 px-3 py-2 text-xs"><div className="truncate font-medium">{profile.model || labels.noModel}</div><div dir="ltr" className="mt-1 truncate text-[10px] text-muted-foreground">{profile.baseUrl}</div></div><div className="flex items-center gap-2"><Button type="button" variant="outline" size="sm" className="flex-1" onClick={() => beginProfileEdit(profile)}>{labels.editProfile}</Button>{!profile.active && <Button type="button" size="sm" className="flex-1" disabled={!profile.model} onClick={() => void activateProfile(profile)}>{labels.setActive}</Button>}<Button type="button" variant="ghost" size="icon" aria-label={`${messages.remove} ${profile.name}`} disabled={profile.active} onClick={() => void deleteProfile(profile)}><Trash2 className="size-4" /></Button></div>{profile.active && providers.find((item) => item.id === profile.provider)?.fetchesModels && <div className="space-y-2 border-t border-[var(--app-line)] pt-3"><div className="flex flex-wrap items-center gap-2"><Input aria-label={labels.downloadPlaceholder} value={downloadModel} onChange={(event) => setDownloadModel(event.target.value)} placeholder={labels.downloadPlaceholder} className="min-w-0 flex-1" /><Button type="button" variant="outline" disabled={pulling || !downloadModel.trim()} onClick={() => startModelPull(profile)} className="gap-2"><Download className="size-4" />{messages.operationText.download}</Button>{pulling && <Button type="button" variant="ghost" onClick={cancelModelPull}>{messages.cancel}</Button>}</div>{pulling && <div className="space-y-1.5"><div className="flex justify-between gap-3 text-[10px] text-muted-foreground"><span className="truncate">{pullStatus}</span><span>{Math.round(pullProgress)}%</span></div><div role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pullProgress)} className="h-2 overflow-hidden rounded-full bg-[var(--app-wash)]"><div className="h-full rounded-full bg-[var(--app-accent)] transition-[width]" style={{ width: `${pullProgress}%` }} /></div></div>}</div>}</CardContent></Card>)}
          {profiles.length === 0 && <button type="button" onClick={() => beginProfileEdit()} className="grid min-h-36 place-items-center rounded-2xl border border-dashed border-[var(--app-line)] p-5 text-center text-xs text-muted-foreground transition-colors hover:border-[var(--app-accent)]"><span><Plus className="mx-auto mb-2 size-5" />{labels.addProfile}</span></button>}
        </div>
      </CardContent></Card>}
      {isDesktopApp() && activeCategory === 'models' && <ModelLendingSettings messages={messages} />}
      </div>}
    </div>
    </div>
    {compactViewport ? <Drawer.Root open={profileOpen} onOpenChange={handleProfileOpenChange} direction="bottom"><Drawer.Portal><Drawer.Overlay className="fixed inset-0 z-50 bg-black/30 backdrop-blur-sm" /><Drawer.Content dir={direction} className="liquid-glass-menu fixed inset-x-0 bottom-0 z-50 max-h-[92dvh] overflow-y-auto rounded-t-[1.5rem] bg-[var(--app-card)] pb-[max(1rem,env(safe-area-inset-bottom))] outline-none"><div aria-hidden="true" className="mx-auto mt-3 h-1 w-10 rounded-full bg-[var(--app-line)]" /><Drawer.Title className="px-5 pt-4 text-lg font-semibold">{editingProfile ? labels.editProfile : labels.addProfile}</Drawer.Title><Drawer.Description className="px-5 pt-1 text-xs text-muted-foreground">{labels.profileDescription}</Drawer.Description>{profileForm}</Drawer.Content></Drawer.Portal></Drawer.Root> : <Dialog open={profileOpen} onOpenChange={handleProfileOpenChange}><DialogContent className="liquid-glass-menu max-h-[min(90dvh,52rem)] w-[min(38rem,calc(100vw-2rem))] max-w-none overflow-y-auto rounded-[1.5rem] border border-[var(--app-line)] bg-[var(--app-card)] p-0"><DialogHeader className="px-6 pt-6"><DialogTitle>{editingProfile ? labels.editProfile : labels.addProfile}</DialogTitle><DialogDescription>{labels.profileDescription}</DialogDescription></DialogHeader>{profileForm}</DialogContent></Dialog>}
  </div>
}
