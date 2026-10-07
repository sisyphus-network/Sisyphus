import type { LucideIcon } from "lucide-react";
import { Skeleton } from "@/components/ui/skeleton";

export function WorkspaceHeading({ icon: Icon, eyebrow, title, description }: {
  icon: LucideIcon;
  eyebrow: string;
  title: string;
  description?: string;
}) {
  return (
    <div className="mx-auto flex max-w-2xl flex-col items-center text-center">
      <div aria-hidden="true" className="mb-5 flex items-center gap-4">
        <span className="w-10 border-t border-dashed border-[var(--app-border)] sm:w-16" />
        <span className="relative grid size-14 shrink-0 place-items-center rounded-[1.3rem] border border-[var(--app-border)] bg-[var(--app-accent-soft)] text-[var(--app-accent)] sm:size-16 sm:rounded-[1.5rem]">
          <Icon className="size-7 rtl:-scale-x-100 sm:size-8" strokeWidth={1.5} />
        </span>
        <span className="w-10 border-t border-dashed border-[var(--app-border)] sm:w-16" />
      </div>
      <p className="text-xs font-medium uppercase tracking-[0.22em] text-[var(--app-muted)]">{eyebrow}</p>
      <h1 className="mt-3 text-balance text-4xl font-semibold tracking-[-0.055em] sm:text-6xl">{title}</h1>
      {/* {description ? <p className="mt-3 max-w-xl text-pretty text-sm leading-7 text-[var(--app-muted)] sm:text-base">{description}</p> : null} */}
    </div>
  );
}

export function WorkspaceHeadingSkeleton() {
  return <div className="flex flex-col items-center"><Skeleton className="size-14 rounded-[1.3rem] sm:size-16" /><Skeleton className="mt-5 h-3 w-24" /><Skeleton className="mt-4 h-12 w-full max-w-64 sm:h-16 sm:max-w-96" /><Skeleton className="mt-3 h-4 w-full max-w-lg" /></div>;
}
