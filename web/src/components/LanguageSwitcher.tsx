import { useTranslation } from "react-i18next"
import { Languages } from "lucide-react"
import { LOCALES, LOCALE_LABELS, useLocale } from "@/i18n"
import type { Locale } from "@/i18n"

/**
 * Language switcher for the six supported locales.
 *
 * It sits in the sidebar next to the actor switcher on purpose: both answer "who am I and in
 * which language am I working", and neither affects the data being shown - switching language
 * must not refetch anything (no endpoint takes a locale).
 */
export function LanguageSwitcher() {
  const { t } = useTranslation()
  const { locale, setLocale } = useLocale()

  return (
    <div>
      <label
        htmlFor="gr-locale"
        className="mb-1 block text-[11px] uppercase tracking-wide"
      >
        {t("locale.label")}
      </label>
      <div className="flex items-center gap-1.5">
        <Languages className="size-3.5 shrink-0 opacity-60" aria-hidden />
        <select
          id="gr-locale"
          value={locale}
          onChange={(e) => setLocale(e.target.value as Locale)}
          className="min-w-0 flex-1 rounded border border-input bg-transparent px-1 py-1 text-[11px]"
        >
          {LOCALES.map((l) => (
            <option key={l} value={l}>
              {LOCALE_LABELS[l]}
            </option>
          ))}
        </select>
      </div>
    </div>
  )
}
