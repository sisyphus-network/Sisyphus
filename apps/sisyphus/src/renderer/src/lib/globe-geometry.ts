import { Quaternion, Vector3, type BufferGeometry } from 'three'

// GeoJsonGeometry's owner relies on parameters to recognize its geometry;
// Three.js's toNonIndexed() does not carry that metadata across.
export function unindexGlobeGeometry(geometry: BufferGeometry): BufferGeometry {
  const result = geometry.toNonIndexed()
  const source = geometry as BufferGeometry & { parameters?: unknown }
  const target = result as BufferGeometry & { parameters?: unknown }
  if (source.parameters) target.parameters = source.parameters
  return result
}

// Move around the origin rather than cutting through the globe when the
// starting and ending views face opposite hemispheres.
export function interpolateOrbitPosition(from: Vector3, to: Vector3, progress: number, output: Vector3): Vector3 {
  const fromLength = from.length()
  const toLength = to.length()
  if (fromLength < 1e-6 || toLength < 1e-6) return output.lerpVectors(from, to, progress)
  const fromDirection = from.clone().divideScalar(fromLength)
  const rotation = new Quaternion().setFromUnitVectors(fromDirection, to.clone().divideScalar(toLength))
  const partialRotation = new Quaternion().slerp(rotation, progress)
  return output.copy(fromDirection).applyQuaternion(partialRotation).multiplyScalar(fromLength + (toLength - fromLength) * progress)
}

// Rotate the camera and its position along the same orbit. Interpolating the
// orientations independently can point away from the globe midway through a
// southern-hemisphere transition. The residual orientation is applied locally
// so the exact starting and ending camera poses are preserved.
export function createOrbitCameraTransition(from: Vector3, to: Vector3, fromOrientation: Quaternion, toOrientation: Quaternion) {
  const fromLength = from.length()
  const toLength = to.length()
  if (fromLength < 1e-6 || toLength < 1e-6) {
    return (progress: number, position: Vector3, orientation: Quaternion) => {
      position.lerpVectors(from, to, progress)
      orientation.slerpQuaternions(fromOrientation, toOrientation, progress)
    }
  }
  const direction = from.clone().divideScalar(fromLength)
  const orbit = new Quaternion().setFromUnitVectors(direction, to.clone().divideScalar(toLength))
  const finalOrbitOrientation = orbit.clone().multiply(fromOrientation)
  const correction = finalOrbitOrientation.clone().invert().multiply(toOrientation)
  const partialOrbit = new Quaternion()
  const partialCorrection = new Quaternion()
  const identity = new Quaternion()
  return (progress: number, position: Vector3, orientation: Quaternion) => {
    partialOrbit.slerpQuaternions(identity, orbit, progress)
    partialCorrection.slerpQuaternions(identity, correction, progress)
    position.copy(direction).applyQuaternion(partialOrbit).multiplyScalar(fromLength + (toLength - fromLength) * progress)
    orientation.copy(partialOrbit).multiply(fromOrientation).multiply(partialCorrection).normalize()
  }
}
