import { useEffect, useRef, useState, type Dispatch, type SetStateAction } from 'react'
import { createPortal } from 'react-dom'
import { Link } from 'react-router-dom'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { ArrowDown, Boxes, CheckCircle2, ChevronDown, CircleAlert, File, History, LoaderCircle, Plus, Search, Settings2, Sparkles, Trash2, Wrench, X } from 'lucide-react'
import { Drawer } from 'vaul'
import { toast } from 'sonner'
import type { NodeSnapshot } from '../../../../preload'
import { Button } from '@/components/ui/button'
import { Message, MessageContent, MessageResponse } from '@/components/ai-elements/message'
import { SisyphusPrompt, type ChatAttachment } from '@/components/assistant/sisyphus-prompt'
import { DeleteChatConfirmation } from '@/components/assistant/delete-chat-confirmation'
import { getMessageDirection } from '@/lib/text-direction'
import { getJobHref } from '@/lib/job-navigation'
import ShinyText from '@/components/ui/shiny-text'
import { getNodeApi, isDesktopApp } from '@/lib/node-api'
import { useBreakpoint } from '@/hooks/use-breakpoint'
import { type getMessages } from '@/i18n/messages'

type Messages = ReturnType<typeof getMessages>
type ChatSummary = { chatId: string; title: string; createdAtMs: string | number }
type StoredCall = { name: string; arguments: string }
type Activity = { id: string; tool: string; arguments: string; result?: string; jobId?: string; state: 'working' | 'complete' | 'failed' }
type AttachedFile = { name: string; cid: string; image: boolean }
type ChatItem = { id: string; role: 'user' | 'assistant' | 'tool'; content: string; tool?: string; calls?: StoredCall[]; activities?: Activity[]; attachments?: AttachedFile[] }
type AskEvent = { chatId: string; kind: string; text: string; jobId?: string; tool?: string }
type StoredChatMessage = { role: string; content: string; tool?: string; calls?: StoredCall[] }

function newId() { return globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}` }
function findLast<T>(items: T[], predicate: (item: T) => boolean) {
  for (let index = items.length - 1; index >= 0; index -= 1) if (predicate(items[index])) return items[index]
  return undefined
}
function findLastIndex<T>(items: T[], predicate: (item: T) => boolean) {
  for (let index = items.length - 1; index >= 0; index -= 1) if (predicate(items[index])) return index
  return -1
}

function readAttachments(content: string): { content: string; attachments: AttachedFile[] } {
  const marker = '\n\n[Sisyphus attachments]\n'
  const index = content.lastIndexOf(marker)
  if (index < 0) return { content, attachments: [] }
  const attachments = content.slice(index + marker.length).split('\n').flatMap((line) => {
    const match = /^- (.*) \| cid:([^|]+) \| image:(true|false)$/.exec(line)
    if (!match) return []
    try { return [{ name: JSON.parse(match[1]) as string, cid: match[2], image: match[3] === 'true' }] } catch { return [] }
  })
  return attachments.length ? { content: content.slice(0, index), attachments } : { content, attachments: [] }
}

function loadMessages(messages: StoredChatMessage[]): ChatItem[] {
  const output: ChatItem[] = []
  for (const stored of messages) {
    if (stored.role === 'user') {
      const parsed = readAttachments(stored.content)
      output.push({ id: newId(), role: 'user', ...parsed })
    } else if (stored.role === 'assistant') {
      const activities = (stored.calls ?? []).map((call) => ({ id: newId(), tool: call.name, arguments: call.arguments, state: 'working' as const }))
      output.push({ id: newId(), role: 'assistant', content: stored.content, calls: stored.calls ?? [], activities })
    } else if (stored.role === 'tool') {
      const assistant = [...output].reverse().find((item) => item.role === 'assistant' && item.activities?.some((activity) => activity.tool === (stored.tool ?? '') && activity.state === 'working'))
      const activity = assistant?.activities && findLast(assistant.activities, (item) => item.tool === (stored.tool ?? '') && item.state === 'working')
      if (activity) { activity.result = stored.content; activity.state = 'complete' }
      else output.push({ id: newId(), role: 'tool', content: stored.content, tool: stored.tool })
    }
  }
  return output
}

function toolLabel(tool: string, messages: Messages) {
  if (tool === 'run_job') return messages.agentActionRunJob
  if (tool === 'get_job') return messages.agentActionGetJob
  return tool.replaceAll('_', ' ')
}

function ActivityTimeline({ activities, direction, messages }: { activities: Activity[]; direction: 'ltr' | 'rtl'; messages: Messages }) {
  const [expanded, setExpanded] = useState(false)
  if (!activities.length) return null
  const working = activities.some((action) => action.state === 'working')
  const current = findLast(activities, (action) => action.state === 'working') ?? activities[activities.length - 1]
  const CurrentIcon = current.tool === 'run_job' ? Boxes : current.tool === 'get_job' ? Search : Wrench
  const summary = working ? toolLabel(current.tool, messages) : activities.length === 1 ? messages.agentActionComplete : `${messages.agentActions} · ${activities.length}`
  return <div dir={direction} className="sisyphus-agent-actions mx-auto w-full max-w-2xl text-start">
    <button type="button" aria-expanded={expanded} onClick={() => setExpanded((value) => !value)} className="group flex min-h-9 w-full items-center justify-center gap-2 rounded-lg px-2 text-xs text-muted-foreground transition-colors hover:bg-[var(--app-wash)]">
      <AnimatePresence initial={false} mode="wait"><motion.span key={working ? current.id : `done-${current.id}`} initial={{ opacity: 0, scale: .85 }} animate={{ opacity: 1, scale: 1 }} exit={{ opacity: 0, scale: .85 }} className="grid size-6 shrink-0 place-items-center">{working ? <LoaderCircle className="size-4 animate-spin" /> : <CurrentIcon className="size-4" />}</motion.span></AnimatePresence>
      <AnimatePresence initial={false} mode="wait"><motion.span key={summary} initial={{ opacity: 0, y: 3 }} animate={{ opacity: 1, y: 0 }} exit={{ opacity: 0, y: -3 }} className="min-w-0 max-w-[min(100%,32rem)] truncate text-start font-semibold">{working ? <ShinyText text={summary} speed={2.4} delay={.7} direction={direction === 'rtl' ? 'right' : 'left'} /> : summary}</motion.span></AnimatePresence>
      <ChevronDown className={`size-3.5 shrink-0 transition-transform duration-200 ${expanded ? 'rotate-180' : ''}`} />
    </button>
    <motion.div initial={false} animate={{ height: expanded ? 'auto' : 0, opacity: expanded ? 1 : 0 }} transition={{ duration: .24, ease: [.22, 1, .36, 1] }} aria-hidden={!expanded} className="overflow-hidden" style={{ pointerEvents: expanded ? 'auto' : 'none' }}>
      {activities.map((action) => {
        const Icon = action.tool === 'run_job' ? Boxes : action.tool === 'get_job' ? Search : Wrench
        const StateIcon = action.state === 'working' ? LoaderCircle : action.state === 'failed' ? CircleAlert : CheckCircle2
        let detail = action.arguments
        try { detail = JSON.stringify(JSON.parse(action.arguments), null, 2) } catch { /* Keep original tool arguments. */ }
        return <motion.div layout="position" key={action.id} className="flex min-w-0 items-start gap-2.5 px-2 py-2 text-start">
          <span className="relative mt-0.5 grid size-6 shrink-0 place-items-center text-muted-foreground"><Icon className="size-3.5" /></span>
          <div className="min-w-0 flex-1"><div className="flex items-center gap-2 text-xs font-semibold text-muted-foreground"><span className="truncate">{toolLabel(action.tool, messages)}</span><StateIcon className={`size-3 shrink-0 ${action.state === 'working' ? 'animate-spin' : ''}`} /></div>
            {action.jobId && <Link dir="ltr" to={getJobHref(action.jobId)} className="mt-1 block truncate rounded text-[10px] text-[var(--app-accent)] underline-offset-2 hover:underline focus-visible:outline-2 focus-visible:outline-[var(--app-accent)]"><code>{messages.jobId}: {action.jobId}</code></Link>}
            <pre dir="auto" className="mt-1 max-h-28 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-[var(--app-wash)] p-2 text-[10px] text-muted-foreground">{action.result ?? detail}</pre>
          </div>
        </motion.div>
      })}
    </motion.div>
  </div>
}

function AssistantMark({ working = false, large = false }: { working?: boolean; large?: boolean }) {
  return <motion.span animate={working ? { rotate: [0, -4, 4, 0], scale: [1, 1.04, 1] } : { rotate: 0, scale: 1 }} transition={working ? { repeat: Infinity, duration: 2.4, ease: 'easeInOut' } : { duration: .3 }} className={`sisyphus-assistant-mark relative grid shrink-0 place-items-center ${large ? 'is-large' : 'is-small'} ${working ? 'is-working' : ''}`}>
    <span className="sisyphus-assistant-mark__halo" aria-hidden="true" /><img src="/logo.png" alt="" className="relative z-[1] size-full rounded-[28%] object-cover" />
  </motion.span>
}

export function AgentChat({ snapshot, messages, direction, historyOpen, setHistoryOpen, historyPanelHost, onOpenSettings }: { snapshot: NodeSnapshot; messages: Messages; direction: 'ltr' | 'rtl'; historyOpen: boolean; setHistoryOpen: Dispatch<SetStateAction<boolean>>; historyPanelHost: HTMLDivElement | null; onOpenSettings: () => void }) {
  const reduceMotion = useReducedMotion()
  const [chats, setChats] = useState<ChatSummary[]>([])
  const [deleteTarget, setDeleteTarget] = useState<{ id: string; title: string } | null>(null)
  const [deletingChat, setDeletingChat] = useState(false)
  const [chatId, setChatId] = useState('')
  const [items, setItems] = useState<ChatItem[]>([])
  const [prompt, setPrompt] = useState('')
  const [attachments, setAttachments] = useState<ChatAttachment[]>([])
  const [asking, setAsking] = useState(false)
  const [busy, setBusy] = useState(false)
  const [showScrollToBottom, setShowScrollToBottom] = useState(false)
  const [composerHeight, setComposerHeight] = useState(0)
  const [conversationLoading, setConversationLoading] = useState(false)
  const [browserOnly] = useState(() => !isDesktopApp())
  const isMobile = useBreakpoint('(max-width: 900px)')
  const scrollRef = useRef<HTMLDivElement>(null)
  const composerRef = useRef<HTMLDivElement>(null)
  const previewUrls = useRef(new Set<string>())
  const requestRef = useRef(0)
  const streamCancel = useRef<(() => void) | null>(null)
  const assistantId = useRef('')
  const stickToBottom = useRef(true)
  const isBusy = asking || busy

  async function refreshChats() {
    try {
      const result = await getNodeApi().call<{ chats?: ChatSummary[] }>('listChats')
      setChats(result.chats ?? [])
    } catch { /* The daemon can be offline or may not implement planner history. */ }
  }

  useEffect(() => {
    if (snapshot.status === 'connected' && !browserOnly) void refreshChats()
  }, [snapshot.status, browserOnly])
  useEffect(() => () => {
    streamCancel.current?.()
    previewUrls.current.forEach((url) => URL.revokeObjectURL(url))
  }, [])
  useEffect(() => {
    const composer = composerRef.current
    const scroller = scrollRef.current
    if (!composer || !scroller) return
    const observer = new ResizeObserver(() => {
      const height = composer.getBoundingClientRect().height
      setComposerHeight((current) => Math.abs(current - height) < 1 ? current : height)
      if (stickToBottom.current) scroller.scrollTo({ top: scroller.scrollHeight, behavior: 'auto' })
    })
    observer.observe(composer)
    return () => observer.disconnect()
  }, [])
  useEffect(() => {
    const scroller = scrollRef.current
    if (!scroller || composerHeight === 0) return
    const gap = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight
    if (gap > 180) return
    const frame = window.requestAnimationFrame(() => scroller.scrollTo({ top: scroller.scrollHeight, behavior: 'smooth' }))
    return () => window.cancelAnimationFrame(frame)
  }, [composerHeight])
  useEffect(() => {
    const frame = window.requestAnimationFrame(() => {
      const scroller = scrollRef.current
      if (scroller && stickToBottom.current) scroller.scrollTo({ top: scroller.scrollHeight, behavior: 'smooth' })
    })
    return () => window.cancelAnimationFrame(frame)
  }, [items])

  async function openChat(id: string) {
    const requestId = ++requestRef.current
    streamCancel.current?.()
    streamCancel.current = null
    setAsking(false)
    setChatId(id)
    setConversationLoading(Boolean(id))
    if (!id) { setItems([]); setConversationLoading(false); stickToBottom.current = true; return }
    try {
      const result = await getNodeApi().call<{ messages?: StoredChatMessage[] }>('getChat', { chatId: id })
      if (requestId === requestRef.current) {
        setItems(loadMessages(result.messages ?? []))
        stickToBottom.current = true
      }
    } catch (error) {
      if (requestId === requestRef.current) toast.error(messages.openConversationFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally { if (requestId === requestRef.current) setConversationLoading(false) }
  }

  async function deleteChat(id: string) {
    if (deletingChat) return
    setDeletingChat(true)
    try {
      await getNodeApi().call('deleteChat', { chatId: id })
      setChats((current) => current.filter((chat) => chat.chatId !== id))
      if (chatId === id) { ++requestRef.current; streamCancel.current?.(); streamCancel.current = null; setAsking(false); setChatId(''); setItems([]) }
      toast.success(messages.conversationDeleted)
      setDeleteTarget(null)
    } catch (error) { toast.error(messages.deleteConversationFailed, { description: error instanceof Error ? error.message : String(error) }) }
    finally { setDeletingChat(false) }
  }

  function addAttachments(files: File[]) {
    const next = files.filter((file) => file.size <= 256 * 1024 * 1024).map((file) => {
      if (file.size > 0 && !file.type) { /* Keep extension-only files; the daemon stores opaque bytes. */ }
      const previewUrl = URL.createObjectURL(file)
      previewUrls.current.add(previewUrl)
      return { id: newId(), file, previewUrl }
    })
    const tooLarge = files.some((file) => file.size > 256 * 1024 * 1024)
    if (tooLarge) toast.error(messages.attachmentTooLarge)
    setAttachments((current) => [...current, ...next])
  }

  function removeAttachment(id: string) {
    setAttachments((current) => {
      const removed = current.find((item) => item.id === id)
      if (removed) { URL.revokeObjectURL(removed.previewUrl); previewUrls.current.delete(removed.previewUrl) }
      return current.filter((item) => item.id !== id)
    })
  }

  function updateAssistant(id: string, update: (item: ChatItem) => ChatItem) {
    setItems((current) => current.map((item) => item.id === id ? update(item) : item))
  }

  async function sendMessage(text = prompt) {
    const value = text.trim()
    if ((!value && attachments.length === 0) || isBusy || browserOnly || snapshot.status !== 'connected') return
    setBusy(true)
    const pendingFiles = [...attachments]
    try {
      const storedAttachments = await Promise.all(pendingFiles.map(async ({ file }) => {
        const result = await getNodeApi().storeFile({ name: file.name, data: new Uint8Array(await file.arrayBuffer()), private: false }) as { cid?: string }
        if (!result.cid) throw new Error(messages.fileUploadMissingCid)
        return { name: file.name, cid: result.cid, image: file.type.startsWith('image/') }
      }))
      const fullText = storedAttachments.length
        ? `${value}${value ? '\n\n' : ''}[Sisyphus attachments]\n${storedAttachments.map((file) => `- ${JSON.stringify(file.name)} | cid:${file.cid} | image:${file.image}`).join('\n')}`
        : value
      setPrompt('')
      setAttachments([])
      pendingFiles.forEach(({ previewUrl }) => { URL.revokeObjectURL(previewUrl); previewUrls.current.delete(previewUrl) })
      const userItem: ChatItem = { id: newId(), role: 'user', content: value || messages.attachedFilesOnly, attachments: storedAttachments }
      const answerId = newId()
      assistantId.current = answerId
      stickToBottom.current = true
      setItems((current) => [...current, userItem, { id: answerId, role: 'assistant', content: '', activities: [] }])
      setBusy(false)
      setAsking(true)
      streamCancel.current = getNodeApi().stream<AskEvent>('ask', { chatId, text: fullText }, (event) => {
        if (event.chatId && !chatId) setChatId(event.chatId)
        if (event.kind === 'text') updateAssistant(answerId, (item) => ({ ...item, content: item.content + event.text }))
        if (event.kind === 'call') updateAssistant(answerId, (item) => ({ ...item, activities: [...(item.activities ?? []), { id: newId(), tool: event.tool || messages.agentActionUnknown, arguments: event.text, state: 'working' }] }))
        if (event.kind === 'job') updateAssistant(answerId, (item) => {
          const activities = [...(item.activities ?? [])]
          const index = findLastIndex(activities, (action) => action.state === 'working')
          if (index >= 0) activities[index] = { ...activities[index], jobId: event.jobId }
          return { ...item, activities }
        })
        if (event.kind === 'result') updateAssistant(answerId, (item) => {
          const activities = [...(item.activities ?? [])]
          const index = findLastIndex(activities, (action) => action.tool === event.tool && action.state === 'working')
          if (index >= 0) activities[index] = { ...activities[index], result: event.text, state: event.text.includes('"error"') ? 'failed' : 'complete' }
          return { ...item, activities }
        })
        if (event.kind === 'done') {
          setAsking(false)
          streamCancel.current = null
          void refreshChats()
        }
      }, (error) => {
        setAsking(false)
        streamCancel.current = null
        updateAssistant(answerId, (item) => ({ ...item, activities: (item.activities ?? []).map((action) => action.state === 'working' ? { ...action, state: 'failed' } : action) }))
        const detail = error
        if (/model|planner|language model/i.test(detail) && /not|set|configured|pool/i.test(detail)) {
          toast.error(messages.modelSetupRequired, { description: detail, action: { label: messages.openSettings, onClick: onOpenSettings } })
        } else toast.error(messages.agentRequestFailed, { description: detail })
      }, () => {
        setAsking(false)
        streamCancel.current = null
      })
    } catch (error) {
      setBusy(false)
      const detail = error instanceof Error ? error.message : String(error)
      toast.error(messages.agentRequestFailed, { description: detail })
    }
  }

  const paneItems = items
  const lastAssistant = findLast(paneItems, (item) => item.role === 'assistant')
  const suggestions = [messages.suggestionComputePrimes, messages.suggestionExplainNetwork]
  const stopScroll = () => {
    const element = scrollRef.current
    if (!element) return
    const gap = element.scrollHeight - element.scrollTop - element.clientHeight
    stickToBottom.current = gap < 100
    setShowScrollToBottom(gap > 180)
  }
  const scrollBottom = () => { const element = scrollRef.current; if (element) { stickToBottom.current = true; element.scrollTo({ top: element.scrollHeight, behavior: 'smooth' }) } }
  const selectedChat = chats.find((chat) => chat.chatId === chatId)
  const historyList = <>
    <Button type="button" variant="outline" disabled={browserOnly || asking} onClick={() => { if (isMobile) setHistoryOpen(false); void openChat('') }} className="mx-2 mb-2 flex h-10 w-[calc(100%-1rem)] min-w-0 justify-start gap-2 rounded-xl border-[var(--app-line)] bg-[var(--app-card)] px-3 text-sm font-medium text-foreground shadow-sm shadow-black/[0.03] hover:border-[color-mix(in_srgb,var(--app-accent)_42%,var(--app-line))] hover:bg-[var(--app-wash)]">
      <Plus className="size-4 text-[var(--app-accent)]" /><span className="truncate">{messages.newConversation}</span>
    </Button>
    {chats.length ? <nav aria-label={messages.conversationHistory} className="grid gap-1 px-2">
      {chats.map((chat) => {
        const numericDate = typeof chat.createdAtMs === 'number' || /^\d+$/.test(String(chat.createdAtMs)) ? Number(chat.createdAtMs) : NaN
        const date = new Date(Number.isFinite(numericDate) ? numericDate : chat.createdAtMs)
        return <div key={chat.chatId} className={`group/conversation flex min-w-0 items-center gap-1 rounded-2xl p-0.5 transition-colors ${chatId === chat.chatId ? 'bg-[var(--app-wash)]' : 'hover:bg-[var(--app-wash)]'}`}>
          <button type="button" onClick={() => { if (isMobile) setHistoryOpen(false); void openChat(chat.chatId) }} disabled={asking} aria-current={chatId === chat.chatId ? 'page' : undefined} className="flex min-h-14 min-w-0 flex-1 flex-col items-start justify-center rounded-[0.9rem] px-3 py-2 text-start text-foreground/85 transition-colors hover:text-foreground disabled:opacity-60">
            <span title={chat.title || messages.conversation} className="block w-full truncate text-sm font-medium">{chat.title || messages.conversation}</span>
            <span className="mt-1 block w-full truncate text-xs text-muted-foreground">{Number.isNaN(date.getTime()) ? '' : date.toLocaleDateString(direction === 'rtl' ? 'he-IL' : 'en-US')}</span>
          </button>
          <Button type="button" variant="ghost" size="icon" onClick={() => setDeleteTarget({ id: chat.chatId, title: chat.title || messages.conversation })} disabled={asking || browserOnly} aria-label={`${messages.deleteConversation}: ${chat.title || messages.conversation}`} className="me-1 size-8 shrink-0 rounded-lg text-muted-foreground hover:bg-red-500/10 hover:text-red-500 md:opacity-0 md:group-hover/conversation:opacity-100 focus-visible:opacity-100">
            <Trash2 className="size-4" />
          </Button>
        </div>
      })}
    </nav> : <div className="mx-2 flex min-h-40 flex-col items-center justify-center px-5 py-8 text-center">
      <span className="grid size-11 place-items-center rounded-2xl bg-[var(--app-wash)] text-[var(--app-accent)]"><History className="size-5" /></span><p className="mt-3 max-w-[15rem] text-xs leading-5 text-muted-foreground">{messages.emptyHistory}</p>
    </div>}
  </>

  return <>
    <DeleteChatConfirmation target={deleteTarget} direction={direction} deleteTitle={messages.deleteChatTitle} deleteDescription={messages.deleteChatDescription} cancelLabel={messages.cancel} confirmLabel={messages.deleteConversation} deletingLabel={messages.deletingChat} pending={deletingChat} onClose={() => { if (!deletingChat) setDeleteTarget(null) }} onConfirm={() => { if (deleteTarget) void deleteChat(deleteTarget.id) }} />
    {historyPanelHost && !isMobile ? createPortal(<aside dir={direction} aria-hidden={!historyOpen} className="sisyphus-history-desktop" inert={!historyOpen}>
      <div className="flex h-full min-h-0 flex-col overflow-hidden rounded-2xl border border-[var(--app-line)] bg-[var(--app-sidebar)] shadow-xl shadow-black/10">
        <header className="flex items-start gap-2.5 border-b border-[var(--app-line)] p-4">
          <div className="min-w-0 flex-1"><div className="flex min-w-0 items-start gap-2"><h2 className="min-w-0 flex-1 text-base font-semibold leading-5 tracking-tight text-foreground">{messages.conversationHistory}</h2><span className="grid size-7 shrink-0 place-items-center rounded-full bg-[var(--app-wash)] text-[11px] font-semibold text-muted-foreground">{chats.length}</span></div><p className="mt-1.5 text-start text-[11px] leading-4 text-muted-foreground">{messages.historyDescription}</p></div>
          <Button type="button" variant="ghost" size="icon" onClick={() => setHistoryOpen(false)} aria-label={messages.closeHistory} className="-me-1 -mt-1 size-8 shrink-0 rounded-full text-muted-foreground"><X className="size-4" /></Button>
        </header>
        <div className="min-h-0 flex-1 overflow-y-auto py-3">{historyList}</div>
      </div>
    </aside>, historyPanelHost) : null}
    <div dir={direction} data-chat-loading={conversationLoading ? '' : undefined} className={`sisyphus-agent-chat relative flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden text-foreground ${browserOnly ? 'is-browser' : ''}`}>
    <div className="sisyphus-chat-toolbar relative z-20 flex h-10 shrink-0 items-center gap-1 px-1">
      <Button type="button" size="icon" variant="ghost" onClick={() => setHistoryOpen((open) => !open)} aria-label={historyOpen ? messages.closeHistory : messages.openHistory} aria-haspopup="dialog" aria-expanded={historyOpen} aria-pressed={historyOpen} data-state={historyOpen ? 'open' : 'closed'} className="grid size-10 shrink-0 touch-manipulation place-items-center rounded-full text-foreground transition-colors hover:bg-[var(--app-wash)] focus-visible:ring-2 focus-visible:ring-[var(--app-accent)]/60"><History className="size-[22px]" strokeWidth={1.8} /></Button>
      <span className="sisyphus-conversation-title hidden min-w-0 flex-1 truncate px-1 text-start text-xs text-muted-foreground min-[901px]:block">{selectedChat?.title || messages.newConversation}</span>
      <div className="ms-auto flex shrink-0 items-center gap-1">
      {chatId && <Button type="button" size="icon" variant="ghost" disabled={asking || browserOnly} className="hidden size-8 rounded-full text-muted-foreground min-[901px]:inline-flex" aria-label={messages.deleteConversation} title={messages.deleteConversation} onClick={() => setDeleteTarget({ id: chatId, title: selectedChat?.title || messages.conversation })}><Trash2 className="size-4" /></Button>}
      <Button type="button" size="icon" variant="ghost" onClick={onOpenSettings} aria-label={messages.openAdvanced} title={messages.openAdvanced} className="size-10 rounded-full text-muted-foreground"><Settings2 className="size-[18px]" /></Button>
      </div>
    </div>
    <Drawer.Root open={isMobile && historyOpen} onOpenChange={setHistoryOpen} direction="bottom" shouldScaleBackground={false}>
      <Drawer.Portal>
        <Drawer.Overlay className="sisyphus-history-overlay fixed inset-0 z-50 bg-black/25 backdrop-blur-sm min-[901px]:hidden" />
        <Drawer.Content dir={direction} className="sisyphus-history-drawer liquid-glass-menu fixed inset-x-0 bottom-0 z-50 flex h-[88dvh] max-h-[88dvh] flex-col rounded-t-[1.5rem] border border-b-0 border-[var(--app-line)] bg-[var(--app-sidebar)] p-4 shadow-[0_0_50px_color-mix(in_srgb,#000_24%,transparent)] outline-none min-[901px]:hidden sm:p-5">
          <div aria-hidden="true" className="mx-auto mb-4 h-1 w-10 shrink-0 rounded-full bg-[var(--app-line)]" />
          <div className="flex items-start gap-4 border-b border-[var(--app-line)] pb-4"><div className="min-w-0 flex-1"><div className="flex items-center gap-2"><Drawer.Title className="text-2xl font-semibold tracking-[-0.04em]">{messages.conversationHistory}</Drawer.Title><span className="grid size-8 shrink-0 place-items-center rounded-full bg-[var(--app-wash)] text-xs font-semibold">{chats.length}</span></div><Drawer.Description className="mt-1 text-start text-xs leading-5 text-muted-foreground">{messages.historyDescription}</Drawer.Description></div><Button type="button" variant="ghost" size="icon" onClick={() => setHistoryOpen(false)} aria-label={messages.closeHistory} className="size-9 shrink-0 rounded-full text-muted-foreground"><X className="size-4" /></Button></div>
          <div className="min-h-0 flex-1 overflow-y-auto py-3">{historyList}</div>
        </Drawer.Content>
      </Drawer.Portal>
    </Drawer.Root>
    {browserOnly && <div role="status" className="relative z-20 mx-2 mt-2 rounded-lg border border-[var(--app-line)] bg-[var(--app-wash)] px-3 py-2 text-[11px] text-muted-foreground">{messages.browserChatReadOnly}</div>}
    <div ref={scrollRef} onScroll={stopScroll} className="sisyphus-chat-scroll absolute inset-x-0 bottom-0 top-10 overflow-x-hidden overflow-y-auto overscroll-y-contain px-2 [scrollbar-gutter:stable]" style={{ WebkitOverflowScrolling: 'touch', touchAction: 'pan-y', paddingBottom: Math.max(composerHeight + 24, 100) }}>
      <div className="mx-auto flex h-full min-h-full min-w-0 w-full max-w-3xl flex-col gap-6 px-2 pb-5 pt-5 sm:px-4">
        <AnimatePresence mode="wait" initial={false}>
          {conversationLoading ? <motion.div key="loading" initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }} className="flex min-h-[55vh] flex-1 items-center justify-center"><AssistantMark working /></motion.div>
            : paneItems.length === 0 ? <motion.div key="empty" initial={reduceMotion ? false : { opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }} transition={{ duration: .22 }} className="flex min-h-full flex-1 flex-col items-center justify-center gap-5 px-3 text-center">
              <AssistantMark large />
              <div className="max-w-md"><h2 className="text-lg font-semibold leading-8 text-foreground sm:text-xl">{messages.agentEmptyTitle}</h2><p className="mt-2 text-sm leading-6 text-muted-foreground">{messages.agentEmptyDescription}</p></div>
            </motion.div>
              : <motion.div key={`conversation-${chatId || 'new'}`} initial={reduceMotion ? false : { opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }} transition={{ duration: .22 }} className="flex min-h-full flex-col gap-7 pb-5">
                {paneItems.map((item) => {
                  if (item.role === 'tool') return null
                  const assistant = item.role === 'assistant'
                  const messageDirection = getMessageDirection(item.content, direction)
                  const itemBusy = asking && item.id === lastAssistant?.id
                  return <motion.article key={item.id} initial={reduceMotion ? false : { opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }} transition={{ duration: .22 }} className="flex w-full flex-col gap-3">
                    {!assistant && item.attachments?.length ? <div dir="auto" className="ms-auto flex w-full max-w-[85%] flex-nowrap justify-end gap-2 overflow-x-auto pb-1 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">{item.attachments.map((file) => <span key={file.cid} title={file.cid} className="inline-flex h-8 max-w-56 shrink-0 items-center gap-2 rounded-full border border-[var(--app-line)] bg-[var(--app-card)]/90 px-3 text-xs text-muted-foreground shadow-sm"><File className="size-3.5 shrink-0" /><span className="truncate">{file.name}</span></span>)}</div> : null}
                    <div dir={messageDirection} className="flex min-w-0 items-start gap-3">
                      {assistant && <span className="mt-0.5 size-7 shrink-0"><AssistantMark working={itemBusy} /></span>}
                      <Message dir={messageDirection} from={item.role} className="min-w-0 text-start text-[15px] leading-7 text-foreground">
                        {item.content ? <MessageContent className={assistant ? 'max-w-full text-[15px] leading-7 text-foreground' : 'max-w-[min(85%,38rem)] rounded-2xl border border-[var(--app-user-border)] bg-[var(--app-user-bubble)] px-3 py-2 text-[15px] leading-7 text-foreground shadow-none'}><MessageResponse dir={messageDirection} className={assistant ? '[&_a]:[unicode-bidi:isolate] [&_code]:[direction:ltr] [&_code]:[unicode-bidi:isolate] [&_pre]:[direction:ltr] [&_pre]:text-left' : 'whitespace-pre-wrap break-words [overflow-wrap:anywhere]'}>{item.content}</MessageResponse></MessageContent> : itemBusy && <LoaderCircle className="mt-2 size-4 animate-spin text-muted-foreground" />}
                      </Message>
                    </div>
                    {assistant && item.activities?.length ? <ActivityTimeline activities={item.activities} direction={messageDirection} messages={messages} /> : null}
                  </motion.article>
                })}
                <div aria-hidden="true" className="shrink-0" style={{ height: Math.max(composerHeight + 48, 180) }} />
              </motion.div>}
        </AnimatePresence>
      </div>
    </div>
    <div ref={composerRef} className="pointer-events-none absolute inset-x-0 bottom-0 z-20 min-w-0 pb-1">
      {showScrollToBottom && <Button type="button" variant="outline" size="icon" onClick={scrollBottom} className="pointer-events-auto absolute bottom-[calc(100%+0.75rem)] left-1/2 z-10 size-9 -translate-x-1/2 rounded-full border-[var(--app-line)] bg-[var(--app-card)] text-foreground shadow-lg shadow-black/10 hover:-translate-x-1/2 hover:-translate-y-0.5" aria-label={messages.scrollToBottom}><ArrowDown size={17} /></Button>}
      {paneItems.length === 0 && !conversationLoading && <div className="sisyphus-composer-suggestions pointer-events-auto relative z-10 mx-auto mb-1 flex w-full max-w-3xl flex-wrap justify-center gap-2 overflow-visible px-2 py-2">{suggestions.map((suggestion, index) => <Button variant="outline" size="sm" type="button" key={suggestion} disabled={browserOnly || snapshot.status !== 'connected' || isBusy} onClick={() => void sendMessage(suggestion)} className="h-9 shrink-0 gap-2 whitespace-nowrap rounded-full border-[var(--app-line)] bg-[var(--app-card)]/85 px-3.5 text-xs font-medium text-foreground shadow-sm shadow-black/5 transition-[transform,border-color,background-color,box-shadow] hover:-translate-y-0.5 hover:border-[var(--app-accent)] hover:bg-[var(--app-wash)] hover:shadow-md"><span className={`grid size-5 shrink-0 place-items-center rounded-full ${index === 0 ? 'bg-sky-500/12 text-sky-500' : 'bg-violet-500/12 text-violet-500'}`}><Sparkles className="size-3.5" strokeWidth={2} /></span>{suggestion}</Button>)}</div>}
      <SisyphusPrompt direction={direction} value={prompt} onValueChange={setPrompt} onSubmit={() => void sendMessage()} placeholder={snapshot.status === 'connected' ? messages.promptPlaceholder : messages.agentNotReadyShort} sendLabel={messages.sendMessage} addAttachmentLabel={messages.addAttachment} removeAttachmentLabel={messages.removeAttachment} closePreviewLabel={messages.closePreview} attachments={attachments} onFilesPicked={addAttachments} onAttachmentRemove={removeAttachment} disabled={browserOnly || snapshot.status !== 'connected' || isBusy} />
    </div>
    </div>
  </>
}
