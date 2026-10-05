"use client";

import type { Dispatch, ReactNode, SetStateAction } from "react";
import { useSyncExternalStore } from "react";
import { createPortal } from "react-dom";
import { Drawer } from "vaul";
import { AnimatePresence, motion } from "motion/react";
import { cn } from "@/lib/utils";
import { useFloatingSelector } from "@/hooks/use-floating-selector";

const subscribeToCompactViewport = (callback: () => void) => {
  const mediaQuery = window.matchMedia("(max-width: 767px)");
  mediaQuery.addEventListener("change", callback);
  return () => mediaQuery.removeEventListener("change", callback);
};

const getCompactViewport = () => window.matchMedia("(max-width: 767px)").matches;

type SelectorApi = {
  open: boolean;
  setOpen: Dispatch<SetStateAction<boolean>>;
};

type ResponsiveSelectorProps = {
  label: string;
  direction: "rtl" | "ltr" | "auto";
  trigger: (api: SelectorApi) => ReactNode;
  children: (api: SelectorApi) => ReactNode;
  className?: string;
};

export function ResponsiveSelector({ label, direction, trigger, children, className }: ResponsiveSelectorProps) {
  const { rootRef, contentRef, open, setOpen, placement, floatingStyle } = useFloatingSelector();
  const compact = useSyncExternalStore(subscribeToCompactViewport, getCompactViewport, () => false);
  const api = { open, setOpen };

  return (
    <div ref={rootRef} className={cn("relative", className)}>
      {trigger(api)}
      {compact ? (
        <Drawer.Root open={open} onOpenChange={setOpen} direction="bottom">
          <Drawer.Portal>
            <Drawer.Overlay className="fixed inset-0 z-50 bg-black/25 backdrop-blur-sm" />
            <Drawer.Content ref={contentRef} dir={direction} className="liquid-glass-menu fixed inset-x-0 bottom-0 z-50 max-h-[82dvh] rounded-t-[1.5rem] p-4 pb-[max(1rem,env(safe-area-inset-bottom))] outline-none">
              <div aria-hidden="true" className="mx-auto mb-4 h-1 w-10 rounded-full bg-[var(--app-border)]" />
              <Drawer.Title className="px-3 pb-2 text-base font-semibold">{label}</Drawer.Title>
              <div data-vaul-no-drag className="max-h-[68dvh] overflow-y-auto">{children(api)}</div>
            </Drawer.Content>
          </Drawer.Portal>
        </Drawer.Root>
      ) : (
        typeof document === "undefined" ? null : createPortal(
          <AnimatePresence initial={false}>
            {open ? (
              <motion.div
                ref={contentRef}
                key="responsive-selector"
                role="dialog"
                aria-label={label}
                style={floatingStyle}
                initial={{ opacity: 0, y: placement === "top" ? 10 : -10, scale: 0.97 }}
                animate={{ opacity: 1, y: 0, scale: 1 }}
                exit={{ opacity: 0, y: placement === "top" ? 6 : -6, scale: 0.98 }}
                transition={{ duration: 0.22, ease: [0.22, 1, 0.36, 1] }}
                className={cn(
                  "liquid-glass-menu fixed z-[1000] overflow-y-auto rounded-2xl p-2",
                  placement === "top" ? "origin-bottom" : "origin-top",
                )}
              >
                {children(api)}
              </motion.div>
            ) : null}
          </AnimatePresence>,
          document.body,
        )
      )}
    </div>
  );
}
