import type { LucideIcon } from 'lucide-react'

export function SettingsIcon({ icon: Icon, compact = false }: { icon: LucideIcon; compact?: boolean }) {
  return <span aria-hidden="true" className={`grid shrink-0 place-items-center rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)] text-[var(--app-accent)] ${compact ? 'size-8' : 'size-10'}`}><Icon className={compact ? 'size-4' : 'size-[18px]'} /></span>
}
