// Label humanization — turns machine field / column / page /
// widget identifiers into human-readable labels so the admin UI
// never leaks raw wire names (`org_id`, `amount_units`,
// `TopupAdmin`) at end users. Mirrors Django admin's
// `verbose_name` defaulting, with acronym-awareness on top:
//
//   org_id            → "Org ID"
//   amount_units      → "Amount units"
//   auth_user_count   → "Auth user count"
//   createdAt         → "Created at"
//   TopupAdmin        → "Topup admin"
//
// Sentence-case (first word capitalized, rest lowercased) reads
// calmer than Title Case for dense forms/tables; known acronyms
// are upper-cased wherever they land.

import type { AdminPageSpec } from "./types";

// Tokens that should render fully upper-cased when they appear as
// a whole word. Lower-cased for the membership test.
const ACRONYMS = new Set([
  "id",
  "url",
  "uri",
  "api",
  "ip",
  "uuid",
  "http",
  "https",
  "sql",
  "db",
  "ui",
  "ux",
  "cpu",
  "gpu",
  "ram",
  "io",
  "ssh",
  "tls",
  "ssl",
  "jwt",
  "css",
  "html",
  "json",
  "xml",
  "csv",
  "pdf",
  "sku",
  "vat",
  "acl",
  "dns",
  "cdn",
  "otp",
  "sso",
  "kyc",
]);

/**
 * humanizeLabel converts a machine identifier to a display label.
 * camelCase / PascalCase boundaries and letter→digit runs split
 * into words; `_ - .` become spaces; known acronyms upper-case;
 * the result is sentence-cased. Empty / whitespace input → "".
 */
export function humanizeLabel(raw: string): string {
  if (!raw) return "";
  const spaced = raw
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .replace(/([A-Za-z])([0-9])/g, "$1 $2")
    .replace(/[_\-.]+/g, " ")
    .replace(/\s+/g, " ")
    .trim();
  if (!spaced) return "";
  return spaced
    .split(" ")
    .map((word, i) => {
      const lower = word.toLowerCase();
      if (ACRONYMS.has(lower)) return lower.toUpperCase();
      if (i === 0) return word.charAt(0).toUpperCase() + word.slice(1).toLowerCase();
      return lower;
    })
    .join(" ");
}

/**
 * columnHeader resolves the header text a list column shows: the
 * column's declared `label` override (Django `verbose_name`) when
 * present and non-empty, otherwise the humanized field `name`.
 * Empty / whitespace overrides fall back to humanization so a
 * malformed spec never renders a blank header.
 */
export function columnHeader(col: { name: string; label?: string }): string {
  if (col.label && col.label.trim()) return col.label;
  return humanizeLabel(col.name);
}

/**
 * pageLabel resolves the label a page shows in nav / headings:
 * the author-declared `title` when present, otherwise the
 * humanized page name.
 */
export function pageLabel(page: AdminPageSpec): string {
  return page.title && page.title.trim() ? page.title : humanizeLabel(page.name);
}
