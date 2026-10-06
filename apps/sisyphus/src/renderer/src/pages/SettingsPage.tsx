import { useLayoutEffect, useRef } from 'react'
import { Globe2, Languages, Palette, ServerCog } from 'lucide-react'
import { LanguagePicker } from '@/components/language-picker'
import { ThemeModeSwitch } from '@/components/theme-mode-switch'
import { ThemeStylePicker } from '@/components/theme-style-picker'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import type { AppLocale } from '@/i18n/locales'
import { type getMessages } from '@/i18n/messages'

type Messages = ReturnType<typeof getMessages>
export function SettingsPage({ locale, setLocale, messages, direction }: { locale: AppLocale; setLocale: (locale: AppLocale) => void; messages: Messages; direction: 'ltr' | 'rtl' }) {
  const scrollRef = useRef<HTMLDivElement>(null)
  useLayoutEffect(() => {
    if (!window.matchMedia('(min-width: 761px)').matches) return
    const scroller = scrollRef.current
    if (!scroller) return
    const key = 'sisyphus-page-scroll-positions'
    const save = () => {
      try {
        const positions: unknown = JSON.parse(window.localStorage.getItem(key) ?? '{}')
        const stored = positions && typeof positions === 'object' ? positions as Record<string, unknown> : {}
        window.localStorage.setItem(key, JSON.stringify({ ...stored, '/settings': scroller.scrollTop }))
      } catch { /* Storage may be unavailable in restricted contexts. */ }
    }
    try {
      const positions: unknown = JSON.parse(window.localStorage.getItem(key) ?? '{}')
      if (positions && typeof positions === 'object' && typeof (positions as Record<string, unknown>)['/settings'] === 'number') {
        scroller.scrollTop = (positions as Record<string, number>)['/settings']
      }
    } catch { /* Ignore malformed saved scroll positions. */ }
    scroller.addEventListener('scroll', save, { passive: true })
    return () => {
      save()
      scroller.removeEventListener('scroll', save)
    }
  }, [])

  return <div className="settings-page">
    <section className="settings-page__heading py-8"><div className="text-[10px] font-semibold tracking-[0.14em] text-muted-foreground">{messages.preferences}</div><h1 className="mt-2 text-3xl font-semibold tracking-tight">{messages.settings}</h1><p className="mt-2 text-sm text-muted-foreground">{messages.settingsDescription}</p></section>
    <div ref={scrollRef} className="settings-page__scroll">
    <div className="grid max-w-3xl gap-4">
      <Card><CardHeader className="flex flex-row items-center gap-3 border-b border-[var(--app-line)] py-4"><div className="grid size-9 place-items-center rounded-xl bg-[var(--app-wash)]"><Globe2 className="size-4 text-muted-foreground" /></div><div><div className="text-[9px] font-semibold tracking-[0.13em] text-muted-foreground">{messages.node}</div><CardTitle className="mt-0.5 text-sm">{messages.localNodeSettings}</CardTitle></div></CardHeader><CardContent><div className="flex items-center gap-3 py-2"><div className="grid size-8 place-items-center rounded-lg border border-[var(--app-line)]"><ServerCog className="size-4 text-muted-foreground" /></div><div className="min-w-0 flex-1"><div className="text-xs font-medium">{messages.localDaemon}</div><div className="mt-1 text-[10px] text-muted-foreground">{messages.localDaemonSettingsHint}</div></div><span className="rounded-full border border-emerald-500/25 bg-emerald-500/5 px-2 py-1 text-[9px] text-emerald-700 dark:text-emerald-300">{messages.managedByDaemon}</span></div></CardContent></Card>
      <Card><CardHeader className="flex flex-row items-center gap-3 border-b border-[var(--app-line)] py-4"><div className="grid size-9 place-items-center rounded-xl bg-[var(--app-wash)]"><Languages className="size-4 text-muted-foreground" /></div><div><div className="text-[9px] font-semibold tracking-[0.13em] text-muted-foreground">{messages.language}</div><CardTitle className="mt-0.5 text-sm">{messages.languageSettings}</CardTitle></div></CardHeader><CardContent><div className="max-w-sm py-2"><LanguagePicker value={locale} label={messages.language} onChange={setLocale} /></div><p className="mt-2 text-[10px] text-muted-foreground">{messages.languageSettingsHint}</p></CardContent></Card>
      <Card><CardHeader className="flex flex-row items-center gap-3 border-b border-[var(--app-line)] py-4"><div className="grid size-9 place-items-center rounded-xl bg-[var(--app-wash)]"><Palette className="size-4 text-muted-foreground" /></div><div><div className="text-[9px] font-semibold tracking-[0.13em] text-muted-foreground">{messages.appearance}</div><CardTitle className="mt-0.5 text-sm">{messages.theme}</CardTitle></div></CardHeader><CardContent><div className="flex flex-wrap items-center justify-between gap-5 py-2"><div><div className="text-xs font-medium">{messages.theme}</div><p className="mt-1 text-[10px] text-muted-foreground">{messages.themeSettingsHint}</p></div><ThemeModeSwitch direction={direction} labels={{ system: messages.themeSystem, light: messages.themeLight, dark: messages.themeDark, switchTheme: messages.switchTheme }} /></div><div className="mt-3 flex flex-wrap items-center justify-between gap-5 border-t border-[var(--app-line)] pt-4"><div><div className="text-xs font-medium">{messages.accent}</div><p className="mt-1 text-[10px] text-muted-foreground">{messages.accentSettingsHint}</p></div><ThemeStylePicker direction={direction} labels={{ label: messages.accent, neutral: messages.accentNeutral, blue: messages.accentBlue, pink: messages.accentPink }} /></div></CardContent></Card>
    </div>
    </div>
  </div>
}
