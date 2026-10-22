import { Skeleton } from '@/components/ui/skeleton'

// Mirrors the icon, copy and control columns of a settings row, so loading
// stays inside the disclosure instead of changing the page's whole layout.
export function SettingsSectionSkeleton({ label, rows = 2, controls = true }: { label: string; rows?: number; controls?: boolean }) {
  return <div role="status" aria-label={label} aria-busy="true" className="min-w-0" >
    {Array.from({ length: rows }, (_, index) => <div key={index} aria-hidden="true" className="settings-row settings-row--divider flex-wrap">
      <div className="flex min-w-0 flex-1 items-start gap-3">
        <Skeleton className="size-10 shrink-0 rounded-xl" />
        <div className="grid min-w-0 flex-1 gap-2 py-1"><Skeleton className="h-4 w-32 max-w-[70%]" /><Skeleton className="h-3 w-64 max-w-[85%]" /></div>
      </div>
      {controls && <Skeleton className="h-14 w-full shrink-0 rounded-2xl sm:w-48" />}
    </div>)}
    <div aria-hidden="true" className="mt-4 flex justify-end border-t border-[var(--app-line)] pt-4"><Skeleton className="h-9 w-28 rounded-xl" /></div>
  </div>
}
