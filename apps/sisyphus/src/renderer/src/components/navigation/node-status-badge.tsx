import type { LucideIcon } from 'lucide-react'
import type { NodeSnapshot } from '../../../../preload'
import { Badge } from '@/components/ui/badge'

export function NodeStatusBadge({ status, label, icon: Icon }: {
  status: NodeSnapshot['status']
  label: string
  icon: LucideIcon
}) {
  const stateClass = status === 'connected'
    ? 'border-emerald-500/30 bg-emerald-500/5 text-emerald-700 dark:text-emerald-300'
    : status === 'connecting'
      ? 'border-amber-500/30 bg-amber-500/5 text-amber-700 dark:text-amber-300'
      : 'border-red-500/30 bg-red-500/5 text-red-700 dark:text-red-300'

  return <Badge variant="outline" className={`h-7 gap-1.5 rounded-full px-2.5 text-[10px] font-medium ${stateClass}`}><Icon aria-hidden="true" />{label}</Badge>
}
