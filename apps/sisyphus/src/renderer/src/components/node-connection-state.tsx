import { useEffect, useRef, useState, type FormEvent } from 'react'
import { LoaderCircle, RefreshCw, ShieldCheck, Unplug } from 'lucide-react'
import { motion } from 'motion/react'
import type { NodeSnapshot } from '../../../preload'
import type { SisyphusMessages } from '@/i18n/messages'
import { formatMessage } from '@/i18n/messages'
import { getNodeApi } from '@/lib/node-api'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/page-layout'
import { toast } from 'sonner'
import { pairBrowser, startBrowserSession } from '@sisyphus/client'
import { isDesktopApp } from '@/lib/node-api'
import { Input } from '@/components/ui/input'
import { InputOTP, InputOTPSlot } from '@/components/ui/input-otp'
import { getBrowserDeviceName } from '@/lib/browser-device-name'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'

export function NodeConnectionState({ snapshot, messages, onConnected, direction, fullScreen = false }: { snapshot: NodeSnapshot; messages: SisyphusMessages; onConnected: (snapshot: NodeSnapshot) => void; direction?: 'ltr' | 'rtl'; fullScreen?: boolean }) {
  const [busy, setBusy] = useState(false)
  const [apiToken, setApiToken] = useState('')
  const [pairingCode, setPairingCode] = useState('')
  const lastPairingErrorRef = useRef<string | null>(null)
  const connecting = snapshot.status === 'connecting'
  const browserAuthRequired = snapshot.authRequired && !isDesktopApp()
  useEffect(() => {
    if (!snapshot.error?.startsWith('Pairing failed:') || snapshot.error === lastPairingErrorRef.current) return
    lastPairingErrorRef.current = snapshot.error
    toast.error(messages.browserPair, { description: snapshot.error.slice('Pairing failed:'.length).trim() })
  }, [messages.browserPair, snapshot.error])
  const reconnect = async () => {
    setBusy(true)
    try { await getNodeApi().reconnect() }
    catch (error) { toast.error(messages.reconnect, { description: error instanceof Error ? error.message : String(error) }) }
    finally { setBusy(false) }
  }
  const connectBrowser = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setBusy(true)
    try {
      await startBrowserSession(apiToken)
      setApiToken('')
      const next = await getNodeApi().getSnapshot()
      onConnected(next)
    } catch (error) {
      toast.error(messages.browserAuthConnect, { description: error instanceof Error ? error.message : String(error) })
    } finally {
      setBusy(false)
    }
  }
  const pairBrowserDevice = async (code: string) => {
    setBusy(true)
    try {
      await pairBrowser(code, getBrowserDeviceName())
      setPairingCode('')
      const next = await getNodeApi().getSnapshot()
      onConnected(next)
    } catch (error) {
      setPairingCode('')
      toast.error(messages.browserPair, { description: error instanceof Error ? error.message : String(error) })
    } finally { setBusy(false) }
  }
  const authForm = <div className="grid gap-5">
    <div className="grid justify-items-center gap-2">
      <label className="sr-only" htmlFor="browser-pair-code">{messages.browserPairCodeLabel}</label>
      <InputOTP id="browser-pair-code" autoComplete="one-time-code" aria-label={messages.browserPairCodeLabel} maxLength={8} value={pairingCode} onChange={(value) => {
        setPairingCode(value)
        if (value.length === 8) void pairBrowserDevice(value)
      }} containerClassName="mx-auto w-full max-w-[22rem] flex-wrap justify-center sm:max-w-none" disabled={busy}>
        <div dir="ltr" className="flex flex-wrap items-center justify-center gap-1 sm:gap-2">
          {Array.from({ length: 8 }, (_, index) => <InputOTPSlot key={index} index={index} className="auth-otp-slot size-7 shrink-0 rounded-lg text-sm font-semibold shadow-sm outline-none transition sm:size-9 sm:rounded-xl sm:text-lg md:size-11 md:rounded-2xl md:text-xl" />)}
        </div>
      </InputOTP>
      {busy && <span className="flex items-center gap-2 text-xs text-slate-300"><LoaderCircle className="size-3.5 animate-spin" />{messages.connecting}</span>}
    </div>
    <p role="note" className="text-center text-[11px] leading-relaxed text-[var(--app-muted)]">{messages.browserHttpWarning}</p>
    <details className="border-t border-[var(--app-line)] pt-4 text-start">
      <summary className="cursor-pointer text-xs text-[var(--app-muted)]">{messages.browserTokenAlternative}</summary>
      <form onSubmit={(event) => void connectBrowser(event)} className="mt-3 grid gap-3">
        <label className="grid gap-1.5 text-xs font-medium"><span>{messages.browserAuthTokenLabel}</span><Input type="password" autoComplete="current-password" dir="ltr" value={apiToken} onChange={(event) => setApiToken(event.target.value)} /></label>
        <Button type="submit" variant="outline" disabled={busy || !apiToken.trim()} className="gap-2">{busy ? <LoaderCircle className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}{messages.browserAuthConnect}</Button>
      </form>
    </details>
  </div>

  if (fullScreen && browserAuthRequired) {
    return <main dir={direction ?? 'auto'} className="auth-route relative flex min-h-dvh w-full items-center justify-center overflow-hidden px-4 py-10 sm:px-6">
      <div aria-hidden="true" className="auth-route__wash absolute inset-0" />
      <div aria-hidden="true" className="auth-route__grid absolute inset-0" />
      <div aria-hidden="true" className="auth-route__glow absolute left-1/2 top-1/2 h-[28rem] w-[28rem] -translate-x-1/2 -translate-y-1/2 rounded-full blur-3xl" />
      <div className="relative flex w-full justify-center">
      <motion.div className="w-full max-w-lg" initial={{ opacity: 0, y: 18, scale: 0.98 }} animate={{ opacity: 1, y: 0, scale: 1 }} transition={{ duration: 0.45, ease: 'easeOut' }}>
        <Card className="auth-route__card overflow-visible rounded-[2rem] py-5 backdrop-blur-xl">
          <CardHeader className="items-center space-y-4 text-center">
            <div className="auth-route__mark mx-auto flex size-14 items-center justify-center rounded-2xl">
              {busy ? <LoaderCircle className="size-7 animate-spin" /> : <ShieldCheck className="size-7" />}
            </div>
            <div className="grid gap-2">
              <CardTitle className="text-3xl font-semibold tracking-tight text-[var(--app-ink)]">{messages.browserAuthTitle}</CardTitle>
              <CardDescription className="mt-3 text-base leading-7 text-[var(--app-muted)]">{messages.browserAuthDescription}</CardDescription>
            </div>
          </CardHeader>
          <CardContent className="mt-2 text-[var(--app-ink)]">
            <div className="space-y-5">
              {authForm}
            </div>
          </CardContent>
        </Card>
      </motion.div>
      </div>
    </main>
  }

  return <div className="flex h-full min-h-[65dvh] w-full items-center justify-center">
    <EmptyState icon={connecting ? LoaderCircle : Unplug} loading={connecting} title={connecting ? messages.connectingDaemon : messages.daemonUnavailable}
      description={browserAuthRequired ? messages.browserAuthWaiting : formatMessage(messages.waitingForDaemon, { endpoint: snapshot.endpoint })}
      action={browserAuthRequired ? authForm : <Button variant="outline" disabled={connecting || busy} onClick={() => void reconnect()} className="gap-2"><RefreshCw className={`size-4 ${busy ? 'animate-spin' : ''}`} />{connecting || busy ? messages.connecting : messages.reconnect}</Button>} />
  </div>
}
