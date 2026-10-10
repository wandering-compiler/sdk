// InlineSection — renders one nested-inline list below the
// parent's detail form. Mirrors ListPage's table for TABULAR
// layout; STACKED layout renders cards per child row.
//
// Inline write UI:
//   - `Add` button at the top renders when the spec carries
//     `create_endpoint` (POST /api/inline/<parent>/{id}/<inline>).
//   - Per-row `Edit` / `Delete` buttons render when the spec
//     carries `update_endpoint` / `delete_endpoint`. Update PATCHes
//     /api/inline/.../{child_id}; Delete DELETEs the same.
//   - Form pulls editable fields from `spec.pages[inline.page].
//     detail.fields` when present, falling back to the list's
//     columns minus "id". Inputs are plain TextInput — same
//     shape as ActionModal; typed inputs are an iter-3 lift.

import { useEffect, useState } from "react";
import {
  Anchor,
  Button,
  Card,
  Group,
  Loader,
  Modal,
  Stack,
  Table,
  Text,
  TextInput,
  PasswordInput,
  Title,
} from "@mantine/core";

import { apiDelete, apiGet, apiPatch, apiPost, displayString } from "./api";
import { useT } from "./i18n";
import { columnHeader, humanizeLabel } from "./format";
import type { AdminInlineSpec, AdminPageSpec, AdminSpec } from "./types";

export interface InlineSectionProps {
  spec: AdminSpec;
  inline: AdminInlineSpec;
  parentId: string;
  // Click on the link column → navigate to the inline page's
  // detail view. ListPage uses the same onSelectRow shape; the
  // parent (DetailPage) threads this through so consumers reach
  // the child detail without leaving the admin.
  onSelectChild: (childPageName: string, childId: string) => void;
}

type Row = Record<string, unknown>;

interface ListResp {
  [key: string]: unknown;
}

export function InlineSection({ spec, inline, parentId, onSelectChild }: InlineSectionProps) {
  const t = useT();
  const [rows, setRows] = useState<Row[] | null>(null);
  // TWO errors, because they mean different things to the reader.
  //
  // A failed LOAD means there is no collection to show, so the body gives way to
  // the message. A failed DELETE means the collection on screen is still the
  // truth and one action did not happen — hiding the table there loses data the
  // operator already had, and leaves them with no way back: nothing re-fetches
  // except a successful mutation.
  //
  // Both were one `error` before, and neither was ever cleared, so a single
  // failed request blanked the section for the life of the page even after a
  // later load succeeded.
  const [loadError, setLoadError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [reloadTick, setReloadTick] = useState(0);
  const [formMode, setFormMode] = useState<"add" | "edit" | null>(null);
  const [formRow, setFormRow] = useState<Row | null>(null);

  const targetPage: AdminPageSpec | undefined = spec.pages[inline.page];

  useEffect(() => {
    let cancelled = false;
    const url = inline.endpoint.replace("{id}", encodeURIComponent(parentId));
    apiGet<ListResp>(url)
      .then((resp) => {
        if (cancelled) return;
        setLoadError(null);
        // And the ACTION error, because a load replaces the collection: a delete
        // that failed against the previous parent has nothing to say about these
        // rows, and leaving it up attaches a message to data it was never about.
        // Splitting the two errors without this only moved the stale-message bug
        // one level down.
        setActionError(null);
        setRows(extractRows(resp));
      })
      .catch((err) => {
        if (cancelled) return;
        setLoadError(err instanceof Error ? err.message : String(err));
      });
    return () => {
      cancelled = true;
    };
  }, [inline.endpoint, parentId, reloadTick]);

  if (!targetPage) {
    return (
      <Stack gap="xs">
        <Title order={5}>{inline.label ? t(inline.label) : humanizeLabel(inline.page)}</Title>
        <Text c="red">
          {t("Inline target page {page} is missing from the spec.", { page: inline.page })}
        </Text>
      </Stack>
    );
  }

  // Columns inherited from the target page's list.columns. When
  // the target page has no list (detail-only target), fall back
  // to "(no columns declared)" — caller's storage method must
  // return a shape the SPA can introspect; iter-3 lift.
  const columns = targetPage.list?.columns || [];
  const columnNames = columns.map((c) => c.name);
  // Same rule as ListPage: an inlined pivot page may itself be list-only,
  // and the inline table's link column would open a detail it has not got
  // (the same defect as ListPage's, through the inline door).
  const linkCol = targetPage.detail
    ? targetPage.list?.detail_link_column || columnNames[0]
    : undefined;

  // Editable field list for the create/update form. Prefer the
  // child page's `detail.fields` (those are the consumer's
  // authored editable set); fall back to list.columns minus
  // "id" when the child page has no detail spec.
  const editableFields: string[] = targetPage.detail?.fields
    ? targetPage.detail.fields
    : columnNames.filter((c) => c !== "id");

  const canCreate = !!inline.create_endpoint;
  const canUpdate = !!inline.update_endpoint;
  const canDelete = !!inline.delete_endpoint;

  const openAdd = () => {
    setFormRow(null);
    setFormMode("add");
  };
  const openEdit = (row: Row) => {
    setFormRow(row);
    setFormMode("edit");
  };
  const closeForm = () => {
    setFormMode(null);
    setFormRow(null);
  };
  const afterMutation = () => {
    closeForm();
    setReloadTick((t) => t + 1);
  };

  const handleDelete = async (childId: string) => {
    if (!inline.delete_endpoint) return;
    if (!window.confirm(t("Delete this row?"))) return;
    try {
      const url = inline.delete_endpoint
        .replace("{id}", encodeURIComponent(parentId))
        .replace("{child_id}", encodeURIComponent(childId));
      setActionError(null);
      await apiDelete(url);
      setReloadTick((t) => t + 1);
    } catch (err) {
      setActionError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <Stack gap="xs">
      <Group justify="space-between">
        <Title order={5}>{inline.label ? t(inline.label) : humanizeLabel(inline.page)}</Title>
        {canCreate && (
          <Button size="xs" variant="light" onClick={openAdd}>
            {t("Add {name}", { name: humanizeLabel(inline.page) })}
          </Button>
        )}
      </Group>

      {loadError && <Text c="red">{loadError}</Text>}
      {actionError && <Text c="red">{actionError}</Text>}
      {!loadError && rows == null && (
        <Group>
          <Loader size="sm" />
          <Text size="sm">{t("Loading…")}</Text>
        </Group>
      )}
      {!loadError && rows != null && rows.length === 0 && (
        <Text c="dimmed" size="sm">
          {t("No related {name} rows.", { name: humanizeLabel(inline.page) })}
        </Text>
      )}
      {!loadError &&
      rows != null &&
      rows.length > 0 &&
      columns.length > 0 &&
      inline.layout === "STACKED" ? (
        <Stack gap="xs">
          {rows.map((row, i) => {
            const id = String(getRowId(row, i));
            return (
              <Card key={id} withBorder shadow="xs" padding="sm" radius="sm">
                <Stack gap={4}>
                  {columns.map((col) => {
                    const c = col.name;
                    const display = renderCell(row[c]);
                    if (c === linkCol) {
                      return (
                        <Group key={c} gap={4}>
                          <Text size="xs" c="dimmed" tt="uppercase">
                            {columnHeader(col)}
                          </Text>
                          <Anchor
                            component="button"
                            size="sm"
                            onClick={() => onSelectChild(inline.page, id)}
                          >
                            {display}
                          </Anchor>
                        </Group>
                      );
                    }
                    return (
                      <Group key={c} gap={4}>
                        <Text size="xs" c="dimmed" tt="uppercase">
                          {columnHeader(col)}
                        </Text>
                        <Text size="sm">{display}</Text>
                      </Group>
                    );
                  })}
                  {(canUpdate || canDelete) && (
                    <Group gap="xs" mt="xs">
                      {canUpdate && (
                        <Button size="xs" variant="subtle" onClick={() => openEdit(row)}>
                          {t("Edit")}
                        </Button>
                      )}
                      {canDelete && (
                        <Button
                          size="xs"
                          variant="subtle"
                          color="red"
                          onClick={() => handleDelete(id)}
                        >
                          {t("Delete")}
                        </Button>
                      )}
                    </Group>
                  )}
                </Stack>
              </Card>
            );
          })}
        </Stack>
      ) : !loadError && rows != null && rows.length > 0 && columns.length > 0 ? (
        <Table striped withTableBorder>
          <Table.Thead>
            <Table.Tr>
              {columns.map((col) => (
                <Table.Th key={col.name}>{columnHeader(col)}</Table.Th>
              ))}
              {(canUpdate || canDelete) && (
                <Table.Th style={{ textAlign: "right" }}>{/* actions */}</Table.Th>
              )}
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {rows.map((row, i) => {
              const id = String(getRowId(row, i));
              return (
                <Table.Tr key={id}>
                  {columns.map((col) => {
                    const c = col.name;
                    const display = renderCell(row[c]);
                    if (c === linkCol) {
                      return (
                        <Table.Td key={c}>
                          <Anchor component="button" onClick={() => onSelectChild(inline.page, id)}>
                            {display}
                          </Anchor>
                        </Table.Td>
                      );
                    }
                    return <Table.Td key={c}>{display}</Table.Td>;
                  })}
                  {(canUpdate || canDelete) && (
                    <Table.Td style={{ textAlign: "right" }}>
                      <Group gap="xs" justify="flex-end">
                        {canUpdate && (
                          <Button size="xs" variant="subtle" onClick={() => openEdit(row)}>
                            {t("Edit")}
                          </Button>
                        )}
                        {canDelete && (
                          <Button
                            size="xs"
                            variant="subtle"
                            color="red"
                            onClick={() => handleDelete(id)}
                          >
                            {t("Delete")}
                          </Button>
                        )}
                      </Group>
                    </Table.Td>
                  )}
                </Table.Tr>
              );
            })}
          </Table.Tbody>
        </Table>
      ) : null}

      {formMode && (
        <InlineFormModal
          mode={formMode}
          row={formRow}
          fields={editableFields}
          fieldTypes={targetPage.detail?.field_types || {}}
          inline={inline}
          parentId={parentId}
          onClose={closeForm}
          onSuccess={afterMutation}
        />
      )}
    </Stack>
  );
}

// InlineFormModal — Mantine Modal collecting the editable-field
// values for an inline create or update. Plain TextInput per
// field (same shape as ActionModal). On submit:
//   - "add"  → POST inline.create_endpoint with body =
//              { [field]: value, ... }; parent_id stamped from
//              the URL on the server.
//   - "edit" → PATCH inline.update_endpoint with body = same
//              shape; parent_id + child_id stamped from the URL.
interface InlineFormModalProps {
  mode: "add" | "edit";
  row: Row | null;
  fields: string[];
  // The CHILD page's semantic field types, so a secret is not treated like a string.
  // Without them this form seeded every field from the row into a plain TextInput —
  // including a password column, whose stored hash then sat there in clear text and
  // went back on submit as the new password. Same defect DetailPage carried,
  // one layer down and unmasked.
  fieldTypes: Record<string, string>;
  inline: AdminInlineSpec;
  parentId: string;
  onClose: () => void;
  onSuccess: () => void;
}

function InlineFormModal({
  mode,
  row,
  fields,
  fieldTypes,
  inline,
  parentId,
  onClose,
  onSuccess,
}: InlineFormModalProps) {
  const t = useT();
  const initial: Record<string, string> = {};
  for (const f of fields) {
    // A PASSWORD field is never seeded: the row carries the stored hash, and an empty
    // submit means "leave it alone" — the same contract DetailPage states. SECRET and
    // CRYPTED_SECRET read back their real value, so they ARE seeded; they are only
    // masked below.
    const v = fieldTypes[f] === "PASSWORD" ? undefined : row ? row[f] : undefined;
    initial[f] = displayString(v);
  }
  const [values, setValues] = useState<Record<string, string>>(initial);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const childId = row ? String(getRowId(row, 0)) : "";

  const handleSubmit = async () => {
    setError(null);
    setSubmitting(true);
    try {
      const body: Record<string, unknown> = {};
      for (const f of fields) {
        if (f in values) body[f] = values[f];
      }
      if (mode === "add") {
        if (!inline.create_endpoint) throw new Error("create_endpoint missing");
        const url = inline.create_endpoint.replace("{id}", encodeURIComponent(parentId));
        await apiPost(url, body);
      } else {
        if (!inline.update_endpoint) throw new Error("update_endpoint missing");
        const url = inline.update_endpoint
          .replace("{id}", encodeURIComponent(parentId))
          .replace("{child_id}", encodeURIComponent(childId));
        await apiPatch(url, body);
      }
      onSuccess();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setSubmitting(false);
    }
  };

  const title =
    mode === "add"
      ? t("Add {name}", { name: humanizeLabel(inline.page) })
      : t("Edit {name}", { name: humanizeLabel(inline.page) });

  return (
    <Modal opened onClose={onClose} title={title} centered>
      <Stack>
        {fields.map((f) =>
          fieldTypes[f] === "PASSWORD" ||
          fieldTypes[f] === "SECRET" ||
          fieldTypes[f] === "CRYPTED_SECRET" ? (
            <PasswordInput
              key={f}
              label={humanizeLabel(f)}
              // Only a password is empty-means-unchanged; a seeded secret must not be
              // told to leave itself empty.
              placeholder={
                fieldTypes[f] === "PASSWORD" ? t("Leave empty to keep current") : undefined
              }
              value={values[f] || ""}
              onChange={(e) => {
                const value = e.currentTarget.value;
                setValues((prev) => ({ ...prev, [f]: value }));
              }}
              disabled={submitting}
            />
          ) : (
            <TextInput
              key={f}
              label={humanizeLabel(f)}
              value={values[f] || ""}
              // Value read BEFORE the updater — see eventhandlers.test.ts. React nulls a
              // synthetic event's `currentTarget` once the handler returns and a
              // functional updater runs later, so the inline form's fields threw
              // "Cannot read properties of null (reading 'value')" on the first
              // keystroke. Same defect as ActionModal's, found by sweeping for it.
              onChange={(e) => {
                const value = e.currentTarget.value;
                setValues((prev) => ({ ...prev, [f]: value }));
              }}
              disabled={submitting}
            />
          ),
        )}
        {error && (
          <Text c="red" size="sm">
            {error}
          </Text>
        )}
        <Group justify="flex-end">
          <Button variant="subtle" onClick={onClose} disabled={submitting}>
            {t("Cancel")}
          </Button>
          <Button onClick={handleSubmit} loading={submitting}>
            {mode === "add" ? t("Create") : t("Save")}
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}

function extractRows(resp: ListResp): Row[] {
  for (const k of Object.keys(resp)) {
    const v = resp[k];
    if (Array.isArray(v)) return v as Row[];
  }
  return [];
}

function getRowId(row: Row, fallback: number): string | number {
  if (row.id != null) return displayString(row.id);
  return fallback;
}

function renderCell(v: unknown): string {
  return displayString(v);
}
