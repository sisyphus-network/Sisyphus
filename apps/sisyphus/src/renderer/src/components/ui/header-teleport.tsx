import { createContext, useContext, useId, useLayoutEffect, useMemo, useRef, useState, type Dispatch, type ReactNode, type SetStateAction } from 'react'
import { createPortal } from 'react-dom'
import { motion, useReducedMotion } from 'motion/react'

type HeaderSlot = { target: HTMLDivElement | null; setTarget: Dispatch<SetStateAction<HTMLDivElement | null>>; owner: string | null; setOwner: Dispatch<SetStateAction<string | null>> }
const HeaderSlotContext = createContext<HeaderSlot | null>(null)
const HeaderSearchContext = createContext<{ open: boolean; setOpen: Dispatch<SetStateAction<boolean>> } | null>(null)

export function useHeaderSearch() {
  const context = useContext(HeaderSearchContext)
  if (!context) throw new Error('Header search requires HeaderTeleportProvider')
  return context
}

export function useHeaderSearchReady() {
  const slot = useContext(HeaderSlotContext)
  const search = useContext(HeaderSearchContext)
  return Boolean(slot?.owner && search?.open)
}

export function HeaderTeleportProvider({ children }: { children: ReactNode }) {
  const [target, setTarget] = useState<HTMLDivElement | null>(null)
  const [owner, setOwner] = useState<string | null>(null)
  const [searchOpen, setSearchOpen] = useState(false)
  const value = useMemo(() => ({ target, setTarget, owner, setOwner }), [target, owner])
  const search = useMemo(() => ({ open: searchOpen, setOpen: setSearchOpen }), [searchOpen])
  return <HeaderSlotContext.Provider value={value}><HeaderSearchContext.Provider value={search}>{children}</HeaderSearchContext.Provider></HeaderSlotContext.Provider>
}

export function HeaderTeleportSlot({ fallback }: { fallback: ReactNode }) {
  const context = useContext(HeaderSlotContext)
  const search = useContext(HeaderSearchContext)
  const searching = Boolean(context?.owner && search?.open)
  const reduced = useReducedMotion()
  const transition = { duration: reduced ? 0 : 0.2, ease: [0.22, 1, 0.36, 1] as const }
  return <div className="grid h-11 min-w-0 flex-1 grid-cols-[minmax(0,1fr)] items-center">
    <motion.div initial={false} animate={{ opacity: searching ? 0 : 1, y: searching && !reduced ? -5 : 0 }} transition={transition}
      inert={searching} aria-hidden={searching} className="col-start-1 row-start-1 min-w-0 overflow-hidden whitespace-nowrap" style={{ pointerEvents: searching ? 'none' : 'auto' }}>{fallback}</motion.div>
    <motion.div ref={context?.setTarget} initial={false} animate={{ opacity: searching ? 1 : 0, y: !searching && !reduced ? 5 : 0 }} transition={transition}
      inert={!searching} aria-hidden={!searching} className="col-start-1 row-start-1 mx-auto w-full min-w-0 max-w-3xl" style={{ pointerEvents: searching ? 'auto' : 'none' }} />
  </div>
}

// A stable portal container moves the same mounted input between the page
// and header: drafts, focus and selection survive the transfer.
export function HeaderTeleport({ active, headerOnly = false, children }: { active: boolean; headerOnly?: boolean; children: ReactNode }) {
  const context = useContext(HeaderSlotContext)
  const target = context?.target
  const setOwner = context?.setOwner
  const id = useId()
  const home = useRef<HTMLDivElement>(null)
  const [container] = useState(() => document.createElement('div'))
  const [height, setHeight] = useState<number>()
  const [leaving, setLeaving] = useState(false)
  const reducedMotion = useReducedMotion()
  const docked = Boolean(target && (active || leaving))
  useLayoutEffect(() => {
    if (active) { setLeaving(true); return }
    if (!leaving) return
    const timer = window.setTimeout(() => setLeaving(false), reducedMotion ? 0 : 200)
    return () => window.clearTimeout(timer)
  }, [active, leaving, reducedMotion])
  useLayoutEffect(() => {
    const destination = docked ? target : home.current
    if (!destination) return
    if (docked && home.current) setHeight(home.current.getBoundingClientRect().height)
    const focused = document.activeElement instanceof HTMLInputElement && container.contains(document.activeElement) ? document.activeElement : null
    const selection = focused ? { start: focused.selectionStart, end: focused.selectionEnd, direction: focused.selectionDirection } : null
    container.dataset.headerDocked = String(docked)
    container.className = 'header-teleport-content flow-root min-w-0 w-full'
    destination.appendChild(container)
    if (docked || !headerOnly) focused?.focus({ preventScroll: true })
    if (focused && selection?.start != null && selection.end != null) focused.setSelectionRange(selection.start, selection.end, selection.direction ?? undefined)
  }, [container, docked, headerOnly, target])
  useLayoutEffect(() => {
    if (!docked || !setOwner) return
    setOwner(id)
    return () => setOwner((current) => current === id ? null : current)
  }, [docked, id, setOwner])
  useLayoutEffect(() => () => container.remove(), [container])
  return <><div ref={home} hidden={headerOnly || docked && height === 0} style={{ minHeight: !headerOnly && docked ? height : undefined }} />{createPortal(children, container)}</>
}
