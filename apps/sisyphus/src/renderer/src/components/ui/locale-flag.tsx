import { getLocaleOption, type AppLocale } from "@/i18n/locales";
import { AssetImage } from "@/components/ui/asset-image";
import flagIL from "flagpack-core/svg/m/IL.svg?url";
import flagUS from "flagpack-core/svg/m/US.svg?url";
import flagAE from "flagpack-core/svg/m/AE.svg?url";
import flagRU from "flagpack-core/svg/m/RU.svg?url";
import flagPL from "flagpack-core/svg/m/PL.svg?url";
import flagES from "flagpack-core/svg/m/ES.svg?url";
import flagFR from "flagpack-core/svg/m/FR.svg?url";

// Vite owns these package assets, including their file:// URLs in Electron.
const flagSources = { IL: flagIL, US: flagUS, AE: flagAE, RU: flagRU, PL: flagPL, ES: flagES, FR: flagFR };

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
      src={flagSources[option.flag]}
      alt={label ?? option.nativeName}
      width={pixels}
      height={pixels}
      containerClassName="inline-flex shrink-0 items-center justify-center overflow-visible align-middle leading-none"
      className="rounded-full object-cover shadow-sm"
    />
  </span>;
}
