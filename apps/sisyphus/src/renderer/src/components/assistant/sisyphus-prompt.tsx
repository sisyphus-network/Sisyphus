import { useLayoutEffect, useRef, useState, type ChangeEvent, type ClipboardEvent } from 'react'
import { ArrowUp, File, Plus, X } from 'lucide-react'
import { AnimatePresence, motion } from 'motion/react'
import { Button } from '@/components/ui/button'

export type ChatAttachment = { id: string; file: File; previewUrl: string }

export function SisyphusPrompt({
  direction, value, onValueChange, onSubmit, placeholder, sendLabel, addAttachmentLabel,
  removeAttachmentLabel, attachments, onFilesPicked, onAttachmentRemove, disabled = false,
  closePreviewLabel,
}: {
  direction: 'ltr' | 'rtl'
  value: string
  onValueChange: (value: string) => void
  onSubmit: () => void
  placeholder: string
  sendLabel: string
  addAttachmentLabel: string
  removeAttachmentLabel: string
  attachments: ChatAttachment[]
  onFilesPicked: (files: File[]) => void
  onAttachmentRemove: (id: string) => void
  disabled?: boolean
  closePreviewLabel: string
}) {
  const inputRef = useRef<HTMLInputElement>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const [preview, setPreview] = useState<ChatAttachment | null>(null)
  const canSubmit = !disabled && (value.trim().length > 0 || attachments.length > 0)

  function resizeTextarea() {
    const textarea = textareaRef.current
    if (!textarea) return
    textarea.style.height = 'auto'
    textarea.style.height = `${Math.max(36, Math.min(textarea.scrollHeight, 144))}px`
    textarea.style.overflowY = textarea.scrollHeight > 144 ? 'auto' : 'hidden'
  }

  useLayoutEffect(resizeTextarea, [value])

  function picked(event: ChangeEvent<HTMLInputElement>) {
    const files = Array.from(event.target.files ?? [])
    if (files.length) onFilesPicked(files)
    event.target.value = ''
  }

  function pasted(event: ClipboardEvent<HTMLFormElement>) {
    const files = Array.from(event.clipboardData.files)
    const clipboardFiles = files.length ? files : Array.from(event.clipboardData.items)
      .filter((item) => item.kind === 'file')
      .map((item) => item.getAsFile())
      .filter((file): file is File => file !== null)
    if (clipboardFiles.length) onFilesPicked(clipboardFiles)
  }

  return <>
    <form onSubmit={(event) => { event.preventDefault(); if (canSubmit) onSubmit() }} onPaste={pasted} className="sisyphus-composer pointer-events-auto relative z-10 mx-auto w-full min-w-0 overflow-hidden rounded-[24px] border border-[var(--app-line)] bg-[color-mix(in_srgb,var(--app-card)_95%,transparent)] p-2.5 shadow-sm backdrop-blur">
      <input ref={inputRef} type="file" multiple className="hidden" onChange={picked} />
      <div className="flex flex-col gap-2.5">
        {attachments.length > 0 && <div dir={direction} className="h-9 max-h-9 w-full min-w-0 overflow-x-auto overflow-y-hidden overscroll-x-contain px-1 pb-1 touch-pan-x [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"><div className="flex w-max flex-nowrap items-center gap-2">
          <AnimatePresence initial={false} mode="popLayout">
            {attachments.map((attachment) => <motion.div key={attachment.id} layout initial={{ opacity: 0, scale: .86 }} animate={{ opacity: 1, scale: 1 }} exit={{ opacity: 0, scale: .86, width: 0 }} transition={{ layout: { type: 'spring', stiffness: 520, damping: 38 }, opacity: { duration: .16 }, scale: { duration: .18 } }} className="group inline-flex h-8 max-w-56 shrink-0 items-center overflow-hidden rounded-full border border-[var(--app-line)] bg-[var(--app-wash)]/60 text-sm text-foreground shadow-sm">
              <button type="button" onClick={() => attachment.file.type.startsWith('image/') && setPreview(attachment)} className="inline-flex min-w-0 items-center" aria-label={attachment.file.type.startsWith('image/') ? `${attachment.file.name} preview` : attachment.file.name}>
                <span className="ms-2.5 flex size-5 shrink-0 items-center justify-center overflow-hidden rounded-full bg-[var(--app-card)]">{attachment.file.type.startsWith('image/') ? <img src={attachment.previewUrl} alt="" className="size-full object-cover" /> : <File className="size-3 text-muted-foreground" />}</span>
                <span className="min-w-0 max-w-52 truncate px-2">{attachment.file.name}</span>
              </button>
              <button type="button" onClick={() => onAttachmentRemove(attachment.id)} aria-label={`${removeAttachmentLabel}: ${attachment.file.name}`} className="me-1 grid size-5 shrink-0 place-items-center rounded-full text-muted-foreground hover:bg-[var(--app-card)] hover:text-foreground"><X size={12} /></button>
            </motion.div>)}
          </AnimatePresence>
        </div></div>}
        <div dir={direction} className="flex min-w-0 items-end gap-2">
          <Button type="button" disabled={disabled} variant="ghost" size="icon" onClick={() => inputRef.current?.click()} aria-label={addAttachmentLabel} className="size-9 shrink-0 self-center rounded-full text-muted-foreground hover:bg-[var(--app-wash)] hover:text-foreground"><Plus className="size-6" strokeWidth={1.8} /></Button>
          <textarea ref={textareaRef} dir={value ? 'auto' : direction} value={value} onChange={(event) => { onValueChange(event.target.value); requestAnimationFrame(resizeTextarea) }} onKeyDown={(event) => { if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing && canSubmit) { event.preventDefault(); onSubmit() } }} rows={1} placeholder={placeholder} aria-label={placeholder} disabled={disabled} className="min-h-9 max-h-36 min-w-0 flex-1 resize-none overflow-y-hidden bg-transparent px-1 py-1 text-start text-[15px] leading-7 text-foreground outline-none placeholder:text-muted-foreground transition-[height] duration-150" />
          <Button type="submit" disabled={!canSubmit} aria-label={sendLabel} className="size-9 shrink-0 rounded-full bg-[var(--app-accent)] text-[var(--app-accent-ink)] hover:opacity-90"><ArrowUp size={17} /></Button>
        </div>
      </div>
    </form>
    <AnimatePresence>
      {preview && <motion.div className="fixed inset-0 z-[100] flex h-dvh w-screen items-center justify-center overflow-hidden bg-black/70 px-4 py-5 backdrop-blur-sm" initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }} onClick={() => setPreview(null)} role="dialog" aria-modal="true" aria-label={preview.file.name}>
        <button type="button" onClick={() => setPreview(null)} className="fixed end-5 top-5 z-[110] grid size-10 place-items-center rounded-full bg-black/30 text-white/80 backdrop-blur-sm hover:bg-black/50" aria-label={closePreviewLabel}><X size={16} /></button>
        <motion.div initial={{ opacity: 0, scale: .975, y: 14, filter: 'blur(6px)' }} animate={{ opacity: 1, scale: 1, y: 0, filter: 'blur(0px)' }} transition={{ duration: .34, ease: [.22, 1, .36, 1] }} onClick={(event) => event.stopPropagation()} className="flex max-h-[82dvh] w-full max-w-5xl justify-center overflow-hidden rounded-2xl">
          <img src={preview.previewUrl} alt={preview.file.name} className="max-h-[82dvh] max-w-full rounded-2xl object-contain" />
        </motion.div>
      </motion.div>}
    </AnimatePresence>
  </>
}
