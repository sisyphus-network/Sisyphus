import { useEffect, useRef, useState } from 'react'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { X } from 'lucide-react'
import { FloatingLabelInput } from '@/components/ui/floating-label-input'
import { Button } from '@/components/ui/button'
import type { SisyphusMessages } from '@/i18n/messages'
import { HeaderTeleport, useHeaderSearch, useHeaderSearchReady } from '@/components/ui/header-teleport'

export function NodeSettingsSearchInput({ value, onChange, onClear, messages, mode = 'node' }: {
  value: string
  onChange: (value: string) => void
  onClear: () => void
  messages: SisyphusMessages
  mode?: 'node' | 'models'
}) {
  const input = useRef<HTMLInputElement>(null)
  const { open, setOpen } = useHeaderSearch()
  const ready = useHeaderSearchReady()
  const reduced = useReducedMotion()
  const [focused, setFocused] = useState(false)
  const [example, setExample] = useState(0)
  const labels = mode === 'node' ? messages.nodeSettingsSearchExamples : { label: messages.modelSettings.searchProfiles, examples: [messages.modelSettings.searchProfilesPlaceholder] }
  const inputId = mode === 'node' ? 'node-settings-header-search' : 'model-settings-header-search'
  const triggerId = mode === 'node' ? 'node-settings-search-trigger' : 'model-settings-search-trigger'
  const direction = document.documentElement.dir === 'rtl' ? 'rtl' : 'ltr'
  useEffect(() => {
    if (open && ready) input.current?.focus({ preventScroll: true })
  }, [open, ready])
  useEffect(() => {
    if (reduced || focused || value || labels.examples.length < 2) return
    const timer = window.setInterval(() => setExample((current) => (current + 1) % labels.examples.length), 4000)
    return () => window.clearInterval(timer)
  }, [reduced, focused, value, labels.examples.length])

  return <HeaderTeleport active={open} headerOnly><div className="node-search-field relative mb-6 pt-2" onBlurCapture={(event) => { if (!value && !event.currentTarget.contains(event.relatedTarget)) setOpen(false) }}>
    <FloatingLabelInput ref={input} id={inputId} type="search" label={labels.label} value={value} onChange={(event) => onChange(event.target.value)}
      onKeyDown={(event) => { if (event.key === 'Escape') { event.preventDefault(); if (value) { onClear(); return } setOpen(false); document.getElementById(triggerId)?.focus({ preventScroll: true }) } }}
      onFocus={() => setFocused(true)} onBlur={() => setFocused(false)} autoComplete="off" spellCheck={false} dir={direction} uiDirection={direction} placeholder="" labelClassName="bg-transparent"
      className="h-16 rounded-[1.25rem] border-0 bg-transparent text-base shadow-none focus:border-0 focus:bg-transparent focus:shadow-none [&::-webkit-search-cancel-button]:appearance-none" />
    {!value && focused && <div aria-hidden="true" className="pointer-events-none absolute top-2 start-4 end-12 flex h-16 min-w-0 items-center overflow-hidden">
      <AnimatePresence initial={false} mode="wait"><motion.span key={`${labels.label}-${example}`} initial={{ opacity: reduced ? 1 : 0, y: reduced ? 0 : 6 }} animate={{ opacity: 1, y: 0 }} exit={{ opacity: reduced ? 1 : 0, y: reduced ? 0 : -6 }} transition={{ duration: reduced ? 0 : 0.2 }} className="block truncate text-sm text-[var(--app-placeholder)]">{labels.examples[example % labels.examples.length]}</motion.span></AnimatePresence>
    </div>}
    <AnimatePresence initial={false}>{value && <motion.div initial={{ opacity: 0, scale: reduced ? 1 : 0.8 }} animate={{ opacity: 1, scale: 1 }} exit={{ opacity: 0, scale: reduced ? 1 : 0.8 }} transition={{ duration: reduced ? 0 : 0.15 }} className="absolute end-3 top-6 z-20">
      <Button type="button" variant="ghost" size="icon" className="size-8 rounded-full bg-[var(--app-wash)] text-muted-foreground hover:text-[var(--app-accent)]" aria-label={messages.nodeSettingsSearch.clear} onClick={() => { onClear(); input.current?.focus() }}><X className="size-3.5" /></Button>
    </motion.div>}</AnimatePresence>
  </div></HeaderTeleport>
}
