import { useState } from 'react'
import { LoaderCircle, RefreshCw, Unplug } from 'lucide-react'
import type { NodeSnapshot } from '../../../preload'
import type { SisyphusMessages } from '@/i18n/messages'
import { formatMessage } from '@/i18n/messages'
import { getNodeApi } from '@/lib/node-api'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/page-layout'
import { toast } from 'sonner'

export function NodeConnectionState({ snapshot, messages }: { snapshot: NodeSnapshot; messages: SisyphusMessages }) {
  const [busy, setBusy] = useState(false)
  const connecting = snapshot.status === 'connecting'
  const reconnect = async () => {
    setBusy(true)
    try { await getNodeApi().reconnect() }
    catch (error) { toast.error(messages.reconnect, { description: error instanceof Error ? error.message : String(error) }) }
    finally { setBusy(false) }
  }
  return <div className="flex h-full min-h-[65dvh] w-full items-center justify-center">
    <EmptyState icon={connecting ? LoaderCircle : Unplug} loading={connecting} title={connecting ? messages.connectingDaemon : messages.daemonUnavailable}
      description={formatMessage(messages.waitingForDaemon, { endpoint: snapshot.endpoint })}
      action={<Button variant="outline" disabled={connecting || busy} onClick={() => void reconnect()} className="gap-2"><RefreshCw className={`size-4 ${busy ? 'animate-spin' : ''}`} />{connecting || busy ? messages.connecting : messages.reconnect}</Button>} />
  </div>
}
