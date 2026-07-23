// InlineSection — renders one nested-inline list below the
// parent's detail form. Mirrors ListPage's table for TABULAR
// layout; STACKED layout renders cards per child row.
//
// REV-150 P32: inline write UI surfaced.
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
  Title,
} from "@mantine/core";

import { apiDelete, apiGet, apiPatch, apiPost, displayString } from "./api";
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
  const [rows, setRows] = useState<Row[] | null>(null);
  const [error, setError] = useState<string | null>(null);
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
        setRows(extractRows(resp));
      })
      .catch((err) => {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : String(err));
      });
    return () => {
      cancelled = true;
    };
  }, [inline.endpoint, parentId, reloadTick]);

  if (!targetPage) {
    return (
      <Stack gap="xs">
        <Title order={5}>{inline.label || humanizeLabel(inline.page)}</Title>
        <Text c="red">Inline target page {inline.page} missing from spec.</Text>
      </Stack>
    );
  }

  // Columns inherited from the target page's list.columns. When
  // the target page has no list (detail-only target), fall back
  // to "(no columns declared)" — caller's storage method must
  // return a shape the SPA can introspect; iter-3 lift.
  const columns = targetPage.list?.columns || [];
  const columnNames = columns.map((c) => c.name);
  const linkCol = targetPage.list?.detail_link_column || columnNames[0];

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
    if (!window.confirm("Delete this row?")) return;
    try {
      const url = inline.delete_endpoint
        .replace("{id}", encodeURIComponent(parentId))
        .replace("{child_id}", encodeURIComponent(childId));
      await apiDelete(url);
      setReloadTick((t) => t + 1);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <Stack gap="xs">
      <Group justify="space-between">
        <Title order={5}>{inline.label || humanizeLabel(inline.page)}</Title>
        {canCreate && (
          <Button size="xs" variant="light" onClick={openAdd}>
            Add {humanizeLabel(inline.page)}
          </Button>
        )}
      </Group>

      {error && <Text c="red">{error}</Text>}
      {!error && rows == null && (
        <Group>
          <Loader size="sm" />
          <Text size="sm">Loading…</Text>
        </Group>
      )}
      {!error && rows != null && rows.length === 0 && (
        <Text c="dimmed" size="sm">
          No related {humanizeLabel(inline.page)} rows.
        </Text>
      )}
      {!error &&
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
                          Edit
                        </Button>
                      )}
                      {canDelete && (
                        <Button
                          size="xs"
                          variant="subtle"
                          color="red"
                          onClick={() => handleDelete(id)}
                        >
                          Delete
                        </Button>
                      )}
                    </Group>
                  )}
                </Stack>
              </Card>
            );
          })}
        </Stack>
      ) : !error && rows != null && rows.length > 0 && columns.length > 0 ? (
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
                            Edit
                          </Button>
                        )}
                        {canDelete && (
                          <Button
                            size="xs"
                            variant="subtle"
                            color="red"
                            onClick={() => handleDelete(id)}
                          >
                            Delete
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
  inline: AdminInlineSpec;
  parentId: string;
  onClose: () => void;
  onSuccess: () => void;
}

function InlineFormModal({
  mode,
  row,
  fields,
  inline,
  parentId,
  onClose,
  onSuccess,
}: InlineFormModalProps) {
  const initial: Record<string, string> = {};
  for (const f of fields) {
    const v = row ? row[f] : undefined;
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
    mode === "add" ? `Add ${humanizeLabel(inline.page)}` : `Edit ${humanizeLabel(inline.page)}`;

  return (
    <Modal opened onClose={onClose} title={title} centered>
      <Stack>
        {fields.map((f) => (
          <TextInput
            key={f}
            label={humanizeLabel(f)}
            value={values[f] || ""}
            onChange={(e) => setValues((prev) => ({ ...prev, [f]: e.currentTarget.value }))}
            disabled={submitting}
          />
        ))}
        {error && (
          <Text c="red" size="sm">
            {error}
          </Text>
        )}
        <Group justify="flex-end">
          <Button variant="subtle" onClick={onClose} disabled={submitting}>
            Cancel
          </Button>
          <Button onClick={handleSubmit} loading={submitting}>
            {mode === "add" ? "Create" : "Save"}
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
