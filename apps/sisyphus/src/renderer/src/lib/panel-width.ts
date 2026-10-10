// Interaction starts at the width actually rendered, not a saved preference
// that may be larger than the space available after a viewport resize.
export function visiblePanelWidth(preferred: number, maximum: number): number {
  return Math.max(0, Math.min(preferred, maximum))
}

export function stepPanelWidth(preferred: number, delta: number, minimum: number, maximum: number): number {
  const upper = Math.max(0, maximum)
  const lower = Math.max(0, Math.min(minimum, upper))
  return Math.max(lower, Math.min(upper, visiblePanelWidth(preferred, upper) + delta))
}
