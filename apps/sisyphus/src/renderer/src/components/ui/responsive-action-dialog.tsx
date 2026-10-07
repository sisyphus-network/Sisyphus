"use client";

import type { ReactNode } from "react";
import { Drawer } from "vaul";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "@/components/ui/dialog";
import { useIsDesktop } from "@/hooks/use-mobile";
import { cn } from "@/lib/utils";

type ResponsiveActionDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onAfterClose?: () => void;
  direction: "rtl" | "ltr" | "auto";
  title: string;
  description?: string;
  icon: ReactNode;
  children: ReactNode;
  className?: string;
};

function DialogBody({ title, description, icon, children, drawer = false }: Pick<ResponsiveActionDialogProps, "title" | "description" | "icon" | "children"> & { drawer?: boolean }) {
  return (
    <div className="relative px-5 pb-5 pt-7 sm:px-6 sm:pb-6 sm:pt-8">
      <div className="pointer-events-none absolute inset-x-0 top-0 flex -translate-y-1/2 justify-center">
        <span className="grid size-14 place-items-center rounded-full border border-[var(--app-border)] bg-[var(--app-card)] text-[var(--app-accent)] shadow-[0_10px_24px_color-mix(in_srgb,var(--app-accent)_18%,transparent)]">
          {icon}
        </span>
      </div>
      <div className="pt-4 text-center">
        {drawer ? <Drawer.Title className="text-xl font-semibold tracking-[-0.035em]">{title}</Drawer.Title> : <DialogTitle className="text-xl font-semibold tracking-[-0.035em]">{title}</DialogTitle>}
        {description ? drawer ? <p className="mx-auto mt-2 max-w-sm text-sm leading-6 text-[var(--app-muted)]">{description}</p> : <DialogDescription className="mx-auto mt-2 max-w-sm text-sm leading-6">{description}</DialogDescription> : null}
      </div>
      <div className={cn("mt-6", !description && "mt-5")}>{children}</div>
    </div>
  );
}

export function ResponsiveActionDialog({ open, onOpenChange, onAfterClose, direction, title, description, icon, children, className }: ResponsiveActionDialogProps) {
  const isDesktop = useIsDesktop();

  if (isDesktop) {
    return (
      <Dialog open={open} onOpenChange={onOpenChange} onOpenChangeComplete={isOpen => { if (!isOpen && !open) onAfterClose?.(); }}>
        <DialogContent showCloseButton={false} className={cn("liquid-glass-menu w-[min(42rem,calc(100vw-2rem))] !max-w-2xl rounded-[1.6rem] border border-[var(--app-line)] bg-[var(--app-card)] p-0", className)}>
          <DialogBody title={title} description={description} icon={icon}>{children}</DialogBody>
        </DialogContent>
      </Dialog>
    );
  }

  return (
    <Drawer.Root open={open} onOpenChange={onOpenChange} onAnimationEnd={isOpen => { if (!isOpen && !open) onAfterClose?.(); }} direction="bottom">
      <Drawer.Portal>
        <Drawer.Overlay className="fixed inset-0 z-50 bg-black/25 backdrop-blur-sm" />
        <Drawer.Content dir={direction} className={cn("liquid-glass-menu fixed inset-x-0 bottom-0 z-50 rounded-t-[1.6rem] bg-[var(--app-card)] pb-[max(1rem,env(safe-area-inset-bottom))] outline-none", className)}>
          <div aria-hidden="true" className="mx-auto mt-3 h-1 w-10 rounded-full bg-[var(--app-border)]" />
          <DialogBody title={title} description={description} icon={icon} drawer>{children}</DialogBody>
        </Drawer.Content>
      </Drawer.Portal>
    </Drawer.Root>
  );
}
