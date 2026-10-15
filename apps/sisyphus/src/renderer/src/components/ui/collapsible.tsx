import * as React from 'react'
import { ChevronDown } from 'lucide-react'
import { motion, type Transition } from 'motion/react'
import { cn } from '@/lib/utils'

const COLLAPSIBLE_TRANSITION = { duration: 0.25, ease: 'easeInOut' } as const
type CollapsibleContextValue = { open: boolean; setOpen: (open: boolean) => void }
const CollapsibleContext = React.createContext<CollapsibleContextValue | null>(null)

function useCollapsibleState() {
  const value = React.useContext(CollapsibleContext)
  if (!value) throw new Error('Collapsible components must be used within Collapsible')
  return value
}

export function Collapsible({ open, onOpenChange, defaultOpen = false, className, children, ...props }: React.HTMLAttributes<HTMLDivElement> & {
  open?: boolean
  defaultOpen?: boolean
  onOpenChange?: (open: boolean) => void
}) {
  const [uncontrolledOpen, setUncontrolledOpen] = React.useState(defaultOpen)
  const isControlled = open !== undefined
  const isOpen = isControlled ? open : uncontrolledOpen
  const setOpen = (next: boolean) => {
    if (!isControlled) setUncontrolledOpen(next)
    onOpenChange?.(next)
  }

  return <CollapsibleContext.Provider value={{ open: isOpen, setOpen }}>
    <div data-slot="collapsible" data-state={isOpen ? 'open' : 'closed'} className={className} {...props}>
      {children}
    </div>
  </CollapsibleContext.Provider>
}

type CollapsibleTriggerProps = React.ButtonHTMLAttributes<HTMLButtonElement> & {
  title?: string
  description?: string
  icon?: React.ComponentType<{ className?: string }>
  rightSlot?: React.ReactNode
}

export function CollapsibleTrigger({ className, title, description, icon: Icon, rightSlot, children, onClick, ...props }: CollapsibleTriggerProps) {
  const { open, setOpen } = useCollapsibleState()
  return <button type="button" data-slot="collapsible-trigger" data-state={open ? 'open' : 'closed'} aria-expanded={open} onClick={event => { onClick?.(event); if (!event.defaultPrevented) setOpen(!open) }} className={cn('group flex w-full items-center justify-between gap-4 rounded-xl px-4 py-3 text-start transition-colors hover:bg-[var(--app-wash)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--app-accent)]', className)} {...props}>
    {title || description || Icon ? <>
      <span className="flex min-w-0 items-center gap-3 text-start">
        {Icon ? <span className="grid size-10 shrink-0 place-items-center rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)] text-[var(--app-accent)]"><Icon className="size-[18px]" /></span> : null}
        <span className="flex min-w-0 flex-col items-start">
          {title ? <span className="truncate text-sm font-semibold text-[var(--app-ink)]">{title}</span> : null}
          {description ? <span className="mt-0.5 truncate text-xs font-normal text-[var(--app-muted)]">{description}</span> : null}
        </span>
      </span>
      <span className="flex shrink-0 items-center gap-3">
        {rightSlot ? <span onClick={event => event.stopPropagation()}>{rightSlot}</span> : null}
        <ChevronDown aria-hidden="true" className="size-4 text-[var(--app-muted)] transition-transform duration-200 group-data-[state=open]:rotate-180" />
      </span>
    </> : children}
  </button>
}

export function CollapsibleContent({ className, children, animateSpacing = true, animateOpacity = true, transition = COLLAPSIBLE_TRANSITION }: {
  className?: string
  children?: React.ReactNode
  animateSpacing?: boolean
  animateOpacity?: boolean
  transition?: Transition
}) {
  const { open } = useCollapsibleState()
  return <motion.div data-slot="collapsible-content" aria-hidden={!open} inert={!open} className={cn('grid overflow-hidden', className)} initial={false} animate={{
    height: open ? 'auto' : 0,
    opacity: animateOpacity ? (open ? 1 : 0) : 1,
    marginTop: animateSpacing && open ? 12 : 0,
    paddingInline: animateSpacing && open ? 12 : 0,
    paddingBottom: animateSpacing && open ? 8 : 0,
  }} transition={transition} style={{ pointerEvents: open ? 'auto' : 'none' }}>
    {children}
  </motion.div>
}
