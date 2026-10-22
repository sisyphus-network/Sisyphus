import { useEffect, useRef, useState, type DragEvent } from 'react'
import { CloudUpload, Download, FileArchive, FileImage, FileMusic, FileSpreadsheet, FileText, FileVideo, LoaderCircle, RefreshCw, Trash2, Upload, X } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { prettyBytes } from '@/components/job-result-view'
import type { getMessages } from '@/i18n/messages'
import { formatMessage } from '@/i18n/messages'
import { fileUploadLimitLabel } from '../../../shared/file-upload-limit.ts'

type Messages = ReturnType<typeof getMessages>
type StoredFile = { cid: string; name: string; sizeBytes: string | number; private: boolean; storedAtMs: string | number }
type UploadAttempt = { id: string; file: File; preview?: string; status: 'uploading' | 'error'; error?: string }

function fileIcon(name: string) {
  if (/\.(png|jpe?g|gif|webp|avif|svg)$/i.test(name)) return FileImage
  if (/\.(mp4|mkv|mov|webm|avi)$/i.test(name)) return FileVideo
  if (/\.(mp3|wav|flac|ogg|m4a)$/i.test(name)) return FileMusic
  if (/\.(zip|tar|gz|rar|7z)$/i.test(name)) return FileArchive
  if (/\.(csv|xlsx?|ods)$/i.test(name)) return FileSpreadsheet
  return FileText
}

export function OperationsFileUpload({ files, privateFile, busy, messages, onPrivateChange, onUpload, onDownload, onRemove }: {
  files: StoredFile[]
  privateFile: boolean
  busy: boolean
  messages: Messages
  onPrivateChange: (value: boolean) => void
  onUpload: (file: File) => Promise<void>
  onDownload: (file: StoredFile) => void
  onRemove: (cid: string) => void
}) {
  const input = useRef<HTMLInputElement>(null)
  const nextAttemptId = useRef(0)
  const previews = useRef(new Set<string>())
  const processing = useRef(false)
  const [dragging, setDragging] = useState(false)
  const [attempts, setAttempts] = useState<UploadAttempt[]>([])

  useEffect(() => () => {
    for (const url of previews.current) URL.revokeObjectURL(url)
    previews.current.clear()
  }, [])

  async function upload(attempt: UploadAttempt) {
    try {
      await onUpload(attempt.file)
      setAttempts((current) => current.filter((item) => item.id !== attempt.id))
      if (attempt.preview) { URL.revokeObjectURL(attempt.preview); previews.current.delete(attempt.preview) }
    } catch (error) {
      setAttempts((current) => current.map((item) => item.id === attempt.id ? { ...item, status: 'error', error: error instanceof Error ? error.message : String(error) } : item))
    }
  }

  async function addFiles(selected: FileList | File[]) {
    if (busy || processing.current || selected.length === 0) return
    processing.current = true
    const next = Array.from(selected).map((file) => {
      const preview = file.type.startsWith('image/') ? URL.createObjectURL(file) : undefined
      if (preview) previews.current.add(preview)
      return { id: String(++nextAttemptId.current), file, preview, status: 'uploading' as const }
    })
    setAttempts((current) => [...current, ...next])
    try {
      for (const attempt of next) await upload(attempt)
    } finally {
      processing.current = false
    }
  }

  function drop(event: DragEvent<HTMLDivElement>) {
    event.preventDefault()
    setDragging(false)
    void addFiles(event.dataTransfer.files)
  }

  return <div className="space-y-5">
    <div className={`rounded-2xl border border-dashed p-6 text-center transition-colors sm:p-8 ${dragging ? 'border-[var(--app-accent)] bg-[var(--app-accent)]/[0.06]' : 'border-[var(--app-line)] hover:border-[var(--app-accent)]/50'}`}
      onDragEnter={(event) => { event.preventDefault(); if (!busy && !processing.current) setDragging(true) }}
      onDragLeave={(event) => { if (!event.currentTarget.contains(event.relatedTarget as Node)) setDragging(false) }}
      onDragOver={(event) => { event.preventDefault(); event.dataTransfer.dropEffect = busy || processing.current ? 'none' : 'copy' }} onDrop={drop}>
      <input ref={input} type="file" multiple className="sr-only" tabIndex={-1} aria-hidden="true" disabled={busy} onChange={(event) => { if (event.target.files) void addFiles(event.target.files); event.currentTarget.value = '' }} />
      <span aria-hidden="true" className="mx-auto mb-4 grid size-12 place-items-center rounded-full bg-[var(--app-wash)] text-[var(--app-accent)]"><Upload className="size-5" /></span>
      <p className="text-sm font-medium">{messages.dropFiles}</p>
      <p className="mt-1 text-xs text-muted-foreground">{formatMessage(messages.operationText.fileSizeLimit, { size: fileUploadLimitLabel() })}</p>
      <Button type="button" variant="link" className="mt-1 h-auto px-2 text-sm" disabled={busy} onClick={() => input.current?.click()}><CloudUpload className="size-4" />{messages.chooseFiles}</Button>
      <label className="mt-4 inline-flex cursor-pointer items-center gap-2 text-xs text-muted-foreground"><Checkbox checked={privateFile} disabled={busy || processing.current} onCheckedChange={onPrivateChange} />{messages.privateFile}</label>
      {privateFile && <p role="note" className="mx-auto mt-3 max-w-xl text-xs leading-relaxed text-muted-foreground">{messages.privateFileWarning}</p>}
    </div>

    {(files.length > 0 || attempts.length > 0) && <div className="flex justify-end"><Button type="button" size="sm" variant="outline" disabled={busy} onClick={() => input.current?.click()}><CloudUpload className="size-4" />{messages.chooseFiles}</Button></div>}
    {(files.length > 0 || attempts.length > 0) && <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
      {attempts.map((attempt) => {
        const Icon = fileIcon(attempt.file.name)
        return <article key={attempt.id} className="group relative min-w-0 overflow-hidden rounded-xl border border-[var(--app-line)] bg-[var(--app-card)]">
          <div className="relative grid aspect-[4/3] place-items-center border-b border-[var(--app-line)] bg-[var(--app-wash)]/60">
            {attempt.preview ? <img src={attempt.preview} alt="" className="absolute inset-0 size-full object-cover" /> : <Icon aria-hidden="true" className="size-9 text-[var(--app-muted)]" />}
            {attempt.status === 'uploading' && <div role="status" aria-label={messages.storeFile} className="absolute inset-0 grid place-items-center bg-black/40 text-white"><LoaderCircle className="size-8 animate-spin motion-reduce:animate-none" /></div>}
          </div>
          <div className="flex items-center gap-2 p-3"><div className="min-w-0 flex-1"><p dir="auto" className="truncate text-sm font-medium">{attempt.file.name}</p><p className="text-xs text-muted-foreground">{prettyBytes(attempt.file.size)}</p></div>{attempt.status === 'error' && <Button type="button" size="icon-sm" variant="ghost" disabled={busy} aria-label={`${messages.refresh} ${attempt.file.name}`} onClick={() => { if (processing.current) return; processing.current = true; setAttempts((current) => current.map((item) => item.id === attempt.id ? { ...item, status: 'uploading', error: undefined } : item)); void upload(attempt).finally(() => { processing.current = false }) }}><RefreshCw className="size-4" /></Button>}</div>
          {attempt.error && <p role="alert" className="px-3 pb-3 text-xs text-destructive">{attempt.error}</p>}
          {attempt.status === 'error' && <Button type="button" size="icon-sm" variant="outline" className="absolute end-2 top-2 size-7 rounded-full" aria-label={`${messages.remove} ${attempt.file.name}`} onClick={() => { setAttempts((current) => current.filter((item) => item.id !== attempt.id)); if (attempt.preview) { URL.revokeObjectURL(attempt.preview); previews.current.delete(attempt.preview) } }}><X className="size-3.5" /></Button>}
        </article>
      })}
      {files.map((file) => {
        const Icon = fileIcon(file.name)
        return <article key={file.cid} className="group relative min-w-0 overflow-hidden rounded-xl border border-[var(--app-line)] bg-[var(--app-card)] transition-colors hover:border-[var(--app-accent)]/40">
          <div className="grid aspect-[4/3] place-items-center border-b border-[var(--app-line)] bg-[var(--app-wash)]/60"><Icon aria-hidden="true" className="size-9 text-[var(--app-muted)]" /></div>
          <div className="min-w-0 space-y-1 p-3"><p dir="auto" title={file.name} className="truncate text-sm font-medium">{file.name || file.cid}</p><div className="flex items-center justify-between gap-2 text-xs text-muted-foreground"><span>{prettyBytes(file.sizeBytes)}</span>{file.private && <Badge variant="secondary">{messages.private}</Badge>}</div><code dir="ltr" title={file.cid} className="block truncate text-[10px] text-muted-foreground">{file.cid}</code></div>
          <div className="flex justify-end gap-1 border-t border-[var(--app-line)] px-2 py-1"><Button type="button" size="icon-sm" variant="ghost" aria-label={`${messages.operationText.download} ${file.name}`} onClick={() => onDownload(file)}><Download className="size-4" /></Button><Button type="button" size="icon-sm" variant="ghost" aria-label={`${messages.remove} ${file.name}`} onClick={() => onRemove(file.cid)}><Trash2 className="size-4" /></Button></div>
        </article>
      })}
    </div>}
  </div>
}
