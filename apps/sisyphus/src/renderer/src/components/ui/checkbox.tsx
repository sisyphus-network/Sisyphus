import { Checkbox as CheckboxPrimitive } from '@base-ui/react/checkbox'
import { Check } from 'lucide-react'
import { cn } from '@/lib/utils'

function Checkbox({ className, ...props }: CheckboxPrimitive.Root.Props) {
  return <CheckboxPrimitive.Root data-slot="checkbox" className={cn('inline-flex size-4 shrink-0 items-center justify-center rounded-[4px] border border-[var(--app-border)] bg-[var(--app-card)] text-[var(--app-accent-ink)] outline-none transition-colors focus-visible:ring-2 focus-visible:ring-[var(--app-accent)]/40 data-[checked]:border-[var(--app-accent)] data-[checked]:bg-[var(--app-accent)] disabled:cursor-not-allowed disabled:opacity-50', className)} {...props}>
    <CheckboxPrimitive.Indicator className="grid place-items-center"><Check className="size-3" strokeWidth={3} /></CheckboxPrimitive.Indicator>
  </CheckboxPrimitive.Root>
}

export { Checkbox }
