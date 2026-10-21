import { LoaderCircle } from 'lucide-react'

export function LiquidSwitch({ checked, loading = false, onChange, label, disabled = false, id, describedBy }: {
  checked: boolean
  loading?: boolean
  onChange: (checked: boolean) => void
  label: string
  disabled?: boolean
  id?: string
  describedBy?: string
}) {
  return <button
    type="button"
    role="switch"
    id={id}
    aria-label={label}
    aria-describedby={describedBy}
    aria-checked={checked}
    aria-busy={loading}
    data-checked={checked ? 'true' : 'false'}
    disabled={loading || disabled}
    onClick={() => onChange(!checked)}
    className="startly-liquid-switch relative inline-flex h-8 w-[3.25rem] shrink-0 items-center rounded-full border p-1 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--app-accent)] disabled:pointer-events-none disabled:opacity-45"
  >
    <span className={`startly-liquid-switch__thumb-wrap grid size-6 place-items-center transition-transform duration-200 ${checked ? 'translate-x-[1.25rem] rtl:-translate-x-[1.25rem]' : ''}`}>
      <span className={`startly-liquid-switch__thumb grid size-full place-items-center rounded-full transition-[background-color,box-shadow] duration-200 ${checked ? 'bg-[var(--app-accent-ink)]' : ''}`}>
        {loading ? <LoaderCircle className="size-3.5 animate-spin text-[var(--app-muted)]" /> : null}
      </span>
    </span>
  </button>
}
