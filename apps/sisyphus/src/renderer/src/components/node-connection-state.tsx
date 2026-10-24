import { useEffect, useRef, useState, type FormEvent } from 'react'
import { KeyRound, Link2, LoaderCircle, RefreshCw, ShieldAlert, Unplug } from 'lucide-react'
import { motion } from 'motion/react'
import { Tabs } from '@base-ui/react/tabs'
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
import { appIconUrl } from '@/lib/brand'

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
    <Tabs.Root defaultValue="pair" className="grid gap-6">
    <Tabs.List aria-label={messages.browserAuthTitle} className="auth-method-tabs relative mx-auto grid w-full max-w-[22rem] grid-cols-2">
      <Tabs.Tab value="pair" aria-label={messages.browserPairCodeTab} title={messages.browserPairCodeTab} className="auth-method-tab relative z-10 inline-flex items-center justify-center gap-2 px-3 py-2.5 text-xs font-medium transition-colors sm:text-sm"><Link2 aria-hidden="true" className="size-4 shrink-0" /><span className="hidden sm:inline">{messages.browserPairCodeTab}</span></Tabs.Tab>
      <Tabs.Tab value="token" aria-label={messages.browserAuthTokenLabel} title={messages.browserAuthTokenLabel} className="auth-method-tab relative z-10 inline-flex items-center justify-center gap-2 px-3 py-2.5 text-xs font-medium transition-colors sm:text-sm"><KeyRound aria-hidden="true" className="size-4 shrink-0" /><span className="hidden sm:inline">{messages.browserAuthTokenLabel}</span></Tabs.Tab>
      <Tabs.Indicator className="auth-method-indicator" />
    </Tabs.List>
    <Tabs.Panel value="pair" className="auth-method-panel grid gap-4">
      <div className="grid justify-items-center gap-2">
        <label className="sr-only" htmlFor="browser-pair-code">{messages.browserPairCodeLabel}</label>
        <InputOTP id="browser-pair-code" autoComplete="one-time-code" aria-label={messages.browserPairCodeLabel} maxLength={8} value={pairingCode} onChange={(value) => {
          setPairingCode(value)
          if (value.length === 8) void pairBrowserDevice(value)
        }} containerClassName="mx-auto w-full max-w-[22rem] justify-center" className="auth-otp-native-input" disabled={busy}>
          <div dir="ltr" className="auth-otp-slots flex w-full items-center justify-center gap-2">
            {Array.from({ length: 8 }, (_, index) => <InputOTPSlot key={index} index={index} className="auth-otp-slot size-7 shrink-0 text-base sm:size-9 sm:text-xl" />)}
          </div>
        </InputOTP>
        {busy && <span className="flex items-center gap-2 text-xs text-[var(--app-muted)]"><LoaderCircle className="size-3.5 shrink-0 animate-spin" />{messages.connecting}</span>}
      </div>
    </Tabs.Panel>
    <Tabs.Panel value="token" className="auth-method-panel">
      <form onSubmit={(event) => void connectBrowser(event)} className="mx-auto grid w-full max-w-[22rem] gap-3">
        <label className="grid gap-1.5 text-xs font-medium"><span>{messages.browserAuthTokenLabel}</span><Input type="password" autoComplete="current-password" dir="ltr" value={apiToken} onChange={(event) => setApiToken(event.target.value)} /></label>
        <Button type="submit" variant="outline" disabled={busy || !apiToken.trim()} className="gap-2">{busy ? <LoaderCircle className="size-4 shrink-0 animate-spin" /> : <RefreshCw className="size-4 shrink-0" />}{messages.browserAuthConnect}</Button>
      </form>
    </Tabs.Panel>
    </Tabs.Root>
    {window.location.protocol === 'http:' && <div role="note" className="auth-http-alert relative mx-auto w-full max-w-[22rem] px-4 pb-3.5 pt-7 text-center">
      <span aria-hidden="true" className="auth-http-alert__icon pointer-events-none absolute left-1/2 top-0 grid size-12 -translate-x-1/2 -translate-y-1/2 place-items-center rounded-full border">
        <ShieldAlert className="size-5 shrink-0" />
      </span>
      <p className="text-xs leading-relaxed">{messages.browserHttpWarning}</p>
    </div>}
  </div>

  if (fullScreen && browserAuthRequired) {
    return <main dir={direction ?? 'auto'} className="auth-route relative flex min-h-dvh w-full items-center justify-center overflow-hidden px-4 py-10 sm:px-6">
      <div aria-hidden="true" className="auth-route__wash absolute inset-0" />
      <div className="relative flex w-full justify-center">
        <motion.div className="w-full max-w-md" initial={{ opacity: 0, y: 14, filter: 'blur(8px)' }} animate={{ opacity: 1, y: 0, filter: 'blur(0px)' }} transition={{ duration: 0.55, ease: [0.22, 1, 0.36, 1] }}>
          <section aria-labelledby="browser-auth-title" className="relative px-1 py-5 sm:px-8 sm:py-9">
            <div className="mb-8 flex flex-col items-center text-center">
              <div className="relative mb-7 grid size-36 place-items-center sm:size-40">
                <img aria-hidden="true" src={appIconUrl} className={`size-full object-contain drop-shadow-[0_16px_34px_color-mix(in_srgb,var(--app-accent)_20%,transparent)] ${busy ? 'animate-pulse' : ''}`} />
                {busy && <LoaderCircle aria-label={messages.connecting} className="absolute size-7 shrink-0 animate-spin text-[var(--app-accent)]" />}
              </div>
              <h1 id="browser-auth-title" className="max-w-sm text-balance text-2xl font-semibold tracking-tight text-[var(--app-ink)] sm:text-[1.8rem]">{messages.browserAuthTitle}</h1>
            </div>
            <div className="text-[var(--app-ink)]">{authForm}</div>
          </section>
        </motion.div>
      </div>
    </main>
  }

  return <div className="flex h-full min-h-[65dvh] w-full items-center justify-center">
    <EmptyState icon={connecting ? LoaderCircle : Unplug} loading={connecting} title={connecting ? messages.connectingDaemon : messages.daemonUnavailable}
      description={browserAuthRequired ? messages.browserAuthWaiting : formatMessage(messages.waitingForDaemon, { endpoint: snapshot.endpoint })}
      action={browserAuthRequired ? authForm : <Button variant="outline" disabled={connecting || busy} onClick={() => void reconnect()} className="gap-2"><RefreshCw className={`size-4 shrink-0 ${busy ? 'animate-spin' : ''}`} />{connecting || busy ? messages.connecting : messages.reconnect}</Button>} />
  </div>
}
