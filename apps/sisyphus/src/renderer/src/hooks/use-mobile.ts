import * as React from "react"

const MOBILE_BREAKPOINT = 768
const DESKTOP_BREAKPOINT = 1024

const subscribe = (callback: () => void) => {
  window.addEventListener("resize", callback)
  return () => window.removeEventListener("resize", callback)
}

const getSnapshot = () => window.innerWidth < MOBILE_BREAKPOINT
const getServerSnapshot = () => false
const subscribeToHydration = () => () => {}

function useHydrated() {
  return React.useSyncExternalStore(subscribeToHydration, () => true, () => false)
}

export function useIsMobile() {
  const hydrated = useHydrated()
  const isMobile = React.useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot)
  return hydrated && isMobile
}

export function useIsDesktop() {
  const hydrated = useHydrated()
  const isDesktop = React.useSyncExternalStore(
    subscribe,
    () => window.innerWidth >= DESKTOP_BREAKPOINT,
    getServerSnapshot,
  )
  return hydrated && isDesktop
}
