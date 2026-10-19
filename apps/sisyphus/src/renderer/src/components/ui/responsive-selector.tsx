"use client";

import type { Dispatch, ReactElement, ReactNode, SetStateAction } from "react";
import { useSyncExternalStore } from "react";
import { Drawer } from "vaul";
import { Popover as PopoverPrimitive } from "@base-ui/react/popover";
import { cn } from "@/lib/utils";
import { useFloatingSelector } from "@/hooks/use-floating-selector";

const subscribeToCompactViewport = (callback: () => void) => {
  const mediaQuery = window.matchMedia("(max-width: 767px)");
  mediaQuery.addEventListener("change", callback);
  return () => mediaQuery.removeEventListener("change", callback);
};

const getCompactViewport = () => window.matchMedia("(max-width: 767px)").matches;

const focusableItems = (content: HTMLElement) => Array.from(content.querySelectorAll<HTMLElement>(
  'button:not(:disabled), a[href], input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex="0"]',
)).filter((item) => item.getClientRects().length > 0);

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
      {compact ? (
        <>
          {trigger(api)}
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
        </>
      ) : (
        <PopoverPrimitive.Root open={open} onOpenChange={setOpen} modal={false}>
          <PopoverPrimitive.Trigger nativeButton={false} render={trigger(api) as ReactElement} />
          <PopoverPrimitive.Portal>
            <PopoverPrimitive.Positioner side={placement} sideOffset={8} collisionPadding={8} style={{ display: "contents" }}>
              <PopoverPrimitive.Popup
                ref={contentRef}
                aria-label={label}
                dir={direction}
                style={floatingStyle}
                onKeyDown={(event) => {
                  if (event.key === "Escape") {
                    event.stopPropagation();
                    setOpen(false);
                  } else if (event.key === "Tab") {
                    const content = event.currentTarget;
                    const items = focusableItems(content);
                    const first = items[0];
                    const last = items.at(-1);
                    if (!first) {
                      event.preventDefault();
                      content.focus();
                    } else if (event.shiftKey && (document.activeElement === first || document.activeElement === content)) {
                      event.preventDefault();
                      last?.focus();
                    } else if (!event.shiftKey && document.activeElement === last) {
                      event.preventDefault();
                      first.focus();
                    }
                  }
                }}
                className={cn(
                  "liquid-glass-menu z-[1000] overflow-y-auto rounded-2xl p-2 outline-none duration-200 data-open:animate-in data-open:fade-in-0 data-open:zoom-in-95 data-closed:animate-out data-closed:fade-out-0 data-closed:zoom-out-95",
                  placement === "top" ? "origin-bottom" : "origin-top",
                )}
              >
                {children(api)}
              </PopoverPrimitive.Popup>
            </PopoverPrimitive.Positioner>
          </PopoverPrimitive.Portal>
        </PopoverPrimitive.Root>
      )}
    </div>
  );
}
