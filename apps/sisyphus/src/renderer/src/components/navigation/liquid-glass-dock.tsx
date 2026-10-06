"use client";

import type { CSSProperties, ReactNode } from "react";
import { useState } from "react";
import { AnimatePresence, motion } from "motion/react";

export type LiquidDockItem = {
  id: string;
  label: string;
  href: string;
  icon: ReactNode;
  active?: boolean;
};

/** Liquid-glass navigation dock used by the Sisyphus renderer. */
export function LiquidGlassDock({
  items,
  label,
  onNavigate,
  className,
}: {
  items: LiquidDockItem[];
  label: string;
  onNavigate?: (id: string, href: string) => void;
  className?: string;
}) {
  const activeIndex = items.findIndex((item) => item.active);
  const visualIndex = activeIndex >= 0 ? activeIndex : 0;
  const [previousIndex, setPreviousIndex] = useState(visualIndex);

  return (
    <div className={`liquid-dock-wrap ${className ?? ""}`} dir="rtl">
      <fieldset className="liquid-dock" aria-label={label} data-active={activeIndex >= 0} data-previous={previousIndex}>
        <legend className="sr-only">{label}</legend>
        <span aria-hidden="true" className="liquid-dock__active" style={{ "--liquid-index": visualIndex } as CSSProperties} />
        {items.map((item) => (
          <label key={item.id} className="liquid-dock__option" data-active={item.active ? "true" : "false"}>
            <input className="liquid-dock__input" type="radio" name={`sisyphus-navigation-${label}`} checked={item.active} readOnly aria-label={item.label} />
            <button type="button" className="liquid-dock__link" aria-label={item.label} title={item.label} onClick={() => {
              setPreviousIndex(activeIndex >= 0 ? activeIndex : visualIndex);
              onNavigate?.(item.id, item.href);
            }} />
            <span className="liquid-dock__icon" aria-hidden="true">
              <AnimatePresence mode="wait" initial={false}>
                <motion.span key={`${item.id}-icon`} initial={{ opacity: 0, scale: 0.7, rotate: -10 }} animate={{ opacity: 1, scale: 1, rotate: 0 }} exit={{ opacity: 0, scale: 0.7, rotate: 10 }} transition={{ duration: 0.16, ease: [0.22, 1, 0.36, 0.1] }} className="relative -top-px inline-flex items-center justify-center align-middle">
                  {item.icon}
                </motion.span>
              </AnimatePresence>
            </span>
          </label>
        ))}
      </fieldset>
    </div>
  );
}
