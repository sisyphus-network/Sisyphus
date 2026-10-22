import type { ReactNode } from 'react'
import { Check, ListFilter, type LucideIcon } from 'lucide-react'
import { FloatingLabelInput } from '@/components/ui/floating-label-input'
import { ResponsiveSelector } from '@/components/ui/responsive-selector'
import { SettingsIcon } from '@/components/ui/settings-icon'

export function SettingsSelector({ id, label, ariaLabel, value, options, onChange, disabled, icon, className, technical = false }: {
  id?: string
  label: string
  ariaLabel?: string
  value: string
  options: { value: string; label: string; description?: string; icon?: LucideIcon }[]
  onChange: (value: string) => void
  disabled?: boolean
  icon?: ReactNode
  className?: string
  technical?: boolean
}) {
  const direction = document.documentElement.dir === 'rtl' ? 'rtl' : 'ltr'
  const selected = options.find((option) => option.value === value)
  const displayValue = selected ? [selected.label, selected.description].filter(Boolean).join(' · ') : value
  return <ResponsiveSelector label={label} direction={direction} className={className}
    trigger={({ open, setOpen }) => <FloatingLabelInput id={id} label={label} aria-label={ariaLabel ?? label} title={ariaLabel ?? label} icon={selected?.icon ? <SettingsIcon icon={selected.icon} compact /> : icon ?? <SettingsIcon icon={ListFilter} compact />} value={displayValue} readOnly role="button" disabled={disabled} uiDirection={direction} dir={technical ? 'ltr' : direction} aria-haspopup="dialog" aria-expanded={open} onClick={() => setOpen((current) => !current)} className="cursor-pointer text-ellipsis" />}>
    {({ setOpen }) => <div className="max-h-72 space-y-1 overflow-y-auto" dir={technical ? 'ltr' : direction}>
      {options.map((option) => <button key={option.value} type="button" aria-pressed={option.value === value} onClick={() => { onChange(option.value); setOpen(false) }} className={`flex w-full items-center gap-3 rounded-xl px-3 py-3 text-start text-sm transition-colors hover:bg-[var(--app-wash)] focus-visible:outline-2 focus-visible:outline-[var(--app-accent)] ${option.value === value ? 'bg-[var(--app-wash)] font-medium' : ''}`}>
        {option.icon ? <SettingsIcon icon={option.icon} compact /> : icon ?? <SettingsIcon icon={ListFilter} compact />}
        <span className="min-w-0 flex-1"><bdi className="block break-words">{option.label}</bdi>{option.description && <bdi className="mt-1 block break-words text-xs font-normal text-muted-foreground">{option.description}</bdi>}</span>
        <span className="grid size-5 shrink-0 place-items-center">{option.value === value && <Check aria-hidden="true" className="size-4 text-[var(--app-accent)]" />}</span>
      </button>)}
    </div>}
  </ResponsiveSelector>
}
