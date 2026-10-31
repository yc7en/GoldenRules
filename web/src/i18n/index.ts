import i18n from "i18next"
import { initReactI18next } from "react-i18next"
import { useTranslation } from "react-i18next"
import en from "./locales/en.json"

/** Supported UI locales, as BCP-47 tags: `Intl.PluralRules` accepts nothing else. */
export const LOCALES = ["en", "zh-CN", "ja", "ar", "fr", "de"] as const
export type Locale = (typeof LOCALES)[number]

/** Shown in the switcher. Each name is written in its own language, not in the current one. */
export const LOCALE_LABELS: Record<Locale, string> = {
  en: "English",
  "zh-CN": "简体中文",
  ja: "日本語",
  ar: "العربية",
  fr: "Français",
  de: "Deutsch",
}

export const DEFAULT_LOCALE: Locale = "en"
const DEFAULT_NS = "translation"
const STORAGE_KEY = "gr.locale"
const RTL_LOCALES: ReadonlySet<Locale> = new Set<Locale>(["ar"])

function isLocale(v: string): v is Locale {
  return (LOCALES as readonly string[]).includes(v)
}

/**
 * Prefix match, so `zh-Hans-CN` and `ar-EG` still resolve.
 * Returns null for anything unsupported instead of silently falling back: the caller decides
 * whether that means "keep looking" or "use the default".
 */
export function normalizeLocale(tag: string | undefined | null): Locale | null {
  if (!tag) return null
  const lower = tag.toLowerCase()
  if (isLocale(tag)) return tag
  const exact = LOCALES.find((l) => l.toLowerCase() === lower)
  if (exact) return exact
  const base = lower.split("-")[0]
  return LOCALES.find((l) => l.toLowerCase().split("-")[0] === base) ?? null
}

function detectedLocale(): Locale {
  try {
    const saved = normalizeLocale(localStorage.getItem(STORAGE_KEY))
    if (saved) return saved
  } catch {
    // Storage can be disabled; language selection is not worth failing the app over.
  }
  const nav = typeof navigator === "undefined" ? [] : navigator.languages ?? [navigator.language]
  for (const tag of nav) {
    const hit = normalizeLocale(tag)
    if (hit) return hit
  }
  return DEFAULT_LOCALE
}

/**
 * One bundle per language, loaded on demand: switching to Japanese must not pull Arabic
 * (right-to-left, six plural forms, different font) into the page.
 */
const loaders: Record<Locale, () => Promise<{ default: Record<string, unknown> }>> = {
  en: async () => ({ default: en }),
  "zh-CN": () => import("./locales/zh-CN.json"),
  ja: () => import("./locales/ja.json"),
  ar: () => import("./locales/ar.json"),
  fr: () => import("./locales/fr.json"),
  de: () => import("./locales/de.json"),
}

const loaded = new Set<Locale>(["en"])

async function ensureBundle(locale: Locale): Promise<void> {
  if (loaded.has(locale)) return
  const mod = await loaders[locale]()
  i18n.addResourceBundle(locale, DEFAULT_NS, mod.default, true, true)
  loaded.add(locale)
}

/** `lang` drives font selection and hyphenation, `dir` drives the whole layout. */
function applyDocument(locale: Locale) {
  document.documentElement.lang = locale
  document.documentElement.dir = RTL_LOCALES.has(locale) ? "rtl" : "ltr"
  document.documentElement.dataset.locale = locale
}

export function isRtl(locale: Locale): boolean {
  return RTL_LOCALES.has(locale)
}

async function boot(): Promise<void> {
  const start = detectedLocale()
  await i18n.use(initReactI18next).init({
    resources: { en: { translation: en } },
    // Start from the default so the first synchronous init never asks for a bundle that is
    // still being fetched; the detected language is switched in below, before the first render.
    lng: DEFAULT_LOCALE,
    fallbackLng: DEFAULT_LOCALE,
    defaultNS: DEFAULT_NS,
    interpolation: { escapeValue: false },
    // Synchronous init: the English bundle is already in hand, and an async init here would let
    // the first render slip through before any translation exists.
    initAsync: false,
    returnNull: false,
  })
  if (start !== DEFAULT_LOCALE) {
    await ensureBundle(start)
    await i18n.changeLanguage(start)
  }
  applyDocument(start)
}

let booting: Promise<void> | null = null

/**
 * Resolved before the app renders (see main.tsx). Rendering first and switching later would
 * paint a frame of English keys for a Chinese or Arabic user.
 */
export function initI18n(): Promise<void> {
  booting ??= boot()
  return booting
}

export async function changeLocale(locale: Locale): Promise<void> {
  await ensureBundle(locale)
  await i18n.changeLanguage(locale)
  applyDocument(locale)
  try {
    localStorage.setItem(STORAGE_KEY, locale)
  } catch {
    // Persisting the choice is best-effort.
  }
}

export function useLocale(): { locale: Locale; setLocale: (next: Locale) => void } {
  const { i18n: instance } = useTranslation()
  const locale = normalizeLocale(instance.resolvedLanguage ?? instance.language) ?? DEFAULT_LOCALE
  return { locale, setLocale: (next) => void changeLocale(next) }
}

/**
 * The bare instance, for code outside React: `api.ts` builds transport diagnostics and has no
 * hook context to translate from.
 */
export default i18n
