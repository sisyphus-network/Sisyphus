import { createContext, useCallback, useContext, useId, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { LoaderCircle, Save, Undo2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import type { SisyphusMessages } from '@/i18n/messages'
import { savePendingSettings, pendingChangeCount, type PendingSettingsSave } from '@/lib/settings-save'
import { toast } from 'sonner'

type PendingSave = PendingSettingsSave & { revert: () => void }
type Registry = { update: (id: string, entry: PendingSave | null) => void }
const RegistryContext = createContext<Registry | null>(null)
const DockContext = createContext<{ entries: Map<string, PendingSave>; saving: boolean; save: () => Promise<void>; revert: () => void } | null>(null)
const ScopeContext = createContext(false)

export function SettingsSaveProvider({ children, messages }: { children: ReactNode; messages: SisyphusMessages }) {
  const [entries, setEntries] = useState(() => new Map<string, PendingSave>())
  const [saving, setSaving] = useState(false)
  const running = useRef(false)
  const update = useCallback((id: string, entry: PendingSave | null) => {
    setEntries(current => {
      if (!entry && !current.has(id)) return current
      const next = new Map(current)
      if (entry) next.set(id, entry)
      else next.delete(id)
      return next
    })
  }, [])
  const registry = useMemo(() => ({ update }), [update])
  const save = async () => {
    if (running.current || !entries.size || [...entries.values()].some(entry => entry.disabled)) return
    running.current = true
    setSaving(true)
    try {
      // Each section reports its own failure and only clears its draft on success.
      // Serialize writes because these settings share the daemon configuration.
      const results = await savePendingSettings(entries.values())
      if (results.some(result => result.status === 'rejected')) toast.error(messages.settingsSaveDock.failed)
    } finally {
      running.current = false
      setSaving(false)
    }
  }
  const revert = () => {
    if (running.current) return
    for (const entry of entries.values()) entry.revert()
  }
  return <RegistryContext.Provider value={registry}><DockContext.Provider value={{ entries, saving, save, revert }}>{children}</DockContext.Provider></RegistryContext.Provider>
}

export function SettingsSaveScope({ children }: { children: ReactNode }) {
  const dock = useContext(DockContext)
  return <ScopeContext.Provider value><fieldset disabled={dock?.saving} className="m-0 min-w-0 border-0 p-0 pb-24">{children}</fieldset></ScopeContext.Provider>
}

// Sections can also be used outside the shared settings screen, where their
// ordinary save button remains available.
export function SettingsSaveAction({ dirty, changeCount = 1, disabled, onSave, onRevert, children }: { dirty: boolean; changeCount?: number; disabled: boolean; onSave: () => Promise<void>; onRevert: () => void; children: ReactNode }) {
  const scope = useContext(ScopeContext)
  const registry = useContext(RegistryContext)
  const id = useId()
  const callback = useRef(onSave)
  const revertCallback = useRef(onRevert)
  useLayoutEffect(() => { callback.current = onSave; revertCallback.current = onRevert })
  useLayoutEffect(() => {
    if (!scope || !registry) return
    registry.update(id, dirty ? { disabled, changeCount, getSave: () => callback.current, revert: () => revertCallback.current() } : null)
    return () => registry.update(id, null)
  }, [scope, registry, id, dirty, changeCount, disabled])
  if (scope && registry) return null
  return <Button type="button" disabled={disabled} onClick={() => void onSave()} className="gap-2">{children}</Button>
}

export function SettingsSaveDock({ compact, navigation, messages }: { compact: boolean; navigation: ReactNode; messages: SisyphusMessages }) {
  const dock = useContext(DockContext)
  const reduced = useReducedMotion()
  const dirty = Boolean(dock?.entries.size)
  const visible = dirty || Boolean(dock?.saving)
  const blocked = Boolean(dock?.saving || [...(dock?.entries.values() ?? [])].some(entry => entry.disabled))
  return createPortal(<div className={compact ? 'app-mobile-dock' : 'settings-save-desktop'}>
    <AnimatePresence initial={false} mode="wait">
      {visible ? <motion.div key="save" initial={{ opacity: 0, y: reduced ? 0 : 20, scale: reduced ? 1 : 0.97 }} animate={{ opacity: 1, y: 0, scale: 1 }} exit={{ opacity: 0, y: reduced ? 0 : 12, scale: reduced ? 1 : 0.97 }} transition={{ duration: reduced ? 0 : 0.22 }} className="settings-save-surface">
        <div className="flex items-center gap-2">
        <Button type="button" disabled={blocked} aria-busy={dock?.saving} onClick={() => void dock?.save()} className="h-12 min-w-0 flex-1 gap-2 rounded-full px-4 text-sm">
          {dock?.saving ? <LoaderCircle className="size-5 animate-spin" /> : <Save className="size-5" />}
          <span className="truncate">{dock?.saving ? messages.settingsSaveDock.saving : messages.settingsSaveDock.save}</span>
          {dirty && <span className="grid min-w-6 place-items-center rounded-full bg-current/10 px-1.5 text-xs tabular-nums">{pendingChangeCount(dock?.entries.values() ?? [])}</span>}
        </Button>
        <Button type="button" variant="ghost" size="icon" disabled={dock?.saving} onClick={() => dock?.revert()} aria-label={messages.settingsSaveDock.revert} title={messages.settingsSaveDock.revert} className="size-12 shrink-0 rounded-full bg-[var(--app-wash)]"><Undo2 aria-hidden="true" className="size-5 rtl:rotate-y-180" /></Button>
        </div>
      </motion.div> : compact ? <motion.nav key="navigation" aria-label={messages.preferences} initial={{ opacity: 0, y: reduced ? 0 : 8 }} animate={{ opacity: 1, y: 0 }} exit={{ opacity: 0, y: reduced ? 0 : 8 }} transition={{ duration: reduced ? 0 : 0.18 }} className="pointer-events-auto w-full max-w-[280px]">{navigation}</motion.nav> : null}
    </AnimatePresence>
  </div>, document.body)
}
