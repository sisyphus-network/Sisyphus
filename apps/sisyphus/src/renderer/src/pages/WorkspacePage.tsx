import { memo, useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent, type PointerEvent as ReactPointerEvent, type TouchEvent } from 'react'
import { createPortal } from 'react-dom'
import { Eye, MessageSquare } from 'lucide-react'
import { useNavigate } from 'react-router-dom'
import { motion, useReducedMotion } from 'motion/react'
import Globe, { type GlobeMethods } from 'react-globe.gl'
import { AdditiveBlending, Color, MeshBasicMaterial, NormalBlending, type LineSegments, type PerspectiveCamera } from 'three'
import type { NodeSnapshot } from '../../../preload'
import { type getMessages } from '@/i18n/messages'
import centroidsGeoJson from 'world-countries-centroids/dist/countries.geojson?raw'
import countriesGeoJsonRaw from '../data/countries.geojson?raw'
import { LiquidGlassDock, type LiquidDockItem } from '@/components/navigation/liquid-glass-dock'
import { useBreakpoint } from '@/hooks/use-breakpoint'
import { AgentChat } from '@/components/assistant/agent-chat'

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
  const dark = explicitTheme ? explicitTheme === 'dark' : window.matchMedia('(prefers-color-scheme: dark)').matches
  const selectedStyle = root.dataset.themeStyle || window.localStorage.getItem('sisyphus-theme-style')
  const selectedAccent = selectedStyle === 'blue'
    ? dark ? '#83adff' : '#4f8cff'
    : selectedStyle === 'pink'
      ? dark ? '#f59ac4' : '#e968a5'
      : undefined
  return {
    // Theme styles are also persisted in localStorage. Reading their palette
    // directly avoids briefly falling back to the neutral computed accent while
    // the document theme attributes are being restored after navigation.
    accent: selectedAccent || getComputedStyle(root).getPropertyValue('--app-accent').trim() || '#4f8cff',
    dark,
  }
}
const countryBoundaries = (JSON.parse(countriesGeoJsonRaw) as { features: CountryBoundaryFeature[] }).features
const countryCentroids = new Map<string, { lat: number; lng: number }>(
  (JSON.parse(centroidsGeoJson) as { features: CountryCentroidFeature[] }).features.map(({ geometry, properties }) => [
    properties.ISO,
    { lng: geometry.coordinates[0], lat: geometry.coordinates[1] },
  ]),
)

const NetworkGlobe = memo(function NetworkGlobe({ countryCode, peers, nodeLabel, countryLabel, reduceMotion, active }: { countryCode?: string; peers: NodeSnapshot['peers']; nodeLabel: string; countryLabel: string; reduceMotion: boolean; active: boolean }) {
  const containerRef = useRef<HTMLDivElement>(null)
  const globeRef = useRef<GlobeMethods | undefined>(undefined)
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
  const applyHologramStyle = useCallback(() => {
    hologramMaterial.color.set(theme.accent)
    hologramMaterial.opacity = theme.dark ? 0.055 : 0.065
    hologramMaterial.blending = theme.dark ? AdditiveBlending : NormalBlending
    hologramMaterial.needsUpdate = true
    const scene = globeRef.current?.scene()
    scene?.traverse((object) => {
      const line = object as LineSegments
      if (!line.isLineSegments) return
      const isCountryBoundary = (line.parent as { __globeObjType?: string } | null)?.__globeObjType === 'polygon'
      const isGraticule = !isCountryBoundary && line.geometry.type === 'GeoJsonGeometry'
      if (!isCountryBoundary && !isGraticule) return
      const materials = Array.isArray(line.material) ? line.material : [line.material]
      for (const lineMaterial of materials) {
        const material = lineMaterial as { color?: Color; opacity: number; transparent: boolean; needsUpdate: boolean }
        if (!material.color) continue
        material.color.set(theme.accent)
        // Polygon outlines also use LineSegments. Restore their own opacity
        // instead of applying the faint graticule style when resuming a route.
        material.opacity = isCountryBoundary
          ? theme.dark ? 0.72 : 0.62
          : theme.dark ? 0.19 : 0.17
        material.transparent = true
        material.needsUpdate = true
      }
    })
  }, [hologramMaterial, theme])
  useEffect(() => {
    applyHologramStyle()
  }, [applyHologramStyle])
  const fitCameraToViewport = useCallback(() => {
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
    controls.autoRotate = !reduceMotion && active
    controls.autoRotateSpeed = 0.45
    controls.enableDamping = true
    controls.update()
  }, [active, reduceMotion])
  useEffect(() => {
    const container = containerRef.current
    if (!container) return
    let frame = 0
    const measureAndCommit = () => {
      const rect = container.getBoundingClientRect()
      const diameter = Math.floor(Math.min(rect.width, rect.height, window.innerWidth, window.innerHeight) * 0.9)
      if (diameter < 1) return
      setSize((current) => current.width === diameter && current.height === diameter ? current : { width: diameter, height: diameter })
    }
    const updateSize = () => {
      if (frame) return
      frame = window.requestAnimationFrame(() => {
        frame = 0
        measureAndCommit()
      })
    }
    const observer = new ResizeObserver(updateSize)
    observer.observe(container)
    window.addEventListener('resize', updateSize)
    updateSize()
    return () => {
      if (frame) window.cancelAnimationFrame(frame)
      observer.disconnect()
      window.removeEventListener('resize', updateSize)
    }
  }, [])
  useEffect(() => {
    fitCameraToViewport()
  }, [fitCameraToViewport, size.width, size.height])

  useEffect(() => {
    const globe = globeRef.current
    if (!globe) return
    if (active) globe.resumeAnimation()
    else globe.pauseAnimation()
    if (!active) return

    // The canvas and scene remain mounted while hidden. Re-apply the accent
    // after the browser has laid the workspace out again, preserving camera state.
    const frame = window.requestAnimationFrame(() => {
      fitCameraToViewport()
      applyHologramStyle()
      globe.renderer().render(globe.scene(), globe.camera())
    })
    return () => window.cancelAnimationFrame(frame)
  }, [active, applyHologramStyle, fitCameraToViewport])

  const location = countryCode ? countryCentroids.get(countryCode.toUpperCase()) : undefined
  const points = useMemo(() => {
    const own = location ? [{ ...location, label: nodeLabel, color: theme.accent }] : []
    const known = peers.flatMap((peer) => {
      const peerLocation = peer.countryCode ? countryCentroids.get(peer.countryCode.toUpperCase()) : undefined
      return peerLocation ? [{ ...peerLocation, label: `${peer.peerId.slice(0, 12)} · ${peer.countryCode}`, color: theme.accent }] : []
    })
    return [...own, ...known]
  }, [location, nodeLabel, peers, theme.accent])
  const rings = useMemo(() => location ? [{ ...location, maxRadius: 5, propagationSpeed: 2, repeatPeriod: 1200 }] : [], [location])
  const polygonCapColor = useCallback(() => 'rgba(0,0,0,0)', [])
  const polygonSideColor = useCallback(() => 'rgba(0,0,0,0)', [])
  const polygonStrokeColor = useCallback(() => {
    const { r, g, b } = new Color(theme.accent)
    const opacity = theme.dark ? 0.72 : 0.62
    return `rgba(${Math.round(r * 255)}, ${Math.round(g * 255)}, ${Math.round(b * 255)}, ${opacity})`
  }, [theme.accent, theme.dark])
  const ringColor = useCallback(() => (t: number) => {
    const { r, g, b } = new Color(theme.accent)
    return `rgba(${Math.round(r * 255)}, ${Math.round(g * 255)}, ${Math.round(b * 255)}, ${1 - t})`
  }, [theme.accent])
  const rendererConfig = useMemo(() => ({ alpha: true, antialias: true, powerPreference: 'low-power' as const }), [])
  const ringMaxRadius = useCallback(() => 5, [])
  return <div ref={containerRef} className="network-globe-canvas" role="img" aria-label={location ? `${nodeLabel} · ${countryLabel}` : nodeLabel}>
    {size.width > 0 && size.height > 0 && <div className="network-globe-stage" style={{ width: size.width, height: size.height }}>
      <Globe
      ref={globeRef}
      width={size.width}
      height={size.height}
      backgroundColor="rgba(0,0,0,0)"
      showGlobe
      globeMaterial={hologramMaterial}
      showGraticules={false}
      globeCurvatureResolution={6}
      showAtmosphere
      atmosphereColor={theme.accent}
      atmosphereAltitude={theme.dark ? 0.1 : 0.075}
      rendererConfig={rendererConfig}
      polygonsData={countryBoundaries}
      polygonCapColor={polygonCapColor}
      polygonSideColor={polygonSideColor}
      polygonStrokeColor={polygonStrokeColor}
      polygonAltitude={0.006}
      pointsData={points}
      pointLat="lat"
      pointLng="lng"
      pointColor="color"
      pointAltitude={0.16}
      pointRadius={0.8}
      pointLabel="label"
      ringsData={rings}
      ringLat="lat"
      ringLng="lng"
      ringColor={ringColor}
      ringMaxRadius={ringMaxRadius}
      ringPropagationSpeed="propagationSpeed"
      ringRepeatPeriod="repeatPeriod"
      enablePointerInteraction={active && Boolean(location)}
      onGlobeReady={() => {
        fitCameraToViewport()
        applyHologramStyle()
      }}
      animateIn
      />
    </div>}
  </div>
})

type WorkspaceLayout = { chat: number; network: number; historyOpen: boolean; historyWidth: number; topologyOpen: boolean }
function readLayout(): WorkspaceLayout {
  try {
    const parsed: unknown = JSON.parse(window.localStorage.getItem(layoutStorageKey) ?? 'null')
    if (parsed && typeof parsed === 'object' && 'chat' in parsed && 'network' in parsed && typeof parsed.chat === 'number' && typeof parsed.network === 'number') {
      const saved = parsed as Record<string, unknown>
      return {
        chat: saved.chat as number,
        network: saved.network as number,
        historyOpen: typeof saved.historyOpen === 'boolean' ? saved.historyOpen : false,
        historyWidth: typeof saved.historyWidth === 'number' && Number.isFinite(saved.historyWidth) ? saved.historyWidth : 300,
        topologyOpen: typeof saved.topologyOpen === 'boolean' ? saved.topologyOpen : true,
      }
    }
  } catch { /* Ignore invalid or unavailable saved layout. */ }
  return { chat: 48, network: 52, historyOpen: false, historyWidth: 300, topologyOpen: true }
}
function saveLayout(update: Partial<WorkspaceLayout>) {
  try { window.localStorage.setItem(layoutStorageKey, JSON.stringify({ ...readLayout(), ...update })) }
  catch { /* Storage can be unavailable in restricted browser contexts. */ }
}

export function WorkspacePage({ snapshot, messages, direction, visible = true }: { snapshot: NodeSnapshot; messages: Messages; direction: 'ltr' | 'rtl'; visible?: boolean }) {
  const navigate = useNavigate()
  const compact = useBreakpoint('(max-width: 900px)')
  const reduceMotion = useReducedMotion()
  const [savedLayout] = useState(readLayout)
  const [mobileView, setMobileView] = useState<'chat' | 'network'>('chat')
  const [historyOpen, setHistoryOpen] = useState(savedLayout.historyOpen)
  const [historyWidth, setHistoryWidth] = useState(savedLayout.historyWidth)
  const [historyResizing, setHistoryResizing] = useState(false)
  const [historyPanelHost, setHistoryPanelHost] = useState<HTMLDivElement | null>(null)
  const desktopStageRef = useRef<HTMLDivElement>(null)
  const historyDragRef = useRef<{ pointerId: number; startX: number; startWidth: number } | null>(null)
  const [topologyOpen, setTopologyOpen] = useState(savedLayout.topologyOpen)
  const [topologyRatio, setTopologyRatio] = useState(() => {
    const savedRatio = savedLayout.network / 100
    return savedRatio > 0.05 && savedRatio < 0.8 ? savedRatio : 0.52
  })
  const [topologyResizing, setTopologyResizing] = useState(false)
  const [desktopStageWidth, setDesktopStageWidth] = useState(() => window.innerWidth)
  const topologyDragRef = useRef<{ pointerId: number; startX: number; startWidth: number; startRatio: number; startOpen: boolean } | null>(null)
  const [globeReady, setGlobeReady] = useState(false)
  const [touchStart, setTouchStart] = useState<{ x: number; y: number } | null>(null)
  useEffect(() => {
    if (compact || historyResizing || topologyResizing) return
    saveLayout({ historyOpen, historyWidth, topologyOpen, network: topologyRatio * 100, chat: (1 - topologyRatio) * 100 })
  }, [compact, historyOpen, historyWidth, historyResizing, topologyOpen, topologyRatio, topologyResizing])
  useEffect(() => {
    if (!compact) { setGlobeReady(true); return }
    if (mobileView !== 'network' || globeReady) return
    const timeout = window.setTimeout(() => setGlobeReady(true), reduceMotion ? 0 : 240)
    return () => window.clearTimeout(timeout)
  }, [compact, mobileView, globeReady, reduceMotion])
  useEffect(() => {
    if (compact) return
    const stage = desktopStageRef.current
    if (!stage) return
    const updateWidth = () => setDesktopStageWidth(stage.clientWidth)
    const observer = new ResizeObserver(updateWidth)
    observer.observe(stage)
    updateWidth()
    return () => observer.disconnect()
  }, [compact])
  const paneItems: LiquidDockItem[] = [
    { id: 'chat', label: messages.computeAssistant, href: '#chat', icon: <MessageSquare size={20} strokeWidth={1.8} />, active: mobileView === 'chat' },
    { id: 'network', label: messages.networkMap, href: '#network', icon: <Eye size={20} strokeWidth={1.8} />, active: mobileView === 'network' },
  ]
  const connectedPeers = snapshot.peers.filter(connected)
  const historyMinimumWidth = 280
  const minimumContentWidth = 280
  const fixedHandleWidth = 34
  const historyAvailableWidth = desktopStageWidth - fixedHandleWidth - minimumContentWidth * 2
  const historyMaxWidth = Math.min(480, Math.max(historyMinimumWidth, Math.min(desktopStageWidth * 0.4, historyAvailableWidth)))
  const visibleHistoryWidth = Math.min(historyWidth, historyMaxWidth)
  const panelAreaWidth = Math.max(0, desktopStageWidth - (historyOpen ? visibleHistoryWidth : 0) - fixedHandleWidth)
  const topologyMaxWidth = Math.max(0, Math.min(panelAreaWidth * 0.7, panelAreaWidth - minimumContentWidth))
  const topologyMinimumWidth = Math.min(280, topologyMaxWidth)
  const topologyWidth = topologyOpen ? Math.min(panelAreaWidth * topologyRatio, topologyMaxWidth) : 0
  const startHistoryResize = (event: ReactPointerEvent<HTMLButtonElement>) => {
    event.preventDefault()
    event.currentTarget.setPointerCapture(event.pointerId)
    const startWidth = historyOpen ? historyWidth : 0
    historyDragRef.current = { pointerId: event.pointerId, startX: event.clientX, startWidth }
    if (!historyOpen) setHistoryWidth(0)
    if (historyOpen) setHistoryOpen(true)
    setHistoryResizing(true)
  }
  const moveHistoryResize = (event: ReactPointerEvent<HTMLButtonElement>) => {
    const drag = historyDragRef.current
    if (!drag || drag.pointerId !== event.pointerId) return
    const delta = (event.clientX - drag.startX) * (direction === 'rtl' ? -1 : 1)
    const rawWidth = drag.startWidth + delta
    const minimumWidth = Math.min(historyMinimumWidth, historyMaxWidth)
    if (drag.startWidth > 0 && rawWidth < minimumWidth - 72) {
      const previousWidth = drag.startWidth
      drag.startX = event.clientX
      drag.startWidth = 0
      setHistoryWidth(Math.max(historyMinimumWidth, Math.min(historyMaxWidth, previousWidth)))
      setHistoryOpen(false)
      setHistoryResizing(false)
      return
    }
    if (drag.startWidth === 0 && rawWidth <= 0) {
      setHistoryOpen(false)
      setHistoryResizing(false)
      return
    }
    const nextWidth = Math.min(historyMaxWidth, rawWidth)
    if (drag.startWidth > 0) setHistoryWidth(Math.max(minimumWidth, nextWidth))
    else if (nextWidth > 0) {
      setHistoryWidth(nextWidth)
      setHistoryOpen(true)
      setHistoryResizing(true)
      if (nextWidth >= minimumWidth) {
        drag.startX = event.clientX
        drag.startWidth = nextWidth
      }
    }
  }
  const finishHistoryResize = (event: ReactPointerEvent<HTMLButtonElement>) => {
    if (historyDragRef.current?.pointerId !== event.pointerId) return
    if (historyOpen && historyWidth < historyMinimumWidth) setHistoryWidth(historyMinimumWidth)
    historyDragRef.current = null
    setHistoryResizing(false)
    if (event.currentTarget.hasPointerCapture(event.pointerId)) event.currentTarget.releasePointerCapture(event.pointerId)
  }
  const keyHistoryResize = (event: ReactKeyboardEvent<HTMLButtonElement>) => {
    if (event.key === 'Enter' || event.key === ' ') {
      setHistoryOpen((open) => !open)
      event.preventDefault()
      return
    }
    const growKey = direction === 'rtl' ? 'ArrowLeft' : 'ArrowRight'
    const shrinkKey = direction === 'rtl' ? 'ArrowRight' : 'ArrowLeft'
    if (!historyOpen && [growKey, 'Home', 'End'].includes(event.key)) {
      setHistoryWidth(event.key === 'End' ? historyMaxWidth : historyMinimumWidth)
      setHistoryOpen(true)
      event.preventDefault()
      return
    }
    if (event.key === growKey) setHistoryWidth((width) => Math.min(historyMaxWidth, width + 16))
    else if (event.key === shrinkKey) setHistoryWidth((width) => Math.max(historyMinimumWidth, width - 16))
    else if (event.key === 'Home') setHistoryWidth(historyMinimumWidth)
    else if (event.key === 'End') setHistoryWidth(historyMaxWidth)
    else return
    event.preventDefault()
  }
  const startTopologyResize = (event: ReactPointerEvent<HTMLButtonElement>) => {
    event.preventDefault()
    event.currentTarget.setPointerCapture(event.pointerId)
    topologyDragRef.current = { pointerId: event.pointerId, startX: event.clientX, startWidth: topologyWidth, startRatio: topologyRatio, startOpen: topologyOpen }
    setTopologyResizing(true)
  }
  const moveTopologyResize = (event: ReactPointerEvent<HTMLButtonElement>) => {
    const drag = topologyDragRef.current
    if (!drag || drag.pointerId !== event.pointerId) return
    const delta = (event.clientX - drag.startX) * (direction === 'rtl' ? 1 : -1)
    const rawWidth = drag.startWidth + delta
    const minimumWidth = topologyMinimumWidth
    if (drag.startOpen && rawWidth < minimumWidth - 72) {
      drag.startX = event.clientX
      drag.startWidth = 0
      drag.startOpen = false
      setTopologyOpen(false)
      setTopologyResizing(false)
      return
    }
    if (!drag.startOpen && rawWidth <= 0) return
    const nextWidth = drag.startOpen
      ? Math.max(minimumWidth, Math.min(topologyMaxWidth, rawWidth))
      : Math.max(0, Math.min(topologyMaxWidth, rawWidth))
    if (!drag.startOpen && nextWidth > 0) {
      setTopologyOpen(true)
      setTopologyResizing(true)
      if (nextWidth >= minimumWidth) {
        drag.startX = event.clientX
        drag.startWidth = nextWidth
        drag.startOpen = true
      }
    }
    if (panelAreaWidth > 0) setTopologyRatio(nextWidth / panelAreaWidth)
  }
  const finishTopologyResize = (event: ReactPointerEvent<HTMLButtonElement>) => {
    const drag = topologyDragRef.current
    if (!drag || drag.pointerId !== event.pointerId) return
    if (topologyOpen && topologyWidth < topologyMinimumWidth && topologyWidth > 0) setTopologyRatio(Math.min(1, topologyMinimumWidth / Math.max(1, panelAreaWidth)))
    topologyDragRef.current = null
    setTopologyResizing(false)
    if (topologyOpen) {
      const ratio = topologyWidth < topologyMinimumWidth && topologyWidth > 0 ? Math.min(1, topologyMinimumWidth / Math.max(1, panelAreaWidth)) : topologyRatio
      saveLayout({ chat: (1 - ratio) * 100, network: ratio * 100, topologyOpen: true })
    }
    if (event.currentTarget.hasPointerCapture(event.pointerId)) event.currentTarget.releasePointerCapture(event.pointerId)
  }
  const keyTopologyResize = (event: ReactKeyboardEvent<HTMLButtonElement>) => {
    if (event.key === 'Enter' || event.key === ' ') {
      setTopologyOpen((open) => !open)
      event.preventDefault()
      return
    }
    const growKey = direction === 'rtl' ? 'ArrowRight' : 'ArrowLeft'
    const shrinkKey = direction === 'rtl' ? 'ArrowLeft' : 'ArrowRight'
    if (!topologyOpen && event.key === growKey) {
      setTopologyRatio(Math.min(topologyMaxWidth / Math.max(1, panelAreaWidth), Math.max(topologyMinimumWidth / Math.max(1, panelAreaWidth), 0.52)))
      setTopologyOpen(true)
    } else if (event.key === growKey) setTopologyRatio(Math.min(topologyMaxWidth / Math.max(1, panelAreaWidth), topologyRatio + 0.02))
    else if (event.key === shrinkKey) setTopologyRatio(Math.max(topologyMinimumWidth / Math.max(1, panelAreaWidth), topologyRatio - 0.02))
    else return
    event.preventDefault()
  }
  const chatPane = <section className="workspace-chat workspace-glass liquid-glass-menu relative flex h-full min-h-0 flex-col rounded-[22px] px-8 pb-6 pt-7 max-[760px]:px-5">
      <div className="workspace-chat-content mx-auto flex min-h-0 w-full flex-1 flex-col"><div className="workspace-chat-surface flex min-h-0 flex-1 flex-col overflow-hidden"><AgentChat snapshot={snapshot} messages={messages} direction={direction} historyOpen={historyOpen} setHistoryOpen={setHistoryOpen} historyPanelHost={historyPanelHost} onOpenSettings={() => navigate('/settings')} /></div></div>
    </section>
  const countryCode = snapshot.info?.countryCode?.toUpperCase()
  const countryPosition = countryCode ? countryCentroids.get(countryCode) : undefined
  const networkPane = <section className="workspace-network workspace-glass liquid-glass-menu relative flex h-full min-h-0 items-center justify-center overflow-hidden rounded-[22px]">
          <div className="network-visual absolute inset-0 overflow-hidden">{globeReady && <NetworkGlobe countryCode={countryCode} peers={snapshot.peers} nodeLabel={messages.thisNode} countryLabel={countryCode ? `${messages.geoCountry}: ${countryCode} · ${messages.countryEstimateDisclaimer}` : messages.thisNode} reduceMotion={Boolean(reduceMotion)} active={visible && topologyOpen && (!compact || mobileView === 'network')} />}</div>
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
        <motion.div aria-hidden={mobileView !== 'chat'} className="workspace-mobile-pane" data-active={mobileView === 'chat'} animate={reduceMotion ? undefined : { opacity: mobileView === 'chat' ? 1 : 0, x: mobileView === 'chat' ? 0 : -24 }} transition={reduceMotion ? { duration: 0 } : { type: 'tween', duration: 0.24, ease: [0.22, 1, 0.36, 1] }} style={{ zIndex: mobileView === 'chat' ? 2 : 1, pointerEvents: mobileView === 'chat' ? 'auto' : 'none' }}>
          {chatPane}
        </motion.div>
        <motion.div aria-hidden={mobileView !== 'network'} className="workspace-mobile-pane" data-active={mobileView === 'network'} animate={reduceMotion ? undefined : { opacity: mobileView === 'network' ? 1 : 0, x: mobileView === 'network' ? 0 : 24 }} transition={reduceMotion ? { duration: 0 } : { type: 'tween', duration: 0.24, ease: [0.22, 1, 0.36, 1] }} style={{ zIndex: mobileView === 'network' ? 2 : 1, pointerEvents: mobileView === 'network' ? 'auto' : 'none' }}>
          {networkPane}
        </motion.div>
      </div>
      {visible && createPortal(<nav className="workspace-mobile-dock" aria-label={messages.workspace}><LiquidGlassDock items={paneItems} label={messages.workspace} onNavigate={(id) => setMobileView(id as 'chat' | 'network')} /></nav>, document.body)}
    </> : <div ref={desktopStageRef} className="workspace-desktop-stage">
    <div data-open={historyOpen} data-resizing={historyResizing} aria-hidden={!historyOpen} style={{ flexBasis: historyOpen ? visibleHistoryWidth : 0 }} className="workspace-history-slot"><div ref={setHistoryPanelHost} className="workspace-history-host" /></div>
    <button type="button" role="separator" aria-orientation="vertical" aria-label={messages.resizeHistory} aria-valuemin={0} aria-valuemax={Math.round(historyMaxWidth)} aria-valuenow={Math.round(visibleHistoryWidth)} onPointerDown={startHistoryResize} onPointerMove={moveHistoryResize} onPointerUp={finishHistoryResize} onPointerCancel={finishHistoryResize} onKeyDown={keyHistoryResize} onDoubleClick={(event) => { event.preventDefault(); setHistoryOpen((open) => !open) }} className="workspace-history-resize-handle" />
    <div className="workspace-chat-desktop-pane">{chatPane}</div>
    <button type="button" role="separator" aria-orientation="vertical" aria-label={messages.resizeTopology} aria-valuemin={0} aria-valuemax={Math.round(topologyMaxWidth)} aria-valuenow={Math.round(topologyWidth)} onPointerDown={startTopologyResize} onPointerMove={moveTopologyResize} onPointerUp={finishTopologyResize} onPointerCancel={finishTopologyResize} onKeyDown={keyTopologyResize} onDoubleClick={(event) => { event.preventDefault(); setTopologyOpen((open) => !open) }} className="workspace-topology-resize-handle"><span /></button>
    <div data-open={topologyOpen} data-resizing={topologyResizing} aria-hidden={!topologyOpen} style={{ flexBasis: topologyWidth }} className="workspace-topology-slot">{networkPane}</div>
    </div>
    }
  </div>
}
