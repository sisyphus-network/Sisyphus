"use client";

import type { CSSProperties, ReactNode } from "react";
import { useLayoutEffect, useRef, useState } from "react";
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
  const dockRef = useRef<HTMLFieldSetElement>(null);
  const activeRef = useRef<HTMLSpanElement>(null);

  useLayoutEffect(() => {
    const dock = dockRef.current;
    const active = activeRef.current;
    if (!dock || !active) return;

    let frame = 0;
    const measure = () => {
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => {
        const option = dock.querySelectorAll<HTMLElement>(".liquid-dock__option")[visualIndex];
        if (!option) return;
        // The dock can be entering with a scale transform. Viewport bounds
        // include that transform, but the pill's CSS offsets are in layout
        // pixels; mixing the two leaves it misaligned after the animation.
        const optionWidth = option.offsetWidth;
        // Keep the selected glass pill visually substantial as the dock gains
        // items, while centering it on the actual option and staying in bounds.
        const width = optionWidth + (items.length > 4 ? 8 : 0);
        const centeredRight = dock.clientWidth - option.offsetLeft - optionWidth - (width - optionWidth) / 2;
        active.style.setProperty("--liquid-width", `${width}px`);
        active.style.setProperty("--liquid-offset", `${centeredRight}px`);
      });
    };

    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(dock);
    dock.querySelectorAll(".liquid-dock__option").forEach((option) => observer.observe(option));
    return () => {
      cancelAnimationFrame(frame);
      observer.disconnect();
    };
  }, [items.length, visualIndex]);

  return (
    <div className={`liquid-dock-wrap ${className ?? ""}`} dir="rtl">
      <fieldset ref={dockRef} className="liquid-dock" aria-label={label} data-active={activeIndex >= 0} data-previous={previousIndex}>
        <legend className="sr-only">{label}</legend>
        <span ref={activeRef} aria-hidden="true" className="liquid-dock__active" style={{ "--liquid-index": visualIndex } as CSSProperties} />
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
