import { memo, useDeferredValue, useEffect, useMemo, useState, type ReactNode } from 'react'
import { motion, useReducedMotion } from 'motion/react'
import { Cpu, Globe2, Network, Search, type LucideIcon } from 'lucide-react'
import { getMessages, formatMessage, type SisyphusMessages } from '@/i18n/messages'
import { supportedLocales, type AppLocale } from '@/i18n/locales'
import { normalizeSettingsSearch } from '@/lib/settings-search'
import { useStickyState } from '@/hooks/use-sticky-state'
import { NodeSettingsSearchContext, useSettingsSearchExpansion } from '@/components/node-settings-search-context'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { Button } from '@/components/ui/button'
import { SettingsSaveScope } from '@/components/ui/settings-save-dock'
import { EmptyState } from '@/components/ui/page-layout'
import { NodeSettingsSearchInput } from '@/components/node-settings-search-input'
import { PoolHostingSettingsCard } from '@/components/pool-hosting-settings'
import { ContainerSettingsCard } from '@/components/container-settings'
import { NodeLocationSettingsCard } from '@/components/node-location-settings'
import { LocationDiscoverySettingsCard } from '@/components/location-discovery-settings'
import { CityTableSettingsCard } from '@/components/city-table-settings'
import { PoolLimitsSettingsCard } from '@/components/pool-limits-settings'
import { ModelLendingSettings } from '@/components/model-lending-settings'

type Group = keyof SisyphusMessages['nodeSettingsGroups']
const sections = [
  { id: 'poolHostingSettings', group: 'network', aliases: 'interface IP TCP LAN port P2P pool NAT' },
  { id: 'nodeLocationSettings', group: 'location', aliases: 'latitude longitude coordinates location privacy' },
  { id: 'locationDiscoverySettings', group: 'location', aliases: 'STUN NAT-PMP UPnP public IP gateway router' },
  { id: 'cityTableSettings', group: 'location', aliases: 'GeoIP DB-IP city database geography' },
  { id: 'containerSettings', group: 'resources', aliases: 'Docker container sandbox network filesystem temporary storage' },
  { id: 'poolLimitsSettings', group: 'resources', aliases: 'quota limits tasks jobs timeout memory storage upload' },
  { id: 'modelSettings', group: 'resources', aliases: 'Ollama lending inference GPU CPU VRAM threads context keep alive' },
] as const

function strings(value: unknown): string[] {
  if (typeof value === 'string') return [value]
  if (value && typeof value === 'object') return Object.values(value).flatMap(strings)
  return []
}

// Search every translation, independently of the currently displayed locale.
const index = sections.map((section) => ({ ...section, terms: [section.aliases, ...supportedLocales.flatMap((locale) => {
  const messages = getMessages(locale)
  const labels = messages[section.id]
  const searchable = section.id === 'modelSettings'
    ? Object.fromEntries(Object.entries(labels).filter(([key]) => /lending|ollama|gpu|threads|context|keepAlive|loadedModels|modelsOffered/i.test(key)))
    : labels
  return [messages.nodeSettingsGroups[section.group], ...strings(searchable)]
})] })).map((section) => ({ ...section, text: normalizeSettingsSearch(section.terms.join(' ')) }))

function FilteredSection({ visible, children }: { visible: boolean; children: ReactNode }) {
  const reduced = useReducedMotion()
  return <motion.div initial={false} animate={{ height: visible ? 'auto' : 0, opacity: visible ? 1 : 0 }} transition={{ duration: reduced ? 0 : 0.24, ease: 'easeInOut' }} aria-hidden={!visible} inert={!visible} className="min-w-0 overflow-clip">
    <div className="pb-4">{children}</div>
  </motion.div>
}

function SettingsGroup({ title, icon, query, defaultOpen, children }: { title: string; icon: LucideIcon; query: string; defaultOpen?: boolean; children: ReactNode }) {
  const expansion = useSettingsSearchExpansion(query, defaultOpen)
  const { sentinelRef, stuck } = useStickyState()
  return <Collapsible {...expansion} className="rounded-2xl border border-[var(--app-line)] bg-[var(--app-card)] p-1">
    <span ref={sentinelRef} aria-hidden="true" className="pointer-events-none block h-px -mb-px" />
    <CollapsibleTrigger title={title} icon={icon} className={`node-settings-group-heading px-3 py-3 sm:px-4 sm:py-4${stuck ? ' is-stuck' : ''}`} />
    <CollapsibleContent animateSpacing={false}><div className="min-w-0 px-2 pt-2 sm:px-4">{children}</div></CollapsibleContent>
  </Collapsible>
}

const SettingsResults = memo(function SettingsResults({ messages, locale, search, onClear }: { messages: SisyphusMessages; locale: AppLocale; search: string; onClear: () => void }) {
  const tokens = search.split(/\s+/).filter(Boolean)
  const matched = new Set(index.filter((section) => tokens.every((token) => section.text.includes(token))).map((section) => section.id))
  // Keep disclosure contexts stable while the result set stays the same.
  const expansionKey = search ? [...matched].join('|') : ''
  const labels = messages.nodeSettingsSearch
  const views = useMemo(() => ({
    poolHostingSettings: <PoolHostingSettingsCard messages={messages} />,
    nodeLocationSettings: <NodeLocationSettingsCard messages={messages} />,
    locationDiscoverySettings: <LocationDiscoverySettingsCard messages={messages} />,
    cityTableSettings: <CityTableSettingsCard messages={messages} locale={locale} />,
    containerSettings: <ContainerSettingsCard messages={messages} />,
    poolLimitsSettings: <PoolLimitsSettingsCard messages={messages} />,
    modelSettings: <ModelLendingSettings messages={messages} />,
  }), [messages, locale])

  const groups: { id: Group; icon: LucideIcon }[] = [{ id: 'network', icon: Network }, { id: 'location', icon: Globe2 }, { id: 'resources', icon: Cpu }]

  return <div>
    <p role="status" aria-live="polite" aria-atomic="true" className={search ? 'mb-4 px-1 text-xs text-muted-foreground' : 'sr-only'}>{search ? formatMessage(labels.results, { count: matched.size }) : ''}</p>
    {groups.map((group) => <FilteredSection key={group.id} visible={sections.some((section) => section.group === group.id && matched.has(section.id))}>
      <SettingsGroup title={messages.nodeSettingsGroups[group.id]} icon={group.icon} query={expansionKey} defaultOpen={group.id === 'network'}>
        {sections.filter((section) => section.group === group.id).map((section) => <FilteredSection key={section.id} visible={matched.has(section.id)}><NodeSettingsSearchContext.Provider value={matched.has(section.id) ? expansionKey : ''}>{views[section.id]}</NodeSettingsSearchContext.Provider></FilteredSection>)}
      </SettingsGroup>
    </FilteredSection>)}
    <FilteredSection visible={matched.size === 0}><EmptyState icon={Search} title={labels.empty} description={labels.hint} action={<Button variant="outline" onClick={onClear}>{labels.clear}</Button>} /></FilteredSection>
  </div>
})

export function NodeSettingsPanel({ messages, locale }: { messages: SisyphusMessages; locale: AppLocale }) {
  const [query, setQuery] = useState('')
  const [filterQuery, setFilterQuery] = useState('')
  useEffect(() => {
    const timer = window.setTimeout(() => setFilterQuery(query), 120)
    return () => window.clearTimeout(timer)
  }, [query])
  const deferred = useDeferredValue(filterQuery)
  const search = normalizeSettingsSearch(deferred)
  const clear = useMemo(() => () => { setQuery(''); setFilterQuery('') }, [])
  return <div className="node-settings-panel min-w-0">
    <NodeSettingsSearchInput value={query} onChange={setQuery} onClear={clear} messages={messages} />
    <SettingsSaveScope><SettingsResults messages={messages} locale={locale} search={search} onClear={clear} /></SettingsSaveScope>
  </div>
}
