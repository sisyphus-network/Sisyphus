import { useEffect, useMemo, useRef, useState, type TouchEvent } from 'react'
import { ArrowUpRight, Eye, MessageSquare, Settings2 } from 'lucide-react'
import { useNavigate } from 'react-router-dom'
import { AnimatePresence, motion, useIsPresent, useReducedMotion } from 'motion/react'
import Globe, { type GlobeMethods } from 'react-globe.gl'
import { AdditiveBlending, Color, MeshBasicMaterial, NormalBlending, type LineSegments, type PerspectiveCamera } from 'three'
import type { NodeSnapshot } from '../../../preload'
import { type getMessages } from '@/i18n/messages'
import centroidsGeoJson from 'world-countries-centroids/dist/countries.geojson?raw'
import countriesGeoJsonRaw from '../data/countries.geojson?raw'
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from '@/components/ui/resizable'
import { Button } from '@/components/ui/button'
import { LiquidGlassDock, type LiquidDockItem } from '@/components/navigation/liquid-glass-dock'
import { useBreakpoint } from '@/hooks/use-breakpoint'

type Messages = ReturnType<typeof getMessages>
function connected(peer: NodeSnapshot['peers'][number]) { return peer.connectionState === 2 || (typeof peer.connectionState === 'string' && peer.connectionState.endsWith('_CONNECTED')) }
function countryFlag(countryCode?: string) {
  if (!countryCode || !/^[a-z]{2}$/i.test(countryCode)) return ''
  return String.fromCodePoint(...countryCode.toUpperCase().split('').map((letter) => letter.charCodeAt(0) + 127397))
}
const layoutStorageKey = 'sisyphus-workspace-layout'

type CountryCentroidFeature = { geometry: { coordinates: [number, number] }; properties: { ISO: string } }
type CountryBoundaryFeature = { type: 'Feature'; properties: Record<string, unknown>; geometry: { type: string; coordinates: unknown } }
type HologramTheme = { accent: string; dark: boolean }
function readHologramTheme(): HologramTheme {
  const root = document.documentElement
  const explicitTheme = root.dataset.theme
  return {
    accent: getComputedStyle(root).getPropertyValue('--app-accent').trim() || '#38e9ff',
    dark: explicitTheme ? explicitTheme === 'dark' : window.matchMedia('(prefers-color-scheme: dark)').matches,
  }
}
const countryBoundaries = (JSON.parse(countriesGeoJsonRaw) as { features: CountryBoundaryFeature[] }).features
const countryCentroids = new Map<string, { lat: number; lng: number }>(
  (JSON.parse(centroidsGeoJson) as { features: CountryCentroidFeature[] }).features.map(({ geometry, properties }) => [
    properties.ISO,
    { lng: geometry.coordinates[0], lat: geometry.coordinates[1] },
  ]),
)

function NetworkGlobe({ countryCode, nodeLabel, countryLabel, reduceMotion }: { countryCode?: string; nodeLabel: string; countryLabel: string; reduceMotion: boolean }) {
  const containerRef = useRef<HTMLDivElement>(null)
  const globeRef = useRef<GlobeMethods | undefined>(undefined)
  const isPresent = useIsPresent()
  const [size, setSize] = useState({ width: 0, height: 0 })
  const [theme, setTheme] = useState<HologramTheme>(readHologramTheme)
  const hologramMaterial = useMemo(() => new MeshBasicMaterial({
    color: '#ffffff',
    transparent: true,
    opacity: 0.03,
    depthWrite: false,
    blending: AdditiveBlending,
  }), [])
  useEffect(() => {
    const update = () => setTheme(readHologramTheme())
    const observer = new MutationObserver(update)
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme', 'data-theme-style'] })
    const colorScheme = window.matchMedia('(prefers-color-scheme: dark)')
    colorScheme.addEventListener('change', update)
    update()
    return () => {
      observer.disconnect()
      colorScheme.removeEventListener('change', update)
    }
  }, [])
  useEffect(() => {
    hologramMaterial.color.set(theme.accent)
    hologramMaterial.opacity = theme.dark ? 0.055 : 0.065
    hologramMaterial.blending = theme.dark ? AdditiveBlending : NormalBlending
    const scene = globeRef.current?.scene()
    scene?.traverse((object) => {
      const line = object as LineSegments
      if (!line.isLineSegments) return
      const materials = Array.isArray(line.material) ? line.material : [line.material]
      for (const lineMaterial of materials) {
        const material = lineMaterial as { color?: Color; opacity: number; transparent: boolean; needsUpdate: boolean }
        if (!material.color) continue
        material.color.set(theme.accent)
        material.opacity = theme.dark ? 0.19 : 0.14
        material.transparent = true
        material.needsUpdate = true
      }
    })
    hologramMaterial.needsUpdate = true
  }, [hologramMaterial, theme])
  const fitCameraToViewport = () => {
    const globe = globeRef.current
    if (!globe) return
    const camera = globe.camera() as PerspectiveCamera
    const verticalFov = camera.fov * Math.PI / 180
    const horizontalFov = 2 * Math.atan(Math.tan(verticalFov / 2) * Math.min(1, camera.aspect))
    const limitingFov = Math.min(verticalFov, horizontalFov)
    const safeDistance = globe.getGlobeRadius() / Math.sin(limitingFov / 2) * 1.08
    const controls = globe.controls()
    controls.minDistance = safeDistance
    controls.maxDistance = globe.getGlobeRadius() * 9
    controls.autoRotate = !reduceMotion && isPresent
    controls.autoRotateSpeed = 0.45
    controls.enableDamping = true
    controls.update()
  }
  useEffect(() => {
    const container = containerRef.current
    if (!container) return
    const updateSize = () => {
      const rect = container.getBoundingClientRect()
      const diameter = Math.floor(Math.min(rect.width, rect.height, window.innerWidth, window.innerHeight) * 0.9)
      const width = diameter
      const height = diameter
      setSize((current) => current.width === width && current.height === height ? current : { width, height })
    }
    const observer = new ResizeObserver(updateSize)
    observer.observe(container)
    window.addEventListener('resize', updateSize)
    updateSize()
    return () => {
      observer.disconnect()
      window.removeEventListener('resize', updateSize)
    }
  }, [])
  useEffect(() => {
    fitCameraToViewport()
  }, [size.width, size.height, reduceMotion, isPresent])

  useEffect(() => {
    const globe = globeRef.current
    if (!globe) return
    if (isPresent) globe.resumeAnimation()
    else globe.pauseAnimation()
  }, [isPresent])

  const location = countryCode ? countryCentroids.get(countryCode.toUpperCase()) : undefined
  const points = useMemo(() => location ? [{ ...location, label: nodeLabel }] : [], [location, nodeLabel])
  const rings = useMemo(() => location ? [{ ...location, maxRadius: 5, propagationSpeed: 2, repeatPeriod: 1200 }] : [], [location])
  return <div ref={containerRef} className="network-globe-canvas" role="img" aria-label={location ? `${nodeLabel} · ${countryLabel}` : nodeLabel}>
    {size.width > 0 && size.height > 0 && <div className="network-globe-stage" style={{ width: size.width, height: size.height }}>
      <Globe
      ref={globeRef}
      width={size.width}
      height={size.height}
      backgroundColor="rgba(0,0,0,0)"
      showGlobe
      globeMaterial={hologramMaterial}
      showGraticules
      globeCurvatureResolution={6}
      showAtmosphere
      atmosphereColor={theme.accent}
      atmosphereAltitude={theme.dark ? 0.1 : 0.075}
      rendererConfig={{ alpha: true, antialias: true, powerPreference: 'low-power' }}
      polygonsData={countryBoundaries}
      polygonCapColor={() => 'rgba(0,0,0,0)'}
      polygonSideColor={() => 'rgba(0,0,0,0)'}
      polygonStrokeColor={() => {
        const { r, g, b } = new Color(theme.accent)
        const opacity = theme.dark ? 0.72 : 0.62
        return `rgba(${Math.round(r * 255)}, ${Math.round(g * 255)}, ${Math.round(b * 255)}, ${opacity})`
      }}
      polygonAltitude={0.006}
      pointsData={points}
      pointLat="lat"
      pointLng="lng"
      pointColor={() => theme.accent}
      pointAltitude={0.16}
      pointRadius={0.8}
      pointLabel="label"
      ringsData={rings}
      ringLat="lat"
      ringLng="lng"
      ringColor={() => (t: number) => {
        const { r, g, b } = new Color(theme.accent)
        return `rgba(${Math.round(r * 255)}, ${Math.round(g * 255)}, ${Math.round(b * 255)}, ${1 - t})`
      }}
      ringMaxRadius={() => 5}
      ringPropagationSpeed="propagationSpeed"
      ringRepeatPeriod="repeatPeriod"
      enablePointerInteraction={Boolean(location)}
      onGlobeReady={fitCameraToViewport}
      animateIn
      />
    </div>}
  </div>
}

function readLayout() {
  try {
    const parsed: unknown = JSON.parse(window.localStorage.getItem(layoutStorageKey) ?? 'null')
    if (parsed && typeof parsed === 'object' && 'chat' in parsed && 'network' in parsed && typeof parsed.chat === 'number' && typeof parsed.network === 'number') return parsed as Record<string, number>
  } catch { /* Ignore invalid or unavailable saved layout. */ }
  return { chat: 48, network: 52 }
}

export function WorkspacePage({ snapshot, messages }: { snapshot: NodeSnapshot; messages: Messages }) {
  const navigate = useNavigate()
  const compact = useBreakpoint('(max-width: 900px)')
  const reduceMotion = useReducedMotion()
  const [mobileView, setMobileView] = useState<'chat' | 'network'>('chat')
  const [touchStart, setTouchStart] = useState<{ x: number; y: number } | null>(null)
  const paneItems: LiquidDockItem[] = [
    { id: 'chat', label: messages.computeAssistant, href: '#chat', icon: <MessageSquare size={20} strokeWidth={1.8} />, active: mobileView === 'chat' },
    { id: 'network', label: messages.networkMap, href: '#network', icon: <Eye size={20} strokeWidth={1.8} />, active: mobileView === 'network' },
  ]
  const connectedPeers = snapshot.peers.filter(connected)
  const chatPane = <section className="workspace-chat workspace-glass liquid-glass-menu relative flex h-full min-h-0 flex-col rounded-[22px] px-8 pb-6 pt-7 max-[760px]:px-5">
      <div className="flex items-center gap-3"><img src="/logo.png" alt="" className="size-8 rounded-lg object-cover" /><span className="text-sm font-semibold tracking-tight">Sisyphus</span><span className="ms-auto flex items-center gap-1.5 text-[10px] text-muted-foreground"><span className={`size-1.5 rounded-full ${snapshot.status === 'connected' ? 'bg-emerald-500' : snapshot.status === 'connecting' ? 'animate-pulse bg-amber-500' : 'bg-muted-foreground'}`} />{snapshot.status === 'connected' ? messages.nodeOnline : snapshot.status === 'connecting' ? messages.nodeConnecting : messages.offline}</span><Button variant="ghost" size="icon" onClick={() => navigate('/settings')} aria-label={messages.openAdvanced} title={messages.openAdvanced} className="size-9 rounded-full text-muted-foreground hover:text-foreground"><Settings2 className="size-[17px]" /></Button></div>
      <div className="flex flex-1 flex-col justify-center pb-20 max-[900px]:pb-12"><div className="mx-auto w-full max-w-[590px]"><div className="mb-6"><div className="text-[10px] font-semibold tracking-[0.15em] text-muted-foreground">{messages.workspaceEyebrow}</div><h1 className="mt-3 text-3xl font-semibold tracking-tight max-[500px]:text-2xl">{messages.workspaceTitle}</h1><p className="mt-2 text-sm text-muted-foreground">{messages.workspaceDescription}</p></div>
        <div className="rounded-2xl border border-[var(--app-line)] bg-[var(--app-card)] p-3 shadow-sm"><textarea disabled aria-label={messages.promptPlaceholder} placeholder={messages.promptPlaceholder} className="min-h-28 w-full resize-none bg-transparent p-2 text-sm outline-none placeholder:text-muted-foreground/70 disabled:cursor-not-allowed max-[500px]:min-h-20" /><div className="flex items-center justify-between border-t border-[var(--app-line)] pt-3"><span className="text-[10px] text-muted-foreground">{messages.agentNotReadyShort}</span><button disabled aria-label={messages.sendMessage} className="grid size-8 place-items-center rounded-lg bg-[var(--app-accent)] text-[var(--app-accent-ink)] opacity-45"><ArrowUpRight className="size-4" /></button></div></div>
      </div></div>
    </section>
  const countryCode = snapshot.info?.countryCode?.toUpperCase()
  const countryPosition = countryCode ? countryCentroids.get(countryCode) : undefined
  const networkPane = <section className="workspace-network workspace-glass liquid-glass-menu relative flex h-full min-h-0 items-center justify-center overflow-hidden rounded-[22px]">
          <div className="network-visual absolute inset-0 overflow-hidden"><NetworkGlobe countryCode={countryCode} nodeLabel={messages.thisNode} countryLabel={countryCode ? `${messages.geoCountry}: ${countryCode} · ${messages.countryEstimateDisclaimer}` : messages.thisNode} reduceMotion={Boolean(reduceMotion)} /></div>
          <div className="absolute inset-x-5 top-5 z-[2] flex items-center justify-between gap-3 text-[10px]">
            <div className="rounded-lg border border-[var(--app-line)] bg-[var(--app-card)]/85 px-3 py-2 text-muted-foreground shadow-sm backdrop-blur"><span className="font-mono text-foreground">{connectedPeers.length}</span> {messages.connectedPeers.toLowerCase()}</div>
            <div className="max-w-[62%] rounded-lg border border-[var(--app-line)] bg-[var(--app-card)]/85 px-3 py-2 text-end text-muted-foreground shadow-sm backdrop-blur"><span className="me-1.5 inline-block size-1.5 rounded-full bg-[var(--app-accent)] align-middle" />{messages.thisNode}{countryFlag(countryCode) ? ` · ${countryFlag(countryCode)}` : ''}{countryPosition ? <span className="ms-1.5">{countryCode}</span> : ''}</div>
          </div>
    </section>
  const handleTouchEnd = (event: TouchEvent<HTMLDivElement>) => {
    if (touchStart === null) return
    const deltaX = event.changedTouches[0].clientX - touchStart.x
    const deltaY = event.changedTouches[0].clientY - touchStart.y
    if (Math.abs(deltaX) > 55 && Math.abs(deltaX) > Math.abs(deltaY) * 1.25) setMobileView(deltaX < 0 ? 'network' : 'chat')
    setTouchStart(null)
  }
  return <div className={`workspace-shell ${compact ? 'workspace-shell--compact' : ''}`} onTouchStart={(event) => setTouchStart({ x: event.touches[0].clientX, y: event.touches[0].clientY })} onTouchEnd={handleTouchEnd}>
    {compact ? <>
      <div className="workspace-mobile-stage">
        <AnimatePresence mode="sync" initial={false} custom={mobileView}>
          <motion.div key={mobileView} className="workspace-mobile-pane" custom={mobileView}
            initial={reduceMotion ? false : { opacity: 0, x: mobileView === 'network' ? 24 : -24 }}
            animate={{ opacity: 1, x: 0 }}
            exit={reduceMotion ? undefined : { opacity: 0, x: mobileView === 'network' ? -24 : 24 }}
            transition={reduceMotion ? { duration: 0 } : { type: 'tween', duration: 0.2, ease: [0.22, 1, 0.36, 1] }}>
            {mobileView === 'chat' ? chatPane : networkPane}
          </motion.div>
        </AnimatePresence>
      </div>
      <nav className="workspace-mobile-dock" aria-label={messages.workspace}><LiquidGlassDock items={paneItems} label={messages.workspace} onNavigate={(id) => setMobileView(id as 'chat' | 'network')} /></nav>
    </> : <ResizablePanelGroup orientation="horizontal" defaultLayout={readLayout()} onLayoutChanged={(layout) => window.localStorage.setItem(layoutStorageKey, JSON.stringify(layout))} className="workspace-panels" aria-label={messages.workspace}>
    <ResizablePanel id="chat" defaultSize="48%" minSize="30%" className="workspace-panel">
    {chatPane}
    </ResizablePanel>
    <ResizableHandle withHandle className="workspace-resize-handle" />
    <ResizablePanel id="network" defaultSize="52%" minSize="28%" className="workspace-panel">
    {networkPane}
    </ResizablePanel>
    </ResizablePanelGroup>
    }
  </div>
}
