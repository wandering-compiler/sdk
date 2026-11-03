import { describe, expect, it } from "vitest";

import { readdirSync, readFileSync } from "node:fs";

// Every place the admin renders an input for a request field must honour
// `field_refs`. The picker shipped on three of five such places — the forms —
// and the list filters and the inline form kept their text boxes until an
// adopter's operator had to type an organization's uuid into a filter. A
// component that labels an input with a spec field (`humanizeLabel(...)`) and
// never reaches the picker is that gap again; this finds it before an
// adopter does.
//
// The sources are read from disk, not imported `?raw`: a `?raw` import is a
// module on the same path as the real one, and its coverage entry replaced
// the real file's — bootstrap.tsx dropped below the coverage gate. See
// vocab.test.ts.
//
// A file that renders such inputs and has no picker on purpose goes in
// `noPicker`, with the reason.
const noPicker: Record<string, string> = {};

function components(): [string, string][] {
  const here = import.meta.dirname;
  return readdirSync(here, { recursive: true })
    .filter(
      (f) =>
        !f.includes("/") && f.endsWith(".tsx") && !f.includes(".test.") && f !== "refPicker.tsx",
    )
    .map((f) => [f, readFileSync(`${here}/${f}`, "utf8")]);
}

describe("input sites", () => {
  it("every component that renders a field input reaches the ref picker", () => {
    const sites = components().filter(([, src]) => /label=\{humanizeLabel\(/.test(src));
    expect(sites.length, "found no input sites — the pattern no longer matches").toBeGreaterThan(3);
    const missing = sites
      .filter(([f, src]) => !(f in noPicker) && !/\bRef(Picker|Field|Text)\b/.test(src))
      .map(([f]) => f);
    expect(missing, "these render field inputs without field_refs pickers").toEqual([]);
  });
});
