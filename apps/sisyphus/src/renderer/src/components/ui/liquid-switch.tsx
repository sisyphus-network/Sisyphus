import { LoaderCircle } from 'lucide-react'

export function LiquidSwitch({ checked, loading = false, onChange, label, disabled = false }: {
  checked: boolean
  loading?: boolean
  onChange: (checked: boolean) => void
  label: string
  disabled?: boolean
}) {
  return <button type="button" role="switch" aria-label={label} aria-checked={checked} aria-busy={loading} disabled={loading || disabled} onClick={() => onChange(!checked)} className={`relative inline-flex h-8 w-[3.25rem] shrink-0 items-center rounded-full border p-1 transition-colors duration-200 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--app-accent)] disabled:cursor-not-allowed disabled:opacity-50 ${checked ? 'border-[var(--app-accent)] bg-[var(--app-accent)]/20' : 'border-[var(--app-border)] bg-[var(--app-wash)]'}`}>
    <span className={`grid size-6 place-items-center rounded-full transition-transform duration-200 ${checked ? 'translate-x-[1.25rem] rtl:-translate-x-[1.25rem] bg-[var(--app-accent)] text-[var(--app-accent-ink)] shadow-sm' : 'bg-[var(--app-card)] text-[var(--app-muted)] shadow-sm'}`}>
      {loading ? <LoaderCircle className="size-3.5 animate-spin" /> : null}
    </span>
  </button>
}
