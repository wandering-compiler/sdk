import { describe, expect, it } from "vitest";

import { adminUiLanguage, makeTranslator, translatorFor } from "./i18n";
import type { AdminCatalogs } from "./i18n";

// docs/specs/i18n/formatting.md — the admin's gettext runtime.
//
// The lookup chain mirrors i18n.T on the server and `t` in the generated
// client, so a string resolves the same way on every surface. What is
// admin-specific is which LANGUAGE is asked for, and that the whole thing
// degrades to English rather than to nothing.

const catalogs: AdminCatalogs = {
  cs: { Save: "Uložit", "Add {name}": "Přidat {name}" },
  de: { Save: "Speichern" },
};

function withNavigatorLanguage(lang: string) {
  Object.defineProperty(window.navigator, "language", { value: lang, configurable: true });
}

describe("adminUiLanguage", () => {
  it("takes the browser's exact tag when a catalog exists for it", () => {
    withNavigatorLanguage("cs");
    expect(adminUiLanguage(catalogs, "en")).toBe("cs");
  });

  // A catalog is shipped per LANGUAGE (`cs`) while a browser announces a
  // LOCALE (`cs-CZ`). Without the base-language step, a Czech browser would
  // read an English admin next to Czech-formatted dates.
  it("falls back to the base language", () => {
    withNavigatorLanguage("cs-CZ");
    expect(adminUiLanguage(catalogs, "en")).toBe("cs");
  });

  it("falls back to the surface default, then to en", () => {
    withNavigatorLanguage("fr");
    expect(adminUiLanguage(catalogs, "de")).toBe("de");
    expect(adminUiLanguage(catalogs, undefined)).toBe("en");
    expect(adminUiLanguage({}, undefined)).toBe("en");
  });
});

describe("makeTranslator", () => {
  it("translates a known msgid and substitutes params", () => {
    const t = makeTranslator(catalogs, "cs");
    expect(t("Save")).toBe("Uložit");
    expect(t("Add {name}", { name: "Faktura" })).toBe("Přidat Faktura");
  });

  // The bare msgid IS the English source text, so an untranslated admin
  // renders exactly what it rendered before catalogs existed.
  it("falls through to the msgid, params and all", () => {
    const t = makeTranslator(catalogs, "cs");
    expect(t("Search")).toBe("Search");
    expect(t("Add {name}", { name: "Note" })).toBe("Přidat Note");
    expect(makeTranslator(undefined, "cs")("Save")).toBe("Save");
  });

  // Catalog selection is WHOLE-catalog, the same rule the server follows: a
  // key a partial language lacks renders ENGLISH, never a third language.
  it("selects a whole catalog, not a key at a time", () => {
    const t = makeTranslator(catalogs, "de", "cs");
    expect(t("Save")).toBe("Speichern");
    expect(t("Add {name}", { name: "x" })).toBe("Add x");
  });

  it("uses the default language's catalog when the requested one is absent", () => {
    expect(makeTranslator(catalogs, "fr", "cs")("Save")).toBe("Uložit");
  });

  // A placeholder the caller does not supply survives verbatim — a translator
  // who typed {foo} should see it, not break the page.
  it("leaves an unsupplied placeholder alone", () => {
    expect(makeTranslator({}, "en")("Hi {who}", { other: 1 })).toBe("Hi {who}");
  });
});

describe("translatorFor", () => {
  it("derives the translator a spec implies", () => {
    withNavigatorLanguage("cs");
    expect(translatorFor({ default_language: "en", catalogs })("Save")).toBe("Uložit");
  });

  it("degrades to identity for a spec with no catalogs", () => {
    withNavigatorLanguage("cs");
    expect(translatorFor({ default_language: "en" })("Save")).toBe("Save");
  });
});
