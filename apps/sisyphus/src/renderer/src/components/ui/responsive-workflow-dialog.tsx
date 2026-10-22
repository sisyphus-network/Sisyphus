import type { ReactNode } from 'react'
import { Drawer } from 'vaul'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { useIsMobile } from '@/hooks/use-mobile'

export function ResponsiveWorkflowDialog({ open, onOpenChange, direction, title, description, children, footer }: {
  open: boolean
  onOpenChange: (open: boolean) => void
  direction: 'rtl' | 'ltr'
  title: string
  description: string
  children: ReactNode
  footer: ReactNode
}) {
  const compact = useIsMobile()
  const body = <div className="flex min-h-0 flex-1 flex-col">
    <div className="min-h-0 flex-1 overflow-y-auto overscroll-contain px-5 py-5 sm:px-8 lg:px-10">{children}</div>
    <div className="grid shrink-0 grid-cols-2 gap-2 border-t border-[var(--app-line)] px-5 py-3 sm:flex sm:justify-end sm:px-7">{footer}</div>
  </div>

  if (compact) return <Drawer.Root open={open} onOpenChange={onOpenChange} dismissible={false} direction="bottom">
    <Drawer.Portal>
      <Drawer.Overlay className="fixed inset-0 z-50 bg-black/30 backdrop-blur-sm" />
      <Drawer.Content dir={direction} className="liquid-glass-menu fixed inset-x-0 bottom-0 z-50 flex h-[min(92dvh,56rem)] max-h-[92dvh] flex-col overflow-hidden rounded-t-[1.5rem] bg-[var(--app-card)] pb-[max(0.75rem,env(safe-area-inset-bottom))] outline-none">
        <div aria-hidden="true" className="mx-auto mt-3 h-1 w-10 shrink-0 rounded-full bg-[var(--app-line)]" />
        <Drawer.Title className="shrink-0 px-5 pt-3 text-lg font-semibold">{title}</Drawer.Title>
        <Drawer.Description className="shrink-0 px-5 pt-1 text-xs text-muted-foreground">{description}</Drawer.Description>
        {body}
      </Drawer.Content>
    </Drawer.Portal>
  </Drawer.Root>

  return <Dialog open={open} disablePointerDismissal onOpenChange={(next, details) => { if (next || details.reason === 'close-press') onOpenChange(next) }}>
    <DialogContent dir={direction} className="liquid-glass-menu flex h-[min(50rem,90dvh)] w-[min(72rem,calc(100vw-2rem))] max-w-none flex-col overflow-hidden rounded-[1.5rem] border border-[var(--app-line)] bg-[var(--app-card)] p-0 sm:max-w-none">
      <DialogHeader className="shrink-0 px-8 pt-7 lg:px-10 lg:pt-8"><DialogTitle>{title}</DialogTitle><DialogDescription>{description}</DialogDescription></DialogHeader>
      {body}
    </DialogContent>
  </Dialog>
}
