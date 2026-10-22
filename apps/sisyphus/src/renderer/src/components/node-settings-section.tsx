import { useContext, type ReactNode } from 'react'
import type { LucideIcon } from 'lucide-react'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { NodeSettingsSearchContext, useSettingsSearchExpansion } from '@/components/node-settings-search-context'

export function NodeSettingsSection({ title, description, icon, action, children }: {
  title: string
  description: string
  icon: LucideIcon
  action?: ReactNode
  children: ReactNode
}) {
  const search = useContext(NodeSettingsSearchContext)
  const expansion = useSettingsSearchExpansion(search)
  return <Collapsible {...expansion} className="node-settings-section min-w-0 rounded-2xl border border-[var(--app-line)] bg-[var(--app-card)] p-1">
    <div className="flex min-w-0 items-center gap-2">
      <CollapsibleTrigger title={title} icon={icon} className="min-w-0 flex-1 px-3 py-3 sm:px-4 sm:py-4" />
      {action && <div className="shrink-0 pe-3 sm:pe-4">{action}</div>}
    </div>
    <CollapsibleContent animateSpacing={false}><div className="min-w-0 px-3 pb-4 pt-2 sm:px-4 sm:pb-5"><p className="mb-4 text-sm leading-relaxed text-muted-foreground">{description}</p>{children}</div></CollapsibleContent>
  </Collapsible>
}
