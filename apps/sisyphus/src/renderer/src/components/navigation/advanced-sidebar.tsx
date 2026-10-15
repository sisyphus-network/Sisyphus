import { useEffect, useState } from 'react'
import { Link, NavLink } from 'react-router-dom'
import { Activity, Boxes, ChevronLeft, Ellipsis, House, MonitorCog, PanelLeftOpen, Settings2 } from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import type { NodeSnapshot } from '../../../../preload'
import type { getMessages } from '@/i18n/messages'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'

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
  useEffect(() => {
    try { window.localStorage.setItem('sisyphus-advanced-sidebar-open', String(isOpen)) } catch { /* Keep this preference for the current session. */ }
  }, [isOpen])
  const groups: Array<{ label: string; items: NavigationItem[] }> = [
    { label: messages.workspace, items: [{ to: '/', label: messages.workspace, icon: House, end: true }] },
    { label: messages.node, items: [{ to: '/node', label: messages.nodeOverview, icon: MonitorCog }, { to: '/operations', label: messages.operations, icon: Boxes }] },
    { label: messages.preferences, items: [{ to: '/settings', label: messages.settings, icon: Settings2 }] },
  ]
  const toggleOpen = () => setIsOpen((current) => !current)
  const dotClass = `advanced-sidebar__status-dot ${snapshot.status === 'connected' ? 'is-online' : snapshot.status === 'connecting' ? 'is-connecting' : ''}`
  const statusText = <>{messages.localDaemon}: {statusLabel}</>

  return <aside className="advanced-sidebar" data-open={isOpen} aria-label={messages.preferences}>
    <div className="advanced-sidebar__inner">
      <div className="advanced-sidebar__brand-row">
        {isOpen ? <Link to="/" className="advanced-sidebar__brand" aria-label="Sisyphus"><img className="advanced-sidebar__logo" src="/logo.png" alt="" /><span className="advanced-sidebar__brand-name">Sisyphus</span></Link> : <button type="button" className="advanced-sidebar__brand advanced-sidebar__brand--collapsed-trigger" onClick={toggleOpen} aria-label={messages.preferences}><img className="advanced-sidebar__logo" src="/logo.png" alt="" /><span className="advanced-sidebar__brand-name">Sisyphus</span><PanelLeftOpen className="advanced-sidebar__expand-icon size-5" aria-hidden="true" /></button>}
        {isOpen && <button type="button" className="advanced-sidebar__toggle" onClick={toggleOpen} aria-label={messages.preferences} title={messages.preferences}><ChevronLeft className="size-4 rtl:rotate-180" /></button>}
      </div>
      <nav className="advanced-sidebar__nav" aria-label={messages.preferences}>
        {groups.map((group) => <section className="advanced-sidebar__group" key={group.label}><h2 className="advanced-sidebar__label">{group.label}</h2><div className="advanced-sidebar__collapsed-label"><Ellipsis className="size-5" /></div><ul className="advanced-sidebar__menu">
          {group.items.map(({ to, label, icon: Icon, end }) => { const item = <NavLink to={to} end={end} className={({ isActive }) => `advanced-sidebar__item ${isActive ? 'is-active' : ''}`}><span className="advanced-sidebar__item-icon"><Icon className="size-[18px]" /></span><span className="advanced-sidebar__item-label">{label}</span></NavLink>; return <li key={to}>{isOpen ? item : <Tooltip><TooltipTrigger render={item} /><TooltipContent side="right">{label}</TooltipContent></Tooltip>}</li> })}
        </ul></section>)}
      </nav>
      <div className="advanced-sidebar__status-wrap"><div className="advanced-sidebar__status"><div className="advanced-sidebar__status-line"><span className={dotClass} /><span className="advanced-sidebar__status-text">{statusText}</span><Activity className="size-4 shrink-0 text-[var(--app-accent)]" /></div><div className="advanced-sidebar__endpoint" dir="ltr">{snapshot.endpoint}</div><div className="advanced-sidebar__peer-metric"><span>{messages.connectedPeers}</span><strong>{connectedPeers}<span> / {snapshot.peers.length}</span></strong></div></div><Tooltip><TooltipTrigger render={<div className="advanced-sidebar__collapsed-status"><span className={dotClass} /></div>} /><TooltipContent side="right">{statusText}</TooltipContent></Tooltip></div>
    </div>
  </aside>
}
