import { webglAvailable } from '@/lib/webgl'

// Whether this window can draw with WebGL, as a capability any part of the
// interface may ask about: a view that needs it is not offered where there
// is none. It is found out once, the first time anything asks, and does not
// change while the window lives.
export function useWebgl(): boolean {
  return webglAvailable()
}
