"use client";

import { Monitor, Moon, Sun } from "lucide-react";
import { motion } from "motion/react";
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

  return (
    <div dir={direction} role="group" aria-label={`${labels.switchTheme}: ${labels[mode]}`} className="grid w-full min-w-0 grid-cols-3 rounded-full border border-[var(--app-line)] bg-[var(--app-wash)]/70 p-1">
      {modes.map((option) => {
        const active = option === mode;
        return <button key={option} type="button" aria-label={labels[option]} aria-pressed={active} title={labels[option]} onClick={() => setThemeMode(option)} className={`relative z-0 flex h-10 min-w-0 items-center justify-center gap-2 rounded-full px-2 text-xs font-medium transition-colors duration-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--app-accent)]/60 ${active ? "text-[var(--app-accent-ink)]" : "text-[var(--app-muted)] hover:text-[var(--app-ink)]"}`}>
          {active && <motion.span layoutId="theme-mode-slider" aria-hidden="true" className="absolute inset-0 -z-10 rounded-full bg-[var(--app-accent)]" transition={{ type: "spring", stiffness: 420, damping: 34 }} />}
          <span className="relative shrink-0"><ModeIcon mode={option} /></span><span aria-hidden="true" className="relative hidden truncate min-[768px]:inline">{labels[option]}</span>
        </button>
      })}
    </div>
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
