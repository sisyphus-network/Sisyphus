import { memo, useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent, type PointerEvent as ReactPointerEvent, type WheelEvent as ReactWheelEvent } from 'react'
import { createPortal } from 'react-dom'
import { Check, Copy, Eye, MessageSquare, Minus, Plus, X } from 'lucide-react'
import { Link, useNavigate } from 'react-router-dom'
import { motion, useReducedMotion } from 'motion/react'
import Globe, { type GlobeMethods } from 'react-globe.gl'
import { geoGraticule10, geoMercator, geoPath, type GeoPermissibleObjects } from 'd3-geo'
import { AdditiveBlending, BufferGeometry, Color, Float32BufferAttribute, LineBasicMaterial, LineSegments, MeshBasicMaterial, NormalBlending, type PerspectiveCamera } from 'three'
import type { NodeSnapshot } from '../../../preload'
import { formatMessage, type getMessages } from '@/i18n/messages'
import centroidsGeoJson from 'world-countries-centroids/dist/countries.geojson?raw'
import countriesGeoJsonRaw from '../data/countries.geojson?raw'
import { LiquidGlassDock, type LiquidDockItem } from '@/components/navigation/liquid-glass-dock'
import { useBreakpoint } from '@/hooks/use-breakpoint'
import { AgentChat } from '@/components/assistant/agent-chat'
import { visiblePanelWidth, stepPanelWidth } from '@/lib/panel-width'
import { locatePeers, peerLocationsKey, sharedPlaces, type Place } from '@/lib/globe-markers'
import { webglAvailable } from '@/lib/webgl'
import { getNodeApi, isDesktopApp } from '@/lib/node-api'
import { locationFromProtocol, type LocationShare } from '@/lib/node-location'
import { cityEstimateFromProtocol, nodeDetails, peerDetails, poolWorkers, renderDetailsHtml, type CityEstimate, type GlobeDetails, type WorkerFacts } from '@/lib/globe-details'
import { SharedStateDomain } from '@sisyphus/client'
import { sharedStateDomainCode } from '@/lib/shared-state-domain'
import { useWebgl } from '@/hooks/use-webgl'
import { autoRotateSpeed, decodeRegionLines, localMapThresholdReached, nearbyRegionLines, regionSegments, zoomLimits, zoomState, zoomStep, type ZoomLimits, type ZoomState } from '@/lib/globe-view'

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
// Country outlines: crisp, as WebGL lines are one pixel, but kept quiet
// behind the points.
const countryLineOpacity = { light: 0.5, dark: 0.6 }
const countryBoundaries = (JSON.parse(countriesGeoJsonRaw) as { features: CountryBoundaryFeature[] }).features
const countryCentroids = new Map<string, { lat: number; lng: number }>(
  (JSON.parse(centroidsGeoJson) as { features: CountryCentroidFeature[] }).features.map(({ geometry, properties }) => [
    properties.ISO,
    { lng: geometry.coordinates[0], lat: geometry.coordinates[1] },
  ]),
)

// The ID the globe's point for this node goes by; a peer's is its peer ID.
const ownPointId = 'this-node'
type GlobePoint = { id: string; lat: number; lng: number; label: string; color: string }

// Lines on the far side of the globe show through its glass. Country lines
// there are drawn at backShare of their opacity, fading in across the limb,
// and none once the globe is brought in close, where the far side would be
// a mirror image behind the near one. Region lines are never drawn there.
const countryBackShare = { value: 0.3 }
const regionBackShare = { value: 0 }
function drawFacing(material: LineBasicMaterial, backShare: { value: number }) {
  material.onBeforeCompile = (shader) => {
    shader.uniforms.backShare = backShare
    shader.vertexShader = shader.vertexShader
      .replace('#include <common>', '#include <common>\nvarying float vFacing;')
      .replace('#include <project_vertex>', '#include <project_vertex>\nvec3 facingWorld = (modelMatrix * vec4(transformed, 1.0)).xyz;\nvFacing = dot(normalize(facingWorld), normalize(cameraPosition - facingWorld));')
    shader.fragmentShader = shader.fragmentShader
      .replace('#include <common>', '#include <common>\nvarying float vFacing;\nuniform float backShare;')
      .replace('#include <opaque_fragment>', 'diffuseColor.a *= mix(backShare, 1.0, smoothstep(-0.04, 0.2, vFacing));\nif (diffuseColor.a <= 0.001) discard;\n#include <opaque_fragment>')
  }
  // The same source for every material drawn this way, so they share one program.
  material.customProgramCacheKey = () => 'sisyphus-facing'
  material.needsUpdate = true
}
// countryLinesFacing draws the globe's country outlines that way, and says
// whether there were any yet: the polygon layer makes them after the globe
// is ready.
function countryLinesFacing(scene: { traverse: (visit: (object: unknown) => void) => void }): boolean {
  let found = false
  scene.traverse((object) => {
    const line = object as LineSegments
    if (!line.isLineSegments || (line.parent as { __globeObjType?: string } | null)?.__globeObjType !== 'polygon') return
    found = true
    for (const material of Array.isArray(line.material) ? line.material : [line.material]) {
      if (!(material instanceof LineBasicMaterial) || material.userData.facing === true) continue
      drawFacing(material, countryBackShare)
      material.userData.facing = true
    }
  })
  return found
}
// Region lines are drawn this much fainter than country lines.
const regionLineShare = 0.42
function regionLineMaterial() {
  const material = new LineBasicMaterial({ transparent: true, depthWrite: false })
  drawFacing(material, regionBackShare)
  return material
}
let regionLinesData: Promise<Array<Array<[number, number]>>> | undefined
// Cache the decoded region lines so the local map can be drawn without parsing
// its data during the globe-to-map handoff.
function loadRegionLines() {
  regionLinesData ??= import('../data/admin1-lines.json?raw').then(({ default: raw }) => decodeRegionLines(JSON.parse(raw)))
  return regionLinesData
}

type LocalMapControls = { zoomBy: (direction: 'in' | 'out') => void }
function LocalMapOverlay({ center, width, height, lines, points, interactive, label, onControlsReady, onExit }: { center: { lat: number; lng: number }; width: number; height: number; lines: Array<Array<[number, number]>>; points: GlobePoint[]; interactive: boolean; label: string; onControlsReady: (controls: LocalMapControls | undefined) => void; onExit: () => void }) {
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const viewRef = useRef({ scale: 1, x: 0, y: 0 })
  const dragRef = useRef<{ pointerId: number; x: number; y: number; viewX: number; viewY: number } | undefined>(undefined)
  const frameRef = useRef<number | undefined>(undefined)
  const localLines = useMemo(() => nearbyRegionLines(lines, center), [center.lat, center.lng, lines])
  const localCountries = useMemo(() => countryBoundaries.filter((feature) => {
    const code = String(feature.properties.ISO_A2_EH ?? feature.properties.ISO_A2 ?? '')
    const centroid = countryCentroids.get(code)
    if (!centroid) return false
    const radians = Math.PI / 180
    const lat1 = center.lat * radians
    const lat2 = centroid.lat * radians
    const deltaLng = (centroid.lng - center.lng) * radians
    const cosine = Math.sin(lat1) * Math.sin(lat2) + Math.cos(lat1) * Math.cos(lat2) * Math.cos(deltaLng)
    return cosine >= Math.cos(88 * radians)
  }), [center.lat, center.lng])
  const applyTransform = useCallback(() => {
    const canvas = canvasRef.current
    if (!canvas) return
    const { scale, x, y } = viewRef.current
    canvas.style.transform = `translate3d(${x}px, ${y}px, 0) scale(${scale})`
  }, [])
  const scheduleTransform = useCallback(() => {
    if (frameRef.current !== undefined) return
    frameRef.current = window.requestAnimationFrame(() => {
      frameRef.current = undefined
      applyTransform()
    })
  }, [applyTransform])
  useEffect(() => {
    const canvas = canvasRef.current
    const context = canvas?.getContext('2d', { alpha: false })
    if (!canvas || !context || width < 1 || height < 1) return
    const ratio = Math.min(2, window.devicePixelRatio || 1)
    canvas.width = Math.round(width * ratio)
    canvas.height = Math.round(height * ratio)
    canvas.style.width = `${width}px`
    canvas.style.height = `${height}px`
    context.setTransform(ratio, 0, 0, ratio, 0, 0)
    const style = getComputedStyle(canvas)
    const projection = geoMercator()
      .center([center.lng, center.lat])
      .scale(width / (110 * Math.PI / 180))
      .translate([width / 2, height / 2])
    const path = geoPath(projection, context)
    context.fillStyle = style.getPropertyValue('--app-card').trim() || '#fff'
    context.fillRect(0, 0, width, height)
    context.beginPath()
    path(geoGraticule10())
    context.strokeStyle = style.getPropertyValue('--app-accent').trim() || '#568cff'
    context.globalAlpha = 0.1
    context.lineWidth = 0.7
    context.stroke()
    context.beginPath()
    path({ type: 'FeatureCollection', features: localCountries } as unknown as GeoPermissibleObjects)
    context.strokeStyle = style.getPropertyValue('--app-muted').trim() || '#78808f'
    context.globalAlpha = 0.68
    context.lineWidth = 1.15
    context.stroke()
    if (localLines.length) {
      context.beginPath()
      path({ type: 'MultiLineString', coordinates: localLines.map((line) => line.map(([lat, lng]) => [lng, lat])) } as GeoPermissibleObjects)
      context.strokeStyle = style.getPropertyValue('--app-accent').trim() || '#568cff'
      context.globalAlpha = 0.82
      context.lineWidth = 1.1
      context.stroke()
    }
    for (const point of points) {
      const position = projection([point.lng, point.lat])
      if (!position || position[0] < 0 || position[0] > width || position[1] < 0 || position[1] > height) continue
      context.beginPath()
      context.arc(position[0], position[1], point.id === ownPointId ? 5 : 3.8, 0, Math.PI * 2)
      context.fillStyle = point.color
      context.globalAlpha = 1
      context.fill()
      context.strokeStyle = style.getPropertyValue('--app-card').trim() || '#fff'
      context.lineWidth = 1.5
      context.stroke()
    }
    context.globalAlpha = 1
    applyTransform()
  }, [applyTransform, center.lat, center.lng, height, localCountries, localLines, points, width])
  useEffect(() => () => {
    if (frameRef.current !== undefined) window.cancelAnimationFrame(frameRef.current)
  }, [])
  const zoomBy = useCallback((direction: 'in' | 'out') => {
    const nextScale = viewRef.current.scale * (direction === 'in' ? 1.35 : 1 / 1.35)
    if (direction === 'out' && nextScale < 1.04) { onExit(); return }
    viewRef.current = { ...viewRef.current, scale: Math.max(1, Math.min(12, nextScale)) }
    scheduleTransform()
  }, [onExit, scheduleTransform])
  useEffect(() => {
    onControlsReady(interactive ? { zoomBy } : undefined)
    return () => onControlsReady(undefined)
  }, [interactive, onControlsReady, zoomBy])
  const onWheel = (event: ReactWheelEvent<HTMLCanvasElement>) => {
    event.preventDefault()
    event.stopPropagation()
    const current = viewRef.current
    const scale = Math.max(1, Math.min(12, current.scale * Math.exp(-event.deltaY * 0.0015)))
    if (scale < 1.015) { onExit(); return }
    const rect = event.currentTarget.getBoundingClientRect()
    const x = event.clientX - rect.left
    const y = event.clientY - rect.top
    viewRef.current = { scale, x: x - (x - width / 2 - current.x) * scale / current.scale - width / 2, y: y - (y - height / 2 - current.y) * scale / current.scale - height / 2 }
    scheduleTransform()
  }
  return <div className="network-local-map" role="application" aria-label={label} tabIndex={interactive ? 0 : -1} data-interactive={interactive} onKeyDown={(event) => {
    const delta = 48
    if (event.key === 'ArrowLeft') viewRef.current.x += delta
    else if (event.key === 'ArrowRight') viewRef.current.x -= delta
    else if (event.key === 'ArrowUp') viewRef.current.y += delta
    else if (event.key === 'ArrowDown') viewRef.current.y -= delta
    else if (event.key === '+' || event.key === '=') zoomBy('in')
    else if (event.key === '-') zoomBy('out')
    else return
    scheduleTransform()
    event.preventDefault()
  }}>
    <canvas ref={canvasRef} className="network-local-map__canvas" width={width} height={height} onWheel={onWheel}
      onPointerDown={(event) => {
        if (!interactive || event.button !== 0) return
        event.currentTarget.setPointerCapture(event.pointerId)
        dragRef.current = { pointerId: event.pointerId, x: event.clientX, y: event.clientY, viewX: viewRef.current.x, viewY: viewRef.current.y }
      }}
      onPointerMove={(event) => {
        const drag = dragRef.current
        if (!interactive || !drag || drag.pointerId !== event.pointerId) return
        viewRef.current.x = drag.viewX + event.clientX - drag.x
        viewRef.current.y = drag.viewY + event.clientY - drag.y
        scheduleTransform()
      }}
      onPointerUp={(event) => { if (dragRef.current?.pointerId === event.pointerId) dragRef.current = undefined }}
      onPointerCancel={() => { dragRef.current = undefined }} />
  </div>
}

const NetworkGlobe = memo(function NetworkGlobe({ countryCode, pin, shared, peers, nodeLabel, countryLabel, reduceMotion, active, rotationPaused = false, suspendResize = false, pointNames, pointsLabel, pointButtonLabel, pointCard, onPickPoint, zoomLabels }: { countryCode?: string; pin?: { lat: number; lng: number }; shared?: ReadonlyMap<string, Place>; peers: NodeSnapshot['peers']; nodeLabel: string; countryLabel: string; reduceMotion: boolean; active: boolean; rotationPaused?: boolean; suspendResize?: boolean; pointNames: ReadonlyMap<string, string>; pointsLabel: string; pointButtonLabel: string; pointCard: (id: string) => string; onPickPoint: (id: string, from?: HTMLElement) => void; zoomLabels: { group: string; in: string; out: string } }) {
  const containerRef = useRef<HTMLDivElement>(null)
  const globeRef = useRef<GlobeMethods | undefined>(undefined)
  const [size, setSize] = useState({ width: 0, height: 0 })
  const [theme, setTheme] = useState<HologramTheme>(readHologramTheme)
  const limitsRef = useRef<ZoomLimits | undefined>(undefined)
  const countryFacingRef = useRef(false)
  const [zoom, setZoom] = useState<ZoomState>()
  const mapCenterRef = useRef<{ lat: number; lng: number } | undefined>(undefined)
  const locationRef = useRef<{ lat: number; lng: number } | undefined>(undefined)
  const [mapCenter, setMapCenter] = useState<{ lat: number; lng: number }>()
  const [mapViewport, setMapViewport] = useState({ width: 0, height: 0 })
  const [regionLines, setRegionLines] = useState<Array<Array<[number, number]>>>()
  const mapInteractiveRef = useRef(false)
  const mapInteractive = Boolean(mapCenter)
  const mapSuppressedRef = useRef(false)
  const mapControlsRef = useRef<LocalMapControls | undefined>(undefined)
  // Button steps still under way. The buttons take their last state only when
  // a step ends, so the globe says while any is under way, for whoever has to
  // wait for that state, such as the end-to-end tests.
  const stepsRef = useRef(0)
  const zoomAnimationRef = useRef(false)
  const zoomAnimationTimer = useRef<number | undefined>(undefined)
  const [stepping, setStepping] = useState(false)
  const [zoomStepCount, setZoomStepCount] = useState(0)
  useEffect(() => () => {
    if (zoomAnimationTimer.current !== undefined) window.clearTimeout(zoomAnimationTimer.current)
  }, [])
  const regionsRef = useRef<LineSegments | null>(null)
  const regionMaterial = useMemo(regionLineMaterial, [])
  const [regionsLoaded, setRegionsLoaded] = useState(false)
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
          ? theme.dark ? countryLineOpacity.dark : countryLineOpacity.light
          : theme.dark ? 0.19 : 0.17
        material.transparent = true
        material.needsUpdate = true
      }
    })
    if (scene) countryFacingRef.current = countryLinesFacing(scene)
  }, [hologramMaterial, theme])
  useEffect(() => {
    if (!active || regionLines) return
    let cancelled = false
    let idleHandle: number | undefined
    let timeoutHandle: number | undefined
    const load = () => {
      void loadRegionLines().then((lines) => {
        if (!cancelled) setRegionLines(lines)
      }).catch(() => {})
    }
    const idleWindow = window as Window & {
      requestIdleCallback?: (callback: () => void, options?: { timeout: number }) => number
      cancelIdleCallback?: (handle: number) => void
    }
    if (idleWindow.requestIdleCallback) idleHandle = idleWindow.requestIdleCallback(load, { timeout: 1200 })
    else timeoutHandle = window.setTimeout(load, 250)
    return () => {
      cancelled = true
      if (idleHandle !== undefined) idleWindow.cancelIdleCallback?.(idleHandle)
      if (timeoutHandle !== undefined) window.clearTimeout(timeoutHandle)
    }
  }, [active, regionLines])
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
    // The square stage keeps the globe's fit the same at any size, so a
    // resize leaves the zoom the user chose; the limits only keep it sensible.
    const radius = globe.getGlobeRadius()
    const limits = zoomLimits(radius, safeDistance)
    limitsRef.current = limits
    controls.minDistance = limits.min
    controls.maxDistance = limits.max
    controls.enableZoom = !mapInteractiveRef.current
    controls.enableRotate = !mapCenterRef.current
    controls.autoRotate = !reduceMotion && active && !mapCenterRef.current
    controls.autoRotateSpeed = autoRotateSpeed(camera.position.length(), radius, limits)
    controls.enableDamping = true
    controls.update()
    const next = zoomState(camera.position.length(), limits)
    const displayZoom = { ...next, percent: Math.round(next.percent / 5) * 5 }
    setZoom((current) => current && current.canZoomIn === displayZoom.canZoomIn && current.canZoomOut === displayZoom.canZoomOut && current.regions === displayZoom.regions && current.percent === displayZoom.percent ? current : displayZoom)
  }, [active, reduceMotion])
  const setLocalMapControls = useCallback((controls: LocalMapControls | undefined) => {
    mapControlsRef.current = controls
  }, [])
  const exitLocalMap = useCallback(() => {
    const globe = globeRef.current
    const limits = limitsRef.current
    if (!globe || !limits) return
    const center = mapCenterRef.current ?? globe.pointOfView()
    mapSuppressedRef.current = true
    mapInteractiveRef.current = false
    mapCenterRef.current = undefined
    setMapCenter(undefined)
    globe.resumeAnimation()
    globe.controls().enableZoom = true
    globe.controls().enableRotate = true
    globe.controls().autoRotate = !reduceMotion && active && !rotationPaused
    const radius = globe.getGlobeRadius()
    globe.pointOfView({ lat: center.lat, lng: center.lng, altitude: limits.fit * 0.96 / radius - 1 }, reduceMotion ? 0 : 360)
  }, [active, reduceMotion, rotationPaused])
  // Whenever the camera moves in or out, by wheel, pinch or button.
  const handleZoom = useCallback(() => {
    const globe = globeRef.current
    const limits = limitsRef.current
    if (!globe || !limits) return
    // Called for every move, the globe's own turning too; the outlines are
    // looked for only until they are there.
    if (!countryFacingRef.current) countryFacingRef.current = countryLinesFacing(globe.scene())
    const distance = globe.camera().position.length()
    globe.controls().autoRotateSpeed = autoRotateSpeed(distance, globe.getGlobeRadius(), limits)
    const next = zoomState(distance, limits)
    countryBackShare.value = next.regions ? 0 : 0.3
    if (mapSuppressedRef.current && distance > limits.fit * 0.94) mapSuppressedRef.current = false
    if (!mapCenterRef.current && localMapThresholdReached(distance, limits.fit) && !mapSuppressedRef.current) {
      const view = globe.pointOfView()
      mapCenterRef.current = locationRef.current ?? { lat: view.lat, lng: view.lng }
      mapInteractiveRef.current = true
      setMapCenter(mapCenterRef.current)
      globe.controls().autoRotate = false
      globe.controls().enableRotate = false
      globe.controls().enableZoom = false
      globe.pauseAnimation()
    } else if (mapCenterRef.current && distance > limits.fit * 0.91) {
      mapCenterRef.current = undefined
      setMapCenter(undefined)
      globe.controls().autoRotate = !reduceMotion && active && !rotationPaused
      globe.controls().enableRotate = true
      globe.controls().enableZoom = true
      mapInteractiveRef.current = false
      globe.resumeAnimation()
    }
    const displayZoom = { ...next, percent: Math.round(next.percent / 5) * 5 }
    setZoom((current) => current && current.canZoomIn === displayZoom.canZoomIn && current.canZoomOut === displayZoom.canZoomOut && current.regions === displayZoom.regions && current.percent === displayZoom.percent ? current : displayZoom)
  }, [active, reduceMotion, rotationPaused])
  const zoomBy = useCallback((direction: 'in' | 'out') => {
    if (mapInteractiveRef.current) { mapControlsRef.current?.zoomBy(direction); return }
    const globe = globeRef.current
    const limits = limitsRef.current
    if (!globe || !limits || zoomAnimationRef.current) return
    const radius = globe.getGlobeRadius()
    const view = globe.pointOfView()
    const distance = zoomStep(radius * (1 + view.altitude), direction, radius, limits)
    const duration = reduceMotion ? 0 : 320
    zoomAnimationRef.current = true
    globe.pointOfView({ lat: view.lat, lng: view.lng, altitude: distance / radius - 1 }, duration)
    stepsRef.current++
    setZoomStepCount((step) => step + 1)
    setStepping(true)
    zoomAnimationTimer.current = window.setTimeout(() => {
      zoomAnimationTimer.current = undefined
      handleZoom()
      zoomAnimationRef.current = false
      stepsRef.current--
      if (stepsRef.current === 0) setStepping(false)
    }, duration + 40)
  }, [handleZoom, reduceMotion])
  const wantRegions = Boolean(zoom?.regions)
  useEffect(() => {
    if (!wantRegions || !regionLines || mapInteractiveRef.current || regionsRef.current) return
    const globe = globeRef.current
    if (!globe || regionsRef.current) return
    const geometry = new BufferGeometry()
    geometry.setAttribute('position', new Float32BufferAttribute(regionSegments(regionLines, (lat, lng) => globe.getCoords(lat, lng, 0.0065)), 3))
    const regions = new LineSegments(geometry, regionMaterial)
    regions.renderOrder = 1
    regionsRef.current = regions
    setRegionsLoaded(true)
  }, [wantRegions, regionLines, regionMaterial, mapInteractive])
  useEffect(() => {
    if (regionsRef.current) regionsRef.current.visible = wantRegions
  }, [wantRegions, regionsLoaded])
  const regionLayer = useMemo(() => regionsLoaded ? [{ id: 'regions' }] : [], [regionsLoaded])
  const regionObject = useCallback(() => regionsRef.current ?? new LineSegments(), [])
  useEffect(() => {
    regionMaterial.color.set(theme.accent)
    regionMaterial.opacity = (theme.dark ? countryLineOpacity.dark : countryLineOpacity.light) * regionLineShare
    regionMaterial.needsUpdate = true
  }, [regionMaterial, theme])
  useEffect(() => () => {
    const regions = regionsRef.current
    regions?.removeFromParent()
    regions?.geometry.dispose()
    regionMaterial.dispose()
  }, [regionMaterial])
  useEffect(() => {
    if (suspendResize) return
    const container = containerRef.current
    if (!container) return
    let frame = 0
    let lastMeasureAt = 0
    const measureAndCommit = () => {
      const now = performance.now()
      if (rotationPaused && now - lastMeasureAt < 32) return
      lastMeasureAt = now
      const rect = container.getBoundingClientRect()
      const mapWidth = Math.round(rect.width)
      const mapHeight = Math.round(rect.height)
      setMapViewport((current) => current.width === mapWidth && current.height === mapHeight ? current : { width: mapWidth, height: mapHeight })
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
  }, [rotationPaused, suspendResize])
  useEffect(() => {
    if (size.width > 0 && size.height > 0) fitCameraToViewport()
  }, [fitCameraToViewport, size.width > 0])

  useEffect(() => {
    if (size.width === 0 || size.height === 0) return
    const renderer = globeRef.current?.renderer()
    if (renderer && renderer.getPixelRatio() < 2) renderer.setPixelRatio(2)
  }, [size.width > 0])

  useEffect(() => {
    const globe = globeRef.current
    if (!globe) return
    globe.controls().autoRotate = !reduceMotion && active && !rotationPaused && !mapCenterRef.current
  }, [active, reduceMotion, rotationPaused, mapCenter])

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

  // The owner's pin, if there is one, places this node better than its country can.
  const centroid = countryCode ? countryCentroids.get(countryCode.toUpperCase()) : undefined
  const location = useMemo(() => pin ? { lat: pin.lat, lng: pin.lng } : centroid, [pin?.lat, pin?.lng, centroid])
  locationRef.current = location
  const peerLocations = locatePeers(peers, countryCentroids, shared)
  const locationsKey = peerLocationsKey(peerLocations)
  const stablePeerLocations = useMemo(() => peerLocations, [locationsKey])
  const points = useMemo(() => {
    const own: GlobePoint[] = location ? [{ id: ownPointId, ...location, label: nodeLabel, color: theme.accent }] : []
    const known: GlobePoint[] = stablePeerLocations.map((peer) => ({ id: peer.peerId, lat: peer.lat, lng: peer.lng, label: peer.peerId, color: theme.accent }))
    return [...own, ...known]
  }, [location, nodeLabel, stablePeerLocations, theme.accent])
  // The card is read when the pointer reaches a point, so it says what is
  // known then, not when the point was drawn.
  const pointLabel = useCallback((point: object) => pointCard((point as GlobePoint).id), [pointCard])
  const pickPoint = useCallback((point: object) => onPickPoint((point as GlobePoint).id), [onPickPoint])
  const rings = useMemo(() => location ? [{ ...location, maxRadius: 5, propagationSpeed: 2, repeatPeriod: 1200 }] : [], [location])
  const polygonCapColor = useCallback(() => 'rgba(0,0,0,0)', [])
  const polygonSideColor = useCallback(() => 'rgba(0,0,0,0)', [])
  const polygonStrokeColor = useCallback(() => {
    const { r, g, b } = new Color(theme.accent)
    const opacity = theme.dark ? countryLineOpacity.dark : countryLineOpacity.light
    return `rgba(${Math.round(r * 255)}, ${Math.round(g * 255)}, ${Math.round(b * 255)}, ${opacity})`
  }, [theme.accent, theme.dark])
  const ringColor = useCallback(() => (t: number) => {
    const { r, g, b } = new Color(theme.accent)
    return `rgba(${Math.round(r * 255)}, ${Math.round(g * 255)}, ${Math.round(b * 255)}, ${1 - t})`
  }, [theme.accent])
  const rendererConfig = useMemo(() => ({ alpha: true, antialias: true, powerPreference: 'low-power' as const }), [])
  const ringMaxRadius = useCallback(() => 5, [])
  return <>
  <div ref={containerRef} className="network-globe-canvas" role={mapInteractive ? undefined : 'img'} aria-label={mapInteractive ? undefined : location ? `${nodeLabel} · ${countryLabel}` : nodeLabel} data-active={active} data-rotation-paused={rotationPaused} data-zoom={zoom?.percent} data-zoom-step={zoomStepCount} data-zooming={stepping} data-zoomed={Boolean(zoom && zoom.percent > 100)} data-regions={regionsLoaded && wantRegions ? 'shown' : 'hidden'} data-local-map={mapInteractive}>
    {mapCenter && mapViewport.width > 0 && mapViewport.height > 0 && <LocalMapOverlay center={mapCenter} width={mapViewport.width} height={mapViewport.height} lines={regionLines ?? []} points={points} interactive label={`${nodeLabel} · ${countryLabel}`} onControlsReady={setLocalMapControls} onExit={exitLocalMap} />}
    {webglAvailable() && size.width > 0 && size.height > 0 && <div className="network-globe-stage" style={{ width: size.width, height: size.height, visibility: mapInteractive ? 'hidden' : 'visible' }}>
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
      pointAltitude={0.012}
      pointRadius={0.32}
      pointsTransitionDuration={0}
      pointLabel={pointLabel}
      onPointClick={pickPoint}
      ringsData={rings}
      ringLat="lat"
      ringLng="lng"
      ringColor={ringColor}
      ringMaxRadius={ringMaxRadius}
      ringPropagationSpeed="propagationSpeed"
      ringRepeatPeriod="repeatPeriod"
      enablePointerInteraction={active && points.length > 0}
      customLayerData={regionLayer}
      customThreeObject={regionObject}
      onZoom={handleZoom}
      onGlobeReady={() => {
        fitCameraToViewport()
        applyHologramStyle()
      }}
      animateIn
      />
    </div>}
  </div>
  {/* A point on a WebGL globe cannot take the keyboard's focus, so each is
      also a button here, out of sight until it is focused. */}
  {active && points.length > 0 && <nav className="globe-point-list" aria-label={pointsLabel}>
    <ul>{points.map((point) => <li key={point.id}><button type="button" onClick={(event) => onPickPoint(point.id, event.currentTarget)}>{formatMessage(pointButtonLabel, { name: pointNames.get(point.id) ?? point.label })}</button></li>)}</ul>
  </nav>}
  {/* The wheel and a pinch zoom too; these are for the keyboard, and for a
      touch screen or a mouse with no wheel. */}
  {active && webglAvailable() && zoom && <div className="globe-zoom" role="group" aria-label={zoomLabels.group}>
    <button type="button" aria-label={zoomLabels.in} title={zoomLabels.in} disabled={(!mapInteractive && !zoom.canZoomIn) || stepping} onClick={() => zoomBy('in')}><Plus size={15} strokeWidth={1.9} aria-hidden /></button>
    <button type="button" aria-label={zoomLabels.out} title={zoomLabels.out} disabled={(!mapInteractive && !zoom.canZoomOut) || stepping} onClick={() => zoomBy('out')}><Minus size={15} strokeWidth={1.9} aria-hidden /></button>
  </div>}
  </>
})

// GlobeCard is a point's card kept open: what its hover card says, with its
// peer ID to copy, until it is closed with its button or Escape.
function GlobeCard({ details, html, labels, onClose }: { details: GlobeDetails; html: string; labels: Messages['globeDetails']; onClose: () => void }) {
  const [copied, setCopied] = useState(false)
  const cardRef = useRef<HTMLElement>(null)
  useEffect(() => { cardRef.current?.focus() }, [details.peerId])
  useEffect(() => {
    if (!copied) return
    const timeout = window.setTimeout(() => setCopied(false), 1600)
    return () => window.clearTimeout(timeout)
  }, [copied])
  useEffect(() => {
    const close = (event: KeyboardEvent) => {
      if (event.key !== 'Escape' || event.defaultPrevented) return
      // Escape in the chat, or in a dialog of its own, is not for this card.
      const target = event.target as Element | null
      const here = !target || target === document.body || cardRef.current?.contains(target) || target.closest?.('.workspace-network')
      if (!here) return
      event.preventDefault()
      onClose()
    }
    window.addEventListener('keydown', close)
    return () => window.removeEventListener('keydown', close)
  }, [onClose])
  const title = details.kind === 'node' ? labels.thisNode : details.worker?.name || (details.kind === 'worker' ? labels.worker : labels.peer)
  return <section ref={cardRef} tabIndex={-1} role="dialog" aria-label={formatMessage(labels.pointButton, { name: title })} className="globe-card-pinned liquid-glass-menu">
    <div className="globe-card-pinned__actions">
      <button type="button" onClick={() => { void navigator.clipboard.writeText(details.peerId).then(() => setCopied(true), () => {}) }}>{copied ? <Check size={14} aria-hidden /> : <Copy size={14} aria-hidden />}<span>{copied ? labels.copied : labels.copyPeerId}</span></button>
      <button type="button" aria-label={labels.close} title={labels.close} onClick={onClose}><X size={14} aria-hidden /></button>
    </div>
    <div dangerouslySetInnerHTML={{ __html: html }} />
  </section>
}

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
  // The topology is a globe drawn with WebGL. Where the window has none it
  // is not offered at all: no pane, no dock entry, and nothing to land on.
  const topologyAvailable = useWebgl()
  const [mobileView, setMobileView] = useState<'chat' | 'network'>('chat')
  const [activeChatId, setActiveChatId] = useState('')
  const [historyOpen, setHistoryOpen] = useState(savedLayout.historyOpen)
  const [historyWidth, setHistoryWidth] = useState(savedLayout.historyWidth)
  const [historyResizing, setHistoryResizing] = useState(false)
  const [historyPanelHost, setHistoryPanelHost] = useState<HTMLDivElement | null>(null)
  const desktopStageRef = useRef<HTMLDivElement>(null)
  const historyDragRef = useRef<{ pointerId: number; startX: number; startY: number; startWidth: number; dragging: boolean } | null>(null)
  const historyLastTapRef = useRef<{ at: number; x: number; y: number } | null>(null)
  const [topologyOpen, setTopologyOpen] = useState(savedLayout.topologyOpen)
  const [topologyRatio, setTopologyRatio] = useState(() => {
    const savedRatio = savedLayout.network / 100
    return savedRatio > 0.05 && savedRatio < 0.8 ? savedRatio : 0.52
  })
  const [topologyResizing, setTopologyResizing] = useState(false)
  const [topologyTransitioning, setTopologyTransitioning] = useState(false)
  const topologyTransitionTimer = useRef<number | undefined>(undefined)
  const [desktopStageWidth, setDesktopStageWidth] = useState(() => window.innerWidth)
  const topologyDragRef = useRef<{ pointerId: number; startX: number; startY: number; startWidth: number; startRatio: number; startOpen: boolean; dragging: boolean } | null>(null)
  const topologyLastTapRef = useRef<{ at: number; x: number; y: number } | null>(null)
  const setTopologyOpenWithMotion = (open: boolean | ((current: boolean) => boolean)) => {
    setTopologyTransitioning(true)
    if (topologyTransitionTimer.current !== undefined) window.clearTimeout(topologyTransitionTimer.current)
    setTopologyOpen(open)
    topologyTransitionTimer.current = window.setTimeout(() => {
      topologyTransitionTimer.current = undefined
      setTopologyTransitioning(false)
    }, reduceMotion ? 0 : 440)
  }
  useEffect(() => () => {
    if (topologyTransitionTimer.current !== undefined) window.clearTimeout(topologyTransitionTimer.current)
  }, [])
  const [globeReady, setGlobeReady] = useState(false)
  useEffect(() => {
    if (compact || historyResizing || topologyResizing) return
    // What was chosen for the topology is kept as it was while it cannot be
    // shown, for a window that can show it again.
    if (!topologyAvailable) { saveLayout({ historyOpen, historyWidth }); return }
    saveLayout({ historyOpen, historyWidth, topologyOpen, network: topologyRatio * 100, chat: (1 - topologyRatio) * 100 })
  }, [compact, historyOpen, historyWidth, historyResizing, topologyOpen, topologyRatio, topologyResizing, topologyAvailable])
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
    ...(topologyAvailable ? [{ id: 'network', label: messages.networkMap, href: '#network', icon: <Eye size={20} strokeWidth={1.8} />, active: mobileView === 'network' }] : []),
  ]
  // A view that is selected and then found not to be there gives way to the chat.
  useEffect(() => { if (!topologyAvailable && mobileView === 'network') setMobileView('chat') }, [topologyAvailable, mobileView])
  const connectedPeers = snapshot.peers.filter(connected)
  const historyMinimumWidth = 280
  const minimumContentWidth = 280
  const fixedHandleWidth = 34
  const historyAvailableWidth = desktopStageWidth - fixedHandleWidth - minimumContentWidth * 2
  const historyMaxWidth = Math.min(480, Math.max(historyMinimumWidth, Math.min(desktopStageWidth * 0.4, historyAvailableWidth)))
  const visibleHistoryWidth = visiblePanelWidth(historyWidth, historyMaxWidth)
  const panelAreaWidth = Math.max(0, desktopStageWidth - (historyOpen ? visibleHistoryWidth : 0) - fixedHandleWidth)
  const topologyMaxWidth = Math.max(0, Math.min(panelAreaWidth * 0.7, panelAreaWidth - minimumContentWidth))
  const topologyMinimumWidth = Math.min(280, topologyMaxWidth)
  const topologyWidth = topologyAvailable && topologyOpen ? Math.min(panelAreaWidth * topologyRatio, topologyMaxWidth) : 0
  const startHistoryResize = (event: ReactPointerEvent<HTMLButtonElement>) => {
    event.preventDefault()
    event.currentTarget.setPointerCapture(event.pointerId)
    const startWidth = historyOpen ? visibleHistoryWidth : 0
    historyDragRef.current = { pointerId: event.pointerId, startX: event.clientX, startY: event.clientY, startWidth, dragging: false }
    if (historyOpen) setHistoryOpen(true)
  }
  const moveHistoryResize = (event: ReactPointerEvent<HTMLButtonElement>) => {
    const drag = historyDragRef.current
    if (!drag || drag.pointerId !== event.pointerId) return
    if (!drag.dragging) {
      if (Math.hypot(event.clientX - drag.startX, event.clientY - drag.startY) < 4) return
      drag.dragging = true
      setHistoryResizing(true)
    }
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
    const wasDragging = historyDragRef.current.dragging
    historyDragRef.current = null
    setHistoryResizing(false)
    if (!wasDragging) {
      const previousTap = historyLastTapRef.current
      const isDoubleTap = previousTap && Date.now() - previousTap.at < 450 && Math.hypot(event.clientX - previousTap.x, event.clientY - previousTap.y) < 12
      historyLastTapRef.current = isDoubleTap ? null : { at: Date.now(), x: event.clientX, y: event.clientY }
      if (isDoubleTap) setHistoryOpen((open) => !open)
    } else {
      historyLastTapRef.current = null
    }
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
    if (event.key === growKey) setHistoryWidth((width) => stepPanelWidth(width, 16, historyMinimumWidth, historyMaxWidth))
    else if (event.key === shrinkKey) setHistoryWidth((width) => stepPanelWidth(width, -16, historyMinimumWidth, historyMaxWidth))
    else if (event.key === 'Home') setHistoryWidth(historyMinimumWidth)
    else if (event.key === 'End') setHistoryWidth(historyMaxWidth)
    else return
    event.preventDefault()
  }
  const startTopologyResize = (event: ReactPointerEvent<HTMLButtonElement>) => {
    event.preventDefault()
    event.currentTarget.setPointerCapture(event.pointerId)
    topologyDragRef.current = { pointerId: event.pointerId, startX: event.clientX, startY: event.clientY, startWidth: topologyWidth, startRatio: topologyRatio, startOpen: topologyOpen, dragging: false }
  }
  const moveTopologyResize = (event: ReactPointerEvent<HTMLButtonElement>) => {
    const drag = topologyDragRef.current
    if (!drag || drag.pointerId !== event.pointerId) return
    if (!drag.dragging) {
      if (Math.hypot(event.clientX - drag.startX, event.clientY - drag.startY) < 4) return
      drag.dragging = true
      setTopologyResizing(true)
    }
    const delta = (event.clientX - drag.startX) * (direction === 'rtl' ? 1 : -1)
    const rawWidth = drag.startWidth + delta
    const minimumWidth = topologyMinimumWidth
    if (drag.startOpen && rawWidth < minimumWidth - 72) {
      drag.startX = event.clientX
      drag.startWidth = 0
      drag.startOpen = false
      setTopologyOpenWithMotion(false)
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
    const wasDragging = drag.dragging
    if (topologyOpen && topologyWidth < topologyMinimumWidth && topologyWidth > 0) setTopologyRatio(Math.min(1, topologyMinimumWidth / Math.max(1, panelAreaWidth)))
    topologyDragRef.current = null
    setTopologyResizing(false)
    if (!wasDragging) {
      const previousTap = topologyLastTapRef.current
      const isDoubleTap = previousTap && Date.now() - previousTap.at < 450 && Math.hypot(event.clientX - previousTap.x, event.clientY - previousTap.y) < 12
      topologyLastTapRef.current = isDoubleTap ? null : { at: Date.now(), x: event.clientX, y: event.clientY }
      if (isDoubleTap) setTopologyOpenWithMotion((open) => !open)
      if (event.currentTarget.hasPointerCapture(event.pointerId)) event.currentTarget.releasePointerCapture(event.pointerId)
      return
    }
    topologyLastTapRef.current = null
    if (topologyOpen) {
      const ratio = topologyWidth < topologyMinimumWidth && topologyWidth > 0 ? Math.min(1, topologyMinimumWidth / Math.max(1, panelAreaWidth)) : topologyRatio
      saveLayout({ chat: (1 - ratio) * 100, network: ratio * 100, topologyOpen: true })
    }
    if (event.currentTarget.hasPointerCapture(event.pointerId)) event.currentTarget.releasePointerCapture(event.pointerId)
  }
  const keyTopologyResize = (event: ReactKeyboardEvent<HTMLButtonElement>) => {
    if (event.key === 'Enter' || event.key === ' ') {
      setTopologyOpenWithMotion((open) => !open)
      event.preventDefault()
      return
    }
    const growKey = direction === 'rtl' ? 'ArrowRight' : 'ArrowLeft'
    const shrinkKey = direction === 'rtl' ? 'ArrowLeft' : 'ArrowRight'
    if (!topologyOpen && event.key === growKey) {
      setTopologyRatio(Math.min(topologyMaxWidth / Math.max(1, panelAreaWidth), Math.max(topologyMinimumWidth / Math.max(1, panelAreaWidth), 0.52)))
      setTopologyOpenWithMotion(true)
    } else if (event.key === growKey) setTopologyRatio(stepPanelWidth(topologyWidth, panelAreaWidth * 0.02, topologyMinimumWidth, topologyMaxWidth) / Math.max(1, panelAreaWidth))
    else if (event.key === shrinkKey) setTopologyRatio(stepPanelWidth(topologyWidth, -panelAreaWidth * 0.02, topologyMinimumWidth, topologyMaxWidth) / Math.max(1, panelAreaWidth))
    else return
    event.preventDefault()
  }
  const chatPane = <section className="workspace-chat workspace-glass liquid-glass-menu relative flex h-full min-h-0 flex-col rounded-[22px] px-8 pb-6 pt-7 max-[760px]:px-5">
      <div className="workspace-chat-content mx-auto flex min-h-0 w-full flex-1 flex-col"><div className="workspace-chat-surface flex min-h-0 flex-1 flex-col overflow-hidden"><AgentChat snapshot={snapshot} messages={messages} direction={direction} historyOpen={historyOpen} setHistoryOpen={setHistoryOpen} historyPanelHost={historyPanelHost} onOpenSettings={() => navigate('/settings')} activeChatId={activeChatId} onActiveChatChange={setActiveChatId} /></div></div>
    </section>
  const countryCode = snapshot.info?.countryCode?.toUpperCase()
  const pin = useOwnPin(snapshot, visible)
  const { workers, shared } = usePoolWorkers(snapshot, visible)
  const cityEstimate = useCityEstimate(snapshot, visible)
  const details = useMemo(() => {
    const all = new Map<string, GlobeDetails>()
    const info = snapshot.info
    if (info) all.set(ownPointId, nodeDetails({ ...info, cityEstimate, pin: pin && { lat: pin.lat, lng: pin.lng, share: pin.share } }, workers.get(info.peerId)))
    const peers = new Map(snapshot.peers.map((peer) => [peer.peerId, peer]))
    for (const peerId of new Set([...peers.keys(), ...workers.keys()])) {
      if (peerId === info?.peerId) continue
      all.set(peerId, peerDetails(peerId, peers.get(peerId), workers.get(peerId)))
    }
    return all
  }, [snapshot.info, snapshot.peers, workers, cityEstimate, pin])
  const pointNames = useMemo(() => new Map([...details].map(([id, item]) => [id, id === ownPointId ? messages.thisNode : item.worker?.name || item.peerId])), [details, messages.thisNode])
  const renderCard = useCallback((item: GlobeDetails, pinned: boolean) => renderDetailsHtml(item, messages.globeDetails, { direction, locale: document.documentElement.lang || 'en', pinned }), [messages.globeDetails, direction])
  const pointCard = useCallback((id: string) => { const item = details.get(id); return item ? renderCard(item, false) : '' }, [details, renderCard])
  const [pinnedPoint, setPinnedPoint] = useState<string>()
  const pinnedFromRef = useRef<HTMLElement | null>(null)
  const pickPoint = useCallback((id: string, from?: HTMLElement) => { pinnedFromRef.current = from ?? null; setPinnedPoint(id) }, [])
  const closePinned = useCallback(() => {
    setPinnedPoint(undefined)
    // Back to the point's button, if the card was opened from one.
    const from = pinnedFromRef.current
    pinnedFromRef.current = null
    if (from?.isConnected) from.focus()
  }, [])
  const pinnedDetails = pinnedPoint ? details.get(pinnedPoint) : undefined
  const globeActive = visible && (compact ? mobileView === 'network' : topologyOpen)
  const zoomLabels = useMemo(() => ({ group: messages.globeZoom.group, in: messages.globeZoom.in, out: messages.globeZoom.out }), [messages.globeZoom])
  const countrySource = snapshot.info?.countrySource
  const countryPosition = countryCode ? countryCentroids.get(countryCode) : undefined
  const countryLabel = countrySource === 'pool' ? messages.geoCountryAsSeenByPool : messages.geoCountry
  const networkPane = <section className="workspace-network workspace-glass liquid-glass-menu relative flex h-full min-h-0 items-center justify-center overflow-hidden rounded-[22px]">
          <div className="network-visual absolute inset-0 overflow-hidden">{globeReady && <NetworkGlobe countryCode={countryCode} pin={pin} shared={shared} peers={snapshot.peers} nodeLabel={messages.thisNode} countryLabel={countryCode ? `${countryLabel}: ${countryCode} · ${messages.countryEstimateDisclaimer}` : messages.thisNode} reduceMotion={Boolean(reduceMotion)} active={globeActive} rotationPaused={topologyResizing} suspendResize={!compact && topologyTransitioning} pointNames={pointNames} pointsLabel={messages.globeDetails.points} pointButtonLabel={messages.globeDetails.pointButton} pointCard={pointCard} onPickPoint={pickPoint} zoomLabels={zoomLabels} />}</div>
          {globeActive && pinnedDetails && <GlobeCard details={pinnedDetails} html={renderCard(pinnedDetails, true)} labels={messages.globeDetails} onClose={closePinned} />}
          <div className="absolute inset-x-5 top-5 z-[2] flex items-center justify-between gap-3 text-[10px]">
            <div className="rounded-lg border border-[var(--app-line)] bg-[var(--app-card)]/85 px-3 py-2 text-muted-foreground shadow-sm backdrop-blur"><span className="font-mono text-foreground">{connectedPeers.length}</span> {messages.connectedPeers.toLowerCase()}</div>
            <div className="max-w-[62%] rounded-lg border border-[var(--app-line)] bg-[var(--app-card)]/85 px-3 py-2 text-end text-muted-foreground shadow-sm backdrop-blur"><span className="me-1.5 inline-block size-1.5 rounded-full bg-[var(--app-accent)] align-middle" />{messages.thisNode}{countryFlag(countryCode) ? ` · ${countryFlag(countryCode)}` : ''}{countryPosition ? <span className="ms-1.5">{countryCode}</span> : null}{!countryCode && !pin && isDesktopApp() ? <Link to="/settings/node" className="ms-1.5 text-[var(--app-accent)] underline-offset-2 hover:underline focus-visible:outline-2 focus-visible:outline-[var(--app-accent)]">· {messages.unplacedLink}</Link> : null}</div>
          </div>
    </section>
  return <div className={`workspace-shell ${compact ? 'workspace-shell--compact' : ''}`}>
    <div ref={desktopStageRef} className={`workspace-desktop-stage ${compact ? 'workspace-mobile-stage' : ''}`}>
    <div data-open={historyOpen} data-resizing={historyResizing} aria-hidden={!historyOpen} style={{ flexBasis: historyOpen ? visibleHistoryWidth : 0 }} className="workspace-history-slot"><div ref={setHistoryPanelHost} className="workspace-history-host" /></div>
    <button type="button" role="separator" aria-orientation="vertical" aria-label={messages.resizeHistory} aria-valuemin={0} aria-valuemax={Math.round(historyMaxWidth)} aria-valuenow={Math.round(visibleHistoryWidth)} onPointerDown={startHistoryResize} onPointerMove={moveHistoryResize} onPointerUp={finishHistoryResize} onPointerCancel={finishHistoryResize} onKeyDown={keyHistoryResize} className="workspace-history-resize-handle" />
    <motion.div aria-hidden={compact && mobileView !== 'chat'} className="workspace-chat-desktop-pane workspace-mobile-pane" data-active={!compact || mobileView === 'chat'} initial={false} animate={reduceMotion ? undefined : { opacity: !compact || mobileView === 'chat' ? 1 : 0, x: compact && mobileView !== 'chat' ? -24 : 0 }} transition={reduceMotion ? { duration: 0 } : { type: 'tween', duration: 0.24, ease: [0.22, 1, 0.36, 1] }} style={{ zIndex: compact ? mobileView === 'chat' ? 2 : 1 : undefined, pointerEvents: compact && mobileView !== 'chat' ? 'none' : 'auto' }}>{chatPane}</motion.div>
    {topologyAvailable && <>
    <button type="button" role="separator" aria-orientation="vertical" aria-label={messages.resizeTopology} aria-valuemin={0} aria-valuemax={Math.round(topologyMaxWidth)} aria-valuenow={Math.round(topologyWidth)} onPointerDown={startTopologyResize} onPointerMove={moveTopologyResize} onPointerUp={finishTopologyResize} onPointerCancel={finishTopologyResize} onKeyDown={keyTopologyResize} className="workspace-topology-resize-handle"><span /></button>
    <motion.div data-open={compact ? mobileView === 'network' : topologyOpen} data-active={!compact || mobileView === 'network'} data-resizing={topologyResizing} data-transitioning={topologyTransitioning} aria-hidden={compact ? mobileView !== 'network' : !topologyOpen} initial={false} animate={reduceMotion ? undefined : { opacity: !compact || mobileView === 'network' ? 1 : 0, x: compact && mobileView !== 'network' ? 24 : 0 }} transition={reduceMotion ? { duration: 0 } : { type: 'tween', duration: 0.24, ease: [0.22, 1, 0.36, 1] }} style={{ flexBasis: compact ? '100%' : topologyWidth, zIndex: compact ? mobileView === 'network' ? 2 : 1 : undefined, pointerEvents: compact && mobileView !== 'network' ? 'none' : 'auto' }} className="workspace-topology-slot workspace-mobile-pane" onTransitionEnd={(event) => {
      if (event.target !== event.currentTarget || event.propertyName !== 'flex-basis' || !topologyTransitioning) return
      if (topologyTransitionTimer.current !== undefined) window.clearTimeout(topologyTransitionTimer.current)
      topologyTransitionTimer.current = undefined
      setTopologyTransitioning(false)
    }}>{networkPane}</motion.div>
    </>}
    </div>
    {compact && visible && paneItems.length > 1 && createPortal(<nav className="workspace-mobile-dock" aria-label={messages.workspace}><LiquidGlassDock items={paneItems} label={messages.workspace} onNavigate={(id) => setMobileView(id as 'chat' | 'network')} /></nav>, document.body)}
  </div>
}

// useOwnPin reads the owner-only pin from the authenticated NODE shared-state
// stream. It uses a one-shot request only while that stream is unavailable.
function useOwnPin(snapshot: NodeSnapshot, visible: boolean): { lat: number; lng: number; share: LocationShare } | undefined {
  const [pin, setPin] = useState<{ lat: number; lng: number; share: LocationShare }>()
  const connected = snapshot.status === 'connected'
  useEffect(() => {
    if (!connected || !visible) return
    let active = true
    let cancel: (() => void) | undefined
    let retry: number | undefined
    let attempt = 0
    let fallbackRequested = false
    const apply = (info: unknown) => {
      if (!info || typeof info !== 'object') return
      const ownerLocation = (info as Record<string, unknown>).ownerLocation
      const location = ownerLocation ? locationFromProtocol(ownerLocation) : null
      setPin((current) => {
        if (!location) return current === undefined ? current : undefined
        // A fresh authoritative node snapshot often carries the same pin.
        // Keep its identity so peer updates do not redraw the globe point.
        if (current && current.lat === location.latitude && current.lng === location.longitude && current.share === location.share) return current
        return { lat: location.latitude, lng: location.longitude, share: location.share }
      })
    }
    const fallback = () => {
      if (!active || fallbackRequested) return
      fallbackRequested = true
      void getNodeApi().call<{ location?: unknown }>('getNodeLocation').then((result) => {
        if (active) apply({ ownerLocation: result.location })
      }).catch(() => {})
    }
    const reconnect = () => {
      if (!active || retry !== undefined) return
      cancel = undefined
      fallback()
      retry = window.setTimeout(() => {
        retry = undefined
        connect()
      }, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
    }
    const connect = () => {
      if (!active) return
      retry = undefined
      cancel = getNodeApi().stream<WorkspaceSharedState>('watchSharedState', {
        domains: [SharedStateDomain.NODE],
      }, (event) => {
        if (sharedStateDomainCode(event.domain) !== 'node') return
        if (event.error) { fallback(); return }
        fallbackRequested = false
        if (event.snapshot) attempt = 0
        apply(workspaceNodeInfo(event))
      }, reconnect, reconnect)
    }
    connect()
    return () => {
      active = false
      if (retry !== undefined) window.clearTimeout(retry)
      cancel?.()
    }
  }, [connected, visible])
  return pin
}

type PoolWorkers = { workers: ReadonlyMap<string, WorkerFacts>; shared: ReadonlyMap<string, Place> }
const noPoolWorkers: PoolWorkers = { workers: new Map(), shared: new Map() }
type WorkspaceSharedState = {
  domain: number | string
  error?: string
  snapshot?: boolean
  state?: { case?: string; value?: unknown } | string
  node?: unknown
  workers?: unknown
}

function workspaceNodeInfo(event: WorkspaceSharedState): unknown {
  if (event.state && typeof event.state === 'object' && event.state.case === 'node') {
    const node = event.state.value
    return node && typeof node === 'object' ? (node as { info?: unknown }).info : undefined
  }
  return event.node && typeof event.node === 'object' ? (event.node as { info?: unknown }).info : undefined
}

// usePoolWorkers reads worker state from the shared event stream. A node that
// cannot serve workers reports that domain as unavailable without closing the
// stream, so a later pool join can update the globe without reconnect polling.
function usePoolWorkers(snapshot: NodeSnapshot, visible: boolean): PoolWorkers {
  const [pool, setPool] = useState<PoolWorkers>(noPoolWorkers)
  const connected = snapshot.status === 'connected'
  useEffect(() => {
    if (!connected || !visible) return
    let active = true
    let cancel: (() => void) | undefined
    let retry: number | undefined
    let attempt = 0
    let fallbackRequested = false
    let receivedStreamState = false
    const apply = (workersValue: unknown) => {
      const workers = poolWorkers(workersValue)
      setPool({ workers, shared: sharedPlaces(workersValue) })
    }
    const fallback = () => {
      if (!active || fallbackRequested) return
      fallbackRequested = true
      void getNodeApi().call<{ workers?: unknown }>('listWorkers').then((result) => {
        if (active && !receivedStreamState) apply(result.workers)
      }).catch(() => { if (active && !receivedStreamState) setPool(noPoolWorkers) })
    }
    const reconnect = () => {
      if (!active || retry !== undefined) return
      cancel = undefined
      fallback()
      retry = window.setTimeout(() => {
        retry = undefined
        connect()
      }, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
    }
    const connect = () => {
      if (!active) return
      retry = undefined
      cancel = getNodeApi().stream<WorkspaceSharedState>('watchSharedState', {
        domains: [SharedStateDomain.WORKERS],
      }, (event) => {
        if (sharedStateDomainCode(event.domain) !== 'workers') return
        if (event.error) {
          setPool(noPoolWorkers)
          return
        }
        const workers = event.state && typeof event.state === 'object' && event.state.case === 'workers'
          ? event.state.value
          : event.workers
        if (workers === undefined) return
        receivedStreamState = true
        fallbackRequested = false
        if (!event.snapshot) attempt = 0
        apply(workers)
      }, reconnect, reconnect)
    }
    connect()
    return () => {
      active = false
      if (retry !== undefined) window.clearTimeout(retry)
      cancel?.()
    }
  }, [connected, visible])
  return pool
}

// useCityEstimate returns where the node's city table puts it, which only
// GetNodeInfo itself says, or nothing.
function useCityEstimate(snapshot: NodeSnapshot, visible: boolean): CityEstimate | undefined {
  const [estimate, setEstimate] = useState<CityEstimate>()
  const connected = snapshot.status === 'connected'
  useEffect(() => {
    if (!connected || !visible) return
    const next = cityEstimateFromProtocol(snapshot.info?.cityEstimate)
    setEstimate((current) => JSON.stringify(current) === JSON.stringify(next) ? current : next)
  }, [connected, visible, snapshot.info?.cityEstimate])
  return estimate
}
