export const supportedLocales = ["he", "en", "ar", "ru", "pl", "es", "fr"] as const;

export type AppLocale = (typeof supportedLocales)[number];

export type LocaleOption = {
  code: AppLocale;
  direction: "ltr" | "rtl";
  flag: "IL" | "US" | "AE" | "RU" | "PL" | "ES" | "FR";
  nativeName: string;
};

export const localeOptions: readonly LocaleOption[] = [
  { code: "he", direction: "rtl", flag: "IL", nativeName: "עברית" },
  { code: "en", direction: "ltr", flag: "US", nativeName: "English" },
  { code: "ar", direction: "rtl", flag: "AE", nativeName: "العربية" },
  { code: "ru", direction: "ltr", flag: "RU", nativeName: "Русский" },
  { code: "pl", direction: "ltr", flag: "PL", nativeName: "Polski" },
  { code: "es", direction: "ltr", flag: "ES", nativeName: "Español" },
  { code: "fr", direction: "ltr", flag: "FR", nativeName: "Français" },
] as const;

export function isAppLocale(value: unknown): value is AppLocale {
  return typeof value === "string" && supportedLocales.includes(value as AppLocale);
}

export function getLocaleDirection(locale: string): "ltr" | "rtl" {
  return localeOptions.find((option) => option.code === locale)?.direction ?? "ltr";
}

export function getLocaleOption(locale: AppLocale): LocaleOption {
  return localeOptions.find((option) => option.code === locale) ?? localeOptions[0];
}
