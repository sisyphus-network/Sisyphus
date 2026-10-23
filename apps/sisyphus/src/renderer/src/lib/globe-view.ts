// How far the workspace globe's camera may be from its centre, and the
// region lines it draws once it is close enough to tell them apart.

// The limits of the camera's distance from the globe's centre. "Fit" is the
// distance at which the globe just fills its square: the closest is a
// quarter of the radius above the surface, and the farthest twice the fit,
// the globe at half its size.
export type ZoomLimits = { min: number; max: number; fit: number }
export function zoomLimits(radius: number, fit: number): ZoomLimits {
  return { min: radius * 1.25, max: Math.max(fit * 2, radius * 3.5), fit }
}

export function clampDistance(distance: number, limits: ZoomLimits): number {
  return Math.min(limits.max, Math.max(limits.min, distance))
}

// zoomStep is where one press of a zoom button takes the camera: a fifth
// of the way to the surface in, or back out by as much.
export function zoomStep(distance: number, direction: 'in' | 'out', radius: number, limits: ZoomLimits): number {
  const height = distance - radius
  const next = direction === 'in' ? radius + height * 0.7 : radius + height / 0.7
  return clampDistance(next, limits)
}

export type ZoomState = { canZoomIn: boolean; canZoomOut: boolean; regions: boolean; percent: number }
export function zoomState(distance: number, limits: ZoomLimits): ZoomState {
  const slack = (limits.max - limits.min) * 0.002
  return {
    canZoomIn: distance > limits.min + slack,
    canZoomOut: distance < limits.max - slack,
    // Regions are drawn only once the globe is brought in past its fit,
    // where there is room to tell them apart.
    regions: distance < limits.fit * 0.85,
    percent: Math.round(limits.fit / distance * 100),
  }
}

// How fast the globe turns by itself, slower as it is brought in, so the
// ground does not race past.
export function autoRotateSpeed(distance: number, radius: number, limits: ZoomLimits): number {
  const share = (distance - radius) / Math.max(1e-6, limits.fit - radius)
  return 0.45 * Math.min(1, Math.max(0.1, share))
}

// decodeRegionLines reads admin1-lines.json (see dev/build-region-lines.ts):
// each line is its first point's longitude and latitude in hundredths of a
// degree, then each next point's difference from the one before.
export function decodeRegionLines(data: unknown): Array<Array<[number, number]>> {
  if (!Array.isArray(data)) return []
  const lines: Array<Array<[number, number]>> = []
  for (const encoded of data) {
    if (!Array.isArray(encoded) || encoded.length < 4 || encoded.length % 2) continue
    let lng = 0
    let lat = 0
    const line: Array<[number, number]> = []
    for (let i = 0; i < encoded.length; i += 2) {
      lng += Number(encoded[i])
      lat += Number(encoded[i + 1])
      line.push([lat / 100, lng / 100])
    }
    if (line.every(([a, b]) => Number.isFinite(a) && Number.isFinite(b) && Math.abs(a) <= 90 && Math.abs(b) <= 180)) lines.push(line)
  }
  return lines
}

// regionSegments turns the lines into the pairs of points a single
// THREE.LineSegments draws, each point placed by toXYZ.
export function regionSegments(lines: Array<Array<[number, number]>>, toXYZ: (lat: number, lng: number) => { x: number; y: number; z: number }): Float32Array {
  const pairs = lines.reduce((sum, line) => sum + line.length - 1, 0)
  const positions = new Float32Array(pairs * 6)
  let offset = 0
  for (const line of lines) {
    let previous = toXYZ(line[0][0], line[0][1])
    for (let i = 1; i < line.length; i++) {
      const next = toXYZ(line[i][0], line[i][1])
      positions.set([previous.x, previous.y, previous.z, next.x, next.y, next.z], offset)
      offset += 6
      previous = next
    }
  }
  return positions
}
