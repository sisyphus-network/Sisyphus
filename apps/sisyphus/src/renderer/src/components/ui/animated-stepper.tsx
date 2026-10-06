"use client";

import { AnimatePresence, motion, useReducedMotion } from "motion/react";
import { Check } from "lucide-react";
import type { ComponentType, ReactNode } from "react";
import { cn } from "@/lib/utils";

export type AnimatedStepperStep = {
  id: string;
  label: string;
  icon: ComponentType<{ className?: string }>;
};

export function AnimatedStepper({
  steps,
  currentIndex,
  children,
  direction,
  navigationDirection = 1,
  hideProgress = false,
  className,
}: {
  steps: AnimatedStepperStep[];
  currentIndex: number;
  children: ReactNode;
  direction: "rtl" | "ltr";
  navigationDirection?: 1 | -1;
  hideProgress?: boolean;
  className?: string;
}) {
  const reduceMotion = useReducedMotion();
  const progress = steps.length > 1 ? (currentIndex / (steps.length - 1)) * 100 : 0;
  const mobileProgress = steps.length > 0 ? ((currentIndex + 1) / steps.length) * 100 : 0;
  const localeDirection = direction === "rtl" ? -1 : 1;
  const motionDirection = navigationDirection * localeDirection;

  return (
    <div className={cn("flex h-full min-h-0 w-full flex-col", className)}>
      <motion.nav aria-label="Progress" animate={hideProgress ? { height: 0, opacity: 0, y: -18, paddingTop: 0, paddingBottom: 0 } : { height: "auto", opacity: 1, y: 0 }} transition={reduceMotion ? { duration: 0.12 } : { duration: 0.42, ease: [0.22, 1, 0.36, 1] }} className="mx-auto w-full max-w-3xl shrink-0 overflow-hidden my-2 md:my-1">
        <div className="flex items-center gap-3 lg:hidden" dir={direction}>
          <div
            className="onboarding-mobile-progress__track relative h-2 min-w-0 flex-1 overflow-hidden rounded-full bg-[var(--app-line)]"
            role="progressbar"
            aria-valuemin={1}
            aria-valuemax={steps.length}
            aria-valuenow={currentIndex + 1}
            aria-label={steps[currentIndex]?.label}
          >
            <motion.div
              className="onboarding-mobile-progress__fill absolute inset-y-0 start-0 rounded-full bg-[var(--app-accent)]"
              initial={false}
              animate={{ width: `${mobileProgress}%` }}
              transition={reduceMotion ? { duration: 0 } : { duration: 0.5, ease: [0.22, 1, 0.36, 1] }}
            />
          </div>
          <span className="shrink-0 text-xs font-semibold tabular-nums text-[var(--app-muted)]">{currentIndex + 1}/{steps.length}</span>
          <span className="max-w-[35%] truncate text-xs font-semibold text-[var(--app-ink)]">{steps[currentIndex]?.label}</span>
        </div>
        <ol className="relative hidden  lg:grid" style={{ gridTemplateColumns: `repeat(${steps.length}, minmax(0, 1fr))` }}>
          <div aria-hidden="true" className="absolute top-[1.15rem] h-px overflow-hidden bg-[var(--app-line)]" style={{ insetInline: `${50 / steps.length}%` }}>
            <motion.div
              className="h-full origin-left bg-[var(--app-accent)] rtl:origin-right"
              initial={false}
              animate={{ scaleX: progress / 100 }}
              transition={reduceMotion ? { duration: 0 } : { duration: 0.45, ease: [0.22, 1, 0.36, 1] }}
            />
          </div>
          {steps.map((step, index) => {
            const complete = index < currentIndex;
            const active = index === currentIndex;
            const Icon = step.icon;
            return (
              <li key={step.id} aria-current={active ? "step" : undefined} className="relative z-10 flex min-w-0 flex-col items-center gap-2">
                <motion.span
                  animate={active && !reduceMotion ? { scale: [1, 1.06, 1] } : { scale: 1 }}
                  transition={{ duration: 0.45, ease: [0.22, 1, 0.36, 1] }}
                  className={cn(
                    "grid size-9 place-items-center rounded-full border text-[var(--app-muted)] transition-[color,background-color,border-color,box-shadow] duration-300 sm:size-10",
                    complete && "border-[var(--app-accent)] bg-[var(--app-accent)] text-[var(--app-accent-ink)]",
                    active && "border-[var(--app-border)] bg-[var(--app-card)] text-[var(--app-ink)] shadow-[0_8px_24px_rgba(0,0,0,0.08)] dark:shadow-[0_8px_26px_rgba(0,0,0,0.35)]",
                    !complete && !active && "border-[var(--app-line)] bg-[var(--app-bg)]",
                  )}
                >
                  <AnimatePresence initial={false} mode="wait">
                    {complete ? (
                      <motion.span key="complete" initial={{ opacity: 0, scale: 0.5 }} animate={{ opacity: 1, scale: 1 }} exit={{ opacity: 0, scale: 0.5 }}>
                        <Check className="size-4" strokeWidth={2.4} />
                      </motion.span>
                    ) : (
                      <motion.span key="icon" initial={{ opacity: 0, scale: 0.7 }} animate={{ opacity: 1, scale: 1 }} exit={{ opacity: 0, scale: 0.7 }}>
                        <Icon className="size-4 sm:size-[18px]" />
                      </motion.span>
                    )}
                  </AnimatePresence>
                </motion.span>
                <span className={cn("max-w-full truncate text-[10px] font-medium text-[var(--app-muted)] transition-colors sm:text-xs", active && "font-semibold text-[var(--app-ink)]")}>{step.label}</span>
              </li>
            );
          })}
        </ol>
      </motion.nav>

      <div className="min-h-0 flex-1 overflow-hidden">
        <AnimatePresence initial={false} mode="wait" custom={direction}>
          <motion.div
            key={steps[currentIndex]?.id}
            custom={motionDirection}
            initial={reduceMotion ? { opacity: 0 } : { opacity: 0, x: motionDirection * 34, scale: 0.99 }}
            animate={{ opacity: 1, x: 0, scale: 1 }}
            exit={reduceMotion ? { opacity: 0 } : { opacity: 0, x: motionDirection * -28, scale: 0.99 }}
            transition={{ duration: reduceMotion ? 0.12 : 0.36, ease: [0.22, 1, 0.36, 1] }}
            className="h-full min-h-0"
          >
            {children}
          </motion.div>
        </AnimatePresence>
      </div>
    </div>
  );
}
