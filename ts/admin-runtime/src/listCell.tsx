// What one cell of a page's list shows — the list page's table and an inline
// section that borrows the page's columns render it through here, so a
// column's choices, its format and its foreign-key title read the same in
// both. The inline section used to render every cell with displayString: a
// ref column showed the bare id the list page titled, an enum its number.

import { Anchor, Tooltip } from "@mantine/core";
import type { ReactNode } from "react";

import { displayString, formatTitle } from "./api";
import { formatCellValue } from "./cellFormat";
import type { FormatContext } from "./cellFormat";
import { resolveChoiceLabel, translateChoiceLabel } from "./choiceLabel";
import type { Translate } from "./i18n";
import type { AdminColumnRefSpec, AdminPageSpec } from "./types";

// columnText is the text of a plain (non-ref) column cell of `page`'s list:
// an enum's choice label, a value through the column's locale-aware format,
// anything else as displayString renders it.
//
// The formatter can decline (a template with no slots), and then the raw
// rendering stands. A cell must never come back blank because its format
// had nothing to say.
export function columnText(
  page: AdminPageSpec,
  field: string,
  val: unknown,
  fmt: FormatContext,
  t: Translate,
): string {
  const choices = page.list?.column_choices?.[field];
  const v = choices ? translateChoiceLabel(resolveChoiceLabel(choices, val), t) : val;
  const formatted = formatCellValue(
    v,
    choices ? undefined : page.list?.column_formats?.[field],
    fmt,
    t,
  );
  return formatted !== undefined ? formatted : displayString(v);
}

// resolveRefCell computes what a foreign-key column cell shows
// (ADMIN-FK): its visible text, its hover tooltip, and the link target
// (page + id) when the ref declares a `page` and the id is non-empty.
// Pure (no React) so the decision is unit-testable independent of the
// DataTable render seam, which does not produce rows under jsdom.
//
// The runtime never fetches the referenced row: `title` must be a
// field the list method already projects (e.g. a DQL JOIN that
// returns `organization_name` next to `org_id`).
//
// TEXT — the ref's `title` resolved ROW-LOCALLY when the author
// declared one ("Acme"); otherwise the COLUMN HEADER ("Organization",
// "Wallet"). The header fallback is what makes the no-title case
// usable: several FK columns side by side used to render an identical
// placeholder (or a raw UUID), so thirty rows down — with the real
// headers scrolled off — there was no telling which link went where.
// Falling back to the header also means a technical child table that
// has no name of its own (a user's wallet / limits / subscription)
// simply declares no `title` and reads as its own column name, instead
// of repeating the owner's name in four adjacent cells.
//
// TOOLTIP — always identifies the exact row behind that text:
// "<title> #<id>" when a title resolved, "#<id>" when only the id is
// known. That is the other half of the trade: the cell may now show
// static text, so hovering has to reveal what is behind it.
//
// A ref with NEITHER a resolved title NOR an id is an empty FK — it
// renders blank, never the bare header, which would read as a link to
// something that isn't there.
export function resolveRefCell(
  ref: AdminColumnRefSpec,
  row: Record<string, unknown>,
  field: string,
  header: string,
): { label: string; tooltip: string; link?: { page: string; id: string } } {
  const rawId = displayString(row[field]);
  const resolved = ref.title ? formatTitle(`{${ref.title}}`, row) : "";
  if (resolved === "" && rawId === "") return { label: "", tooltip: "" };
  const label = resolved !== "" ? resolved : header;
  const idPart = rawId !== "" ? `#${rawId}` : "";
  const tooltip = resolved !== "" && idPart !== "" ? `${resolved} ${idPart}` : resolved || idPart;
  if (ref.page && rawId !== "") {
    return { label, tooltip, link: { page: ref.page, id: rawId } };
  }
  return { label, tooltip };
}

// RefCell renders a foreign-key column cell (resolveRefCell): the title,
// a link to the referenced row's page when the ref names one, and the
// identifying tooltip when it says something the cell does not. A title-less
// ref shows the header and the tooltip carries the id, so that case is always
// worth wrapping; an empty FK renders blank with nothing to hover.
export function RefCell({
  refSpec,
  record,
  field,
  header,
  onOpen,
}: {
  refSpec: AdminColumnRefSpec;
  record: Record<string, unknown>;
  field: string;
  header: string;
  onOpen: (pageName: string, id: string) => void;
}): ReactNode {
  const { label, tooltip, link } = resolveRefCell(refSpec, record, field, header);
  const body = link ? (
    <Anchor
      component="button"
      onClick={(e) => {
        e.stopPropagation();
        onOpen(link.page, link.id);
      }}
    >
      {label}
    </Anchor>
  ) : (
    label
  );
  if (!tooltip || tooltip === label) return body;
  return (
    <Tooltip label={tooltip} withinPortal openDelay={250}>
      <span>{body}</span>
    </Tooltip>
  );
}
