import { readFileSync, readdirSync } from "node:fs";
import { describe, expect, it } from "vitest";

// A source-level gate for one defect shape, added because the shape shipped TWICE and
// neither instance was reachable from any test.
//
// `onChange={(e) => setX((prev) => ({ ...prev, k: e.currentTarget.value }))}` throws
// "Cannot read properties of null (reading 'value')" on the first keystroke. React nulls
// a synthetic event's `currentTarget` once the handler returns, and a functional updater
// runs LATER — the stack names `basicStateReducer`. So the read happens after the event
// has been recycled.
//
// It reached `ActionModal.tsx` and `InlineSection.tsx`, both of which build a form from
// a spec's field list, which is exactly where the accumulate-into-an-object shape is
// natural. The sibling one line away in `Login.tsx` —
// `onChange={(e) => setPassword(e.currentTarget.value)}` — is CORRECT and looks
// identical at a glance, which is why a reviewer does not catch this and a grep does.
//
// Deliberately a text scan and not an eslint rule: the rule that would express it
// ("event fields are invalid inside a deferred callback") does not exist in
// typescript-eslint, and writing a custom plugin for one shape is more machinery than
// the shape deserves. `vocab.test.ts` reads the sources the same way and says why the
// files are read rather than imported.

function sources(): Record<string, string> {
  const here = import.meta.dirname;
  const out: Record<string, string> = {};
  for (const rel of readdirSync(here, { recursive: true })) {
    if (!/\.tsx?$/.test(rel) || /\.test\.tsx?$/.test(rel)) continue;
    out[`./${rel}`] = readFileSync(`${here}/${rel}`, "utf8");
  }
  return out;
}

/**
 * One handler body per match: everything from `=> ` up to the end of the statement,
 * collapsed onto one line so a prettier-wrapped handler reads the same as an inline
 * one. Crude on purpose — it only has to bring the updater and the event read close
 * enough together to be seen in the same string.
 */
function handlerBodies(body: string): string[] {
  const flat = body.replace(/\s+/g, " ");
  // `setX((prev) => …` / `setX(prev => …` — the state updater form, whatever the
  // parameter is called.
  return flat.split(/(?=set[A-Z]\w*\(\s*\(?\s*\w+\s*\)?\s*=>)/g);
}

describe("an event field is never read inside a state updater", () => {
  it("holds across the runtime's own sources", () => {
    const offenders: string[] = [];
    for (const [file, body] of Object.entries(sources())) {
      for (const chunk of handlerBodies(body)) {
        if (!/^set[A-Z]\w*\(\s*\(?\s*\w+\s*\)?\s*=>/.test(chunk)) continue;
        // The updater's own body ends at its closing brace-paren; taking the first
        // 200 characters is enough to cover an object literal and keeps a following
        // statement's legitimate `currentTarget` out of the window.
        const updater = chunk.slice(0, 200);
        if (/\b(currentTarget|nativeEvent)\b/.test(updater)) {
          offenders.push(`${file}: ${updater.slice(0, 120)}`);
        }
      }
    }
    expect(
      offenders,
      "read the value into a const BEFORE the updater — inside it the event is already recycled",
    ).toEqual([]);
  });

  // The gate itself, against the two real bodies it was written for. Without this the
  // check above could pass because the pattern stopped MATCHING, which is the same
  // green as the pattern being absent.
  it("recognises the shape it is looking for", () => {
    const broken = `onChange={(e) => setExtras((prev) => ({ ...prev, [f]: e.currentTarget.value }))}`;
    const fixed = `onChange={(e) => { const value = e.currentTarget.value; setExtras((prev) => ({ ...prev, [f]: value })); }}`;

    const flags = (body: string) =>
      handlerBodies(body)
        .filter((c) => /^set[A-Z]\w*\(\s*\(?\s*\w+\s*\)?\s*=>/.test(c))
        .some((c) => /\b(currentTarget|nativeEvent)\b/.test(c.slice(0, 200)));

    expect(flags(broken), "the gate does not see the defect it exists for").toBe(true);
    expect(flags(fixed), "the gate flags the correct form too").toBe(false);
  });

  // And the scan has to be reaching files at all: a walk that finds nothing reports
  // the same empty list as a codebase with no offenders.
  it("scans the sources it means to", () => {
    const files = Object.keys(sources());
    expect(files.length).toBeGreaterThan(20);
    expect(files, "the components that carried the defect are not being read").toEqual(
      expect.arrayContaining(["./ActionModal.tsx", "./InlineSection.tsx"]),
    );
    expect(
      files.filter((f) => /\.test\.tsx?$/.test(f)),
      "test files are scanned — their deliberate counter-examples would trip the gate",
    ).toEqual([]);
  });
});
