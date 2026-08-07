/**
 * Locale-aware CELL formatting — the ADMIN's half of
 * docs/specs/i18n/formatting.md.
 *
 * The formatting itself is not admin-specific and lives one module down:
 * valueFormat.ts owns the frozen table and the formatters, slotFormat.ts owns
 * slot application and `{name}` substitution, and both are mirrored verbatim
 * into the generated web client. What is admin-specific — the React context
 * every page reads the locale from, and the rule for which locale that is —
 * lives here.
 *
 * Everything from slotFormat.ts is re-exported, so admin code has one import
 * to reach for and the split stays an implementation detail.
 */
import { createContext, useContext } from "react";

import type { FormatOverrides } from "./valueFormat";
import type { FormatContext, FormatTemplate } from "./slotFormat";
import { formatTemplateValue } from "./slotFormat";

export type {
  FormatContext,
  FormatLiteral,
  FormatPreset,
  FormatSlot,
  FormatTemplate,
} from "./slotFormat";
export { formatSlotValue, formatTemplateValue, templateIsNumeric } from "./slotFormat";

/**
 * The locale the admin formats with.
 *
 * The browser's own language IS the user's regional setting, and §3 of the
 * spec is explicit that formatting follows the region rather than the UI
 * language — a Czech user running an English admin still wants 26.07.2026.
 * The spec's `default_language` is the fallback for a headless or
 * locale-less environment, and `en` behind that.
 *
 * A locale the frozen table has no row for is NOT rejected here:
 * resolveFormats walks base-language then `en`, so `en-GB` formats as `en`
 * (day-first, 24-hour), which is right for it.
 */
export function adminFormatLocale(defaultLanguage?: string): string {
  const nav = typeof navigator === "undefined" ? "" : navigator.language;
  return nav || defaultLanguage || "en";
}

/**
 * The format context for the tree below. Provided by the page that holds the
 * spec; the default is a bare `en` so a component rendered outside a provider
 * still formats something rather than throwing.
 */
const Ctx = createContext<FormatContext>({ locale: "en" });

export const FormatContextProvider = Ctx.Provider;

/** Reads the format context. See [FormatContextProvider]. */
export function useFormatContext(): FormatContext {
  return useContext(Ctx);
}

/**
 * The format context a spec implies: the viewer's locale plus the project's
 * remap. Kept here so every surface derives it the same way.
 */
export function formatContextFor(spec: {
  default_language?: string;
  format_overrides?: FormatOverrides;
}): FormatContext {
  return { locale: adminFormatLocale(spec.default_language), overrides: spec.format_overrides };
}

/**
 * Formats one cell through its column's template.
 *
 * `translate` is the tree's translator, so a template's surrounding prose and
 * its `_("…")` literals resolve through the admin's OWN gettext catalog — a
 * separate space from the app's, because the two surfaces are translated by
 * different people for different audiences. Omitting it renders every msgid as
 * itself, which is the English source text and exactly what the admin did
 * before catalogs existed.
 */
export function formatCellValue(
  raw: unknown,
  tmpl: FormatTemplate | undefined,
  ctx: FormatContext,
  translate?: (msgid: string) => string,
): string | undefined {
  return formatTemplateValue(raw, tmpl, ctx, translate);
}
