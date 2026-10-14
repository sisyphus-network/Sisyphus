import { OTPInput, OTPInputContext, REGEXP_ONLY_DIGITS, type OTPInputProps } from 'input-otp'
import { useContext } from 'react'
import { cn } from '@/lib/utils'

function InputOTP({ className, containerClassName, ...props }: OTPInputProps) {
  return <OTPInput data-slot="input-otp" containerClassName={cn('flex items-center gap-2 has-disabled:opacity-50', containerClassName)} className={cn('disabled:cursor-not-allowed', className)} pattern={REGEXP_ONLY_DIGITS} {...props} />
}

function InputOTPSlot({ index, className }: { index: number; className?: string }) {
  const context = useContext(OTPInputContext)
  const slot = context.slots[index]
  return <div data-slot="input-otp-slot" data-active={slot.isActive} className={cn('relative grid size-7 place-items-center rounded-lg border border-[var(--app-line)] bg-background text-xs font-semibold transition-colors data-[active=true]:border-ring data-[active=true]:ring-3 data-[active=true]:ring-ring/30 sm:size-8 sm:rounded-xl sm:text-sm', className)}>
    {slot.char}{slot.hasFakeCaret && <span className="pointer-events-none absolute h-4 w-px animate-pulse bg-foreground" />}
  </div>
}

export { InputOTP, InputOTPSlot }
