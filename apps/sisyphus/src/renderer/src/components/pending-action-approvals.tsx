import { useEffect, useState } from 'react'
import { Check, LoaderCircle, ShieldAlert, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { ResponsiveActionDialog } from '@/components/ui/responsive-action-dialog'
import { getNodeApi } from '@/lib/node-api'
import type { getMessages } from '@/i18n/messages'
import { toast } from 'sonner'

type Messages = ReturnType<typeof getMessages>
type PendingAction = { id: string; action: string; requestedAtUnixMillis: number | string; expiresAtUnixMillis: number | string }

export function PendingActionApprovals({ messages, direction, enabled }: { messages: Messages; direction: 'ltr' | 'rtl'; enabled: boolean }) {
  const [actions, setActions] = useState<PendingAction[]>([])
  const [resolving, setResolving] = useState(false)
  const current = actions[0]
  const labels = messages.pendingActionConfirmation

  useEffect(() => {
    if (!enabled) {
      setActions([])
      return
    }
    let active = true
    let retry: number | undefined
    let cancelStream: (() => void) | undefined
    let attempt = 0
    const connect = () => {
      if (!active) return
      try {
        cancelStream = getNodeApi().stream<{ actions?: PendingAction[] }>(
          'watchPendingActionConfirmations', {}, (state) => {
            attempt = 0
            setActions(Array.isArray(state.actions) ? state.actions : [])
          }, (error, code) => {
            cancelStream = undefined
            if (!active || code === 12) return
            if (attempt === 0) toast.error(labels.loadFailed, { description: error })
            retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
          }, () => {
            cancelStream = undefined
            if (active) retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
          },
        )
      } catch (error) {
        if (!active) return
        if (attempt === 0) toast.error(labels.loadFailed, { description: error instanceof Error ? error.message : String(error) })
        retry = window.setTimeout(connect, Math.min(30_000, 500 * (2 ** Math.min(attempt++, 6))))
      }
    }
    connect()
    return () => {
      active = false
      if (retry !== undefined) window.clearTimeout(retry)
      cancelStream?.()
    }
  }, [enabled, labels.loadFailed])

  const resolve = async (approved: boolean) => {
    if (!current || resolving) return
    setResolving(true)
    try {
      await getNodeApi().call('resolveActionConfirmation', { id: current.id, approved })
      setActions((previous) => previous.filter((action) => action.id !== current.id))
    } catch (error) {
      toast.error(labels.resolveFailed, { description: error instanceof Error ? error.message : String(error) })
    } finally {
      setResolving(false)
    }
  }

  return <ResponsiveActionDialog
    open={Boolean(current)}
    direction={direction}
    title={labels.title}
    description={current ? labels.description : undefined}
    icon={<ShieldAlert className="size-6 text-amber-600" />}
    onOpenChange={() => { /* The owner must explicitly approve or deny. */ }}
  >
    {current ? <>
      <div dir="ltr" className="rounded-xl border border-[var(--app-line)] bg-[var(--app-wash)] px-4 py-3 text-left text-sm leading-6 text-[var(--app-ink)] [unicode-bidi:plaintext]">{current.action}</div>
      {actions.length > 1 ? <p className="mt-3 text-center text-xs text-[var(--app-muted)]">{labels.queued.replace('{count}', String(actions.length - 1))}</p> : null}
      <div className="mt-5 grid min-w-0 gap-2 sm:grid-cols-2">
        <Button type="button" variant="outline" size="lg" disabled={resolving} onClick={() => void resolve(false)} className="order-2 h-12 w-full rounded-xl sm:order-1">
          {resolving ? <LoaderCircle className="size-4 animate-spin" /> : <X className="size-4" />}{labels.deny}
        </Button>
        <Button type="button" variant="destructive" size="lg" disabled={resolving} onClick={() => void resolve(true)} className="order-1 h-12 w-full whitespace-normal rounded-xl sm:order-2">
          {resolving ? <LoaderCircle className="size-4 animate-spin" /> : <Check className="size-4" />}{labels.approve}
        </Button>
      </div>
    </> : null}
  </ResponsiveActionDialog>
}
