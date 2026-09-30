import { describe, expect, it } from "vitest";

import { readdirSync, readFileSync } from "node:fs";

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

// The runtime's own sources, read as TEXT from disk.
//
// ⚠️ NOT `import.meta.glob(..., { query: "?raw" })`, which is what this used to
// be, and the reason is a coverage defect it caused rather than a style
// preference.
//
// A `?raw` import is still a MODULE: vite serves `/src/CreatePage.tsx?raw` as
// `export default "<the whole file on one line>"`. v8 attributes coverage per
// script URL, and the remapper strips the query — so that one-statement text
// module and the real component's 100-statement module land on the same key,
// `src/CreatePage.tsx`, and one of them wins.
//
// Which one won depended on vite's transform cache, and `scripts/cover-js.sh`
// runs `npm ci` first, which deletes `node_modules/.vite`. So the GATE always
// saw the cold order. Measured, three runs each side:
//
//   cold cache   CreatePage.tsx  100% stmts, 73.33% branch, "uncovered: 1"   709/728 total
//   warm cache   CreatePage.tsx   99% stmts,  87.5% branch, "uncovered: 153"  807/827 total
//
// The cold entry is one statement spanning line 1, columns 0-6347 — the string
// literal. So the one .tsx component deliberately admitted to `coverage.include`
// (docs/decisions/which-components-belong-in-the-gate.md) was the one file the
// gate did not measure, and it reported 100% while being uncounted: "uncounted
// reads as covered", the artefact that decision record already names.
//
// Reading the files creates no module for them, so there is nothing to collide
// with. `types/node-fs.d.ts` says why the two signatures are declared locally
// rather than by installing `@types/node`.
// `import.meta.dirname` and not `new URL(".", import.meta.url)`: the URL form
// resolved to `/src/index.ts` under vitest and the suite failed to LOAD, which
// the run reported as "444 passed" with the file counted among failed SUITES —
// a shape worth naming, since a grep for the passing line reads it as green.
declare global {
  interface ImportMeta {
    /** Absolute directory of this module. Node >= 20.11, and vite passes it through. */
    readonly dirname: string;
  }
}

function readSources(): Record<string, string> {
  const here = import.meta.dirname;
  const out: Record<string, string> = {};
  for (const rel of readdirSync(here, { recursive: true })) {
    if (!/\.tsx?$/.test(rel)) continue;
    out[`./${rel}`] = readFileSync(`${here}/${rel}`, "utf8");
  }
  return out;
}

const SOURCES = readSources();

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

  // The READ is the other load-bearing half, and its failure is silent in a way
  // the scanner's is not: a walk that misses `src/components/` still finds
  // plenty of literals in `src/`, so the check above would stay green while the
  // vocabulary went unchecked for every file under it. The glob this replaced
  // was recursive by pattern; `readdirSync` is recursive only because it is
  // asked to be, so the property is asserted rather than assumed.
  it("reads every source file, subdirectories included", () => {
    const files = Object.keys(SOURCES);
    // `.slice(2)` drops the `./` every key carries. Without it the filter below
    // matches EVERY key on that prefix and cannot fail — which is how the first
    // cut of this test passed with the recursion disarmed, reporting 56 of 56
    // keys as nested when the real answer was 0.
    const nested = files.filter((f) => f.slice(2).includes("/"));
    expect(files.length, "the source walk found nothing to scan").toBeGreaterThan(20);
    expect(nested, "no file from a subdirectory — the walk stopped at src/").not.toEqual([]);
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
    // One word is enough — `<Text>Saved</Text>` is prose. An earlier cut of this
    // rewrite required two, on the assumption that single capitalised tokens left
    // over from stripping would be noise. Measured across every .tsx here:
    // allowing one adds ZERO findings, so the narrowing cost real reach and bought
    // nothing.
    const prose = /^[A-Z][A-Za-z]*(?: [A-Za-z]+)*[.?!]?$/;
    for (const [file, body] of Object.entries(SOURCES)) {
      if (!file.endsWith(".tsx") || /\.test\.tsx?$/.test(file)) continue;
      body.split("\n").forEach((line, i) => {
        // Strip what is NOT rendered text, then read what is left.
        //
        // The first version tested the whole trimmed LINE and skipped anything
        // containing <>{}() — so it saw prose only where the formatter had put it
        // on a line of its own, and missed
        //   <Text c="red">Inline target page {x} missing from spec.</Text>
        // which is the same defect with prettier's line width as the difference.
        // A gate whose reach is decided by a formatter is not a gate.
        const code = line.trim();
        if (code.startsWith("//") || code.startsWith("*") || code.startsWith("/*")) return;
        const rendered = code
          .replace(/<[^<>]*>/g, "\u0000") // tags
          .replace(/\{[^{}]*\}/g, "\u0000") // JSX expressions
          .replace(/"[^"]*"|'[^']*'|`[^`]*`/g, "\u0000"); // string literals
        for (const chunk of rendered.split("\u0000")) {
          const text = chunk.trim();
          if (!text || !prose.test(text)) continue;
          raw.push(`${file}:${i + 1} ${JSON.stringify(text)}`);
        }
      });
    }
    expect(
      raw,
      'wrap these in t("…") — prose rendered as JSX text can never be translated, ' +
        "however complete the catalog is",
    ).toEqual([]);
  });

  // TEXT-CARRYING PROPS are a second door, and the check above never covered it.
  //
  // That one reads JSX text children. Three user-facing strings were sitting in
  // props instead — a modal title, a page header, a search placeholder — two of
  // them template literals, which the text check strips as string literals
  // because in every other position that is what they are. ListPage had
  // `label={t("Search")}` on the line directly above an untranslated
  // `placeholder={…}`, which is as clear as an oversight gets.
  //
  // The prop list is deliberately short and named: these are the attributes this
  // runtime renders TO THE OPERATOR. `name`, `id`, `key` and the rest carry
  // identifiers and must not be translated.
  it("has no untranslated prose in a text-carrying prop", () => {
    const raw: string[] = [];
    const textProp =
      /\b(title|label|placeholder|aria-label|description)=\s*(?:"([^"]+)"|\{`([^`]+)`\}|\{"([^"]+)"\})/g;
    for (const [file, body] of Object.entries(SOURCES)) {
      if (!file.endsWith(".tsx") || /\.test\.tsx?$/.test(file)) continue;
      body.split("\n").forEach((line, i) => {
        const code = line.trim();
        if (code.startsWith("//") || code.startsWith("*")) return;
        for (const m of code.matchAll(textProp)) {
          const value = m[2] ?? m[3] ?? m[4] ?? "";
          // A single lower-case token is a variant or a size ("sm", "xs"), not
          // prose. Anything with a space, or starting capitalised, is a sentence
          // fragment somebody reads.
          if (!/\s/.test(value) && !/^[A-Z]/.test(value)) continue;
          raw.push(`${file}:${i + 1} ${JSON.stringify(value)}`);
        }
      });
    }
    expect(
      raw,
      'wrap these in t("…") — a title, label or placeholder is read by the ' +
        "operator just as plainly as the text between the tags",
    ).toEqual([]);
  });

  it("is sorted and free of duplicates, so a diff reads cleanly", () => {
    expect(MSGIDS).toEqual([...new Set(MSGIDS)]);
    expect(MSGIDS).toEqual([...MSGIDS].sort());
  });
});
