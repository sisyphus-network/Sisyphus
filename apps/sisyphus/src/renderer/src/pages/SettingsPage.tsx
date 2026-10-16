import { useCallback, useEffect, useRef, useState } from 'react'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { Check, CheckCircle2, ChevronRight, CircleAlert, Cpu, Download, Globe, Globe2, GlobeX, KeyRound, Languages, Link2, LoaderCircle, Network, Palette, Pencil, Plus, QrCode, Radar, RefreshCw, Search, ServerCog, Settings2, SunMoon, Trash2, UserRound, Wifi } from 'lucide-react'
import { Link, useLocation, useParams } from 'react-router-dom'
import { LanguagePicker } from '@/components/language-picker'
import { ThemeModeSwitch } from '@/components/theme-mode-switch'
import { ThemeStylePicker } from '@/components/theme-style-picker'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import type { AppLocale } from '@/i18n/locales'
import { type getMessages } from '@/i18n/messages'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { FloatingLabelInput } from '@/components/ui/floating-label-input'
import { Badge } from '@/components/ui/badge'
import { getNodeApi, isDesktopApp } from '@/lib/node-api'
import { toast } from 'sonner'
import { PageHeading } from '@/components/ui/page-layout'
import { missingProviderKey } from '@/lib/provider-key'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Drawer } from 'vaul'
import { ModelLendingSettings } from '@/components/model-lending-settings'
import { AccessPage } from '@/pages/AccessPage'
import { ResponsiveSelector } from '@/components/ui/responsive-selector'
import { AnimatedStepper, type AnimatedStepperStep } from '@/components/ui/animated-stepper'
import { ResponsiveActionDialog } from '@/components/ui/responsive-action-dialog'
import ollamaIcon from '@/assets/provider-icons/ollama.svg?url'
import anthropicIcon from '@/assets/provider-icons/anthropic.svg?url'
import openaiIcon from '@/assets/provider-icons/openai.svg?url'

type Messages = ReturnType<typeof getMessages>
type SettingsCategory = 'general' | 'node' | 'access' | 'models'
type ProfileConnectivityStatus = 'checking' | 'connected' | 'error'
type ModelProfile = { id: string; name: string; provider: string; baseUrl: string; model: string; hasApiKey: boolean; active: boolean; previewStatus?: ProfileConnectivityStatus; previewProviderName?: string }

const devPreviewProfiles: ModelProfile[] = [
  { id: 'dev-preview-ollama-local', name: 'Ollama Local', provider: 'ollama', previewProviderName: 'Ollama', baseUrl: 'http://127.0.0.1:11434', model: 'llama3.1:8b', hasApiKey: false, active: true, previewStatus: 'connected' },
  { id: 'dev-preview-ollama-remote', name: 'Ollama Remote', provider: 'ollama', previewProviderName: 'Ollama', baseUrl: 'http://192.168.1.40:11434', model: 'qwen3:8b', hasApiKey: false, active: false, previewStatus: 'connected' },
  { id: 'dev-preview-openai', name: 'OpenAI Production', provider: 'openai', previewProviderName: 'OpenAI', baseUrl: 'https://api.openai.com/v1', model: 'gpt-4.1', hasApiKey: true, active: false, previewStatus: 'connected' },
  { id: 'dev-preview-openai-compatible', name: 'OpenAI Compatible', provider: 'openai', previewProviderName: 'OpenAI compatible', baseUrl: 'https://models.example.ai/v1', model: 'deepseek-chat', hasApiKey: true, active: false, previewStatus: 'checking' },
  { id: 'dev-preview-anthropic', name: 'Anthropic Cloud', provider: 'anthropic', previewProviderName: 'Anthropic', baseUrl: 'https://api.anthropic.com/v1', model: 'claude-sonnet-4-5', hasApiKey: true, active: false, previewStatus: 'connected' },
  { id: 'dev-preview-anthropic-gateway', name: 'Anthropic Gateway', provider: 'anthropic', previewProviderName: 'Anthropic', baseUrl: 'https://gateway.example.ai/v1', model: 'claude-3-5-haiku-latest', hasApiKey: true, active: false, previewStatus: 'error' },
]

function providerServiceIcon(providerId: string): string | undefined {
  switch (providerId.toLowerCase()) {
    case 'ollama': return ollamaIcon
    case 'anthropic': return anthropicIcon
    case 'openai': return openaiIcon
    default: return undefined
  }
}

export function SettingsPage({ locale, setLocale, messages, direction, category: categoryOverride }: { locale: AppLocale; setLocale: (locale: AppLocale) => void; messages: Messages; direction: 'ltr' | 'rtl'; category?: string }) {
  const reduceMotion = useReducedMotion()
  const labels = messages.modelSettings
  const location = useLocation()
  const { category: routeCategory } = useParams()
  const category = categoryOverride ?? routeCategory
  const normalizedCategory = category === 'language' || category === 'appearance' ? 'general' : category
  const activeCategory = normalizedCategory && ['general', 'node', 'access', 'models'].includes(normalizedCategory) ? normalizedCategory as SettingsCategory : undefined
  const scrollRef = useRef<HTMLDivElement>(null)
  const providerToolbarRef = useRef<HTMLDivElement>(null)
  const [providerToolbarStuck, setProviderToolbarStuck] = useState(false)
  const [providers, setProviders] = useState<{ id: string; name: string; about: string; defaultUrl: string; needsKey: string | number; fetchesModels: boolean }[]>([])
  const [profiles, setProfiles] = useState<ModelProfile[]>([])
  const [profileConnectivity, setProfileConnectivity] = useState<Record<string, { status: ProfileConnectivityStatus; modelCount?: number }>>({})
  const profileConnectivitySequence = useRef(0)
  const [profileOpen, setProfileOpen] = useState(false)
  const [compactViewport, setCompactViewport] = useState(false)
  const [profileStep, setProfileStep] = useState(0)
  const [profileNavigationDirection, setProfileNavigationDirection] = useState<1 | -1>(1)
  const [editingProfile, setEditingProfile] = useState('')
  const [profileName, setProfileName] = useState('')
  const [provider, setProvider] = useState('')
  const [baseUrl, setBaseUrl] = useState('')
  const [model, setModel] = useState('')
  const [apiKey, setApiKey] = useState('')
  const [hasApiKey, setHasApiKey] = useState(false)
  const [models, setModels] = useState<{ name: string; label: string; sizeBytes: string | number; tools: string | number }[]>([])
  const [modelSearch, setModelSearch] = useState('')
  const [loadingModels, setLoadingModels] = useState(false)
  const [modelLoadState, setModelLoadState] = useState<'idle' | 'loading' | 'connected' | 'error' | 'key-required'>('idle')
  const modelLoadSequence = useRef(0)
  const [downloadModel, setDownloadModel] = useState('')
  const [removeModelTarget, setRemoveModelTarget] = useState('')
  const [removingModel, setRemovingModel] = useState(false)
  const [pulling, setPulling] = useState(false)
  const [pullProgress, setPullProgress] = useState(0)
  const [pullStatus, setPullStatus] = useState('')
  const pullCancel = useRef<(() => void) | null>(null)
  const currentProvider = providers.find((item) => item.id === provider)
  const currentProviderIcon = providerServiceIcon(provider)
  const displayedProfiles = import.meta.env.DEV ? [...profiles, ...devPreviewProfiles] : profiles
  const editProvider = providers.find((item) => item.id === provider)
  const selectedProviderName = editProvider?.name ?? ''
  const visibleModels = models.filter((item) => `${item.label} ${item.name}`.toLocaleLowerCase(locale).includes(modelSearch.trim().toLocaleLowerCase(locale)))
  const selectedModel = models.find((item) => item.name === model)
  const isOllamaProvider = currentProvider?.id.toLowerCase() === 'ollama'
  const profileSteps: AnimatedStepperStep[] = [
    { id: 'profile', label: labels.profileStep, icon: UserRound },
    { id: 'connection', label: labels.connectionStep, icon: Link2 },
    { id: 'model', label: labels.modelStep, icon: Cpu },
  ]
  const finalProfileStep = profileSteps.length - 1
  const currentStep = profileSteps[Math.min(profileStep, finalProfileStep)]?.id
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
    const toolbar = providerToolbarRef.current
    if (!toolbar || activeCategory !== 'models') {
      setProviderToolbarStuck(false)
      return
    }
    const appScroller = document.querySelector<HTMLElement>('.app-shell--advanced .app-main')
    const updateStickyState = () => {
      if (!appScroller) return setProviderToolbarStuck(false)
      const scroller = appScroller
      const toolbarTop = toolbar.getBoundingClientRect().top
      const scrollerTop = scroller.getBoundingClientRect().top
      setProviderToolbarStuck(toolbarTop <= scrollerTop + 1 && scroller.scrollTop > 0)
    }
    updateStickyState()
    appScroller?.addEventListener('scroll', updateStickyState, { passive: true })
    window.addEventListener('resize', updateStickyState, { passive: true })
    return () => {
      appScroller?.removeEventListener('scroll', updateStickyState)
      window.removeEventListener('resize', updateStickyState)
    }
  }, [activeCategory])

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
    const sequence = ++profileConnectivitySequence.current
    if (activeCategory !== 'models' || !profiles.length) {
      setProfileConnectivity({})
      return
    }
    setProfileConnectivity(Object.fromEntries(profiles.map((profile) => [profile.id, { status: 'checking' as const }])))
    void Promise.all(profiles.map(async (profile) => {
      try {
        const result = await getNodeApi().call<{ models?: string[] }>('listModels', {
          service: { provider: profile.provider, baseUrl: profile.baseUrl, apiKey: '', keepApiKey: profile.hasApiKey },
        })
        return [profile.id, result.models ? { status: 'connected', modelCount: result.models.length } : { status: 'error' }] as const
      } catch {
        return [profile.id, { status: 'error' }] as const
      }
    })).then((results) => {
      if (profileConnectivitySequence.current === sequence) setProfileConnectivity(Object.fromEntries(results))
    })
    return () => { if (profileConnectivitySequence.current === sequence) profileConnectivitySequence.current++ }
  }, [activeCategory, profiles])

  useEffect(() => {
    const media = window.matchMedia('(max-width: 767px)')
    const update = () => setCompactViewport(media.matches)
    update()
    media.addEventListener('change', update)
    return () => media.removeEventListener('change', update)
  }, [])

  function beginProfileEdit(profile?: typeof profiles[number]) {
    const selected = profile ? providers.find((item) => item.id === profile.provider) : providers[0]
    setProfileStep(0)
    setProfileNavigationDirection(1)
    setEditingProfile(profile?.id ?? '')
    setProfileName(profile?.name ?? selected?.name ?? '')
    setProvider(profile?.provider ?? selected?.id ?? '')
    setBaseUrl(profile?.baseUrl ?? selected?.defaultUrl ?? '')
    setModel(profile?.model ?? '')
    setModelSearch('')
    setApiKey('')
    setHasApiKey(Boolean(profile?.hasApiKey))
    setSavedService({ provider: profile?.provider ?? '', baseUrl: profile?.baseUrl ?? '' })
    setProfileOpen(true)
  }

  function handleProfileOpenChange(open: boolean) {
    if (!open) {
      if (pulling) cancelModelPull()
      setProfileStep(0)
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

  const refreshModels = useCallback(async (showToast = true) => {
    if (!provider || !baseUrl.trim()) return
    if (keyMissing) {
      setModelLoadState('key-required')
      return
    }
    const requestSequence = ++modelLoadSequence.current
    setLoadingModels(true)
    setModelLoadState('loading')
    try {
      const result = await getNodeApi().call<{ models?: string[]; details?: typeof models }>('listModels', { service: { provider, baseUrl, apiKey, keepApiKey } })
      const details = result.details ?? (result.models ?? []).map((name) => ({ name, label: '', sizeBytes: 0, tools: 0 }))
      if (requestSequence !== modelLoadSequence.current) return
      setModels(details)
      setModel((current) => details.some((item) => item.name === current) ? current : details[0]?.name ?? '')
      setModelLoadState('connected')
    } catch (error) {
      if (requestSequence !== modelLoadSequence.current) return
      setModelLoadState('error')
      if (showToast) toast.error(labels.listFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally {
      if (requestSequence === modelLoadSequence.current) setLoadingModels(false)
    }
  }, [apiKey, baseUrl, keyMissing, keepApiKey, labels.listFailed, provider])

  useEffect(() => {
    if (!profileOpen) {
      modelLoadSequence.current += 1
      setLoadingModels(false)
      setModelLoadState('idle')
      return
    }
    setLoadingModels(false)
    if (currentStep !== 'connection' || !provider || !baseUrl.trim()) {
      setModelLoadState('idle')
      return
    }
    if (keyMissing) {
      setModelLoadState('key-required')
      return
    }
    setModelLoadState('idle')
    const timer = window.setTimeout(() => { void refreshModels(false) }, 650)
    return () => {
      window.clearTimeout(timer)
      modelLoadSequence.current += 1
    }
  }, [baseUrl, currentStep, keyMissing, profileOpen, provider, refreshModels])

  function startModelPull(service: { provider: string; baseUrl: string; apiKey: string; keepApiKey: boolean }) {
    const name = downloadModel.trim()
    if (!name || pulling) return
    setPullStatus(labels.starting)
    setPullProgress(0)
    setPulling(true)
    pullCancel.current = getNodeApi().stream<{ status: string; completedBytes?: string | number; totalBytes?: string | number }>(
      'pullModel',
      { service, model: name },
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
        setDownloadModel('')
        toast.success(labels.downloaded)
        void refreshModels(false)
      },
    )
  }

  async function removeOllamaModel() {
    if (!removeModelTarget || !currentProvider || !isOllamaProvider) return
    const target = removeModelTarget
    setRemovingModel(true)
    try {
      await getNodeApi().call('removeModel', {
        service: { provider, baseUrl, apiKey, keepApiKey },
        model: target,
      })
      if (model === target) setModel('')
      toast.success(labels.removed)
      setRemoveModelTarget('')
      await refreshModels(false)
    } catch (error) {
      toast.error(labels.removeFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally { setRemovingModel(false) }
  }

  function cancelModelPull() {
    pullCancel.current?.()
    pullCancel.current = null
    setPulling(false)
    setPullStatus(labels.cancelled)
  }
  const profileForm = <div className="flex min-h-0 flex-1 flex-col">
    <AnimatedStepper steps={profileSteps} currentIndex={Math.min(profileStep, finalProfileStep)} direction={direction} navigationDirection={profileNavigationDirection} className="min-h-0 flex-1 px-5 sm:px-8 lg:px-10">
      <div className="h-full overflow-y-auto py-5">
        {currentStep === 'profile' && <div className="mx-auto grid min-h-full w-full max-w-6xl content-center gap-5 px-2 md:grid-cols-[minmax(0,1.05fr)_minmax(19rem,0.95fr)] md:items-stretch md:gap-7 lg:gap-10">
          <section aria-label={labels.profileStep} className="relative flex min-h-44 flex-col justify-between overflow-hidden rounded-[1.75rem] border border-[var(--app-line)] bg-[var(--app-wash)]/55 p-5 sm:p-7 md:min-h-[23rem] md:p-8">
            <div aria-hidden="true" className="pointer-events-none absolute -end-12 -top-16 size-56 rounded-full bg-[var(--app-accent)]/10 blur-3xl" />
            <div aria-hidden="true" className="pointer-events-none absolute inset-0 z-0 grid place-items-center"><span className={`grid size-40 place-items-center opacity-[0.075] sm:size-44 ${currentProviderIcon ? 'bg-[var(--app-accent)]' : 'text-[var(--app-accent)]'}`} style={currentProviderIcon ? { maskImage: `url("${currentProviderIcon}")`, WebkitMaskImage: `url("${currentProviderIcon}")`, maskRepeat: 'no-repeat', WebkitMaskRepeat: 'no-repeat', maskPosition: 'center', WebkitMaskPosition: 'center', maskSize: 'contain', WebkitMaskSize: 'contain' } : undefined}>{!currentProviderIcon && <Network className="size-32" strokeWidth={1.15} />}</span></div>
            <div className="relative flex justify-end">
              <span className="rounded-full border border-[var(--app-line)] bg-[var(--app-card)]/75 px-3 py-1 text-xs font-medium text-muted-foreground">{labels.profileStep}</span>
            </div>
            <div className="relative mt-6 min-w-0 md:mt-10">
              <h3 className="truncate text-2xl font-semibold tracking-tight text-[var(--app-ink)] sm:text-3xl">{profileName.trim() || labels.profileName}</h3>
              <p className="mt-2 truncate text-sm text-muted-foreground">{selectedProviderName || labels.choose}</p>
            </div>
            <div className="relative mt-6 flex items-center gap-3 border-t border-[var(--app-line)]/70 pt-4 text-sm text-muted-foreground md:mt-10">
              <span className="grid size-8 shrink-0 place-items-center rounded-xl bg-[var(--app-card)]"><Link2 className="size-4" /></span>
              <span>{labels.connectionStep}</span>
              <ChevronRight className="ms-auto size-4 rtl:rotate-180" />
            </div>
          </section>

          <div className="mx-auto flex w-full min-w-0 max-w-xl flex-col justify-center gap-5 py-1 sm:gap-6 md:py-5">
            <FloatingLabelInput label={labels.profileName} icon={<UserRound className="size-[18px]" />} value={profileName} onChange={(event) => setProfileName(event.target.value)} autoFocus />
            <ResponsiveSelector
              label={labels.provider}
              direction={direction}
              trigger={({ open, setOpen }) => <FloatingLabelInput label={labels.provider} value={selectedProviderName || labels.choose} readOnly role="button" disabled={providers.length === 0} onClick={() => setOpen((current) => !current)} aria-haspopup="dialog" aria-expanded={open} icon={<ServerCog className="size-[18px]" />} className="cursor-pointer" />}
            >
              {({ setOpen }) => <div className="grid gap-1">{providers.map((item) => { const icon = providerServiceIcon(item.id); const about = labels.providerDescriptions[item.id as keyof typeof labels.providerDescriptions] ?? item.about; return <button type="button" key={item.id} aria-pressed={provider === item.id} onClick={() => { setProvider(item.id); setApiKey(''); setHasApiKey(false); setBaseUrl(item.defaultUrl); setModel(''); setModelSearch(''); setModels([]); setOpen(false) }} className={`flex w-full items-center gap-3 rounded-xl px-3 py-3 text-start transition-colors hover:bg-[var(--app-wash)] ${provider === item.id ? 'bg-[var(--app-wash)] font-semibold' : ''}`}><span className={`grid size-10 shrink-0 place-items-center rounded-xl ${provider === item.id ? 'bg-[var(--app-accent)]/[0.07] text-[var(--app-accent)]' : 'text-[var(--app-muted)]'}`}>{icon ? <img aria-hidden="true" src={icon} className="size-6 dark:invert" /> : <Network aria-hidden="true" className="size-6" />}</span><span className="min-w-0 flex-1"><span className="block truncate text-sm">{item.name}</span>{about && <span className="mt-1 block line-clamp-2 text-xs font-normal leading-relaxed text-muted-foreground">{about}</span>}</span>{provider === item.id ? <Check className="size-4 shrink-0 text-[var(--app-accent)]" /> : null}</button> })}</div>}
            </ResponsiveSelector>
          </div>
        </div>}

        {currentStep === 'connection' && <div className="mx-auto grid min-h-full w-full max-w-5xl content-center gap-5 px-2 lg:grid-cols-2 lg:items-stretch lg:gap-7">
          <div className="flex min-w-0 flex-col justify-center gap-5 py-1 sm:gap-6">
            <FloatingLabelInput label={labels.url} icon={<Link2 className="size-[18px]" />} iconDirection="ltr" dir="ltr" value={baseUrl} onChange={(event) => { setBaseUrl(event.target.value); setModel(''); setModels([]) }} placeholder={editProvider?.defaultUrl} />
            {(isOllamaProvider || (editProvider?.needsKey !== 2 && editProvider?.needsKey !== 'SUPPORT_NO')) && <FloatingLabelInput label={labels.apiKey} icon={<KeyRound className="size-[18px]" />} dir="ltr" type="password" autoComplete="new-password" value={apiKey} onChange={(event) => setApiKey(event.target.value)} placeholder={hasApiKey ? labels.keepKey : isOllamaProvider ? labels.optionalApiKey : labels.enterKey} success={keyIsForThisService && !apiKey} />}
            {editProvider?.about && <p dir="auto" className="px-1 text-xs leading-relaxed text-muted-foreground">{editProvider.about}</p>}
          </div>

          <section aria-label={labels.connectionStep} aria-live="polite" className={`relative flex min-h-64 flex-col justify-between overflow-hidden rounded-[1.75rem] border p-5 transition-colors duration-300 sm:p-7 ${modelLoadState === 'connected' ? 'border-emerald-500/25 bg-emerald-500/[0.045]' : modelLoadState === 'error' || modelLoadState === 'key-required' ? 'border-destructive/25 bg-destructive/[0.035]' : 'border-[var(--app-line)] bg-[var(--app-wash)]/45'}`}>
            <div className="flex items-start gap-3">
              <div className={`grid size-11 shrink-0 place-items-center rounded-2xl border bg-[var(--app-card)] ${modelLoadState === 'connected' ? 'border-emerald-500/20 text-emerald-600 dark:text-emerald-400' : modelLoadState === 'error' || modelLoadState === 'key-required' ? 'border-destructive/20 text-destructive' : 'border-[var(--app-line)] text-[var(--app-accent)]'}`}>
                <AnimatePresence mode="wait" initial={false}>
                  <motion.span key={modelLoadState} initial={reduceMotion ? { opacity: 0 } : { opacity: 0, y: 3, scale: 0.88 }} animate={{ opacity: 1, y: 0, scale: 1 }} exit={reduceMotion ? { opacity: 0 } : { opacity: 0, y: -3, scale: 0.92 }} transition={{ duration: reduceMotion ? 0.12 : 0.22, ease: [0.22, 1, 0.36, 1] }}>
                    {modelLoadState === 'loading' ? <LoaderCircle className="size-5 animate-spin" /> : modelLoadState === 'connected' ? <CheckCircle2 className="size-5" /> : modelLoadState === 'error' || modelLoadState === 'key-required' ? <CircleAlert className="size-5" /> : <ServerCog className="size-5" />}
                  </motion.span>
                </AnimatePresence>
              </div>
              <div className="min-w-0 flex-1 pt-0.5">
                <h3 className="truncate text-sm font-semibold">{selectedProviderName || labels.connectionStep}</h3>
                <p dir="ltr" className={`mt-1 truncate text-xs text-muted-foreground ${direction === 'rtl' ? 'text-right' : 'text-left'}`}>{baseUrl}</p>
              </div>
            </div>

            <div className="mt-auto min-h-32 min-w-0 pt-8">
              <AnimatePresence mode="wait" initial={false}>
                <motion.div key={modelLoadState} initial={reduceMotion ? { opacity: 0 } : { opacity: 0, y: 7, filter: 'blur(3px)' }} animate={{ opacity: 1, y: 0, filter: 'blur(0px)' }} exit={reduceMotion ? { opacity: 0 } : { opacity: 0, y: -4, filter: 'blur(2px)' }} transition={{ duration: reduceMotion ? 0.12 : 0.26, ease: [0.22, 1, 0.36, 1] }}>
              {modelLoadState === 'loading' && <div role="status" className="space-y-3">
                <p className="text-sm text-muted-foreground">{labels.loadingModels}</p>
                <div className="space-y-2.5" aria-hidden="true"><div className="h-9 animate-pulse rounded-xl bg-[var(--app-line)]/55" /><div className="h-9 w-4/5 animate-pulse rounded-xl bg-[var(--app-line)]/40" /><div className="h-9 w-3/5 animate-pulse rounded-xl bg-[var(--app-line)]/30" /></div>
              </div>}
              {modelLoadState === 'connected' && <div role="status" className="flex items-baseline gap-3 text-emerald-700 dark:text-emerald-300"><span className="text-5xl font-semibold tracking-tight tabular-nums">{new Intl.NumberFormat(locale).format(models.length)}</span><span className="text-sm">{labels.modelCountLabel}</span></div>}
              {modelLoadState === 'error' && <div className="flex flex-wrap items-center justify-between gap-3"><p className="text-sm text-muted-foreground">{labels.listFailed}</p><Button type="button" variant="outline" size="sm" className="gap-2" onClick={() => void refreshModels()}><RefreshCw className="size-3.5" />{labels.load}</Button></div>}
              {modelLoadState === 'key-required' && <p className="text-sm leading-relaxed text-muted-foreground">{messages.providerKeyRequired}</p>}
              {modelLoadState === 'idle' && <div className="flex items-center gap-2 text-sm text-muted-foreground"><Link2 className="size-4 shrink-0" /><span>{labels.connectionStep}</span></div>}
                </motion.div>
              </AnimatePresence>
            </div>
          </section>
        </div>}

        {currentStep === 'model' && <div className={`mx-auto grid min-h-full w-full content-center gap-5 px-2 ${isOllamaProvider ? 'max-w-5xl lg:grid-cols-2 lg:items-start lg:gap-7' : 'max-w-3xl'}`}>
          <section className={`mx-auto flex w-full min-w-0 max-w-2xl flex-col justify-center ${isOllamaProvider ? 'self-center' : ''}`}>
            <ResponsiveSelector
              label={labels.model}
              direction={direction}
              trigger={({ open, setOpen }) => <FloatingLabelInput label={labels.model} icon={<Cpu className="size-[18px]" />} value={selectedModel?.label || selectedModel?.name || model || labels.choose} readOnly role="button" disabled={loadingModels || models.length === 0} loading={loadingModels} success={modelLoadState === 'connected' && Boolean(model)} onClick={() => setOpen((current) => !current)} aria-haspopup="dialog" aria-expanded={open} className="cursor-pointer" />}
            >
              {({ setOpen }) => <div className="grid gap-2">
                {models.length > 5 && <div className="relative"><Search className="pointer-events-none absolute start-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" /><Input aria-label={labels.searchModels} placeholder={labels.searchModels} dir="auto" value={modelSearch} onChange={(event) => setModelSearch(event.target.value)} className="ps-9" /></div>}
                <div className="grid max-h-64 gap-1 overflow-y-auto">
                  {visibleModels.map((item) => <button type="button" key={item.name} aria-pressed={model === item.name} onClick={() => { setModel(item.name); setModelSearch(''); setOpen(false) }} className={`flex w-full items-center justify-between gap-4 rounded-xl px-3 py-3 text-start text-sm transition-colors hover:bg-[var(--app-wash)] ${model === item.name ? 'bg-[var(--app-wash)] font-semibold' : ''}`}><span className="min-w-0 truncate">{item.label || item.name}</span>{model === item.name ? <Check className="size-4 shrink-0 text-[var(--app-accent)]" /> : null}</button>)}
                  {!models.length && <p className="px-3 py-5 text-center text-sm text-muted-foreground">{loadingModels ? labels.loadingModels : labels.modelManagementEmpty}</p>}
                  {models.length > 0 && !visibleModels.length && <p className="px-3 py-5 text-center text-sm text-muted-foreground">{labels.noModelMatches}</p>}
                </div>
              </div>}
            </ResponsiveSelector>
          </section>
          {isOllamaProvider && <section className="mx-auto flex h-[min(28rem,60dvh)] max-md:h-[min(26rem,48dvh)] w-full min-w-0 max-w-2xl flex-col gap-3 rounded-2xl border border-[var(--app-line)] bg-[var(--app-wash)]/25 p-4 sm:p-5">
            <div className="flex items-start gap-3"><div className="grid size-9 shrink-0 place-items-center rounded-xl bg-[var(--app-wash)]"><Download className="size-4 text-[var(--app-accent)]" /></div><div className="min-w-0 flex-1"><h3 className="text-sm font-semibold">{labels.modelManagementTitle}</h3><p className="mt-1 text-xs leading-relaxed text-muted-foreground">{labels.modelManagementDescription}</p></div><Badge variant="outline" className="shrink-0">{models.length}</Badge></div>
            <div className="grid min-w-0 gap-2 sm:grid-cols-[minmax(0,1fr)_auto_auto]">
              <Input aria-label={labels.downloadPlaceholder} value={downloadModel} onChange={(event) => setDownloadModel(event.target.value)} placeholder={labels.downloadPlaceholder} className="min-w-0" />
              <Button type="button" variant="outline" disabled={pulling || !downloadModel.trim() || !baseUrl.trim()} onClick={() => startModelPull({ provider, baseUrl, apiKey, keepApiKey })} className="gap-2"><Download className="size-4" />{messages.operationText.download}</Button>
              <Button type="button" variant="ghost" disabled={loadingModels || !baseUrl.trim()} onClick={() => void refreshModels()} className="gap-2">{loadingModels ? <LoaderCircle className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}{labels.load}</Button>
            </div>
            {pulling && <div className="space-y-1.5" role="status"><div className="flex items-center justify-between gap-3 text-xs text-muted-foreground"><span className="min-w-0 truncate">{pullStatus}</span><span className="shrink-0 tabular-nums">{Math.round(pullProgress)}%</span><Button type="button" variant="ghost" size="sm" onClick={cancelModelPull}>{messages.cancel}</Button></div><div role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pullProgress)} className="h-2 overflow-hidden rounded-full bg-[var(--app-line)]"><div className="h-full rounded-full bg-[var(--app-accent)] transition-[width]" style={{ width: `${pullProgress}%` }} /></div></div>}
            <div className="min-h-0 flex-1 divide-y divide-[var(--app-line)] overflow-y-auto overscroll-contain rounded-xl border border-[var(--app-line)] bg-[var(--app-card)] px-3">
              {models.map((item) => <div key={item.name} className="flex min-w-0 items-center gap-3 py-2"><div className="min-w-0 flex-1"><p className="truncate text-xs font-medium">{item.label || item.name}</p>{Number(item.sizeBytes) > 0 && <p className="mt-0.5 text-[10px] text-muted-foreground">{(Number(item.sizeBytes) / 1_000_000_000).toFixed(1)} GB</p>}</div><Button type="button" variant="ghost" size="icon-sm" aria-label={`${labels.removeModel} ${item.name}`} disabled={removingModel || pulling} onClick={() => setRemoveModelTarget(item.name)}><Trash2 className="size-3.5" /></Button></div>)}
              {!models.length && <p className="py-3 text-xs text-muted-foreground">{loadingModels ? labels.loadingModels : labels.modelManagementEmpty}</p>}
            </div>
          </section>}
        </div>}

      </div>
    </AnimatedStepper>
    <div className="grid shrink-0 grid-cols-2 gap-2 border-t border-[var(--app-line)] px-5 py-3 sm:flex sm:justify-end sm:px-7">
      <Button type="button" variant="outline" onClick={() => handleProfileOpenChange(false)}>{messages.cancel}</Button>
      {profileStep > 0 && <Button type="button" variant="outline" onClick={() => { setProfileNavigationDirection(-1); setProfileStep((step) => Math.max(0, step - 1)) }}>{labels.back}</Button>}
      {profileStep < finalProfileStep
        ? <Button type="button" disabled={profileStep === 0 ? !profileName.trim() || !provider : profileStep === 1 ? !baseUrl.trim() || modelLoadState !== 'connected' : false} onClick={() => { if (profileStep === 1 && !validateKey()) return; setProfileNavigationDirection(1); setProfileStep((step) => Math.min(finalProfileStep, step + 1)) }}>{labels.continue}</Button>
        : <>
          <Button type="button" variant="outline" disabled={!profileName.trim() || !provider || !model || keyMissing} onClick={() => void saveProfile(false)}>{labels.saveProfile}</Button>
          <Button type="button" disabled={!profileName.trim() || !provider || !model || keyMissing} onClick={() => void saveProfile(true)}>{labels.saveAndActivate}</Button>
        </>}
    </div>
  </div>

  const categoryMeta: Record<SettingsCategory, { title: string; description: string; icon: typeof Globe2 }> = {
    general: { title: messages.general, description: messages.generalSettingsHint, icon: Settings2 },
    node: { title: messages.localNodeSettings, description: messages.localDaemonSettingsHint, icon: Globe2 },
    access: { title: messages.webAccess.title, description: messages.webAccess.exposureDescription, icon: QrCode },
    models: { title: labels.title, description: labels.hint, icon: ServerCog },
  }
  const categories = (Object.keys(categoryMeta) as SettingsCategory[]).filter((item) => item !== 'access' || isDesktopApp())

  return <div className="settings-page">
    <div className="settings-page__heading"><PageHeading title={activeCategory ? categoryMeta[activeCategory].title : messages.settings} description={activeCategory ? categoryMeta[activeCategory].description : messages.settingsDescription} /></div>
    <div ref={scrollRef} className="settings-page__scroll">
    <div className="settings-page__content grid w-full min-w-0 gap-4">
      {!activeCategory ? <nav aria-label={messages.settings} className="settings-category-grid">
        {categories.map((item) => { const Icon = categoryMeta[item].icon; return <Link key={item} to={`/settings/${item}`} className="settings-category-card"><span className="settings-category-card__icon"><Icon className="size-5" /></span><span className="settings-category-card__copy"><span className="settings-category-card__title">{categoryMeta[item].title}</span><span className="settings-category-card__description">{categoryMeta[item].description}</span></span><ChevronRight className="settings-category-card__arrow" /></Link> })}
      </nav> : <div className="settings-category-content">
      {activeCategory === 'node' && <div className="grid min-w-0 gap-4"><Card className="settings-card"><CardHeader className="settings-card__header"><div className="settings-card__icon"><Globe2 className="size-5" /></div><div><div className="settings-card__eyebrow">{messages.node}</div><CardTitle>{messages.localNodeSettings}</CardTitle></div></CardHeader><CardContent><div className="settings-row"><div className="settings-row__icon"><ServerCog className="size-4" /></div><div className="min-w-0 flex-1"><div className="text-sm font-medium">{messages.localDaemon}</div><div className="mt-1 text-sm text-muted-foreground">{messages.localDaemonSettingsHint}</div></div><span className="settings-status">{messages.managedByDaemon}</span></div></CardContent></Card><ModelLendingSettings messages={messages} /></div>}
      {activeCategory === 'access' && isDesktopApp() && <AccessPage messages={messages} showHeading={false} />}
      {activeCategory === 'general' && <Card className="access-settings-card min-w-0 p-5 sm:p-6"><CardContent className="space-y-0 p-0">
        <div className="settings-row flex-wrap py-3"><div className="flex min-w-0 flex-1 items-center gap-3"><span className="grid size-10 shrink-0 place-items-center rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)] text-[var(--app-muted)]"><Languages className="size-[18px]" /></span><div className="min-w-0"><div className="text-sm font-medium">{messages.languageSettings}</div><p className="mt-1 text-sm text-muted-foreground">{messages.languageSettingsHint}</p></div></div><div className="w-full ps-[52px] sm:w-auto sm:ps-0 sm:[&>*]:min-w-64"><LanguagePicker value={locale} label={messages.language} onChange={setLocale} /></div></div>
        <div className="settings-row settings-row--divider flex-wrap"><div className="flex min-w-0 flex-1 items-center gap-3"><span className="grid size-10 shrink-0 place-items-center rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)] text-[var(--app-muted)]"><SunMoon className="size-[18px]" /></span><div className="min-w-0"><div className="text-sm font-medium">{messages.theme}</div><p className="mt-1 text-sm text-muted-foreground">{messages.themeSettingsHint}</p></div></div><div className="w-full ps-[52px] sm:w-auto sm:ps-0 sm:min-w-[18rem] sm:max-w-[24rem]"><ThemeModeSwitch direction={direction} labels={{ system: messages.themeSystem, light: messages.themeLight, dark: messages.themeDark, switchTheme: messages.switchTheme }} /></div></div>
        <div className="settings-row settings-row--divider flex-wrap"><div className="flex min-w-0 flex-1 items-center gap-3"><span className="grid size-10 shrink-0 place-items-center rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)] text-[var(--app-muted)]"><Palette className="size-[18px]" /></span><div className="min-w-0"><div className="text-sm font-medium">{messages.accent}</div><p className="mt-1 text-sm text-muted-foreground">{messages.accentSettingsHint}</p></div></div><div className="w-full ps-[52px] sm:w-auto sm:ps-0 sm:[&>*]:min-w-64"><ThemeStylePicker direction={direction} labels={{ label: messages.accent, neutral: messages.accentNeutral, blue: messages.accentBlue, pink: messages.accentPink }} /></div></div>
      </CardContent></Card>}
      {activeCategory === 'models' && <div className="settings-card--wide min-w-0 space-y-4">
        <div ref={providerToolbarRef} className={`settings-provider-toolbar${providerToolbarStuck ? ' is-stuck' : ''}`}><Button type="button" variant="ghost" onClick={() => beginProfileEdit()} className={`devices-add-button${providerToolbarStuck ? ' is-stuck' : ''}`}><span className="devices-add-button__icon"><Plus className="size-4" /></span>{labels.addProfile}</Button><Badge aria-label={`${displayedProfiles.length} ${labels.title}`} className={`settings-provider-count${providerToolbarStuck ? ' is-stuck' : ''}`}><Cpu aria-hidden="true" className="size-5 shrink-0 text-[var(--app-muted)]" /><span className="font-semibold tabular-nums leading-none">{new Intl.NumberFormat(locale).format(displayedProfiles.length)}</span></Badge></div>
        <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
          {displayedProfiles.map((profile) => { const isPreview = profile.id.startsWith('dev-preview-'); const connectivityStatus = profileConnectivity[profile.id]?.status ?? profile.previewStatus ?? 'checking'; const icon = providerServiceIcon(profile.provider); return <Card key={profile.id} className={`relative min-w-0 overflow-hidden rounded-[1.5rem] transition-colors ${profile.active ? 'border-[var(--app-accent)]/15 bg-[var(--app-accent)]/[0.025]' : 'border-[var(--app-line)]/35 bg-[var(--app-wash)]/35 hover:border-[var(--app-line)]/55'}`}>
            <div aria-hidden="true" className="pointer-events-none absolute inset-0 z-0 grid place-items-center"><span className={`grid size-40 place-items-center opacity-[0.075] sm:size-44 ${icon ? 'bg-[var(--app-accent)]' : 'text-[var(--app-accent)]'}`} style={icon ? { maskImage: `url("${icon}")`, WebkitMaskImage: `url("${icon}")`, maskRepeat: 'no-repeat', WebkitMaskRepeat: 'no-repeat', maskPosition: 'center', WebkitMaskPosition: 'center', maskSize: 'contain', WebkitMaskSize: 'contain' } : undefined}>{!icon && <Network className="size-32" strokeWidth={1.15} />}</span></div>
            <CardContent className="relative z-[1] flex min-h-[17rem] flex-col justify-between gap-5 p-5 sm:p-6">
            <div aria-hidden="true" className={`pointer-events-none absolute -end-12 -top-16 size-48 rounded-full blur-3xl ${profile.active ? 'bg-[var(--app-accent)]/10' : 'bg-[var(--app-line)]/25'}`} />
            <div className="relative flex items-start justify-end gap-4">{!isPreview && <Button type="button" variant="ghost" size="icon-sm" className="size-9 rounded-full border border-[var(--app-line)] bg-[var(--app-card)]/80" onClick={() => beginProfileEdit(profile)} aria-label={`${labels.editProfile} ${profile.name}`} title={labels.editProfile}><Pencil className="size-4" /></Button>}</div>
            <div className="relative mt-auto min-w-0"><h3 className="truncate text-xl font-semibold tracking-tight">{profile.name}</h3><p className="mt-1 truncate text-xs font-medium text-muted-foreground">{profile.previewProviderName ?? providers.find((item) => item.id === profile.provider)?.name ?? profile.provider}</p></div>
            <div className="relative mt-1 flex min-w-0 flex-wrap items-center gap-3 pt-4"><div aria-hidden="true" className="pointer-events-none absolute inset-x-0 top-0 h-px" style={{ background: 'linear-gradient(90deg, color-mix(in srgb, var(--app-line) 45%, transparent) 0%, color-mix(in srgb, var(--app-line) 45%, transparent) calc(50% - 5.5rem), transparent calc(50% - 5.5rem), transparent calc(50% + 5.5rem), color-mix(in srgb, var(--app-line) 45%, transparent) calc(50% + 5.5rem), color-mix(in srgb, var(--app-line) 45%, transparent) 100%)' }} /><div className="min-w-0 flex-1"><p className="flex min-w-0 items-center gap-1.5 truncate text-sm font-medium"><Cpu aria-hidden="true" className="size-3.5 shrink-0 text-[var(--app-muted)]" /><span className="truncate">{profile.model || labels.noModel}</span></p><div dir={direction} className="mt-2 flex min-w-0 items-center gap-1.5"><Globe aria-hidden="true" className="size-3.5 shrink-0 text-[var(--app-muted)]" /><p dir="ltr" className={`min-w-0 flex-1 truncate text-xs text-muted-foreground ${direction === 'rtl' ? 'text-right' : 'text-left'}`}>{profile.baseUrl}</p><span role="status" aria-live="polite" className={`settings-provider-status inline-flex shrink-0 items-center gap-1.5 rounded-full border px-2.5 py-1 text-[11px] font-medium ${connectivityStatus === 'connected' ? 'border-emerald-500/25 bg-emerald-500/[0.07] text-emerald-700 dark:text-emerald-300' : connectivityStatus === 'error' ? 'border-destructive/25 bg-destructive/[0.06] text-destructive' : 'border-[var(--app-line)] bg-[var(--app-wash)] text-muted-foreground'}`}>{connectivityStatus === 'connected' ? <Wifi className="size-3.5" /> : connectivityStatus === 'error' ? <GlobeX className="size-3.5" /> : <Radar className="size-3.5 animate-pulse" />}{connectivityStatus === 'connected' ? labels.reachable : connectivityStatus === 'error' ? labels.unreachable : labels.checking}</span></div></div>{!isPreview && <div className="flex shrink-0 items-center gap-1.5">{!profile.active && <Button type="button" size="sm" disabled={!profile.model} onClick={() => void activateProfile(profile)}>{labels.setActive}</Button>}{!profile.active && <Button type="button" variant="ghost" size="icon" aria-label={`${messages.remove} ${profile.name}`} title={messages.remove} onClick={() => void deleteProfile(profile)}><Trash2 className="size-4" /></Button>}</div>}</div>
          </CardContent></Card> })}
          {profiles.length === 0 && !import.meta.env.DEV && <button type="button" onClick={() => beginProfileEdit()} className="grid min-h-64 place-items-center rounded-[1.5rem] border border-dashed border-[var(--app-line)] p-6 text-center text-sm text-muted-foreground transition-colors hover:border-[var(--app-accent)]"><span><span className="mx-auto mb-3 grid size-12 place-items-center rounded-2xl border border-[var(--app-line)] bg-[var(--app-wash)]"><Plus className="size-5" /></span>{labels.addProfile}</span></button>}
        </div>
      </div>}
      </div>}
    </div>
    </div>
    {compactViewport ? <Drawer.Root open={profileOpen} onOpenChange={handleProfileOpenChange} dismissible={false} direction="bottom"><Drawer.Portal><Drawer.Overlay className="fixed inset-0 z-50 bg-black/30 backdrop-blur-sm" /><Drawer.Content dir={direction} className="liquid-glass-menu fixed inset-x-0 bottom-0 z-50 flex h-[min(92dvh,56rem)] max-h-[92dvh] flex-col overflow-hidden rounded-t-[1.5rem] bg-[var(--app-card)] pb-[max(0.75rem,env(safe-area-inset-bottom))] outline-none"><div aria-hidden="true" className="mx-auto mt-3 h-1 w-10 shrink-0 rounded-full bg-[var(--app-line)]" /><Drawer.Title className="shrink-0 px-5 pt-3 text-lg font-semibold">{editingProfile ? labels.editProfile : labels.addProfile}</Drawer.Title><Drawer.Description className="shrink-0 px-5 pt-1 text-xs text-muted-foreground">{labels.profileDescription}</Drawer.Description>{profileForm}</Drawer.Content></Drawer.Portal></Drawer.Root> : <Dialog open={profileOpen} disablePointerDismissal onOpenChange={(open, details) => { if (open || details.reason === 'close-press') handleProfileOpenChange(open) }}><DialogContent className="liquid-glass-menu flex h-[min(50rem,90dvh)] w-[min(72rem,calc(100vw-2rem))] max-w-none flex-col overflow-hidden rounded-[1.5rem] border border-[var(--app-line)] bg-[var(--app-card)] p-0 sm:max-w-none"><DialogHeader className="shrink-0 px-8 pt-7 lg:px-10 lg:pt-8"><DialogTitle>{editingProfile ? labels.editProfile : labels.addProfile}</DialogTitle><DialogDescription>{labels.profileDescription}</DialogDescription></DialogHeader>{profileForm}</DialogContent></Dialog>}
    <ResponsiveActionDialog open={Boolean(removeModelTarget)} direction={direction} title={labels.removeModelTitle} description={labels.removeModelDescription.replace('{model}', removeModelTarget)} icon={<Trash2 className="size-6 text-destructive" />} onOpenChange={(open) => { if (!open && !removingModel) setRemoveModelTarget('') }}>
      {removeModelTarget && <code dir="ltr" className="block break-all rounded-xl bg-[var(--app-wash)] p-3 text-center text-sm">{removeModelTarget}</code>}
      <div className="mt-5 grid gap-2 sm:grid-cols-2"><Button type="button" variant="outline" disabled={removingModel} onClick={() => setRemoveModelTarget('')}>{messages.cancel}</Button><Button type="button" variant="destructive" disabled={removingModel || !removeModelTarget} onClick={() => void removeOllamaModel()}>{removingModel ? <LoaderCircle className="size-4 animate-spin" /> : <Trash2 className="size-4" />}{labels.removeModel}</Button></div>
    </ResponsiveActionDialog>
  </div>
}
