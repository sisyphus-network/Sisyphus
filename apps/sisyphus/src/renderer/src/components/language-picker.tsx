import { Check } from "lucide-react";
import { FloatingLabelInput } from "@/components/ui/floating-label-input";
import { LocaleFlag } from "@/components/ui/locale-flag";
import { ResponsiveSelector } from "@/components/ui/responsive-selector";
import { localeOptions, type AppLocale } from "@/i18n/locales";
import { cn } from "@/lib/utils";

export function LanguagePicker({
  value,
  label,
  onChange,
  loading = false,
}: {
  value: AppLocale;
  label: string;
  onChange: (value: AppLocale) => void;
  loading?: boolean;
}) {
  const options = localeOptions
    .map((option) => ({ value: option.code, label: option.nativeName, direction: option.direction }))
    .sort((a, b) => {
      const preferredOrder = ["he", "ar", "en"];
      const aPreferred = preferredOrder.indexOf(a.value);
      const bPreferred = preferredOrder.indexOf(b.value);
      if (aPreferred !== -1 || bPreferred !== -1) {
        if (aPreferred === -1) return 1;
        if (bPreferred === -1) return -1;
        return aPreferred - bPreferred;
      }
      return a.label.localeCompare(b.label, a.value);
    });
  const selectedLabel = options.find((option) => option.value === value)?.label;

  return (
    <ResponsiveSelector
      label={label}
      direction="auto"
      trigger={({ open, setOpen }) => (
        <FloatingLabelInput
          id="settings-language"
          label={label}
          value={selectedLabel}
          readOnly
          role="button"
          onClick={() => setOpen((current) => !current)}
          aria-haspopup="dialog"
          aria-expanded={open}
          aria-busy={loading}
          loading={loading}
          icon={<LocaleFlag locale={value} size="m" label={selectedLabel} />}
          className="cursor-pointer pe-10"
        />
      )}
    >
      {({ setOpen }) => (
        <div className="grid gap-1">
          {options.map((option) => (
            <button
              type="button"
              key={option.value}
              disabled={loading}
              dir={option.direction}
              onClick={() => {
                if (option.value !== value) onChange(option.value);
                setOpen(false);
              }}
              className={cn(
                "flex w-full flex-row items-center justify-between rounded-xl px-3 py-3 text-start text-sm transition-colors hover:bg-[var(--app-wash)] disabled:pointer-events-none disabled:opacity-60",
                value === option.value && "bg-[var(--app-wash)] font-semibold",
              )}
            >
              <span className="flex min-w-0 flex-row items-center gap-3">
                <LocaleFlag locale={option.value} size="m" label={option.label} />
                <span>{option.label}</span>
              </span>
              {value === option.value ? <Check className="size-4 shrink-0" /> : null}
            </button>
          ))}
        </div>
      )}
    </ResponsiveSelector>
  );
}
