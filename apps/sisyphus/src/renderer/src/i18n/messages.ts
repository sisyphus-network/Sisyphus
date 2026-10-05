import en from "@/i18n/messages/en.json";
import he from "@/i18n/messages/he.json";
import ar from "@/i18n/messages/ar.json";
import ru from "@/i18n/messages/ru.json";
import pl from "@/i18n/messages/pl.json";
import es from "@/i18n/messages/es.json";
import fr from "@/i18n/messages/fr.json";
import type { AppLocale } from "@/i18n/locales";

const dictionaries = { en, he, ar, ru, pl, es, fr } satisfies Record<AppLocale, typeof en>;

export type SisyphusMessages = typeof en;

export function getMessages(locale: AppLocale): SisyphusMessages {
  return dictionaries[locale];
}

export function formatMessage(template: string, values: Record<string, string | number> = {}): string {
  return template.replace(/\{(\w+)\}/g, (_, key: string) => String(values[key] ?? `{${key}}`));
}
