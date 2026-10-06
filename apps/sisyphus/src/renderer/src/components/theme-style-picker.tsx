"use client";

import { useSyncExternalStore } from "react";
import { Check } from "lucide-react";
import { FloatingLabelInput } from "@/components/ui/floating-label-input";
import { cn } from "@/lib/utils";
import { ResponsiveSelector } from "@/components/ui/responsive-selector";

export type ThemeStyle = "neutral" | "blue" | "pink";

type ThemeStyleLabels = {
  label: string;
  neutral: string;
  blue: string;
  pink: string;
};

const styles: ThemeStyle[] = ["neutral", "blue", "pink"];

const subscribeToThemeStyle = (callback: () => void) => {
  window.addEventListener("storage", callback);
  window.addEventListener("sisyphus-theme-style-change", callback);
  return () => {
    window.removeEventListener("storage", callback);
    window.removeEventListener("sisyphus-theme-style-change", callback);
  };
};

const getStoredThemeStyle = (): ThemeStyle => {
  const stored = window.localStorage.getItem("sisyphus-theme-style");
  return stored === "blue" || stored === "pink" ? stored : "neutral";
};

function applyThemeStyle(style: ThemeStyle) {
  document.documentElement.dataset.themeStyle = style;
}

function setThemeStyle(style: ThemeStyle) {
  window.localStorage.setItem("sisyphus-theme-style", style);
  applyThemeStyle(style);
  window.dispatchEvent(new Event("sisyphus-theme-style-change"));
}

export function initializeThemeStyle() {
  const sync = () => applyThemeStyle(getStoredThemeStyle())
  const onStorage = (event: StorageEvent) => {
    if (event.key === 'sisyphus-theme-style' || event.key === null) sync()
  }
  sync()
  window.addEventListener('storage', onStorage)
  return () => window.removeEventListener('storage', onStorage)
}

function StyleSwatch({ style, active = false }: { style: ThemeStyle; active?: boolean }) {
  const color = style === "neutral" ? "var(--app-ink)" : style === "blue" ? "#4f8cff" : "#ed6ba8";

  return (
    <span aria-hidden="true" className={cn("relative grid size-10 shrink-0 place-items-center transition-transform duration-300", active && "scale-105 drop-shadow-[0_5px_10px_color-mix(in_srgb,var(--app-accent)_24%,transparent)]")}>
      <svg viewBox="0 0 56 36" className="size-full overflow-visible" fill="none" role="presentation">
        <path d="M7 25C13 12 21 9 29 13C35 16 40 18 50 8" stroke={color} strokeWidth="7" strokeLinecap="round" />
        <path d="M9 27C17 20 23 19 30 22" stroke={color} strokeOpacity="0.28" strokeWidth="3" strokeLinecap="round" />
        <circle cx="47" cy="26" r="2.5" fill={color} fillOpacity="0.82" />
        <circle cx="53" cy="22" r="1.5" fill={color} fillOpacity="0.5" />
      </svg>
    </span>
  );
}

function ThemeStylePicker({ direction, labels }: { direction: "rtl" | "ltr"; labels: ThemeStyleLabels }) {
  const value = useSyncExternalStore(subscribeToThemeStyle, getStoredThemeStyle, () => "neutral" as ThemeStyle);
  const selectedLabel = labels[value];

  return <ResponsiveSelector
    label={labels.label}
    direction={direction}
    trigger={({ open, setOpen }) => <FloatingLabelInput id="settings-theme-style" label={labels.label} value={selectedLabel} readOnly onClick={() => setOpen((current) => !current)} aria-haspopup="dialog" aria-expanded={open} icon={<StyleSwatch style={value} active />} className="cursor-pointer pe-10" />}
  >
    {({ setOpen }) => <div className="grid gap-1">{styles.map((style) => <button type="button" key={style} onClick={() => { setThemeStyle(style); setOpen(false); }} className={cn("flex w-full items-center justify-between rounded-xl px-3 py-3 text-sm transition-colors hover:bg-[var(--app-wash)]", value === style && "bg-[var(--app-wash)] font-semibold")}><span className="flex min-w-0 items-center gap-3"><StyleSwatch style={style} active={value === style} /><span>{labels[style]}</span></span>{value === style ? <Check className="size-4 shrink-0" /> : null}</button>)}</div>}
  </ResponsiveSelector>;
}

export { ThemeStylePicker, setThemeStyle, applyThemeStyle };
