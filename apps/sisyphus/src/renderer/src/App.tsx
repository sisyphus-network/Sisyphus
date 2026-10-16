import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { BrowserRouter, HashRouter, Navigate, NavLink, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import { ArrowLeft, Boxes, Cpu, House, MonitorCog, Settings2, Wifi, WifiOff } from 'lucide-react'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import type { NodeSnapshot } from '../../preload'
import { buttonVariants } from '@/components/ui/button'
import { Breadcrumb as BreadcrumbNav, BreadcrumbItem, BreadcrumbLink, BreadcrumbList, BreadcrumbPage, BreadcrumbSeparator } from '@/components/ui/breadcrumb'
import { getLocaleDirection, isAppLocale, type AppLocale } from '@/i18n/locales'
import { getMessages } from '@/i18n/messages'
import { NodeDetailsPage } from '@/pages/NodeDetailsPage'
import { SettingsPage } from '@/pages/SettingsPage'
import { WorkspacePage } from '@/pages/WorkspacePage'
import { OperationsPage } from '@/pages/OperationsPage'
import { getNodeApi, isDesktopApp } from '@/lib/node-api'
import { isAuthenticationError, pairBrowser } from '@sisyphus/client'
import { getBrowserDeviceName } from '@/lib/browser-device-name'
import { logoUrl } from '@/lib/brand'
import { useBreakpoint } from '@/hooks/use-breakpoint'
import { LiquidGlassDock, type LiquidDockItem } from '@/components/navigation/liquid-glass-dock'
import { AdvancedSidebar } from '@/components/navigation/advanced-sidebar'
import { NodeStatusBadge } from '@/components/navigation/node-status-badge'
import { initializeThemeMode } from '@/components/theme-mode-switch'
import { initializeThemeStyle } from '@/components/theme-style-picker'
import { ToasterResponsive } from '@/components/toaster-responsive'
import { NodeConnectionState } from '@/components/node-connection-state'

const initialSnapshot: NodeSnapshot = {
  status: 'connecting', endpoint: '127.0.0.1:50051', info: null, peers: [], revision: '0', lastUpdated: null, error: null, authRequired: false,
}
const scrollPositionsStorageKey = 'sisyphus-page-scroll-positions'

function readScrollPositions(): Record<string, number> {
  try {
    const value: unknown = JSON.parse(window.localStorage.getItem(scrollPositionsStorageKey) ?? '{}')
    if (value && typeof value === 'object') {
      return Object.fromEntries(Object.entries(value).filter((entry): entry is [string, number] => typeof entry[1] === 'number' && Number.isFinite(entry[1])))
    }
  } catch { /* Ignore unavailable or malformed saved scroll positions. */ }
  return {}
}

function readLocale(): AppLocale {
  const stored = window.localStorage.getItem('sisyphus-locale')
  return isAppLocale(stored) ? stored : 'en'
}

function App() {
  const [snapshot, setSnapshot] = useState(initialSnapshot)
  const [locale, setLocale] = useState<AppLocale>(readLocale)
  const messages = getMessages(locale)
  const direction = getLocaleDirection(locale)
  const connectedPeers = useMemo(() => snapshot.peers.filter((peer) => peer.connectionState === 2 || (typeof peer.connectionState === 'string' && peer.connectionState.endsWith('_CONNECTED'))).length, [snapshot.peers])

  useLayoutEffect(() => {
    const stopModeSync = initializeThemeMode()
    const stopStyleSync = initializeThemeStyle()
    return () => {
      stopModeSync()
      stopStyleSync()
    }
  }, [])

  useEffect(() => {
    let active = true
    const api = getNodeApi()
    const unsubscribe = api.onSnapshot((next) => { if (active) setSnapshot(next) })
    void api.getSnapshot().then((next) => { if (active) setSnapshot(next) }).catch((error: unknown) => {
      if (active) setSnapshot((current) => ({ ...current, status: 'disconnected', error: error instanceof Error ? error.message : String(error), authRequired: !isDesktopApp() && isAuthenticationError(error) }))
    })
    return () => { active = false; unsubscribe() }
  }, [])

  useEffect(() => {
    if (isDesktopApp() || !window.location.hash.startsWith('#pair=')) return
    const code = decodeURIComponent(window.location.hash.slice('#pair='.length)).trim()
    window.history.replaceState(null, '', window.location.pathname + window.location.search)
    let active = true
    void pairBrowser(code, getBrowserDeviceName()).then(async () => {
      const next = await getNodeApi().getSnapshot()
      if (active) setSnapshot(next)
    }).catch((error: unknown) => {
      if (active) setSnapshot((current) => ({ ...current, status: 'disconnected', authRequired: true, error: `Pairing failed: ${error instanceof Error ? error.message : String(error)}` }))
    })
    return () => { active = false }
  }, [])

  useEffect(() => {
    document.documentElement.lang = locale
    document.documentElement.dir = direction
    window.localStorage.setItem('sisyphus-locale', locale)
  }, [direction, locale])

  const statusLabel = snapshot.status === 'connected' ? messages.connected : snapshot.status === 'connecting' ? messages.connecting : messages.offline
  const StatusIcon = snapshot.status === 'connected' ? Wifi : WifiOff

  const Router = isDesktopApp() ? HashRouter : BrowserRouter
  return <Router>
    <ToasterResponsive />
    <AppRoutes snapshot={snapshot} setSnapshot={setSnapshot} locale={locale} setLocale={setLocale} messages={messages} direction={direction} connectedPeers={connectedPeers} statusLabel={statusLabel} StatusIcon={StatusIcon} />
  </Router>
}

function AppRoutes({ snapshot, setSnapshot, locale, setLocale, messages, direction, connectedPeers, statusLabel, StatusIcon }: { snapshot: NodeSnapshot; setSnapshot: (snapshot: NodeSnapshot) => void; locale: AppLocale; setLocale: (locale: AppLocale) => void; messages: ReturnType<typeof getMessages>; direction: 'ltr' | 'rtl'; connectedPeers: number; statusLabel: string; StatusIcon: typeof Wifi }) {
  const location = useLocation()
  const navigate = useNavigate()
  const reduceMotion = useReducedMotion()
  const scrollPathRef = useRef(location.pathname)
  scrollPathRef.current = location.pathname
  const scrollPositionsRef = useRef<Record<string, number>>(readScrollPositions())
  const scrollWriteTimerRef = useRef<number | undefined>(undefined)
  const advanced = location.pathname !== '/'
  const compact = useBreakpoint('(max-width: 900px)')
  useLayoutEffect(() => {
    const mainScroller = advanced ? document.querySelector<HTMLElement>('.app-shell--advanced .app-main') : null
    const scrollTarget: Window | HTMLElement = mainScroller ?? window
    const getTop = () => mainScroller ? mainScroller.scrollTop : window.scrollY
    const setTop = (top: number) => {
      if (mainScroller) mainScroller.scrollTop = top
      else window.scrollTo(0, top)
    }
    const savedTop = scrollPositionsRef.current[location.pathname] ?? 0
    let restored = false
    let latestTop = savedTop
    let lastUserScrollIntent = 0
    let saveTimer = 0

    const savePosition = () => {
      if (!restored) return
      scrollPositionsRef.current[location.pathname] = latestTop
      try {
        // Merge with the persisted map so a stale AppRoutes ref cannot erase
        // a position another route saved while it was active.
        const positions = { ...scrollPositionsRef.current, ...readScrollPositions(), [location.pathname]: latestTop }
        scrollPositionsRef.current = positions
        window.localStorage.setItem(scrollPositionsStorageKey, JSON.stringify(positions))
      } catch { /* Storage can be unavailable in restricted browser contexts. */ }
    }
    const onScroll = () => {
      if (!restored) return
      const nextTop = getTop()
      // Focus/route changes can jump an offscreen navigation target into view. Do not
      // mistake that programmatic reset for the user's new position on this page.
      if (nextTop === 0 && latestTop > 0 && performance.now() - lastUserScrollIntent > 280) return
      latestTop = nextTop
      scrollPositionsRef.current[location.pathname] = latestTop
      if (saveTimer) window.clearTimeout(saveTimer)
      saveTimer = window.setTimeout(savePosition, 120)
    }
    const onPageHide = () => {
      latestTop = getTop()
      savePosition()
    }
    const onRouteLinkClick = (event: MouseEvent) => {
      if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey || !(event.target instanceof Element)) return
      const anchor = event.target.closest<HTMLAnchorElement>('a[href]')
      if (!anchor) return
      try {
        const targetUrl = new URL(anchor.href, window.location.href)
        if (targetUrl.origin !== window.location.origin) return
        const targetPath = window.sisyphus
          ? targetUrl.hash.slice(1).split(/[?#]/)[0] || '/'
          : targetUrl.pathname
        if (targetPath !== location.pathname) scrollPathRef.current = targetPath
      } catch { /* Ignore links that do not resolve to an application route. */ }
    }
    const onHistoryNavigation = () => {
      scrollPathRef.current = window.sisyphus
        ? window.location.hash.slice(1).split(/[?#]/)[0] || '/'
        : window.location.pathname
    }
    const markUserScrollIntent = () => { lastUserScrollIntent = performance.now() }
    const onScrollKeyDown = (event: KeyboardEvent) => {
      if (['ArrowUp', 'ArrowDown', 'PageUp', 'PageDown', 'Home', 'End', ' '].includes(event.key)) markUserScrollIntent()
    }

    // Restore before paint so the new route never fades in at the previous
    // route's scroll offset and then visibly jumps to its own saved position.
    setTop(savedTop)
    latestTop = getTop()
    restored = true
    scrollTarget.addEventListener('scroll', onScroll, { passive: true })
    scrollTarget.addEventListener('wheel', markUserScrollIntent, { passive: true })
    scrollTarget.addEventListener('touchstart', markUserScrollIntent, { passive: true })
    document.addEventListener('keydown', onScrollKeyDown)
    document.addEventListener('click', onRouteLinkClick, true)
    window.addEventListener('popstate', onHistoryNavigation)
    window.addEventListener('hashchange', onHistoryNavigation)
    window.addEventListener('pagehide', onPageHide)

    return () => {
      if (saveTimer) window.clearTimeout(saveTimer)
      savePosition()
      scrollTarget.removeEventListener('scroll', onScroll)
      scrollTarget.removeEventListener('wheel', markUserScrollIntent)
      scrollTarget.removeEventListener('touchstart', markUserScrollIntent)
      document.removeEventListener('keydown', onScrollKeyDown)
      document.removeEventListener('click', onRouteLinkClick, true)
      window.removeEventListener('popstate', onHistoryNavigation)
      window.removeEventListener('hashchange', onHistoryNavigation)
      window.removeEventListener('pagehide', onPageHide)
    }
  }, [advanced, compact, location.pathname, scrollPositionsRef, scrollPathRef])
  const navItems: LiquidDockItem[] = [
    { id: 'workspace', label: messages.workspace, href: '/', icon: <House size={21} strokeWidth={1.8} />, active: location.pathname === '/' },
    { id: 'node', label: messages.nodeOverview, href: '/node', icon: <MonitorCog size={21} strokeWidth={1.8} />, active: location.pathname === '/node' },
    { id: 'operations', label: messages.operations, href: '/operations', icon: <Boxes size={21} strokeWidth={1.8} />, active: location.pathname === '/operations' },
    { id: 'settings', label: messages.settings, href: '/settings', icon: <Settings2 size={21} strokeWidth={1.8} />, active: location.pathname.startsWith('/settings') },
  ]
  if (!isDesktopApp() && snapshot.authRequired) {
    return <Routes>
      <Route path="/auth" element={<NodeConnectionState snapshot={snapshot} messages={messages} onConnected={setSnapshot} direction={direction} fullScreen />} />
      <Route path="*" element={<Navigate to="/auth" replace />} />
    </Routes>
  }
  if (!isDesktopApp() && location.pathname === '/auth') return <Navigate to="/" replace />
  return <div dir={direction} className={`app-shell flex min-h-0 bg-background text-foreground ${advanced ? 'app-shell--advanced' : 'app-shell--workspace'} ${location.pathname.startsWith('/settings') ? 'app-shell--settings' : ''}`}>
      <div className={`app-shell-body flex min-h-0 min-w-0 flex-1 ${advanced ? 'app-shell-body--advanced' : ''}`}>
      {advanced && !compact && <AdvancedSidebar snapshot={snapshot} connectedPeers={connectedPeers} statusLabel={statusLabel} messages={messages} />}

      <div className={`app-content-column flex min-h-0 min-w-0 flex-1 flex-col ${advanced ? 'app-content-column--advanced' : ''}`}>
      {advanced && <header className="app-global-header flex h-[68px] w-full shrink-0 items-center justify-between border-b border-[var(--app-line)] px-8 max-[1000px]:px-5 max-[900px]:h-[58px] max-[900px]:px-4"><Breadcrumb messages={messages} direction={direction} />{compact && <NodeStatusBadge status={snapshot.status} label={statusLabel} icon={StatusIcon} />}</header>}
      <main onScroll={advanced ? (event) => {
        const pathname = location.pathname
        const top = event.currentTarget.scrollTop
        if (scrollWriteTimerRef.current) window.clearTimeout(scrollWriteTimerRef.current)
        scrollWriteTimerRef.current = window.setTimeout(() => {
          try {
            const positions = { ...readScrollPositions(), [pathname]: top }
            scrollPositionsRef.current = positions
            window.localStorage.setItem(scrollPositionsStorageKey, JSON.stringify(positions))
          } catch { /* Storage can be unavailable in restricted browser contexts. */ }
        }, 120)
      } : undefined} className={`app-main mx-auto min-w-0 w-full ${advanced ? 'max-w-[1440px] px-8 pb-7 max-[1000px]:px-5 max-[900px]:px-4' : 'max-w-none max-[760px]:px-0'}`}>
        <motion.div
          className="workspace-persistent"
          aria-hidden={advanced}
          initial={false}
          animate={{ opacity: advanced ? 0 : 1 }}
          style={{ visibility: advanced ? 'hidden' : 'visible', pointerEvents: advanced ? 'none' : 'auto' }}
          transition={reduceMotion ? { duration: 0 } : { opacity: { duration: 0.18, ease: [0.22, 1, 0.36, 1] } }}
        >
          <WorkspacePage snapshot={snapshot} messages={messages} direction={direction} visible={!advanced} />
        </motion.div>
        {advanced && <motion.div
          key={`${location.pathname}:${snapshot.status === 'connected' ? 'ready' : 'offline'}`}
          className="route-transition"
          initial={reduceMotion ? false : { opacity: 0 }}
          animate={{ opacity: 1 }}
          transition={reduceMotion ? { duration: 0 } : { opacity: { duration: 0.18, ease: [0.22, 1, 0.36, 1] } }}
        >
            {snapshot.status !== 'connected' ? <NodeConnectionState snapshot={snapshot} messages={messages} onConnected={setSnapshot} /> : <Routes location={location}>
              <Route path="/node" element={<NodeDetailsPage snapshot={snapshot} messages={messages} />} />
              <Route path="/operations" element={<OperationsPage messages={messages} />} />
              <Route path="/settings" element={<SettingsPage locale={locale} setLocale={setLocale} messages={messages} direction={direction} />} />
              <Route path="/settings/access" element={isDesktopApp() ? <SettingsPage locale={locale} setLocale={setLocale} messages={messages} direction={direction} category="access" /> : <Navigate to="/settings" replace />} />
              <Route path="/settings/:category" element={<SettingsPage locale={locale} setLocale={setLocale} messages={messages} direction={direction} />} />
              <Route path="/access" element={<Navigate to="/settings/access" replace />} />
              <Route path="/pairing" element={<Navigate to="/settings/access" replace />} />
              <Route path="*" element={<Navigate to="/" replace />} />
            </Routes>}
        </motion.div>}
      </main>
      </div>
      </div>
      {advanced && compact && createPortal(<nav className="app-mobile-dock" aria-label={messages.preferences}><LiquidGlassDock items={navItems} label={messages.preferences} onNavigate={(_id, href) => { if (location.pathname !== href) { scrollPathRef.current = href; navigate(href) } }} /></nav>, document.body)}
    </div>
}

function NavItem({ to, icon: Icon, label, end = false }: { to: string; icon: typeof Cpu; label: string; end?: boolean }) {
  return <NavLink to={to} end={end} className={({ isActive }) => `flex h-10 items-center gap-3 rounded-lg px-3 text-sm no-underline transition-colors ${isActive ? 'bg-[var(--app-card)] font-medium text-foreground shadow-sm ring-1 ring-foreground/5' : 'text-muted-foreground hover:bg-[var(--app-wash)] hover:text-foreground'}`}><Icon className="size-4" /><span>{label}</span></NavLink>
}

function Breadcrumb({ messages, direction }: { messages: ReturnType<typeof getMessages>; direction: 'ltr' | 'rtl' }) {
  const location = useLocation()
  const settingsCategory = location.pathname.startsWith('/settings/') ? location.pathname.split('/')[2] : undefined
  const categoryLabels: Record<string, string> = { general: messages.general, node: messages.localNodeSettings, access: messages.webAccess.title, language: messages.general, models: messages.modelSettings.title, appearance: messages.general }
  const label = settingsCategory ? categoryLabels[settingsCategory] ?? messages.settings : location.pathname === '/node' ? messages.nodeOverview : location.pathname === '/operations' ? messages.operations : location.pathname === '/settings' ? messages.settings : location.pathname === '/pairing' ? messages.browserAccess.title : messages.workspace
  const backHref = settingsCategory ? '/settings' : '/'
  const backLabel = settingsCategory ? messages.settings : messages.workspace
  const slideDistance = direction === 'rtl' ? -10 : 10
  return <div dir={direction} className="flex h-full min-w-0 flex-1 items-center gap-3 sm:gap-4">
    <NavLink to={backHref} aria-label={backLabel} title={backLabel} className={`${buttonVariants({ variant: 'ghost', size: 'icon' })} size-10 shrink-0 rounded-full`}>
      <ArrowLeft aria-hidden="true" className="size-4 rtl:rotate-180" />
    </NavLink>
    <BreadcrumbNav aria-label={messages.breadcrumbNavigation} className="min-w-0 flex-1">
      <BreadcrumbList className="flex-nowrap gap-2 sm:gap-3">
        <BreadcrumbItem className="hidden shrink-0 sm:inline-flex">
          <BreadcrumbLink render={<NavLink to="/" />}>{messages.workspace}</BreadcrumbLink>
        </BreadcrumbItem>
        <BreadcrumbSeparator className="hidden shrink-0 sm:block" />
        {settingsCategory ? <>
          <BreadcrumbItem className="min-w-0"><BreadcrumbLink render={<NavLink to="/settings" />}>{messages.settings}</BreadcrumbLink></BreadcrumbItem>
          <BreadcrumbSeparator className="shrink-0" />
        </> : null}
        <BreadcrumbItem className="min-w-0">
          <AnimatePresence initial={false} mode="popLayout">
            <motion.span key={location.pathname} initial={{ opacity: 0, x: slideDistance }} animate={{ opacity: 1, x: 0 }} exit={{ opacity: 0, x: -slideDistance }} transition={{ duration: 0.2, ease: [0.22, 1, 0.36, 1] }}>
              <BreadcrumbPage title={label} className="truncate font-semibold">{label}</BreadcrumbPage>
            </motion.span>
          </AnimatePresence>
        </BreadcrumbItem>
      </BreadcrumbList>
    </BreadcrumbNav>
  </div>
}

export default App
