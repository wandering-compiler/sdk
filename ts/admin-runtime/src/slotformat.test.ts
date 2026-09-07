import { describe, expect, it } from "vitest";

import { formatSlotValue, formatTemplateValue } from "./slotFormat";
import type { FormatContext, FormatSlot, FormatTemplate } from "./slotFormat";

// docs/specs/i18n/formatting.md — the TRANSLATOR seam of template application.
// The rest of the module's behaviour is exercised through the admin's
// re-export in cellformat.test.ts; what only exists here is the optional
// `translate`, which the generated web client passes and the admin does not.

const cs: FormatContext = { locale: "cs" };

// A stand-in catalog. Anything it does not know falls through to the msgid,
// which is what the real `t` does for a scaffolded entry.
const CATALOG: Record<string, string> = {
  "{value} of quota": "{value} z kvóty",
  "none left": "už nic nezbývá",
};
const t = (msgid: string) => CATALOG[msgid] ?? msgid;

describe("formatTemplateValue with a translator", () => {
  const tmpl: FormatTemplate = {
    msgid: "{value} of quota",
    slots: [{ name: "value", preset: "percent", places: 1, has_places: true }],
  };

  it("translates the msgid, then substitutes into the TRANSLATION", () => {
    expect(formatTemplateValue("12.34", tmpl, cs, t)).toBe("12,3 % z kvóty");
  });

  // Without a translator the msgid IS the template — the admin's path, and the
  // reason adding a catalog later changes nothing but where the string comes
  // from.
  it("falls back to the msgid when no translator is supplied", () => {
    expect(formatTemplateValue("12.34", tmpl, cs)).toBe("12,3 % of quota");
  });

  it("leaves a msgid the catalog does not know alone", () => {
    const other: FormatTemplate = {
      msgid: "{value} widgets",
      slots: [{ name: "value", preset: "number" }],
    };
    expect(formatTemplateValue("1234", other, cs, t)).toBe("1 234 widgets");
  });
});

describe("slot literals", () => {
  const translatable: FormatSlot = {
    name: "value",
    preset: "number",
    zero: { text: "none left", translatable: true },
    default: { text: "none left", translatable: true },
  };

  it('translates a `_("…")` literal on the zero and the absent branch', () => {
    expect(formatSlotValue("0", translatable, cs, t)).toBe("už nic nezbývá");
    expect(formatSlotValue(null, translatable, cs, t)).toBe("už nic nezbývá");
  });

  // The flag is the whole reason `default:"—"` and `default:_("none")` are
  // different spellings: an em dash is not a string a translator should ever
  // be shown, so a bare literal never reaches the catalog even when one exists.
  it("renders a BARE literal verbatim even with a catalog present", () => {
    const bare: FormatSlot = {
      name: "value",
      preset: "number",
      zero: { text: "none left" },
    };
    expect(formatSlotValue("0", bare, cs, t)).toBe("none left");
  });

  it("renders a translatable literal verbatim when no translator is supplied", () => {
    expect(formatSlotValue("0", translatable, cs)).toBe("none left");
  });
});

describe("a preset the runtime does not know", () => {
  // The version seam: the console emits the spec, the runtime renders it,
  // and nothing holds their versions together — the runtime is vendored out
  // of whichever w17ctl binary the consumer ran. So a newer console CAN name
  // a preset this build has never compiled a case for.
  //
  // Asserted on the RENDERED STRING rather than on "does not throw": the
  // failure this replaces did not throw. It returned undefined from a
  // function declared to return string, and the cell showed "undefined".
  it("renders the raw value, not 'undefined'", () => {
    // Cast the whole slot, not the field: `FormatPreset` is a closed union,
    // so a preset from a newer console is a shape the type system says
    // cannot exist — which is the entire point of this test, and why the
    // cast belongs here rather than in a widened production type.
    const fromNewerConsole = {
      preset: "currency",
      has_places: true,
      places: 2,
    } as unknown as FormatSlot;
    const out = formatSlotValue("1234.5", fromNewerConsole, {
      locale: "en",
      overrides: {},
    });
    expect(out).toBe("1234.5");
    expect(out).not.toContain("undefined");
  });

  it("still honours default and zero, which run before the preset", () => {
    const ctx = { locale: "en", overrides: {} };
    const fromNewerConsole = {
      preset: "currency",
      default: { text: "—" },
    } as unknown as FormatSlot;
    expect(formatSlotValue(null, fromNewerConsole, ctx)).toBe("—");
  });
});
