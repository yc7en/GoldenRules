import en from "./locales/en.json"

/**
 * Makes `t("scenes.title")` type-checked and autocompleted against the English bundle.
 * English is the reference: every other locale must expose the same key set, and a key that
 * exists only in `zh-CN` would otherwise never fail to compile.
 */
declare module "i18next" {
  interface CustomTypeOptions {
    defaultNS: "translation"
    resources: { translation: typeof en }
  }
}
