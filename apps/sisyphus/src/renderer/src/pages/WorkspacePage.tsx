import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode, type KeyboardEvent as ReactKeyboardEvent, type PointerEvent as ReactPointerEvent } from 'react'
import { createPortal } from 'react-dom'
import { Check, Copy, Eye, MessageSquare, Minus, Plus, X } from 'lucide-react'
import { Link, useNavigate } from 'react-router-dom'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { Drawer } from 'vaul'
import Globe, { type GlobeMethods } from 'react-globe.gl'
import { AdditiveBlending, BufferGeometry, CircleGeometry, Color, Float32BufferAttribute, Group, LineBasicMaterial, LineSegments, Mesh, MeshBasicMaterial, NormalBlending, Points as ThreePoints, PointsMaterial, RingGeometry, Vector3, type BufferAttribute, type PerspectiveCamera } from 'three'
import type { NodeSnapshot } from '../../../preload'
import { formatMessage, type getMessages } from '@/i18n/messages'
import centroidsGeoJson from 'world-countries-centroids/dist/countries.geojson?raw'
import countriesGeoJsonRaw from '../data/countries.geojson?raw'
import { LiquidGlassDock, type LiquidDockItem } from '@/components/navigation/liquid-glass-dock'
import { CountryFlag, hasCountryFlag } from '@/components/ui/country-flag'
import { useBreakpoint } from '@/hooks/use-breakpoint'
import { AgentChat } from '@/components/assistant/agent-chat'
import { visiblePanelWidth, stepPanelWidth } from '@/lib/panel-width'
import { groupCoincidentPoints, locatePeers, peerLocationsKey, peerMarkerStrength, sharedPlaces, type Place } from '@/lib/globe-markers'
import { webglAvailable } from '@/lib/webgl'
import { getNodeApi, isDesktopApp } from '@/lib/node-api'
import { locationFromProtocol, type LocationShare } from '@/lib/node-location'
import { cityEstimateFromProtocol, nodeDetails, peerDetails, poolWorkers, renderDetailsHtml, type CityEstimate, type GlobeDetails, type WorkerFacts } from '@/lib/globe-details'
import { SharedStateDomain } from '@sisyphus/client'
import { sharedStateDomainCode } from '@/lib/shared-state-domain'
import { useWebgl } from '@/hooks/use-webgl'
import { autoRotateSpeed, clampMapCenter, decodeRegionLines, globeFitDistance, localMapThresholdReached, mapLocationAtOffset, mapUnitsPerPixel, mapZoomAnchorShift, mercatorLatitude, nearestMapPoint, projectionLineOpacity, regionSegments, zoomLimits, zoomState, zoomStep, type ZoomLimits, type ZoomState } from '@/lib/globe-view'
import { createOrbitCameraTransition, unindexGlobeGeometry } from '@/lib/globe-geometry'

type Messages = ReturnType<typeof getMessages>
function connected(peer: NodeSnapshot['peers'][number]) { return peer.connectionState === 2 || (typeof peer.connectionState === 'string' && peer.connectionState.endsWith('_CONNECTED')) }
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
type GlobePoint = { id: string; lat: number; lng: number; label: string; color: string; members: string[] }
const previewPeers = import.meta.env.DEV ? [
  { id: 'dev-preview-amsterdam-1', name: 'Preview · Amsterdam 1', countryCode: 'NL', lat: 52.37, lng: 4.9 },
  { id: 'dev-preview-amsterdam-2', name: 'Preview · Amsterdam 2', countryCode: 'NL', lat: 52.37, lng: 4.9 },
  { id: 'dev-preview-amsterdam-3', name: 'Preview · Amsterdam 3', countryCode: 'NL', lat: 52.37, lng: 4.9 },
  { id: 'dev-preview-london-1', name: 'Preview · London 1', countryCode: 'GB', lat: 51.51, lng: -0.13 },
  { id: 'dev-preview-london-2', name: 'Preview · London 2', countryCode: 'GB', lat: 51.51, lng: -0.13 },
  { id: 'dev-preview-berlin', name: 'Preview · Berlin', countryCode: 'DE', lat: 52.52, lng: 13.4 },
  { id: 'dev-preview-new-york', name: 'Preview · New York', countryCode: 'US', lat: 40.71, lng: -74.01 },
  { id: 'dev-preview-tokyo', name: 'Preview · Tokyo', countryCode: 'JP', lat: 35.68, lng: 139.69 },
  { id: 'dev-preview-singapore', name: 'Preview · Singapore', countryCode: 'SG', lat: 1.35, lng: 103.82 },
  { id: 'dev-preview-sydney', name: 'Preview · Sydney', countryCode: 'AU', lat: -33.87, lng: 151.21 },
] : []
const mapMorph = { value: 0 }
const morphLineVisibility = { value: 1 }
const flatMapScale = { value: 0 }

function constrainMapCamera(globe: GlobeMethods, center: { lat: number; lng: number }) {
  const camera = globe.camera() as PerspectiveCamera
  const rect = globe.renderer().domElement.getBoundingClientRect()
  if (rect.width <= 0 || rect.height <= 0) return
  const distance = Math.max(0.01, camera.position.z - globe.getGlobeRadius() * 0.008)
  const unitsPerPixel = mapUnitsPerPixel(distance, camera.fov, rect.height)
  const bounded = clampMapCenter(camera.position.x, camera.position.y, center.lat, flatMapScale.value || globe.getGlobeRadius() * 0.52, rect.width * unitsPerPixel / 2, rect.height * unitsPerPixel / 2)
  camera.position.x = bounded.x
  camera.position.y = bounded.y
}

function flatMapPoint(lat: number, lng: number, center: { lat: number; lng: number }, radius: number): [number, number, number] {
  const delta = ((lng - center.lng + 540) % 360) - 180
  const scale = flatMapScale.value || radius * 0.52
  return [delta * Math.PI / 180 * scale, (mercatorLatitude(lat) - mercatorLatitude(center.lat)) * scale, radius * 0.008]
}

function addFlatPositionAttribute(geometry: BufferGeometry, center: { lat: number; lng: number }, radius: number, sourceYRotation = 0, collapseSeams = true) {
  const positions = geometry.getAttribute('position') as BufferAttribute | undefined
  if (!positions) return
  const flat = new Float32Array(positions.count * 3)
  for (let index = 0; index < positions.count; index++) {
    const x = positions.getX(index)
    const y = positions.getY(index)
    const z = positions.getZ(index)
    const worldX = Math.cos(sourceYRotation) * x + Math.sin(sourceYRotation) * z
    const worldZ = -Math.sin(sourceYRotation) * x + Math.cos(sourceYRotation) * z
    const length = Math.hypot(worldX, y, worldZ) || 1
    const lat = Math.asin(Math.max(-1, Math.min(1, y / length))) * 180 / Math.PI
    const lng = Math.atan2(worldX, worldZ) * 180 / Math.PI
    flat.set(flatMapPoint(lat, lng, center, radius), index * 3)
  }
  // A great-circle edge that crosses the projection's antimeridian would
  // otherwise become a long, straight scar across the local map. Polygon
  // outlines are non-indexed before reaching this function, so each segment
  // can safely collapse just its own two endpoints at that seam.
  const scale = flatMapScale.value || radius * 0.52
  for (let index = 0; collapseSeams && index + 1 < positions.count; index += 2) {
    const first = index * 3
    const second = (index + 1) * 3
    if (Math.abs(flat[first] - flat[second]) > Math.PI * scale) {
      flat[second] = flat[first]
      flat[second + 1] = flat[first + 1]
      flat[second + 2] = flat[first + 2]
    }
  }
  geometry.setAttribute('flatPosition', new Float32BufferAttribute(flat, 3))
}

function applyMorphShader(material: MeshBasicMaterial) {
  const original = material.onBeforeCompile
  material.onBeforeCompile = (shader, renderer) => {
    original.call(material, shader, renderer)
    shader.uniforms.mapMorph = mapMorph
    shader.vertexShader = shader.vertexShader
      .replace('#include <common>', '#include <common>\nattribute vec3 flatPosition;\nuniform float mapMorph;')
      .replace('#include <begin_vertex>', '#include <begin_vertex>\ntransformed = mix(position, flatPosition, mapMorph);')
    shader.fragmentShader = shader.fragmentShader
      .replace('#include <common>', '#include <common>\nuniform float mapMorph;')
      .replace('#include <opaque_fragment>', 'diffuseColor.a *= 1.0 - smoothstep(0.15, 0.85, mapMorph);\n#include <opaque_fragment>')
  }
  material.customProgramCacheKey = () => 'sisyphus-flat-map-surface'
  material.needsUpdate = true
}

// Lines on the far side of the globe show through its glass. Country lines
// there are drawn at backShare of their opacity, fading in across the limb,
// and none once the globe is brought in close, where the far side would be
// a mirror image behind the near one. Region lines are never drawn there.
const countryBackShare = { value: 0.3 }
const regionBackShare = { value: 0 }
function drawFacing(material: LineBasicMaterial, backShare: { value: number }) {
  material.onBeforeCompile = (shader) => {
    shader.uniforms.backShare = backShare
    shader.uniforms.mapMorph = mapMorph
    shader.uniforms.morphLineVisibility = morphLineVisibility
    shader.vertexShader = shader.vertexShader
      .replace('#include <common>', '#include <common>\nattribute vec3 flatPosition;\nuniform float mapMorph;\nvarying float vFacing;')
      .replace('#include <begin_vertex>', '#include <begin_vertex>\ntransformed = mix(position, flatPosition, mapMorph);')
      .replace('#include <project_vertex>', '#include <project_vertex>\nvec3 facingWorld = (modelMatrix * vec4(transformed, 1.0)).xyz;\nvFacing = dot(normalize(facingWorld), normalize(cameraPosition - facingWorld));')
    shader.fragmentShader = shader.fragmentShader
      .replace('#include <common>', '#include <common>\nvarying float vFacing;\nuniform float backShare;\nuniform float morphLineVisibility;')
      .replace('#include <opaque_fragment>', 'diffuseColor.a *= morphLineVisibility * mix(backShare, 1.0, smoothstep(-0.04, 0.2, vFacing));\nif (diffuseColor.a <= 0.001) discard;\n#include <opaque_fragment>')
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

// The polygon layer may replace its geometry after a resize or asynchronous
// update. Prepare every new outline before it is rendered in map mode.
function syncFlatCountryLines(scene: { traverse: (visit: (object: unknown) => void) => void }, center: { lat: number; lng: number }, radius: number) {
  scene.traverse((object) => {
    const line = object as LineSegments
    if (!line.isLineSegments || (line.parent as { __globeObjType?: string } | null)?.__globeObjType !== 'polygon') return
    if (!line.geometry.getAttribute('flatPosition') && line.geometry.getAttribute('position')) {
      if (line.geometry.index) {
        const indexed = line.geometry
        line.geometry = unindexGlobeGeometry(indexed)
        indexed.dispose()
      }
      addFlatPositionAttribute(line.geometry, center, radius)
    }
    if (typeof line.userData.mapOriginalFrustumCulled !== 'boolean') line.userData.mapOriginalFrustumCulled = line.frustumCulled
    line.frustumCulled = false
    for (const material of Array.isArray(line.material) ? line.material : [line.material]) {
      if (material instanceof LineBasicMaterial && material.userData.facing !== true) {
        drawFacing(material, countryBackShare)
        material.userData.facing = true
      }
    }
  })
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

const NetworkGlobe = memo(function NetworkGlobe({ countryCode, pin, shared, peers, nodeLabel, countryLabel, reduceMotion, active, rotationPaused = false, detailsOpen = false, suspendResize = false, pointNames, pointsLabel, pointButtonLabel, onPickPoint, onPickCluster, zoomLabels }: { countryCode?: string; pin?: { lat: number; lng: number }; shared?: ReadonlyMap<string, Place>; peers: NodeSnapshot['peers']; nodeLabel: string; countryLabel: string; reduceMotion: boolean; active: boolean; rotationPaused?: boolean; detailsOpen?: boolean; suspendResize?: boolean; pointNames: ReadonlyMap<string, string>; pointsLabel: string; pointButtonLabel: string; onPickPoint: (id: string, from?: HTMLElement) => void; onPickCluster: (ids: string[], from?: HTMLElement) => void; zoomLabels: { group: string; in: string; out: string } }) {
  const containerRef = useRef<HTMLDivElement>(null)
  const globeRef = useRef<GlobeMethods | undefined>(undefined)
  const [size, setSize] = useState({ width: 0, height: 0 })
  const [theme, setTheme] = useState<HologramTheme>(readHologramTheme)
  const limitsRef = useRef<ZoomLimits | undefined>(undefined)
  const fittingViewportRef = useRef(false)
  const countryFacingRef = useRef(false)
  const [zoom, setZoom] = useState<ZoomState>()
  const mapCenterRef = useRef<{ lat: number; lng: number } | undefined>(undefined)
  const mapMaxDistanceRef = useRef(0)
  const locationRef = useRef<{ lat: number; lng: number } | undefined>(undefined)
  const [mapCenter, setMapCenter] = useState<{ lat: number; lng: number }>()
  const [mapLayout, setMapLayout] = useState(false)
  const [mapSurfaceHidden, setMapSurfaceHidden] = useState(false)
  const [mapHover, setMapHover] = useState<{ id: string; x: number; y: number }>()
  const [regionLines, setRegionLines] = useState<Array<Array<[number, number]>>>()
  const regionLinesRef = useRef<Array<Array<[number, number]>> | undefined>(undefined)
  const currentPointsRef = useRef<GlobePoint[]>([])
  const selectPoint = useCallback((id: string, from?: HTMLElement) => {
    const point = currentPointsRef.current.find((item) => item.id === id)
    if (point && point.members.length > 1) onPickCluster(point.members, from)
    else onPickPoint(id, from)
  }, [onPickCluster, onPickPoint])
  const morphFrameRef = useRef<number | undefined>(undefined)
  const transitioningRef = useRef(false)
  const wheelGestureRef = useRef({ at: 0, direction: 0, transitioned: false })
  const globeSurfaceRef = useRef<Mesh | undefined>(undefined)
  const restoreSceneRenderRef = useRef<(() => void) | undefined>(undefined)
  const mapPointsRef = useRef<ThreePoints | undefined>(undefined)
  const markerObjectsRef = useRef(new WeakMap<object, Group>())
  const markerFramesRef = useRef(new Map<Group, number>())
  const mapInteractiveRef = useRef(false)
  const mapPointersRef = useRef(new Map<number, { x: number; y: number; startX: number; startY: number; moved: boolean; pointId?: string }>())
  const mapInteractive = Boolean(mapCenter)
  const mapSuppressedRef = useRef(false)
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
  const hologramMaterial = useMemo(() => {
    const material = new MeshBasicMaterial({ color: '#ffffff', transparent: true, opacity: 0.03, depthWrite: false, blending: AdditiveBlending })
    applyMorphShader(material)
    return material
  }, [])
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
        if (!cancelled) {
          regionLinesRef.current = lines
          setRegionLines(lines)
        }
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
    mapMorph.value = 0
    morphLineVisibility.value = 1
    return () => {
      if (morphFrameRef.current !== undefined) window.cancelAnimationFrame(morphFrameRef.current)
      restoreSceneRenderRef.current?.()
      for (const frame of markerFramesRef.current.values()) window.cancelAnimationFrame(frame)
      markerFramesRef.current.clear()
      const pointsLayer = mapPointsRef.current
      pointsLayer?.removeFromParent()
      pointsLayer?.geometry.dispose()
      ;(Array.isArray(pointsLayer?.material) ? pointsLayer.material : pointsLayer?.material ? [pointsLayer.material] : []).forEach((material) => material.dispose())
    }
  }, [])
  useEffect(() => {
    applyHologramStyle()
  }, [applyHologramStyle])
  const fitCameraToViewport = useCallback(() => {
    const globe = globeRef.current
    // A closed or animating pane can briefly measure near zero width. Using
    // that transient aspect ratio would move the camera far off the globe.
    if (!globe || !active || suspendResize) return
    const camera = globe.camera() as PerspectiveCamera
    const bounds = containerRef.current?.getBoundingClientRect()
    if (!bounds || bounds.width < 1 || bounds.height < 1) return
    const aspect = bounds.width / bounds.height
    const safeDistance = globeFitDistance(globe.getGlobeRadius(), camera.fov, aspect)
    const controls = globe.controls()
    const radius = globe.getGlobeRadius()
    const limits = zoomLimits(radius, safeDistance)
    const previousLimits = limitsRef.current
    limitsRef.current = limits
    // Preserve relative zoom when the viewport changes shape, so resizing
    // alone cannot cross the threshold into the map.
    if (previousLimits && !mapInteractiveRef.current && !transitioningRef.current && Math.abs(previousLimits.fit - limits.fit) > 0.01) {
      const offset = camera.position.clone().sub(controls.target)
      const distance = Math.max(limits.min, Math.min(limits.max, offset.length() * limits.fit / previousLimits.fit))
      camera.position.copy(controls.target).add(offset.setLength(distance))
    }
    controls.minDistance = limits.min
    controls.maxDistance = mapInteractiveRef.current ? radius * 5.5 : limits.max
    controls.enableZoom = !mapInteractiveRef.current || mapMorph.value >= 0.999
    controls.enableRotate = !mapInteractiveRef.current && mapMorph.value <= 0.001
    // Map drags use a screen-aligned XY translation below; OrbitControls pan
    // moves in camera space and makes the flat map drift in depth.
    controls.enablePan = false
    controls.autoRotate = !reduceMotion && active && !rotationPaused && !detailsOpen && !mapCenterRef.current && mapMorph.value <= 0.001
    const cameraDistance = mapInteractiveRef.current && !controls.enabled ? camera.position.z : camera.position.distanceTo(controls.target)
    controls.autoRotateSpeed = autoRotateSpeed(cameraDistance, radius, limits)
    controls.enableDamping = true
    fittingViewportRef.current = true
    if (controls.enabled) controls.update()
    fittingViewportRef.current = false
    if (mapInteractiveRef.current && !transitioningRef.current && mapCenterRef.current) constrainMapCamera(globe, mapCenterRef.current)
    const next = zoomState(cameraDistance, limits)
    const displayZoom = { ...next, percent: Math.round(next.percent / 5) * 5 }
    setZoom((current) => current && current.canZoomIn === displayZoom.canZoomIn && current.canZoomOut === displayZoom.canZoomOut && current.regions === displayZoom.regions && current.percent === displayZoom.percent ? current : displayZoom)
  }, [active, detailsOpen, reduceMotion, rotationPaused, suspendResize])
  const animateMorph = useCallback((target: number, onComplete?: () => void, onFrame?: (progress: number) => void) => {
    if (morphFrameRef.current !== undefined) window.cancelAnimationFrame(morphFrameRef.current)
    const from = mapMorph.value
    const duration = reduceMotion ? 0 : 420 * Math.abs(target - from)
    const startedAt = performance.now()
    const animate = (now: number) => {
      const progress = duration === 0 ? 1 : Math.min(1, (now - startedAt) / duration)
      // Smootherstep has zero velocity and acceleration at both ends, keeping
      // the camera and projection from visibly kicking as they settle.
      const eased = progress * progress * progress * (progress * (progress * 6 - 15) + 10)
      mapMorph.value = from + (target - from) * eased
      // Soften both layers together without making the map vanish midway.
      morphLineVisibility.value = projectionLineOpacity(mapMorph.value)
      onFrame?.(eased)
      const backShare = Math.max(0, Math.min(1, (mapMorph.value - 0.7) / 0.3))
      countryBackShare.value = backShare
      regionBackShare.value = backShare
      const surface = globeSurfaceRef.current
      if (surface) surface.rotation.y = -Math.PI / 2 * (1 - mapMorph.value)
      if (progress < 1) morphFrameRef.current = window.requestAnimationFrame(animate)
      else {
        morphFrameRef.current = undefined
        onComplete?.()
      }
    }
    morphFrameRef.current = window.requestAnimationFrame(animate)
  }, [reduceMotion])
  const prepareFlatMap = useCallback((center: { lat: number; lng: number }) => {
    const globe = globeRef.current
    if (!globe) return
    const scene = globe.scene()
    const radius = globe.getGlobeRadius()
    // Keep the geographic zoom the user has reached on the globe. A roughly
    // 90-degree local window fills the viewport instead of resetting to a
    // tiny, whole-world map.
    const cameraDistance = globe.camera().position.distanceTo(globe.controls().target)
    flatMapScale.value = cameraDistance * 0.62
    restoreSceneRenderRef.current?.()
    const previousBeforeRender = scene.onBeforeRender
    const beforeRender: typeof scene.onBeforeRender = (...args) => {
      previousBeforeRender.apply(scene, args)
      syncFlatCountryLines(scene, center, radius)
    }
    scene.onBeforeRender = beforeRender
    restoreSceneRenderRef.current = () => { if (scene.onBeforeRender === beforeRender) scene.onBeforeRender = previousBeforeRender }
    scene.traverse((object) => {
      const mesh = object as Mesh
      const parentType = (mesh.parent as { __globeObjType?: string } | null)?.__globeObjType
      if (mesh.isMesh && parentType === 'globe' && mesh.geometry) {
        addFlatPositionAttribute(mesh.geometry, center, radius, -Math.PI / 2, false)
        globeSurfaceRef.current = mesh
        mesh.rotation.y = -Math.PI / 2
      }
      const line = object as LineSegments
      if (line.isLineSegments && (parentType === 'polygon' || line.geometry.type === 'GeoJsonGeometry')) {
        if (line.geometry.index) {
          const indexedGeometry = line.geometry
          // three-globe uses GeoJsonGeometry.parameters to identify unchanged
          // country outlines. Three.js does not copy that metadata when it
          // expands an indexed geometry: without it, a later layer update
          // silently rebuilds spherical outlines without our map positions.
          line.geometry = unindexGlobeGeometry(indexedGeometry)
          indexedGeometry.dispose()
        }
        addFlatPositionAttribute(line.geometry, center, radius)
        line.userData.mapOriginalFrustumCulled = line.frustumCulled
        line.frustumCulled = false
        for (const material of Array.isArray(line.material) ? line.material : [line.material]) {
          if (material instanceof LineBasicMaterial && material.userData.facing !== true) {
            drawFacing(material, parentType === 'polygon' ? countryBackShare : regionBackShare)
            material.userData.facing = true
          }
        }
      }
    })
    const oldPoints = mapPointsRef.current
    oldPoints?.removeFromParent()
    oldPoints?.geometry.dispose()
    if (oldPoints?.material instanceof PointsMaterial) oldPoints.material.dispose()
    const positions: number[] = []
    const flatPositions: number[] = []
    const colors: number[] = []
    for (const point of currentPointsRef.current) {
      const spherical = globe.getCoords(point.lat, point.lng, 0.006)
      positions.push(spherical.x, spherical.y, spherical.z)
      flatPositions.push(...flatMapPoint(point.lat, point.lng, center, radius))
      const color = new Color(point.color)
      colors.push(color.r, color.g, color.b)
    }
    const geometry = new BufferGeometry()
    geometry.setAttribute('position', new Float32BufferAttribute(positions, 3))
    geometry.setAttribute('flatPosition', new Float32BufferAttribute(flatPositions, 3))
    geometry.setAttribute('color', new Float32BufferAttribute(colors, 3))
    geometry.setAttribute('markerScale', new Float32BufferAttribute(currentPointsRef.current.map((point) => peerMarkerStrength(point.members.length)), 1))
    const material = new PointsMaterial({ size: 14, sizeAttenuation: false, vertexColors: true, transparent: true, opacity: 1, depthWrite: false })
    material.onBeforeCompile = (shader) => {
      shader.uniforms.mapMorph = mapMorph
      shader.vertexShader = shader.vertexShader
        .replace('#include <common>', '#include <common>\nattribute vec3 flatPosition;\nattribute float markerScale;\nuniform float mapMorph;\nvarying float markerFacing;')
        .replace('#include <begin_vertex>', '#include <begin_vertex>\nmarkerFacing = dot(normalize(position), normalize(cameraPosition - position));\ntransformed = mix(position, flatPosition, mapMorph);')
        .replace('gl_PointSize = size;', 'gl_PointSize = size * markerScale;')
      shader.fragmentShader = shader.fragmentShader.replace('#include <common>', '#include <common>\nuniform float mapMorph;\nvarying float markerFacing;')
      shader.fragmentShader = shader.fragmentShader.replace('#include <clipping_planes_fragment>', '#include <clipping_planes_fragment>\nfloat markerRadius = distance(gl_PointCoord, vec2(0.5));\nif (markerRadius > 0.5 || (markerRadius > 0.29 && markerRadius < 0.42)) discard;')
        .replace('#include <opaque_fragment>', 'diffuseColor.a *= mix(smoothstep(-0.04, 0.2, markerFacing), 1.0, mapMorph);\nif (markerRadius >= 0.42) diffuseColor.a *= 0.48;\n#include <opaque_fragment>')
    }
    material.customProgramCacheKey = () => 'sisyphus-map-points'
    const pointsLayer = new ThreePoints(geometry, material)
    pointsLayer.frustumCulled = false
    pointsLayer.renderOrder = 5
    scene.add(pointsLayer)
    mapPointsRef.current = pointsLayer
    const lines = regionLinesRef.current
    if (lines) {
      let regions = regionsRef.current
      if (!regions) {
        const regionGeometry = new BufferGeometry()
        regionGeometry.setAttribute('position', new Float32BufferAttribute(regionSegments(lines, (lat, lng) => globe.getCoords(lat, lng, 0.0065)), 3))
        regions = new LineSegments(regionGeometry, regionMaterial)
        regions.renderOrder = 1
        regionsRef.current = regions
        setRegionsLoaded(true)
      }
      addFlatPositionAttribute(regions.geometry, center, radius)
      regions.frustumCulled = false
      regions.visible = true
    }
  }, [regionMaterial])
  const exitLocalMap = useCallback(() => {
    const globe = globeRef.current
    const limits = limitsRef.current
    if (!globe || !limits || !mapCenterRef.current || transitioningRef.current) return
    transitioningRef.current = true
    if (performance.now() - wheelGestureRef.current.at < 250) wheelGestureRef.current.transitioned = true
    const mapOrigin = mapCenterRef.current
    const camera = globe.camera() as PerspectiveCamera
    const scale = flatMapScale.value || globe.getGlobeRadius() * 0.52
    const center = mapLocationAtOffset(mapOrigin, camera.position.x, camera.position.y, scale)
    const destination = globe.getCoords(center.lat, center.lng, limits.fit * 0.96 / globe.getGlobeRadius() - 1)
    const startPosition = camera.position.clone()
    const startOrientation = camera.quaternion.clone()
    const destinationCamera = camera.clone()
    destinationCamera.position.set(destination.x, destination.y, destination.z)
    destinationCamera.lookAt(0, 0, 0)
    const moveCamera = createOrbitCameraTransition(startPosition, destinationCamera.position, startOrientation, destinationCamera.quaternion)
    mapSuppressedRef.current = true
    mapInteractiveRef.current = false
    mapCenterRef.current = undefined
    setMapCenter(undefined)
    setMapHover(undefined)
    setMapSurfaceHidden(false)
    countryBackShare.value = 0.3
    regionBackShare.value = 0
    const controls = globe.controls()
    controls.enabled = false
    controls.enableZoom = false
    controls.enablePan = false
    controls.enableRotate = false
    controls.target.set(0, 0, 0)
    const scene = globe.scene()
    if (globeSurfaceRef.current) {
      globeSurfaceRef.current.visible = true
      if (globeSurfaceRef.current.parent) globeSurfaceRef.current.parent.visible = true
    }
    animateMorph(0, () => {
      restoreSceneRenderRef.current?.()
      restoreSceneRenderRef.current = undefined
      scene.traverse((object) => {
        const type = (object as { __globeObjType?: string }).__globeObjType
        if (type === 'atmosphere' || type === 'point' || type === 'points' || type === 'object' || type === 'ring') object.visible = true
        const line = object as LineSegments
        if (line.isLineSegments && typeof line.userData.mapOriginalFrustumCulled === 'boolean') {
          line.frustumCulled = line.userData.mapOriginalFrustumCulled
          delete line.userData.mapOriginalFrustumCulled
        }
      })
      const pointsLayer = mapPointsRef.current
      pointsLayer?.removeFromParent()
      pointsLayer?.geometry.dispose()
      if (pointsLayer?.material instanceof PointsMaterial) pointsLayer.material.dispose()
      mapPointsRef.current = undefined
      if (regionsRef.current) {
        regionsRef.current.visible = false
        regionsRef.current.frustumCulled = true
      }
      setMapLayout(false)
      transitioningRef.current = false
      controls.enabled = true
      controls.enableZoom = true
      controls.enablePan = false
      controls.enableRotate = true
      controls.autoRotate = !reduceMotion && active && !rotationPaused && !detailsOpen
      controls.update()
      globe.resumeAnimation()
    }, (progress) => {
      moveCamera(progress, camera.position, camera.quaternion)
    })
  }, [active, animateMorph, detailsOpen, reduceMotion, rotationPaused])
  const zoomMap = useCallback((factor: number, clientX?: number, clientY?: number) => {
    const globe = globeRef.current
    if (!globe || !mapInteractiveRef.current || mapMorph.value < 0.999) return
    setMapHover(undefined)
    const camera = globe.camera() as PerspectiveCamera
    const radius = globe.getGlobeRadius()
    const planeZ = radius * 0.008
    const distance = Math.max(0.01, camera.position.z - planeZ)
    const requested = distance * factor
    const sameWheelGesture = wheelGestureRef.current.transitioned && performance.now() - wheelGestureRef.current.at < 250
    const maxDistance = mapMaxDistanceRef.current || radius * 4.8
    if (requested > maxDistance && !sameWheelGesture) {
      exitLocalMap()
      return
    }
    const next = Math.min(maxDistance, Math.max(radius * 0.25, requested))
    if (clientX !== undefined && clientY !== undefined) {
      const rect = globe.renderer().domElement.getBoundingClientRect()
      if (rect.height > 0) {
        const shift = mapZoomAnchorShift(distance, next, camera.fov, rect.height, clientX - (rect.left + rect.width / 2), clientY - (rect.top + rect.height / 2))
        camera.position.x += shift.x
        camera.position.y += shift.y
      }
    }
    camera.position.z = planeZ + next
    if (mapCenterRef.current) constrainMapCamera(globe, mapCenterRef.current)
  }, [exitLocalMap])
  // Whenever the camera moves in or out, by wheel, pinch or button.
  const handleZoom = useCallback(() => {
    const globe = globeRef.current
    const limits = limitsRef.current
    if (!globe || !limits || transitioningRef.current || fittingViewportRef.current) return
    // Called for every move, the globe's own turning too; the outlines are
    // looked for only until they are there.
    if (!countryFacingRef.current) countryFacingRef.current = countryLinesFacing(globe.scene())
    const distance = globe.camera().position.distanceTo(globe.controls().target)
    globe.controls().autoRotateSpeed = autoRotateSpeed(distance, globe.getGlobeRadius(), limits)
    const next = zoomState(distance, limits)
    countryBackShare.value = mapInteractiveRef.current ? 1 : next.regions ? 0 : 0.3
    regionBackShare.value = mapInteractiveRef.current ? 1 : 0
    if (mapSuppressedRef.current && distance > limits.fit * 0.94) mapSuppressedRef.current = false
    const sameWheelGesture = wheelGestureRef.current.transitioned && performance.now() - wheelGestureRef.current.at < 250
    if (!mapCenterRef.current && localMapThresholdReached(distance, limits.fit) && !mapSuppressedRef.current && !sameWheelGesture) {
      transitioningRef.current = true
      if (performance.now() - wheelGestureRef.current.at < 250) wheelGestureRef.current.transitioned = true
      const view = globe.pointOfView()
      // The local map must be centered on the point currently under the
      // camera, not always on this node's saved location.
      mapCenterRef.current = { lat: view.lat, lng: view.lng }
      mapInteractiveRef.current = true
      setMapCenter(mapCenterRef.current)
      // Cancel any in-flight button zoom before starting a single coordinated
      // camera/geometry transition. Keep the exact view that triggered it.
      globe.pointOfView(view, 0)
      prepareFlatMap(mapCenterRef.current)
      const controls = globe.controls()
      controls.enableZoom = false
      controls.enablePan = false
      controls.enableRotate = false
      controls.autoRotate = false
      const radius = globe.getGlobeRadius()
      const camera = globe.camera() as PerspectiveCamera
      const startPosition = camera.position.clone()
      mapMaxDistanceRef.current = Math.max(radius * 4.8, startPosition.length() * 1.4)
      const startOrientation = camera.quaternion.clone()
      const destinationCamera = camera.clone()
      destinationCamera.position.set(0, 0, Math.max(radius * 1.01, startPosition.length()))
      destinationCamera.lookAt(0, 0, 0)
      const moveCamera = createOrbitCameraTransition(startPosition, destinationCamera.position, startOrientation, destinationCamera.quaternion)
      controls.enabled = false
      globe.scene().traverse((object) => {
        const type = (object as { __globeObjType?: string }).__globeObjType
        if (type === 'atmosphere' || type === 'point' || type === 'points' || type === 'object' || type === 'ring') object.visible = false
      })
      animateMorph(1, () => {
        if (globeSurfaceRef.current) globeSurfaceRef.current.visible = false
        setMapSurfaceHidden(true)
        setMapLayout(true)
        countryBackShare.value = 1
        regionBackShare.value = 1
        controls.minDistance = radius * 1.25
        controls.maxDistance = radius * 5.5
        controls.enableZoom = false
        controls.enablePan = false
        controls.enableRotate = false
        transitioningRef.current = false
        if (mapCenterRef.current) constrainMapCamera(globe, mapCenterRef.current)
        globe.resumeAnimation()
      }, (progress) => {
        moveCamera(progress, camera.position, camera.quaternion)
      })
    }
    const displayZoom = { ...next, percent: Math.round(next.percent / 5) * 5 }
    setZoom((current) => current && current.canZoomIn === displayZoom.canZoomIn && current.canZoomOut === displayZoom.canZoomOut && current.regions === displayZoom.regions && current.percent === displayZoom.percent ? current : displayZoom)
  }, [active, animateMorph, exitLocalMap, prepareFlatMap, reduceMotion, rotationPaused])
  useEffect(() => {
    const container = containerRef.current
    if (!container) return
    const trackWheel = (event: WheelEvent) => {
      if (!event.deltaY) return
      const gesture = wheelGestureRef.current
      const now = performance.now()
      const direction = Math.sign(event.deltaY)
      if (now - gesture.at > 250 || direction !== gesture.direction) gesture.transitioned = false
      gesture.at = now
      gesture.direction = direction
    }
    container.addEventListener('wheel', trackWheel, { capture: true, passive: true })
    return () => container.removeEventListener('wheel', trackWheel, { capture: true })
  }, [])
  const findMapPoint = useCallback((clientX: number, clientY: number) => {
    const globe = globeRef.current
    const center = mapCenterRef.current
    const container = containerRef.current
    if (!globe || !center || !container || !mapInteractiveRef.current || mapMorph.value < 0.999) return undefined
    const camera = globe.camera() as PerspectiveCamera
    const rect = globe.renderer().domElement.getBoundingClientRect()
    const containerRect = container.getBoundingClientRect()
    camera.updateMatrixWorld()
    const candidates: Array<{ id: string; x: number; y: number }> = []
    const projected = new Vector3()
    for (const point of currentPointsRef.current) {
      projected.fromArray(flatMapPoint(point.lat, point.lng, center, globe.getGlobeRadius())).project(camera)
      if (projected.z < -1 || projected.z > 1) continue
      const x = rect.left + (projected.x + 1) * rect.width / 2
      const y = rect.top + (1 - projected.y) * rect.height / 2
      candidates.push({ id: point.id, x: x - containerRect.left, y: y - containerRect.top })
    }
    return nearestMapPoint(candidates, clientX - containerRect.left, clientY - containerRect.top)
  }, [])
  const startMapPan = useCallback((event: PointerEvent) => {
    if (!mapInteractiveRef.current || mapMorph.value < 0.999 || (event.pointerType === 'mouse' && event.button !== 0)) return
    if (mapPointersRef.current.size >= 2) return
    if (mapPointersRef.current.size) for (const pointer of mapPointersRef.current.values()) pointer.moved = true
    const pointId = findMapPoint(event.clientX, event.clientY)?.id
    mapPointersRef.current.set(event.pointerId, { x: event.clientX, y: event.clientY, startX: event.clientX, startY: event.clientY, moved: mapPointersRef.current.size > 0, pointId })
    setMapHover(undefined)
    containerRef.current?.setAttribute('data-map-dragging', 'true')
    ;(event.currentTarget as HTMLCanvasElement).setPointerCapture(event.pointerId)
    event.preventDefault()
  }, [findMapPoint])
  const moveMapPan = useCallback((event: PointerEvent) => {
    const pointers = mapPointersRef.current
    const drag = pointers.get(event.pointerId)
    const globe = globeRef.current
    if (!globe || !mapInteractiveRef.current) return
    if (!drag) {
      if (event.pointerType !== 'touch') {
        const point = findMapPoint(event.clientX, event.clientY)
        setMapHover((current) => current?.id === point?.id && current?.x === point?.x && current?.y === point?.y ? current : point)
      }
      return
    }
    if (Math.hypot(event.clientX - drag.startX, event.clientY - drag.startY) > 4) drag.moved = true
    if (!drag.moved) return
    const canvas = event.currentTarget as HTMLCanvasElement
    const height = canvas.getBoundingClientRect().height
    if (height < 1) return
    const camera = globe.camera() as PerspectiveCamera
    const distance = Math.max(0.01, camera.position.z - globe.getGlobeRadius() * 0.008)
    const worldPerPixel = mapUnitsPerPixel(distance, camera.fov, height)
    const other = [...pointers.entries()].find(([id]) => id !== event.pointerId)?.[1]
    const previousDistance = other ? Math.hypot(drag.x - other.x, drag.y - other.y) : 0
    const divisor = other ? 2 : 1
    const deltaX = (event.clientX - drag.x) * worldPerPixel / divisor
    const deltaY = (event.clientY - drag.y) * worldPerPixel / divisor
    drag.x = event.clientX
    drag.y = event.clientY
    // OrbitControls is disabled here: neither panning nor zooming can reset
    // the map camera's position or turn it back toward the globe's centre.
    camera.position.x -= deltaX
    camera.position.y += deltaY
    if (mapCenterRef.current) constrainMapCamera(globe, mapCenterRef.current)
    if (other && previousDistance > 0) {
      const nextDistance = Math.hypot(drag.x - other.x, drag.y - other.y)
      if (nextDistance > 0) zoomMap(previousDistance / nextDistance, (drag.x + other.x) / 2, (drag.y + other.y) / 2)
    }
    event.preventDefault()
  }, [findMapPoint, zoomMap])
  const endMapPan = useCallback((event: PointerEvent) => {
    const drag = mapPointersRef.current.get(event.pointerId)
    if (event.type === 'pointerup' && drag && !drag.moved && drag.pointId && findMapPoint(event.clientX, event.clientY)?.id === drag.pointId) selectPoint(drag.pointId)
    if (!mapPointersRef.current.delete(event.pointerId)) return
    if (mapPointersRef.current.size === 0) containerRef.current?.removeAttribute('data-map-dragging')
    const canvas = event.currentTarget as HTMLCanvasElement
    if (canvas.hasPointerCapture(event.pointerId)) canvas.releasePointerCapture(event.pointerId)
  }, [findMapPoint, selectPoint])
  useEffect(() => {
    if (!mapInteractive) return
    const canvas = globeRef.current?.renderer().domElement
    if (!canvas) return
    canvas.addEventListener('pointerdown', startMapPan)
    canvas.addEventListener('pointermove', moveMapPan)
    canvas.addEventListener('pointerup', endMapPan)
    canvas.addEventListener('pointercancel', endMapPan)
    canvas.addEventListener('lostpointercapture', endMapPan)
    const onLeave = () => { if (mapPointersRef.current.size === 0) setMapHover(undefined) }
    canvas.addEventListener('pointerleave', onLeave)
    const onWheel = (event: WheelEvent) => {
      if (!mapInteractiveRef.current || mapMorph.value < 0.999) return
      event.preventDefault()
      const delta = event.deltaY * (event.deltaMode === WheelEvent.DOM_DELTA_LINE ? 16 : event.deltaMode === WheelEvent.DOM_DELTA_PAGE ? canvas.clientHeight : 1)
      zoomMap(Math.exp(Math.max(-1, Math.min(1, delta * 0.0015))), event.clientX, event.clientY)
    }
    canvas.addEventListener('wheel', onWheel, { passive: false })
    return () => {
      canvas.removeEventListener('pointerdown', startMapPan)
      canvas.removeEventListener('pointermove', moveMapPan)
      canvas.removeEventListener('pointerup', endMapPan)
      canvas.removeEventListener('pointercancel', endMapPan)
      canvas.removeEventListener('lostpointercapture', endMapPan)
      canvas.removeEventListener('pointerleave', onLeave)
      canvas.removeEventListener('wheel', onWheel)
      containerRef.current?.removeAttribute('data-map-dragging')
      mapPointersRef.current.clear()
    }
  }, [endMapPan, mapInteractive, moveMapPan, startMapPan, zoomMap])
  const zoomBy = useCallback((direction: 'in' | 'out') => {
    const globe = globeRef.current
    const limits = limitsRef.current
    if (!globe || !limits) return
    if (mapInteractiveRef.current) {
      zoomMap(direction === 'in' ? 0.78 : 1.28)
      return
    }
    if (zoomAnimationRef.current) return
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
  }, [handleZoom, reduceMotion, zoomMap])
  const wantRegions = Boolean(zoom?.regions)
  useEffect(() => {
    if (!regionLines || regionsRef.current) return
    const globe = globeRef.current
    if (!globe || regionsRef.current) return
    const geometry = new BufferGeometry()
    geometry.setAttribute('position', new Float32BufferAttribute(regionSegments(regionLines, (lat, lng) => globe.getCoords(lat, lng, 0.0065)), 3))
    const regions = new LineSegments(geometry, regionMaterial)
    regions.renderOrder = 1
    if (mapInteractiveRef.current && mapCenterRef.current) {
      addFlatPositionAttribute(geometry, mapCenterRef.current, globe.getGlobeRadius())
      regions.frustumCulled = false
    }
    regions.visible = mapInteractiveRef.current || wantRegions
    regionsRef.current = regions
    setRegionsLoaded(true)
  }, [wantRegions, regionLines, regionMaterial, mapInteractive])
  useEffect(() => {
    if (regionsRef.current) regionsRef.current.visible = mapInteractive || wantRegions
  }, [mapInteractive, wantRegions, regionsLoaded])
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
      const nextSize = { width: Math.floor(Math.min(rect.width, window.innerWidth)), height: Math.floor(Math.min(rect.height, window.innerHeight)) }
      if (nextSize.width < 1 || nextSize.height < 1) return
      setSize((current) => current.width === nextSize.width && current.height === nextSize.height ? current : nextSize)
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
  }, [fitCameraToViewport, size.width, size.height])

  useEffect(() => {
    if (size.width === 0 || size.height === 0) return
    const renderer = globeRef.current?.renderer()
    if (renderer && renderer.getPixelRatio() < 2) renderer.setPixelRatio(2)
  }, [size.width > 0])

  useEffect(() => {
    const globe = globeRef.current
    if (!globe) return
    globe.controls().autoRotate = !reduceMotion && active && !rotationPaused && !detailsOpen && !mapCenterRef.current && mapMorph.value <= 0.001
  }, [active, detailsOpen, reduceMotion, rotationPaused, mapCenter])

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
    const own = location ? [{ id: ownPointId, ...location, label: nodeLabel, color: theme.accent }] : []
    const known = stablePeerLocations.map((peer) => ({ id: peer.peerId, lat: peer.lat, lng: peer.lng, label: peer.peerId, color: theme.accent }))
    const previews = previewPeers.map((peer) => ({ id: peer.id, lat: peer.lat, lng: peer.lng, label: peer.name, color: theme.accent }))
    return groupCoincidentPoints([...own, ...known, ...previews])
  }, [location, nodeLabel, stablePeerLocations, theme.accent])
  const globePoints = useMemo(() => mapInteractive || mapLayout ? [] : points, [mapInteractive, mapLayout, points])
  // A surface-aligned node marker: a compact core and a restrained outer ring,
  // rather than the tall cylinders generated by the points layer.
  const markerAssets = useMemo(() => ({
    core: new CircleGeometry(0.46, 32),
    halo: new RingGeometry(0.68, 0.79, 32),
    hitArea: new CircleGeometry(1.6, 32),
    coreMaterial: new MeshBasicMaterial({ color: theme.accent }),
    haloMaterial: new MeshBasicMaterial({ color: theme.accent, transparent: true, opacity: 0.48, depthWrite: false }),
    hitMaterial: new MeshBasicMaterial({ transparent: true, opacity: 0, depthWrite: false, colorWrite: false }),
  }), [theme.accent])
  useEffect(() => () => {
    markerAssets.core.dispose()
    markerAssets.halo.dispose()
    markerAssets.hitArea.dispose()
    markerAssets.coreMaterial.dispose()
    markerAssets.haloMaterial.dispose()
    markerAssets.hitMaterial.dispose()
  }, [markerAssets])
  const markerObject = useCallback((point: object) => {
    const marker = new Group()
    marker.scale.setScalar(peerMarkerStrength((point as GlobePoint).members.length))
    marker.add(new Mesh(markerAssets.core, markerAssets.coreMaterial))
    marker.add(new Mesh(markerAssets.halo, markerAssets.haloMaterial))
    marker.add(new Mesh(markerAssets.hitArea, markerAssets.hitMaterial))
    markerObjectsRef.current.set(point, marker)
    return marker
  }, [markerAssets])
  const hoverMarker = useCallback((point: object | null, previous: object | null) => {
    const animate = (item: object | null, hovered: boolean) => {
      const marker = item && markerObjectsRef.current.get(item)
      if (!marker) return
      const target = peerMarkerStrength((item as GlobePoint).members.length) * (hovered ? 1.55 : 1)
      const previousFrame = markerFramesRef.current.get(marker)
      if (previousFrame !== undefined) window.cancelAnimationFrame(previousFrame)
      const from = marker.scale.x
      const start = performance.now()
      const tick = (now: number) => {
        const progress = reduceMotion ? 1 : Math.min(1, (now - start) / 180)
        const eased = 1 - (1 - progress) ** 3
        marker.scale.setScalar(from + (target - from) * eased)
        if (progress < 1) markerFramesRef.current.set(marker, window.requestAnimationFrame(tick))
        else markerFramesRef.current.delete(marker)
      }
      markerFramesRef.current.set(marker, window.requestAnimationFrame(tick))
    }
    animate(previous, false)
    animate(point, true)
  }, [reduceMotion])
  currentPointsRef.current = points
  useEffect(() => {
    const layer = mapPointsRef.current
    const center = mapCenterRef.current
    const globe = globeRef.current
    if (!layer || !center || !globe) return
    const positions: number[] = []
    const flatPositions: number[] = []
    const colors: number[] = []
    for (const point of points) {
      const spherical = globe.getCoords(point.lat, point.lng, 0.006)
      positions.push(spherical.x, spherical.y, spherical.z)
      flatPositions.push(...flatMapPoint(point.lat, point.lng, center, globe.getGlobeRadius()))
      const color = new Color(point.color)
      colors.push(color.r, color.g, color.b)
    }
    layer.geometry.setAttribute('position', new Float32BufferAttribute(positions, 3))
    layer.geometry.setAttribute('flatPosition', new Float32BufferAttribute(flatPositions, 3))
    layer.geometry.setAttribute('color', new Float32BufferAttribute(colors, 3))
    layer.geometry.setAttribute('markerScale', new Float32BufferAttribute(points.map((point) => peerMarkerStrength(point.members.length)), 1))
  }, [points, mapInteractive])
  useEffect(() => {
    const layer = mapPointsRef.current
    const scales = layer?.geometry.getAttribute('markerScale') as BufferAttribute | undefined
    if (!mapInteractive || !scales) return
    const startValues = points.map((_, index) => scales.getX(index))
    const startedAt = performance.now()
    let frame = 0
    const animate = (now: number) => {
      const progress = reduceMotion ? 1 : Math.min(1, (now - startedAt) / 180)
      const eased = 1 - (1 - progress) ** 3
      points.forEach((point, index) => {
        const target = peerMarkerStrength(point.members.length) * (point.id === mapHover?.id ? 1.55 : 1)
        scales.setX(index, startValues[index] + (target - startValues[index]) * eased)
      })
      scales.needsUpdate = true
      if (progress < 1) frame = window.requestAnimationFrame(animate)
    }
    frame = window.requestAnimationFrame(animate)
    return () => window.cancelAnimationFrame(frame)
  }, [mapHover?.id, mapInteractive, points, reduceMotion])
  // The card is read when the pointer reaches a point, so it says what is
  // known then, not when the point was drawn.
  const pickPoint = useCallback((point: object) => selectPoint((point as GlobePoint).id), [selectPoint])
  // An alpha-zero color still asks three-globe to build spherical polygon
  // faces with depth writing enabled. Those invisible faces occlude the flat
  // map and its markers, so use an empty color to omit the faces altogether.
  const polygonCapColor = useCallback(() => '', [])
  const polygonSideColor = useCallback(() => '', [])
  const polygonStrokeColor = useCallback(() => {
    const { r, g, b } = new Color(theme.accent)
    const opacity = theme.dark ? countryLineOpacity.dark : countryLineOpacity.light
    return `rgba(${Math.round(r * 255)}, ${Math.round(g * 255)}, ${Math.round(b * 255)}, ${opacity})`
  }, [theme.accent, theme.dark])
  const rendererConfig = useMemo(() => ({ alpha: true, antialias: true, powerPreference: 'low-power' as const }), [])
  const markerPointerFilter = useCallback((object: unknown) => (object as { __globeObjType?: string }).__globeObjType === 'object', [])
  return <>
  <div ref={containerRef} className="network-globe-canvas" role="img" aria-label={location ? `${nodeLabel} · ${countryLabel}` : nodeLabel} data-active={active} data-rotation-paused={rotationPaused} data-zoom={zoom?.percent} data-zoom-step={zoomStepCount} data-zooming={stepping} data-zoomed={Boolean(zoom && zoom.percent > 100)} data-regions={regionsLoaded && (wantRegions || mapInteractive) ? 'shown' : 'hidden'} data-local-map={mapInteractive} data-map-hover={Boolean(mapHover)}>
    {webglAvailable() && size.width > 0 && size.height > 0 && <div className="network-globe-stage" style={{ width: size.width, height: size.height }}>
      <Globe
      ref={globeRef}
      width={size.width}
      height={size.height}
      backgroundColor="rgba(0,0,0,0)"
      showGlobe={!mapSurfaceHidden}
      globeMaterial={hologramMaterial}
      showGraticules={false}
      globeCurvatureResolution={6}
      showAtmosphere={!mapInteractive && !mapLayout}
      atmosphereColor={theme.accent}
      atmosphereAltitude={theme.dark ? 0.1 : 0.075}
      rendererConfig={rendererConfig}
      polygonsData={countryBoundaries}
      polygonCapColor={polygonCapColor}
      polygonSideColor={polygonSideColor}
      polygonStrokeColor={polygonStrokeColor}
      polygonAltitude={0.006}
      objectsData={globePoints}
      objectLat="lat"
      objectLng="lng"
      objectAltitude={0.006}
      objectThreeObject={markerObject}
      onObjectHover={hoverMarker}
      onObjectClick={pickPoint}
      pointerEventsFilter={markerPointerFilter}
      enablePointerInteraction={active && !mapInteractive && !mapLayout && points.length > 0}
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
    <ul>{points.map((point) => <li key={point.id}><button type="button" onClick={(event) => selectPoint(point.id, event.currentTarget)}>{formatMessage(pointButtonLabel, { name: pointNames.get(point.id) ?? point.label })}{point.members.length > 1 ? ` (${point.members.length})` : ''}</button></li>)}</ul>
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
function GlobeCard({ details, html, labels, nodeLabel, onClose, drawer = false, nested = false, anchor, reduceMotion = false }: { details: GlobeDetails; html: string; labels: Messages['globeDetails']; nodeLabel: string; onClose: () => void; drawer?: boolean; nested?: boolean; anchor?: HTMLElement | null; reduceMotion?: boolean }) {
  const [copied, setCopied] = useState(false)
  const [placement, setPlacement] = useState<{ top: number; left: number; origin: string }>()
  const cardRef = useRef<HTMLElement>(null)
  useLayoutEffect(() => {
    const card = cardRef.current
    if (!nested || drawer || !anchor?.isConnected || !card || !card.parentElement) return
    const parent = card.parentElement
    const pane = card.closest('.workspace-network')
    if (!pane) return
    const parentBounds = parent.getBoundingClientRect()
    const paneBounds = pane.getBoundingClientRect()
    const scale = parent.offsetWidth > 0 ? parentBounds.width / parent.offsetWidth : 1
    const inset = 12
    const anchorBounds = anchor.getBoundingClientRect()
    const width = card.offsetWidth * scale
    const height = card.offsetHeight * scale
    if (width <= 0 || height <= 0 || scale <= 0) return
    const anchorX = anchorBounds.left + anchorBounds.width / 2
    const anchorY = anchorBounds.top + anchorBounds.height / 2
    const clamp = (value: number, min: number, max: number) => Math.max(min, Math.min(Math.max(min, max), value))
    // Place the title near the selected row, but keep the card within the
    // pane. Physical coordinates keep this correct in both LTR and RTL.
    const left = clamp(anchorX - width / 2, paneBounds.left + inset, paneBounds.right - width - inset)
    const top = clamp(anchorY - Math.min(44, height / 2), paneBounds.top + inset, paneBounds.bottom - height - inset)
    const x = (anchorX - left) / width * 100
    const y = (anchorY - top) / height * 100
    setPlacement({ left: (left - parentBounds.left) / scale, top: (top - parentBounds.top) / scale, origin: `${x}% ${y}%` })
  }, [anchor, drawer, nested])
  useEffect(() => { if (!drawer) cardRef.current?.focus() }, [details.peerId, drawer])
  useEffect(() => {
    if (!copied) return
    const timeout = window.setTimeout(() => setCopied(false), 1600)
    return () => window.clearTimeout(timeout)
  }, [copied])
  useEffect(() => {
    if (drawer) return
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
  }, [drawer, onClose])
  const kindLabel = details.kind === 'node' ? labels.thisNode : details.kind === 'worker' ? labels.worker : nodeLabel
  const title = details.worker?.name || kindLabel
  return <motion.section ref={cardRef} tabIndex={drawer ? undefined : -1} role={drawer ? undefined : 'dialog'} aria-label={drawer ? undefined : formatMessage(labels.pointButton, { name: title })} className={drawer ? 'globe-card-pinned globe-card-pinned--drawer' : `globe-card-pinned liquid-glass-menu${nested ? ' globe-card-pinned--nested' : ''}`} style={nested && !drawer && placement ? { insetInlineStart: 'auto', insetBlockEnd: 'auto', top: placement.top, left: placement.left, transformOrigin: placement.origin } : undefined} initial={drawer || reduceMotion ? false : nested ? { opacity: 0, scale: 0.2 } : { opacity: 0, y: 14, scale: 0.96 }} animate={{ opacity: 1, y: 0, scale: 1 }} exit={drawer || reduceMotion ? { opacity: 0, transition: { duration: 0 } } : nested ? { opacity: 0, scale: 0.2 } : { opacity: 0, y: 9, scale: 0.97 }} transition={{ duration: reduceMotion ? 0 : 0.24, ease: [0.22, 1, 0.36, 1] }}>
    <div className="globe-card-pinned__actions">
      <button type="button" aria-label={copied ? labels.copied : labels.copyPeerId} title={copied ? labels.copied : labels.copyPeerId} onClick={() => { void navigator.clipboard.writeText(details.peerId).then(() => setCopied(true), () => {}) }}>{copied ? <Check size={15} aria-hidden /> : <Copy size={15} aria-hidden />}</button>
      {!drawer && <button type="button" aria-label={labels.close} title={labels.close} onClick={onClose}><X size={14} aria-hidden /></button>}
    </div>
    <div className="globe-card__title globe-detail-title">{hasCountryFlag(details.countryCode) && <CountryFlag code={details.countryCode} size={18} className="globe-detail-flag" />}<span><bdi>{title}</bdi>{details.worker?.name && <span className="globe-card__kind"> · {kindLabel}</span>}</span></div>
    <div dangerouslySetInnerHTML={{ __html: html }} />
  </motion.section>
}

function GlobeClusterCard({ ids, names, labels, title, flag, onChoose, onClose, drawer = false, childOpen = false, children, reduceMotion = false }: { ids: string[]; names: ReadonlyMap<string, string>; labels: Messages['globeDetails']; title: string; flag: string; onChoose: (id: string, from: HTMLElement) => void; onClose: () => void; drawer?: boolean; childOpen?: boolean; children?: ReactNode; reduceMotion?: boolean }) {
  const ref = useRef<HTMLElement>(null)
  useEffect(() => { if (!drawer) ref.current?.focus() }, [drawer])
  useEffect(() => {
    if (drawer || childOpen) return
    const onKey = (event: KeyboardEvent) => { if (event.key === 'Escape' && ref.current?.contains(event.target as Node)) { event.preventDefault(); onClose() } }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [childOpen, drawer, onClose])
  return <motion.section ref={ref} tabIndex={drawer ? undefined : -1} role={drawer ? undefined : 'dialog'} aria-label={drawer ? undefined : title} className={drawer ? 'globe-card-pinned globe-card-pinned--drawer' : `globe-card-pinned liquid-glass-menu${children ? ' globe-card-pinned--parent' : ''}`} initial={drawer || reduceMotion ? false : { opacity: 0, y: 14, scale: 0.96 }} animate={{ opacity: 1, y: 0, scale: childOpen && !drawer && !reduceMotion ? 0.96 : 1 }} exit={{ opacity: 0, y: reduceMotion ? 0 : 9, scale: reduceMotion ? 1 : 0.97 }} transition={{ duration: reduceMotion ? 0 : 0.24, ease: [0.22, 1, 0.36, 1] }}>
    <div inert={childOpen} className="globe-cluster-surface" data-child-open={childOpen}>
    {!drawer && <div className="globe-card-pinned__actions"><button type="button" aria-label={labels.close} title={labels.close} onClick={onClose}><X size={14} aria-hidden /></button></div>}
    <div className="globe-card__title globe-cluster-title">{flag && <CountryFlag code={flag} size={18} className="globe-cluster-flag" />}{title} <span>{ids.length}</span></div>
    <ul className="globe-cluster-list">{ids.map((id) => <li key={id}><button type="button" onClick={(event) => onChoose(id, event.currentTarget)}><span className="globe-cluster-dot" aria-hidden="true" /><bdi>{names.get(id) ?? id}</bdi></button></li>)}</ul>
    </div>
    {children}
  </motion.section>
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
  const phone = useBreakpoint('(max-width: 640px)')
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
    for (const preview of previewPeers) all.set(preview.id, { ...peerDetails(preview.id), countryCode: preview.countryCode })
    return all
  }, [snapshot.info, snapshot.peers, workers, cityEstimate, pin])
  const pointNames = useMemo(() => new Map([...details].map(([id, item]) => [id, id === ownPointId ? messages.thisNode : previewPeers.find((peer) => peer.id === id)?.name || item.worker?.name || item.peerId])), [details, messages.thisNode])
  const renderCard = useCallback((item: GlobeDetails, pinned: boolean) => renderDetailsHtml(item, messages.globeDetails, { direction, locale: document.documentElement.lang || 'en', pinned, includeTitle: false }), [messages.globeDetails, direction])
  const [pinnedPoint, setPinnedPoint] = useState<string>()
  const [pinnedCluster, setPinnedCluster] = useState<string[]>()
  const pinnedFromRef = useRef<HTMLElement | null>(null)
  const clusterMemberFromRef = useRef<HTMLElement | null>(null)
  const pickPoint = useCallback((id: string, from?: HTMLElement) => { pinnedFromRef.current = from ?? null; setPinnedCluster(undefined); setPinnedPoint(id) }, [])
  const pickCluster = useCallback((ids: string[], from?: HTMLElement) => { pinnedFromRef.current = from ?? null; setPinnedPoint(undefined); setPinnedCluster(ids) }, [])
  const chooseClusterMember = useCallback((id: string, from: HTMLElement) => { clusterMemberFromRef.current = from; setPinnedPoint(id) }, [])
  const closeDetails = useCallback(() => {
    if (!pinnedCluster) { setPinnedPoint(undefined); const from = pinnedFromRef.current; pinnedFromRef.current = null; if (from?.isConnected) from.focus(); return }
    setPinnedPoint(undefined)
    const from = clusterMemberFromRef.current
    clusterMemberFromRef.current = null
    if (from) window.requestAnimationFrame(() => { if (from.isConnected) from.focus() })
  }, [pinnedCluster])
  const closePinned = useCallback(() => {
    setPinnedPoint(undefined)
    setPinnedCluster(undefined)
    clusterMemberFromRef.current = null
    // Back to the point's button, if the card was opened from one.
    const from = pinnedFromRef.current
    pinnedFromRef.current = null
    if (from?.isConnected) from.focus()
  }, [])
  const pinnedDetails = pinnedPoint ? details.get(pinnedPoint) : undefined
  const clusterCountries = pinnedCluster?.map((id) => details.get(id)?.countryCode?.toUpperCase()) ?? []
  const clusterCountry = clusterCountries.length && clusterCountries.every((country) => country === clusterCountries[0]) ? clusterCountries[0] : undefined
  const clusterFlag = hasCountryFlag(clusterCountry) ? clusterCountry : undefined
  let clusterCountryName: string | undefined
  if (clusterFlag && clusterCountry) {
    try { clusterCountryName = new Intl.DisplayNames([document.documentElement.lang || 'en'], { type: 'region' }).of(clusterCountry) } catch { /* Use the general title if localization is unavailable. */ }
  }
  const clusterTitle = clusterCountryName && clusterCountryName !== clusterCountry ? `${messages.globeClusterNodes} · ${clusterCountryName}` : messages.globeClusterNodes
  const displayedClusterFlag = clusterCountryName && clusterCountryName !== clusterCountry ? clusterFlag ?? '' : ''
  const lastPinnedDetailsRef = useRef<GlobeDetails | undefined>(undefined)
  if (pinnedDetails) lastPinnedDetailsRef.current = pinnedDetails
  const drawerDetails = pinnedDetails ?? lastPinnedDetailsRef.current
  const globeActive = visible && (compact ? mobileView === 'network' : topologyOpen)
  const zoomLabels = useMemo(() => ({ group: messages.globeZoom.group, in: messages.globeZoom.in, out: messages.globeZoom.out }), [messages.globeZoom])
  const countrySource = snapshot.info?.countrySource
  const countryPosition = countryCode ? countryCentroids.get(countryCode) : undefined
  const countryLabel = countrySource === 'pool' ? messages.geoCountryAsSeenByPool : messages.geoCountry
  const networkPane = <section className="workspace-network workspace-glass liquid-glass-menu relative flex h-full min-h-0 items-center justify-center overflow-hidden rounded-[22px]">
          <div className="network-visual absolute inset-0 overflow-hidden">{globeReady && <NetworkGlobe countryCode={countryCode} pin={pin} shared={shared} peers={snapshot.peers} nodeLabel={messages.thisNode} countryLabel={countryCode ? `${countryLabel}: ${countryCode} · ${messages.countryEstimateDisclaimer}` : messages.thisNode} reduceMotion={Boolean(reduceMotion)} active={globeActive} rotationPaused={topologyResizing} detailsOpen={Boolean(pinnedDetails || pinnedCluster)} suspendResize={!compact && topologyTransitioning} pointNames={pointNames} pointsLabel={messages.globeDetails.points} pointButtonLabel={messages.globeDetails.pointButton} onPickPoint={pickPoint} onPickCluster={pickCluster} zoomLabels={zoomLabels} />}</div>
          {phone ? <Drawer.Root open={Boolean(globeActive && (pinnedDetails || pinnedCluster))} onOpenChange={(open) => { if (!open) closePinned() }} direction="bottom" shouldScaleBackground={false}>
            <Drawer.Portal>
              <Drawer.Overlay className="globe-details-overlay fixed inset-0 z-50 bg-black/25 backdrop-blur-sm" />
              <Drawer.Content dir={direction} aria-describedby={undefined} className="globe-details-drawer liquid-glass-menu fixed inset-x-0 bottom-0 z-50 flex max-h-[85dvh] flex-col rounded-t-[1.5rem] border border-b-0 border-[var(--app-line)] bg-[var(--app-card)] outline-none">
                <div aria-hidden="true" className="mx-auto mt-3 h-1 w-10 shrink-0 rounded-full bg-[var(--app-line)]" />
                <Drawer.Title className="sr-only">{pinnedCluster ? clusterTitle : drawerDetails ? formatMessage(messages.globeDetails.pointButton, { name: drawerDetails.kind === 'node' ? messages.globeDetails.thisNode : drawerDetails.worker?.name || (drawerDetails.kind === 'worker' ? messages.globeDetails.worker : messages.node) }) : messages.node}</Drawer.Title>
                {pinnedCluster ? <GlobeClusterCard drawer childOpen={Boolean(pinnedDetails)} ids={pinnedCluster} names={pointNames} labels={messages.globeDetails} title={clusterTitle} flag={displayedClusterFlag} onChoose={chooseClusterMember} onClose={closePinned}>
                  <Drawer.NestedRoot open={Boolean(pinnedDetails)} onOpenChange={(open) => { if (!open) closeDetails() }} direction="bottom">
                    <Drawer.Portal>
                      <Drawer.Overlay className="globe-details-overlay fixed inset-0 z-[60] bg-black/20" />
                      <Drawer.Content dir={direction} aria-describedby={undefined} className="globe-details-drawer liquid-glass-menu fixed inset-x-0 bottom-0 z-[60] flex max-h-[85dvh] flex-col rounded-t-[1.5rem] border border-b-0 border-[var(--app-line)] bg-[var(--app-card)] outline-none">
                        <div aria-hidden="true" className="mx-auto mt-3 h-1 w-10 shrink-0 rounded-full bg-[var(--app-line)]" />
                        <Drawer.Title className="sr-only">{drawerDetails ? formatMessage(messages.globeDetails.pointButton, { name: pointNames.get(drawerDetails.peerId) ?? messages.node }) : messages.node}</Drawer.Title>
                        {drawerDetails && <GlobeCard drawer details={drawerDetails} html={renderCard(drawerDetails, true)} labels={messages.globeDetails} nodeLabel={messages.node} onClose={closeDetails} />}
                      </Drawer.Content>
                    </Drawer.Portal>
                  </Drawer.NestedRoot>
                </GlobeClusterCard> : drawerDetails && <GlobeCard drawer details={drawerDetails} html={renderCard(drawerDetails, true)} labels={messages.globeDetails} nodeLabel={messages.node} onClose={closePinned} />}
              </Drawer.Content>
            </Drawer.Portal>
          </Drawer.Root> : <AnimatePresence initial={false}>{globeActive && pinnedCluster ? <GlobeClusterCard key="cluster" ids={pinnedCluster} names={pointNames} labels={messages.globeDetails} title={clusterTitle} flag={displayedClusterFlag} onChoose={chooseClusterMember} onClose={closePinned} childOpen={Boolean(pinnedDetails)} reduceMotion={Boolean(reduceMotion)}>
            <AnimatePresence initial={false}>{pinnedDetails && <GlobeCard key={pinnedDetails.peerId} nested anchor={clusterMemberFromRef.current} details={pinnedDetails} html={renderCard(pinnedDetails, true)} labels={messages.globeDetails} nodeLabel={messages.node} onClose={closeDetails} reduceMotion={Boolean(reduceMotion)} />}</AnimatePresence>
          </GlobeClusterCard> : globeActive && pinnedDetails ? <GlobeCard key={pinnedDetails.peerId} details={pinnedDetails} html={renderCard(pinnedDetails, true)} labels={messages.globeDetails} nodeLabel={messages.node} onClose={closePinned} reduceMotion={Boolean(reduceMotion)} /> : null}</AnimatePresence>}
          <div className="absolute inset-x-5 top-5 z-[2] flex items-center justify-between gap-3 text-[10px]">
            <div className="rounded-lg border border-[var(--app-line)] bg-[var(--app-card)]/85 px-3 py-2 text-muted-foreground shadow-sm backdrop-blur"><span className="font-mono text-foreground">{connectedPeers.length}</span> {messages.connectedPeers.toLowerCase()}</div>
            <div className="max-w-[62%] rounded-lg border border-[var(--app-line)] bg-[var(--app-card)]/85 px-3 py-2 text-end text-muted-foreground shadow-sm backdrop-blur"><span className="me-1.5 inline-block size-1.5 rounded-full bg-[var(--app-accent)] align-middle" />{messages.thisNode}{hasCountryFlag(countryCode) && <><span aria-hidden="true"> · </span><CountryFlag code={countryCode} size={14} className="inline-flex shrink-0 align-middle" /></>}{countryPosition ? <span className="ms-1.5">{countryCode}</span> : null}{!countryCode && !pin && isDesktopApp() ? <Link to="/settings/node" className="ms-1.5 text-[var(--app-accent)] underline-offset-2 hover:underline focus-visible:outline-2 focus-visible:outline-[var(--app-accent)]">· {messages.unplacedLink}</Link> : null}</div>
          </div>
          {import.meta.env.DEV && <div className="globe-preview-indicator">DEV · 10 preview peers</div>}
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
