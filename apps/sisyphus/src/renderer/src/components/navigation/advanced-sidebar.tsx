import { useEffect, useRef, useState } from 'react'
import { Link, NavLink } from 'react-router-dom'
import { Boxes, ChevronLeft, Ellipsis, House, MonitorCog, Network, PanelLeftOpen, Server, Settings2 } from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import type { NodeSnapshot } from '../../../../preload'
import type { getMessages } from '@/i18n/messages'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { logoUrl } from '@/lib/brand'

type Messages = ReturnType<typeof getMessages>
type NavigationItem = { to: string; label: string; icon: LucideIcon; end?: boolean }

export function AdvancedSidebar({ snapshot, connectedPeers, statusLabel, messages }: {
  snapshot: NodeSnapshot
  connectedPeers: number
  statusLabel: string
  messages: Messages
}) {
  const [isOpen, setIsOpen] = useState(() => {
    try { return window.localStorage.getItem('sisyphus-advanced-sidebar-open') !== 'false' } catch { return true }
  })
  const [sidebarPhase, setSidebarPhase] = useState<'expanded' | 'expanding' | 'collapsing' | 'collapsed'>(() => {
    try { return window.localStorage.getItem('sisyphus-advanced-sidebar-open') === 'false' ? 'collapsed' : 'expanded' } catch { return 'expanded' }
  })
  const phaseTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  useEffect(() => {
    try { window.localStorage.setItem('sisyphus-advanced-sidebar-open', String(isOpen)) } catch { /* Keep this preference for the current session. */ }
  }, [isOpen])
  useEffect(() => () => { if (phaseTimer.current) clearTimeout(phaseTimer.current) }, [])
  // Hover expansion is opt-in in the source design; without that preference,
  // the collapsed state remains stable until the user explicitly toggles it.
  const isExpanded = isOpen
  const groups: Array<{ label: string; items: NavigationItem[] }> = [
    { label: messages.workspace, items: [{ to: '/', label: messages.workspace, icon: House, end: true }] },
    { label: messages.node, items: [{ to: '/node', label: messages.nodeOverview, icon: MonitorCog }, { to: '/operations', label: messages.operations, icon: Boxes }] },
    { label: messages.preferences, items: [{ to: '/settings', label: messages.settings, icon: Settings2 }] },
  ]
  const toggleOpen = () => {
    if (phaseTimer.current) clearTimeout(phaseTimer.current)
    if (isOpen && sidebarPhase === 'expanded') {
      setSidebarPhase('collapsing')
      setIsOpen(false)
    } else if (isOpen && (sidebarPhase === 'collapsing' || sidebarPhase === 'expanding')) {
      if (sidebarPhase === 'collapsing') setSidebarPhase('expanded')
      else {
        setIsOpen(false)
        setSidebarPhase('collapsed')
      }
    } else if (!isOpen) {
      setIsOpen(true)
      setSidebarPhase('expanding')
      phaseTimer.current = setTimeout(() => setSidebarPhase('expanded'), 210)
    }
  }
  const handleStatusTransitionEnd = (event: React.TransitionEvent<HTMLDivElement>) => {
    if (event.propertyName !== 'max-height' || sidebarPhase !== 'collapsing') return
    setSidebarPhase('collapsed')
  }
  const captureTogglePointer = (event: React.PointerEvent<HTMLButtonElement>) => {
    event.stopPropagation()
    event.currentTarget.setPointerCapture(event.pointerId)
  }
  const handleToggleClick = (event: React.MouseEvent<HTMLButtonElement>) => {
    event.preventDefault()
    event.stopPropagation()
    toggleOpen()
  }
  const dotClass = `advanced-sidebar__status-dot ${snapshot.status === 'connected' ? 'is-online' : snapshot.status === 'connecting' ? 'is-connecting' : ''}`
  const statusText = <>{messages.daemon}: {statusLabel}</>

  return <aside className="advanced-sidebar" data-open={isExpanded} data-phase={sidebarPhase} aria-label={messages.preferences}>
    <div className="advanced-sidebar__inner">
      <div className="advanced-sidebar__brand-row">
        {isExpanded ? <Link to="/" className="advanced-sidebar__brand" aria-label="Sisyphus">
          <img className="advanced-sidebar__logo" src={logoUrl} alt="" />
          <span className="advanced-sidebar__brand-name">Sisyphus</span>
        </Link> : <button type="button" className="advanced-sidebar__brand advanced-sidebar__brand--collapsed-trigger" onPointerDown={captureTogglePointer} onClick={handleToggleClick} aria-label={messages.preferences}>
          <img className="advanced-sidebar__logo" src={logoUrl} alt="" />
          <span className="advanced-sidebar__brand-name">Sisyphus</span>
          <PanelLeftOpen className="advanced-sidebar__expand-icon size-5" aria-hidden="true" />
        </button>}
        {isExpanded && <button type="button" className="advanced-sidebar__toggle" onPointerDown={captureTogglePointer} onClick={handleToggleClick} aria-label={messages.preferences} title={messages.preferences}><ChevronLeft className="size-4 rtl:rotate-180" /></button>}
      </div>
      <nav className="advanced-sidebar__nav" aria-label={messages.preferences}>
        {groups.map((group) => <section className="advanced-sidebar__group" key={group.label}><h2 className="advanced-sidebar__label">{group.label}</h2><div className="advanced-sidebar__collapsed-label"><Ellipsis className="size-5" /></div><ul className="advanced-sidebar__menu">
          {group.items.map(({ to, label, icon: Icon, end }) => { const item = <NavLink to={to} end={end} className={({ isActive }) => `advanced-sidebar__item ${isActive ? 'is-active' : ''}`}><span className="advanced-sidebar__item-icon"><Icon className="size-[18px]" /></span><span className="advanced-sidebar__item-label">{label}</span></NavLink>; return <li key={to}>{isOpen ? item : <Tooltip><TooltipTrigger render={item} /><TooltipContent side="right">{label}</TooltipContent></Tooltip>}</li> })}
        </ul></section>)}
      </nav>
      <div className="advanced-sidebar__status-wrap"><div className="advanced-sidebar__status" role="status" aria-label={`${messages.daemon}: ${statusLabel}`} onTransitionEnd={handleStatusTransitionEnd}><div className="advanced-sidebar__status-line"><span className={dotClass} /><span className="advanced-sidebar__status-text">{messages.daemon}</span><Server className="advanced-sidebar__daemon-icon" aria-hidden="true" /></div><div className="advanced-sidebar__peer-metric"><span className="advanced-sidebar__peer-label"><Network className="advanced-sidebar__peer-icon" aria-hidden="true" />{messages.peers}</span><strong dir="ltr" aria-label={`${messages.connectedPeers}: ${connectedPeers} / ${snapshot.peers.length}`}>{connectedPeers}<span> / {snapshot.peers.length}</span></strong></div></div><Tooltip><TooltipTrigger render={<div className="advanced-sidebar__collapsed-status"><span className={dotClass} /></div>} /><TooltipContent side="right">{statusText}</TooltipContent></Tooltip></div>
    </div>
  </aside>
}
