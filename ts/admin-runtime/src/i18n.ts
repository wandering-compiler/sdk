/**
 * The admin's gettext runtime — msgid → msgstr lookup plus `{name}`
 * substitution.
 *
 * The catalogs are baked into the spec at codegen time, from the admin's OWN
 * `.po` tree. That tree is separate from the app's per-domain one on purpose:
 * the two surfaces are translated by different people for different audiences
 * and need not even cover the same languages — a single-language internal
 * admin over a five-language public app is the ordinary case. A msgid that
 * appears on both is translated twice, which is the right answer when the
 * register differs.
 *
 * The lookup chain mirrors `i18n.T` on the server and `t` in the generated
 * client, so a string resolves the same way on every surface, PER MESSAGE:
 * the viewer's catalog, else the surface default's entry for that msgid, then
 * the bare msgid — which IS the English source text, so an untranslated admin
 * renders exactly what it rendered before catalogs existed.
 */
import { createContext, useContext } from "react";

/** Per-language msgid → msgstr, as the spec carries it. */
export type AdminCatalogs = Record<string, Record<string, string>>;

/** Looks up a msgid and substitutes `{name}` params. */
export type Translate = (msgid: string, params?: Record<string, unknown>) => string;

/**
 * The UI language the admin renders in.
 *
 * The browser's language is the only per-viewer signal an admin has — there is
 * no per-user preference to read — so it is the request, and the surface's
 * `default_language` is the fallback. Note this is the same SIGNAL the format
 * locale uses but a different QUESTION: §3 of the spec keeps them apart, and
 * they resolve independently, so a `cs-CZ` browser can get Czech dates from
 * the format table while its UI falls back to English because no `cs` catalog
 * was shipped.
 *
 * Matching walks the browser's languages in its order of preference
 * (`navigator.languages`, as the server walks Accept-Language), each by exact
 * tag then base language, then the default — a catalog is shipped per
 * language (`cs`) while a browser announces locales (`sk-SK`, `cs`, `en`).
 */
export function adminUiLanguage(
  catalogs: AdminCatalogs,
  defaultLanguage?: string,
  preferred: readonly string[] = browserLanguages(),
): string {
  for (const tag of preferred) {
    for (const candidate of [tag, tag.split("-")[0]]) {
      if (candidate && catalogs[candidate]) return candidate;
    }
  }
  if (defaultLanguage && catalogs[defaultLanguage]) return defaultLanguage;
  return defaultLanguage || "en";
}

function browserLanguages(): readonly string[] {
  if (typeof navigator === "undefined") return [];
  if (navigator.languages && navigator.languages.length > 0) return navigator.languages;
  return navigator.language ? [navigator.language] : [];
}

/**
 * Builds the translator for one language.
 *
 * Per message, as the server's `i18n.T` resolves: a msgid the viewer's
 * catalog lacks takes the default language's translation of it, then the
 * bare msgid. A partially translated language thus reads like the default
 * language where it has gaps — the same sentence the server's refusals
 * render in the same situation, so one screen never mixes the two rules.
 */
export function makeTranslator(
  catalogs: AdminCatalogs | undefined,
  lang: string,
  defaultLanguage?: string,
): Translate {
  const own = catalogs?.[lang];
  const fallback = defaultLanguage && defaultLanguage !== lang ? catalogs?.[defaultLanguage] : undefined;
  return (msgid, params) => {
    const hit = own?.[msgid] || fallback?.[msgid];
    const text = hit != null && hit !== "" ? hit : msgid;
    return params ? substitute(text, params) : text;
  };
}

/**
 * `{name}` substitution by plain replacement — the same mechanism `i18n.T` and
 * the client's `t` use, and the reason format templates lower to a msgid at
 * all. A placeholder the caller does not supply survives verbatim: a
 * translator who typed `{foo}` should see it in the output, not break the page.
 */
function substitute(text: string, params: Record<string, unknown>): string {
  let out = text;
  for (const [key, value] of Object.entries(params)) {
    out = out.split("{" + key + "}").join(String(value));
  }
  return out;
}

/**
 * The identity translator: every msgid renders as itself. It is the default
 * for a component rendered outside a provider, and it is exactly what the
 * admin did before catalogs existed.
 */
export const identityTranslate: Translate = (msgid, params) =>
  params ? substitute(msgid, params) : msgid;

const Ctx = createContext<Translate>(identityTranslate);

export const TranslateProvider = Ctx.Provider;

/** Reads the translator. See [TranslateProvider]. */
export function useT(): Translate {
  return useContext(Ctx);
}

/**
 * The translator a spec implies. Kept here so every surface derives it the
 * same way, the way `formatContextFor` does for the format context.
 */
export function translatorFor(spec: {
  default_language?: string;
  catalogs?: AdminCatalogs;
}): Translate {
  const catalogs = spec.catalogs ?? {};
  return makeTranslator(
    catalogs,
    adminUiLanguage(catalogs, spec.default_language),
    spec.default_language,
  );
}
