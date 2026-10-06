"use client";

import { Monitor, Moon, Sun } from "lucide-react";
import { AnimatePresence, motion } from "motion/react";
import { useEffect, useSyncExternalStore } from "react";

type ThemeMode = "system" | "light" | "dark";
export type ThemeLabels = { system: string; light: string; dark: string; switchTheme: string };
const modes: ThemeMode[] = ["system", "light", "dark"];

function getStoredMode(): ThemeMode {
  const saved = window.localStorage.getItem("sisyphus-theme");
  return saved === "light" || saved === "dark" || saved === "system" ? saved : "system";
}

function subscribeToTheme(onChange: () => void) {
  window.addEventListener("storage", onChange);
  window.addEventListener("sisyphus-theme-change", onChange);
  return () => {
    window.removeEventListener("storage", onChange);
    window.removeEventListener("sisyphus-theme-change", onChange);
  };
}

const getServerMode = (): ThemeMode => "system";

function ModeIcon({ mode }: { mode: ThemeMode }) {
  const Icon = mode === "light" ? Sun : mode === "dark" ? Moon : Monitor;
  return <Icon size={16} strokeWidth={1.8} />;
}

function applyTheme(nextMode: ThemeMode) {
  const effectiveMode = nextMode === "system"
    ? window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light"
    : nextMode;
  document.documentElement.dataset.sisyphusThemeMode = nextMode;
  document.documentElement.classList.toggle("dark", effectiveMode === "dark");
  if (nextMode === "system") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.dataset.theme = nextMode;
}

export function initializeThemeMode() {
  const sync = () => applyTheme(getStoredMode())
  const onStorage = (event: StorageEvent) => {
    if (event.key === 'sisyphus-theme' || event.key === null) sync()
  }
  const media = window.matchMedia('(prefers-color-scheme: dark)')
  const onSystemChange = () => {
    if (getStoredMode() === 'system') sync()
  }
  sync()
  window.addEventListener('storage', onStorage)
  media.addEventListener('change', onSystemChange)
  return () => {
    window.removeEventListener('storage', onStorage)
    media.removeEventListener('change', onSystemChange)
  }
}

export function setThemeMode(nextMode: ThemeMode) {
  window.localStorage.setItem("sisyphus-theme", nextMode);
  applyTheme(nextMode);
  window.dispatchEvent(new Event("sisyphus-theme-change"));
}

export function ThemeModeSwitch({ labels, direction }: { labels: ThemeLabels; direction: "ltr" | "rtl" }) {
  const mode = useSyncExternalStore(subscribeToTheme, getStoredMode, getServerMode);

  useEffect(() => {
    applyTheme(mode);
    document.documentElement.dataset.sisyphusThemeHydrated = "true";
    return () => document.documentElement.removeAttribute("data-sisyphus-theme-hydrated");
  }, [mode]);

  useEffect(() => {
    if (mode !== "system") return;
    const media = window.matchMedia("(prefers-color-scheme: dark)");
    const syncSystemTheme = () => applyTheme("system");
    media.addEventListener("change", syncSystemTheme);
    return () => media.removeEventListener("change", syncSystemTheme);
  }, [mode]);

  function cycleTheme() {
    const nextMode = modes[(modes.indexOf(mode) + 1) % modes.length];
    setThemeMode(nextMode);
  }

  const textOffset = direction === "rtl" ? -8 : 8;
  const textAlignment = direction === "rtl" ? "text-right" : "text-left";
  return (
    <button type="button" onClick={cycleTheme} aria-label={`${labels.switchTheme}: ${labels[mode]}`} title={labels[mode]} className={`liquid-glass-avatar group relative inline-flex h-9 items-center gap-2 overflow-hidden rounded-full px-2.5 text-xs text-[var(--app-muted)] transition-[transform,color] hover:scale-[1.02] hover:text-[var(--app-ink)] focus-visible:ring-2 focus-visible:ring-[var(--app-accent)]/60 active:scale-[0.97] ${direction === "rtl" ? "flex-row-reverse" : ""}`}>
      <span className="relative grid size-6 place-items-center overflow-hidden rounded-full text-[var(--app-ink)]">
        <span data-theme-mode="system" className="theme-mode-preload-icon absolute"><ModeIcon mode="system" /></span>
        <span data-theme-mode="light" className="theme-mode-preload-icon absolute"><ModeIcon mode="light" /></span>
        <span data-theme-mode="dark" className="theme-mode-preload-icon absolute"><ModeIcon mode="dark" /></span>
        <AnimatePresence mode="wait" initial={false}><motion.span key={mode} initial={{ opacity: 0, y: 8, rotate: -24, scale: 0.72 }} animate={{ opacity: 1, y: 0, rotate: 0, scale: 1 }} exit={{ opacity: 0, y: -8, rotate: 24, scale: 0.72 }} transition={{ duration: 0.22, ease: [0.22, 1, 0.36, 1] }} className="theme-mode-react absolute inline-flex"><ModeIcon mode={mode} /></motion.span></AnimatePresence>
      </span>
      <span data-theme-mode="system" className={`theme-mode-preload-label hidden min-w-12 sm:inline ${textAlignment}`}>{labels.system}</span>
      <span data-theme-mode="light" className={`theme-mode-preload-label hidden min-w-12 sm:inline ${textAlignment}`}>{labels.light}</span>
      <span data-theme-mode="dark" className={`theme-mode-preload-label hidden min-w-12 sm:inline ${textAlignment}`}>{labels.dark}</span>
      <AnimatePresence mode="wait" initial={false}><motion.span key={mode} initial={{ opacity: 0, x: textOffset }} animate={{ opacity: 1, x: 0 }} exit={{ opacity: 0, x: -textOffset }} transition={{ duration: 0.2 }} className={`theme-mode-react hidden min-w-12 sm:inline ${textAlignment}`}>{labels[mode]}</motion.span></AnimatePresence>
    </button>
  );
}

export function ThemeModePreference({ labels, direction }: { labels: ThemeLabels; direction: "ltr" | "rtl" }) {
  const mode = useSyncExternalStore(subscribeToTheme, getStoredMode, getServerMode);

  return (
    <div dir={direction} className="grid grid-cols-3 gap-3">
      {modes.map((option) => {
        const active = option === mode;
        return (
          <button
            type="button"
            key={option}
            aria-pressed={active}
            onClick={() => setThemeMode(option)}
            className={`flex min-h-24 flex-col items-center justify-center gap-2 rounded-2xl border text-sm font-medium transition-[transform,background-color,border-color,color] duration-200 active:scale-[0.97] sm:min-h-28 ${active ? "border-[var(--app-accent)] bg-[var(--app-accent)] text-[var(--app-accent-ink)]" : "border-[var(--app-line)] bg-[var(--app-card)] text-[var(--app-muted)] hover:border-[var(--app-border)] hover:bg-[var(--app-wash)] hover:text-[var(--app-ink)]"}`}
          >
            <ModeIcon mode={option} />
            <span>{labels[option]}</span>
          </button>
        );
      })}
    </div>
  );
}
