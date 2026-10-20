import { useEffect, useState } from 'react'
import { LoaderCircle, RotateCcw, TriangleAlert } from 'lucide-react'
import { getNodeApi, isDesktopApp } from '@/lib/node-api'
import { Button } from '@/components/ui/button'
import { ResponsiveActionDialog } from '@/components/ui/responsive-action-dialog'
import { formatMessage, type getMessages } from '@/i18n/messages'
import { waitForConnectedSnapshot } from '../../../shared/wait-for-connected-snapshot.ts'
import { restartImpact } from '../../../shared/restart-impact.ts'
import { toast } from 'sonner'

type Messages = ReturnType<typeof getMessages>

/**
 * Restarts the node this app started, so settings that wait for a start
 * apply now. When jobs are running or workers are connected it says so and
 * asks first; otherwise it restarts at once. Shown only for the app's own node.
 */
export function RestartNodeButton({ messages, onRestarted }: { messages: Messages; onRestarted?: () => void }) {
  const labels = messages.restartNode
  const [owned, setOwned] = useState(false)
  const [busy, setBusy] = useState(false)
  const [impact, setImpact] = useState<{ jobs: number; workers: number } | null>(null)

  useEffect(() => {
    if (!isDesktopApp()) return
    let active = true
    void getNodeApi().getPoolHosting().then((state) => { if (active) setOwned(state.managed) }).catch(() => { /* The card says why it cannot load. */ })
    return () => { active = false }
  }, [])

  async function restart() {
    setImpact(null)
    setBusy(true)
    try {
      const api = getNodeApi()
      const after = await api.restartNode(labels.reconnectTimeout)
      // The cards read their settings from the node, so they wait for it to
      // answer again before they reload.
      if (after.status !== 'connected') await waitForConnectedSnapshot(api, 30_000, labels.reconnectTimeout)
      toast.success(labels.restarted)
      onRestarted?.()
    } catch (error) {
      toast.error(labels.failed, { description: error instanceof Error ? error.message : String(error) })
    } finally { setBusy(false) }
  }

  async function check() {
    setBusy(true)
    const api = getNodeApi()
    // A node that coordinates no pool answers these with an error: it has
    // no jobs or workers of its own to interrupt.
    const [jobs, workers, snapshot] = await Promise.all([
      api.call<{ jobs?: { state?: string | number }[] }>('listJobs').then((r) => r.jobs ?? [], () => []),
      api.call<{ workers?: { peerId?: string }[] }>('listWorkers').then((r) => r.workers ?? [], () => []),
      api.getSnapshot().catch(() => null),
    ])
    const found = restartImpact(jobs, workers, snapshot?.info?.peerId)
    setBusy(false)
    if (found.jobs > 0 || found.workers > 0) setImpact(found)
    else await restart()
  }

  if (!owned) return null
  return <>
    <Button type="button" variant="outline" disabled={busy} onClick={() => void check()} className="gap-2">
      {busy ? <LoaderCircle className="size-4 animate-spin" /> : <RotateCcw className="size-4" />}{busy ? labels.restarting : labels.restart}
    </Button>
    <ResponsiveActionDialog open={impact !== null} direction={document.documentElement.dir === 'rtl' ? 'rtl' : 'ltr'} title={labels.confirmTitle} description={impact ? formatMessage(labels.confirmDescription, { jobs: impact.jobs, workers: impact.workers }) : undefined} icon={<TriangleAlert className="size-6 text-amber-600" />} onOpenChange={(open) => { if (!open) setImpact(null) }}>
      <div className="grid gap-2 sm:grid-cols-2">
        <Button type="button" variant="outline" onClick={() => setImpact(null)}>{messages.cancel}</Button>
        <Button type="button" variant="destructive" onClick={() => void restart()}>{labels.confirm}</Button>
      </div>
    </ResponsiveActionDialog>
  </>
}
