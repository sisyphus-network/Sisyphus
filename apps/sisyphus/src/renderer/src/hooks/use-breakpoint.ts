import { useCallback, useSyncExternalStore } from 'react'

export function useBreakpoint(query: string) {
  const subscribe = useCallback((callback: () => void) => {
    const media = window.matchMedia(query)
    media.addEventListener('change', callback)
    return () => media.removeEventListener('change', callback)
  }, [query])
  const getSnapshot = useCallback(() => window.matchMedia(query).matches, [query])
  return useSyncExternalStore(
    subscribe,
    getSnapshot,
    () => false,
  )
}
