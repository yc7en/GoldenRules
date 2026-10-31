/**
 * Fails the build when the six locale bundles drift apart.
 *
 * Plural suffixes are stripped before comparing: Arabic needs zero/one/two/few/many/other while
 * Chinese and Japanese only ever use `_other`, so a raw key diff would report dozens of false
 * positives. What must match is the set of *concepts*, not the set of plural forms.
 *
 * Also flags keys that exist in a translation but not in English: those are stale copy that
 * nobody will ever see, and they usually mean a rename was only half applied.
 */
import { readFileSync } from "node:fs"
import { dirname, join } from "node:path"
import { fileURLToPath } from "node:url"

const here = dirname(fileURLToPath(import.meta.url))
const localesDir = join(here, "..", "src", "i18n", "locales")
const LOCALES = ["en", "zh-CN", "ja", "ar", "fr", "de"]
const PLURAL = /_(zero|one|two|few|many|other)$/

function flatten(value, prefix = "", out = new Set()) {
  for (const [k, v] of Object.entries(value)) {
    const key = prefix ? `${prefix}.${k}` : k
    if (v !== null && typeof v === "object") flatten(v, key, out)
    else out.add(key.replace(PLURAL, ""))
  }
  return out
}

function load(locale) {
  const raw = readFileSync(join(localesDir, `${locale}.json`), "utf8")
  return flatten(JSON.parse(raw))
}

const reference = load("en")
let failed = false

for (const locale of LOCALES) {
  if (locale === "en") continue
  const keys = load(locale)
  const missing = [...reference].filter((k) => !keys.has(k)).sort()
  const extra = [...keys].filter((k) => !reference.has(k)).sort()
  if (missing.length > 0) {
    failed = true
    console.error(`${locale}: missing ${missing.length} key(s)`)
    for (const k of missing) console.error(`  - ${k}`)
  }
  if (extra.length > 0) {
    failed = true
    console.error(`${locale}: ${extra.length} key(s) not present in en`)
    for (const k of extra) console.error(`  + ${k}`)
  }
  if (missing.length === 0 && extra.length === 0) {
    console.log(`${locale}: ok (${keys.size} keys)`)
  }
}

if (failed) process.exit(1)
