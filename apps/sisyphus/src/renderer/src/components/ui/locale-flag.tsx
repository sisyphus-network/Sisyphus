import { getLocaleOption, type AppLocale } from "@/i18n/locales";
import { AssetImage } from "@/components/ui/asset-image";

type LocaleFlagProps = {
  locale: AppLocale;
  size?: "s" | "m" | "l";
  label?: string;
  className?: string;
};

export function LocaleFlag({ locale, size = "l", label, className }: LocaleFlagProps) {
  const option = getLocaleOption(locale);
  const pixels = size === "s" ? 18 : size === "m" ? 24 : 32;
  return <span className={className}>
    <AssetImage
      src={`/flags/${size}/${option.flag}.svg`}
      alt={label ?? option.nativeName}
      width={pixels}
      height={pixels}
      containerClassName="inline-flex shrink-0 items-center justify-center overflow-visible align-middle leading-none"
      className="rounded-full object-cover shadow-sm"
    />
  </span>;
}
