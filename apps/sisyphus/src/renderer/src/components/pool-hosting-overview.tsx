import { useEffect, useState } from 'react'
import { Check, Copy, Network, ShieldCheck, TriangleAlert } from 'lucide-react'
import type { PoolHostingState } from '../../../preload'
import { getNodeApi, isDesktopApp } from '@/lib/node-api'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { formatMessage, type getMessages } from '@/i18n/messages'
import { hostingJoinAddresses, windowsFirewallCommand } from '../../../shared/pool-hosting.ts'
import { toast } from 'sonner'

type Messages = ReturnType<typeof getMessages>

function CopyLine({ text, label, copiedLabel }: { text: string; label: string; copiedLabel: string }) {
  const [copied, setCopied] = useState(false)
  async function copy() {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1600)
    } catch (error) {
      toast.error(label, { description: error instanceof Error ? error.message : String(error) })
    }
  }
  return <div className="flex items-start gap-2">
    <code dir="ltr" className="min-w-0 flex-1 break-all rounded-lg bg-[var(--app-wash)] px-3 py-2 text-xs">{text}</code>
    <Button type="button" variant="ghost" size="icon" className="size-8 shrink-0" aria-label={copied ? copiedLabel : label} title={copied ? copiedLabel : label} onClick={() => void copy()}>{copied ? <Check className="size-3.5 text-emerald-600" /> : <Copy className="size-3.5" />}</Button>
  </div>
}

/** What a worker needs to join the pool this app's node hosts: where, and on Windows the firewall rule. */
export function PoolHostingOverview({ messages }: { messages: Messages }) {
  const labels = messages.poolHostingOverview
  const [state, setState] = useState<PoolHostingState>()
  useEffect(() => {
    if (!isDesktopApp()) return
    let active = true
    void getNodeApi().getPoolHosting().then((result) => { if (active) setState(result) }).catch(() => { /* Settings reports load failures. */ })
    return () => { active = false }
  }, [])
  if (!state?.managed || (!state.running.enabled && !state.unavailable)) return null
  const addresses = hostingJoinAddresses(state)
  return <Card className="mt-4 overflow-hidden rounded-2xl border-[var(--app-line)]/75 bg-[var(--app-card)]/90 shadow-sm">
    <CardContent className="space-y-3 p-5">
      <div className="flex items-center gap-3">
        <span className="grid size-9 shrink-0 place-items-center rounded-xl border border-[var(--app-line)]/80 bg-[var(--app-card)] text-[var(--app-accent)]"><Network className="size-[17px]" /></span>
        <h2 className="text-sm font-semibold tracking-tight">{labels.title}</h2>
      </div>
      {state.unavailable ? <p role="alert" className="flex items-start gap-2 text-sm leading-relaxed text-destructive"><TriangleAlert className="mt-0.5 size-4 shrink-0" />{formatMessage(labels.unavailable, { address: state.running.address })}</p> : <>
        <p className="text-sm leading-relaxed text-muted-foreground">{labels.description}</p>
        {addresses.length === 0 ? <p className="text-sm text-muted-foreground">{labels.noAddresses}</p> : addresses.map((address) => <CopyLine key={address} text={address} label={labels.copyAddress} copiedLabel={labels.copied} />)}
        {state.platform === 'win32' ? <div className="space-y-2 border-t border-[var(--app-line)] pt-3">
          <p className="flex items-start gap-2 text-xs leading-relaxed text-muted-foreground"><ShieldCheck className="mt-0.5 size-3.5 shrink-0" />{labels.windowsFirewall}</p>
          <CopyLine text={windowsFirewallCommand(state.running.port)} label={labels.copyCommand} copiedLabel={labels.copied} />
        </div> : <p className="flex items-start gap-2 border-t border-[var(--app-line)] pt-3 text-xs leading-relaxed text-muted-foreground"><ShieldCheck className="mt-0.5 size-3.5 shrink-0" />{formatMessage(labels.firewall, { port: state.running.port })}</p>}
      </>}
    </CardContent>
  </Card>
}
