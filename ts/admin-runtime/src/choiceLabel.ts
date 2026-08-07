// choiceLabel — turning an enum's WIRE value into the label a reader
// needs, shared by every surface that shows one.
//
// It lives in its own React-free module because "show the label, not the
// number" is not a list-cell rule: the same value appears in a list cell,
// a chart's legend, a donut slice and a tooltip, and it has to read
// identically in all of them. Keeping the resolution next to one of those
// renderers is how the others end up printing `1`.

import type { AdminChoicesSpec } from "./types";

// resolveChoiceLabel maps an enum value to its human label using the
// field's catalogue.
//
// The JSON wire carries a proto enum as its NUMBER, so a raw rendering
// reads `1` where the operator needs "Refund" — the value's meaning is
// usually the entire reason it is on the screen. The catalogue carries
// the same labels the detail form's <Select> renders, so one value reads
// identically wherever it appears.
//
// Matching is deliberately loose on type: an int32-carrier catalogue
// holds numbers, but the same value can arrive as a numeric STRING
// (a 64-bit carrier, a hand-built fixture, a host that re-serialized
// the row). Comparing the string forms handles both without a carrier
// check.
//
// A repeated enum maps element-wise. An unrecognised value — a server
// that added a member this build hasn't regenerated against — passes
// through untouched rather than rendering blank: a number the reader
// can look up beats an empty cell.
export function resolveChoiceLabel(choices: AdminChoicesSpec, v: unknown): unknown {
  if (v == null) return v;
  if (Array.isArray(v)) return v.map((x) => resolveChoiceLabel(choices, x));
  if (typeof v !== "number" && typeof v !== "string") return v;
  const key = String(v);
  const hit = choices.values.find((c) => String(c.value) === key);
  return hit ? hit.label : v;
}

// translateChoiceLabel puts a RESOLVED label through the catalog.
//
// An enum's label is author-declared prose, so it is a msgid like any
// other. The raw value it falls back to is NOT — a number that happens to
// match a catalog key would otherwise come back as someone else's
// translation.
export function translateChoiceLabel(resolved: unknown, t: (msgid: string) => string): unknown {
  return typeof resolved === "string" ? t(resolved) : resolved;
}
