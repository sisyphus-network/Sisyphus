import type { ReactNode } from 'react'
import type { LucideIcon } from 'lucide-react'

export function PageHeading({ title, description, actions }: { title: string; description?: string; actions?: ReactNode }) {
  return <header className="flex shrink-0 flex-wrap items-center justify-between gap-4 py-7">
    <div className="min-w-0"><h1 className="text-2xl font-semibold tracking-tight">{title}</h1>{description && <p className="mt-2 max-w-2xl text-sm leading-6 text-muted-foreground">{description}</p>}</div>
    {actions}
  </header>
}

export function EmptyState({ icon: Icon, title, description, action, loading = false }: { icon: LucideIcon; title: string; description?: string; action?: ReactNode; loading?: boolean }) {
  return <div role="status" className="flex min-h-48 flex-col items-center justify-center gap-3 px-6 py-10 text-center">
    <span aria-hidden="true" className="grid size-11 place-items-center rounded-2xl border border-[var(--app-line)] bg-[var(--app-wash)] text-muted-foreground"><Icon strokeWidth={1.6} className={`size-5 ${loading ? 'animate-spin motion-reduce:animate-none' : ''}`} /></span>
    <div><p className="text-sm font-medium">{title}</p>{description && <p className="mx-auto mt-1.5 max-w-md text-xs leading-6 text-muted-foreground">{description}</p>}</div>
    {action && <div className="mt-2">{action}</div>}
  </div>
}
