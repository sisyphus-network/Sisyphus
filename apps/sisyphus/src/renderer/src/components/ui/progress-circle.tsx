import { cn } from "@/lib/utils";

type ProgressCircleProps = {
  value: number;
  className?: string;
  innerClassName?: string;
};

export function ProgressCircle({ value, className, innerClassName }: ProgressCircleProps) {
  const percentage = Math.max(0, Math.min(100, value));

  return (
    <div aria-hidden="true" className={cn("grid shrink-0 place-items-center rounded-full", className)} style={{ background: `conic-gradient(var(--app-accent) ${percentage}%, var(--app-line) 0)` }}>
      <div className={cn("rounded-full bg-[var(--app-card)]", innerClassName)} />
    </div>
  );
}
