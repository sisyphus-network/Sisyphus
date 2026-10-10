// The globe is drawn with WebGL, which some machines do not give a window:
// a remote desktop, a virtual machine, a driver the browser will not use.
// Asked to draw without it, the globe throws, and takes the whole window
// with it. So it is asked for first, once, and the globe left out where
// there is none.
let known: boolean | undefined

export function webglAvailable(canvas: () => Pick<HTMLCanvasElement, 'getContext'> = () => document.createElement('canvas')): boolean {
  if (known === undefined) known = probe(canvas)
  return known
}

export function probe(canvas: () => Pick<HTMLCanvasElement, 'getContext'>): boolean {
  try {
    const made = canvas()
    return Boolean(made.getContext('webgl2') ?? made.getContext('webgl'))
  } catch {
    return false
  }
}
