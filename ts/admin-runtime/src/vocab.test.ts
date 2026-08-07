import { describe, expect, it } from "vitest";

import vocab from "./vocab.json";

// docs/specs/i18n/formatting.md — the admin's CHROME vocabulary.
//
// `vocab.json` is what the compiler seeds the admin's `.po` with, so a string
// a component renders through `t()` but nobody listed there has no msgid in
// any catalog: it renders in English forever, silently, on an admin the
// operator translated. This test is the link between the two — the vocabulary
// is data precisely so it can be checked, rather than a Go-side parser for
// TypeScript that nobody wants to own.

const MSGIDS: string[] = (vocab as { msgids: string[] }).msgids;

// vite's glob import, declared locally. The package depends on vitest but not
// on vite itself, so `vite/client` types are not in scope — and pulling them in
// globally to type one call in one test would be a worse trade than four lines
// of declaration here.
declare global {
  interface ImportMeta {
    glob(
      pattern: string,
      options: { query: string; import: string; eager: true },
    ): Record<string, unknown>;
  }
}

// The runtime's own sources, read through that glob rather than node:fs —
// vitest runs through vite, and reaching for the filesystem would make this
// the one file in the package that needs node types.
const SOURCES = import.meta.glob("./**/*.{ts,tsx}", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

/** Every `t("…")` / `t('…')` literal in the runtime's own source. */
function literalMsgids(): Map<string, string[]> {
  const found = new Map<string, string[]>();
  // A msgid is a literal argument to t(...). A NON-literal argument is a
  // string the compiler resolved (an action label, a fieldset title) and
  // belongs to the project's schema, not to this vocabulary.
  const call = /\bt\(\s*(?:"([^"\\]*)"|'([^'\\]*)')\s*[),]/g;
  for (const [file, body] of Object.entries(SOURCES)) {
    // Tests declare their own fixture msgids; they are not chrome.
    if (/\.test\.tsx?$/.test(file)) continue;
    for (const m of body.matchAll(call)) {
      const msgid = m[1] ?? m[2] ?? "";
      if (!msgid) continue;
      found.set(msgid, [...(found.get(msgid) ?? []), file]);
    }
  }
  return found;
}

describe("the chrome vocabulary", () => {
  it('covers every t("…") literal in the runtime', () => {
    const listed = new Set(MSGIDS);
    const missing: string[] = [];
    for (const [msgid, files] of literalMsgids()) {
      if (!listed.has(msgid)) missing.push(`${JSON.stringify(msgid)} (${files.join(", ")})`);
    }
    expect(
      missing,
      "add these to src/vocab.json, or the compiler will never seed them into a catalog",
    ).toEqual([]);
  });

  // The scanner is the load-bearing half of the test above: if it stopped
  // matching, "nothing is missing" would be vacuously true.
  it("actually finds literals", () => {
    expect(literalMsgids().size).toBeGreaterThan(10);
  });

  // The OTHER direction, and the one that was missing (T2-6 pass #6, A-F1).
  //
  // The test above proves every t("…") is listed. It says nothing about prose
  // that never reaches t() at all — and that is how `Save`, `Delete`,
  // `Cancel`, `Create` and `Back` came to be rendered as raw JSX text while
  // the .po beside them carried "Uložit", "Smazat", "Zrušit". A translator's
  // work sat in the catalog and never appeared on screen, and the comment in
  // vocab.json ("a string added to a component cannot silently go
  // untranslated") was a promise this file did not keep.
  //
  // The scan is deliberately narrow: a line whose entire content is prose,
  // sitting inside JSX. That is the shape a button label takes. Anything with
  // syntax on it — a tag, a brace, an operator, a comment — is code and is
  // skipped, so the check stays quiet about everything except the one shape it
  // knows how to judge.
  it("has no untranslated prose rendered as JSX text", () => {
    const raw: string[] = [];
    const prose = /^[A-Z][A-Za-z]*(?: [A-Za-z]+)*[.?!]?$/;
    for (const [file, body] of Object.entries(SOURCES)) {
      if (!file.endsWith(".tsx") || /\.test\.tsx?$/.test(file)) continue;
      body.split("\n").forEach((line, i) => {
        const text = line.trim();
        if (!prose.test(text)) return;
        // Prose-shaped, but part of a declaration rather than rendered.
        if (/[<>{}()=;:,/*`'"]/.test(text)) return;
        raw.push(`${file}:${i + 1} ${JSON.stringify(text)}`);
      });
    }
    expect(
      raw,
      'wrap these in t("…") — prose rendered as JSX text can never be translated, ' +
        "however complete the catalog is",
    ).toEqual([]);
  });

  it("is sorted and free of duplicates, so a diff reads cleanly", () => {
    expect(MSGIDS).toEqual([...new Set(MSGIDS)]);
    expect(MSGIDS).toEqual([...MSGIDS].sort());
  });
});
