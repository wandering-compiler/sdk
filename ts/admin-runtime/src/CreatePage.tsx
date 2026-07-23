// Create view — the "Add <model>" form behind a page's
// `detail.create_endpoint`.
//
// Deliberately a sibling of DetailPage rather than a mode flag on
// it: DetailPage is built around an existing row (it fetches one,
// renders inlines / row actions / detail tabs / a title template
// derived from row values). None of that exists before the row
// does, so folding create into it would mean guarding every one
// of those branches. What the two genuinely share — the per-field
// input dispatch, including consumer-registered field slots — is
// imported from DetailPage so the forms can't drift.
//
// The field list comes from `detail.create_fields` (the CREATE
// request's own shape), NOT `detail.fields` (which describes the
// update form and is derived from a different message).
// `readonly_fields` are not applied: they are sourced from the
// read response, which has no meaning for a row that does not
// exist yet.

import { useState } from "react";
import { Button, Group, Paper, Stack } from "@mantine/core";
import { useForm } from "react-hook-form";

import { renderFieldInput } from "./DetailPage";
import { PageHeader, StateView } from "./components";
import { pageLabel } from "./format";
import { apiPost } from "./api";
import { hasAllPermissions } from "./auth";
import type { AdminPageSpec, SlotRegistry, WhoAmIResp } from "./types";

export interface CreatePageProps {
  page: AdminPageSpec;
  whoami?: WhoAmIResp | null;
  slots?: SlotRegistry;
  onBack: () => void;
  // Called with the created row's id when the mutation returns
  // one, so the caller can jump straight to the new row's detail.
  // Undefined id = caller falls back to the list.
  onCreated: (newId: string | undefined) => void;
}

export function CreatePage({ page, whoami, slots, onBack, onCreated }: CreatePageProps) {
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const { register, handleSubmit } = useForm<Record<string, unknown>>();

  const endpoint = page.detail?.create_endpoint;
  const fields = page.detail?.create_fields || [];
  const fieldTypes = page.detail?.field_types || {};
  // Backend enforces regardless; this only hides a form the user
  // could not submit anyway.
  const canCreate = hasAllPermissions(
    whoami?.permission_ids,
    page.detail?.required_permissions_create,
  );

  if (!endpoint || !canCreate) {
    return (
      <Stack gap="lg">
        <PageHeader title={pageLabel(page)} onBack={onBack} />
        <StateView kind="error" message="You cannot create records on this page." />
      </Stack>
    );
  }

  async function onSubmit(values: Record<string, unknown>) {
    setSaving(true);
    setError(null);
    try {
      // Send only the declared create fields — anything else the
      // form picked up is not part of the create request.
      const payload: Record<string, unknown> = {};
      for (const f of fields) {
        if (f in values) payload[f] = values[f];
      }
      const created = await apiPost<Record<string, unknown>>(endpoint as string, payload);
      onCreated(extractCreatedId(created));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setSaving(false);
    }
  }

  return (
    <Stack gap="lg">
      <PageHeader title={`Add ${pageLabel(page)}`} onBack={onBack} />
      {error && <StateView kind="error" message={error} />}
      <Paper withBorder p="md" radius="md">
        <form onSubmit={handleSubmit(onSubmit)}>
          <Stack>
            {fields.map((f) =>
              renderFieldInput({
                field: f,
                mode: "editable",
                semType: fieldTypes[f],
                // No existing value to seed — the row is new.
                rawValue: undefined,
                disabled: saving,
                register,
                slots,
                page,
              }),
            )}
            <Group justify="flex-end">
              <Button variant="default" onClick={onBack} disabled={saving}>
                Cancel
              </Button>
              <Button type="submit" loading={saving}>
                Create
              </Button>
            </Group>
          </Stack>
        </form>
      </Paper>
    </Stack>
  );
}

// extractCreatedId digs the new row's id out of the create
// response. Exported for unit tests — it is a heuristic over a
// consumer-defined shape, so the shapes it does and does not
// recognise need pinning. The response shape is consumer-defined, so try the
// conventional spots in order and give up quietly — a missing id
// is not an error, it just means we navigate to the list instead
// of the new row.
export function extractCreatedId(
  resp: Record<string, unknown> | null | undefined,
): string | undefined {
  if (!resp || typeof resp !== "object") return undefined;
  const direct = resp["id"];
  if (typeof direct === "string" && direct) return direct;
  if (typeof direct === "number") return String(direct);
  // Mutations commonly return { <model>: { id } } or { <model>_id }.
  for (const [key, value] of Object.entries(resp)) {
    if (key.endsWith("_id") && (typeof value === "string" || typeof value === "number")) {
      return String(value);
    }
    if (value && typeof value === "object") {
      const nested = (value as Record<string, unknown>)["id"];
      if (typeof nested === "string" && nested) return nested;
      if (typeof nested === "number") return String(nested);
    }
  }
  return undefined;
}
