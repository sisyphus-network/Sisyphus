// Builds src/renderer/src/data/admin1-lines.json, the globe's first-level
// region lines (states and provinces), from Natural Earth's 1:10m
// "Admin 1 – States, Provinces" boundary lines, which are in the public
// domain:
//
//   curl -LO https://raw.githubusercontent.com/nvkelso/natural-earth-vector/v5.1.2/geojson/ne_10m_admin_1_states_provinces_lines.geojson
//   node dev/build-region-lines.mjs ne_10m_admin_1_states_provinces_lines.geojson
//
// Each line is simplified (Douglas–Peucker, to 0.02 degree, a couple of
// kilometres), its points rounded to 0.01 degree, and written as whole
// hundredths of a degree: the first point's longitude and latitude, then
// each next point's difference from the one before. About 0.9 MB.

import { readFileSync, writeFileSync } from 'node:fs'

const [source, target = new URL('../src/renderer/src/data/admin1-lines.json', import.meta.url)] = process.argv.slice(2)
if (!source) throw new Error('usage: node dev/build-region-lines.mjs <ne_10m_admin_1_states_provinces_lines.geojson> [output]')
const tolerance = 0.02

function simplify(points) {
  if (points.length < 3) return points
  const keep = new Array(points.length).fill(false)
  keep[0] = keep[points.length - 1] = true
  const stack = [[0, points.length - 1]]
  while (stack.length) {
    const [a, b] = stack.pop()
    const [ax, ay] = points[a]
    const [bx, by] = points[b]
    const dx = bx - ax
    const dy = by - ay
    const length = Math.hypot(dx, dy)
    let farthest = -1
    let index = -1
    for (let i = a + 1; i < b; i++) {
      const [px, py] = points[i]
      const distance = length ? Math.abs(dy * px - dx * py + bx * ay - by * ax) / length : Math.hypot(px - ax, py - ay)
      if (distance > farthest) { farthest = distance; index = i }
    }
    if (farthest > tolerance) { keep[index] = true; stack.push([a, index], [index, b]) }
  }
  return points.filter((_, i) => keep[i])
}

const lines = []
for (const feature of JSON.parse(readFileSync(source, 'utf8')).features) {
  const geometry = feature.geometry
  if (!geometry) continue
  const parts = geometry.type === 'LineString' ? [geometry.coordinates] : geometry.coordinates
  for (const part of parts) {
    const points = []
    for (const [lng, lat] of simplify(part)) {
      const point = [Math.round(lng * 100), Math.round(lat * 100)]
      const last = points.at(-1)
      if (!last || last[0] !== point[0] || last[1] !== point[1]) points.push(point)
    }
    if (points.length < 2) continue
    const encoded = [...points[0]]
    for (let i = 1; i < points.length; i++) encoded.push(points[i][0] - points[i - 1][0], points[i][1] - points[i - 1][1])
    lines.push(encoded)
  }
}
writeFileSync(target, JSON.stringify(lines))
console.log(`${lines.length} lines, ${lines.reduce((sum, line) => sum + line.length / 2, 0)} points`)
