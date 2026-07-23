// Generic detail view — react-hook-form rendering an
// arbitrary record's fields per spec. Walking-skeleton iter-1
// handles flat fields + readonly_fields; iter-2 lifts
// fieldsets rendering + per-field UI overrides.

import { useEffect, useState } from "react";
import {
  Accordion,
  Button,
  Group,
  Paper,
  PasswordInput,
  Stack,
  Tabs,
  TextInput,
  Title,
} from "@mantine/core";
import { useForm, UseFormRegister } from "react-hook-form";

import { ActionModal } from "./ActionModal";
import { InlineSection } from "./InlineSection";
import { PageHeader, StateView } from "./components";
import { humanizeLabel, pageLabel } from "./format";
import { apiDelete, apiGet, apiPatch, displayString, formatTitle } from "./api";
import { hasAllPermissions } from "./auth";
import { detailFieldSlotKey } from "./types";
import type { SlotComponent } from "./types";
import type {
  AdminActionSpec,
  AdminFieldsetSpec,
  AdminPageSpec,
  AdminSpec,
  SlotRegistry,
  WhoAmIResp,
} from "./types";

export interface DetailPageProps {
  spec: AdminSpec;
  page: AdminPageSpec;
  rowId: string;
  whoami?: WhoAmIResp | null;
  slots?: SlotRegistry;
  onBack: () => void;
  // Called when an inline row's link column is clicked — the
  // child page becomes the current view. App.tsx threads the
  // setView setter here; iter-1+ relies on in-memory routing.
  onSelectInlineRow?: (childPageName: string, childId: string) => void;
}

export function DetailPage({
  spec,
  page,
  rowId,
  whoami,
  slots,
  onBack,
  onSelectInlineRow,
}: DetailPageProps) {
  const [row, setRow] = useState<Record<string, unknown> | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [reloadTick, setReloadTick] = useState(0);
  const [openAction, setOpenAction] = useState<string | null>(null);

  const { register, handleSubmit, reset } = useForm<Record<string, unknown>>();

  useEffect(() => {
    let cancelled = false;
    const url = page.detail.read_endpoint.replace("{id}", encodeURIComponent(rowId));
    apiGet<Record<string, unknown>>(url)
      .then((resp) => {
        if (cancelled) return;
        setRow(resp);
        // Seed the form with current row values. Only the
        // fields we render get registered; extras pass
        // through untouched on submit (they're not in the
        // form state at all).
        reset(resp);
      })
      .catch((err) => {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : String(err));
      });
    return () => {
      cancelled = true;
    };
  }, [page.detail.read_endpoint, rowId, reset, reloadTick]);

  // DETAIL-target actions render as buttons in the detail header
  // alongside Delete. BOTH-target actions render here too.
  // LIST-only filtered out (those appear on ListPage). REV-150
  // iter-3 — actions whose required_permissions the user lacks
  // are filtered out entirely; the button never renders.
  const detailActions: [string, AdminActionSpec][] = Object.entries(page.actions || {}).filter(
    ([, a]) =>
      (a.target === "DETAIL" || a.target === "BOTH") &&
      hasAllPermissions(whoami?.permission_ids, a.required_permissions),
  );

  // Per-endpoint perm gates (REV-150 iter-3). Backend handler
  // still enforces; SPA hides UI consumers shouldn't trigger.
  const canUpdate = hasAllPermissions(
    whoami?.permission_ids,
    page.detail.required_permissions_update,
  );
  const canDelete = hasAllPermissions(
    whoami?.permission_ids,
    page.detail.required_permissions_delete,
  );

  async function onSave(values: Record<string, unknown>) {
    if (!page.detail.update_endpoint) return;
    setSaving(true);
    setError(null);
    try {
      const url = page.detail.update_endpoint.replace("{id}", encodeURIComponent(rowId));
      // Send only the editable fields; readonly + auto-generated
      // fields stay where they were read from.
      const payload: Record<string, unknown> = {};
      for (const f of page.detail.fields ?? []) {
        if (f in values) payload[f] = values[f];
      }
      const updated = await apiPatch<Record<string, unknown>>(url, payload);
      setRow(updated);
      reset(updated);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  }

  async function onDelete() {
    if (!page.detail.delete_endpoint) return;
    // Walking-skeleton: simple confirm(); iter-2 wires Mantine modal.
    if (!window.confirm("Delete this row?")) return;
    setSaving(true);
    setError(null);
    try {
      const url = page.detail.delete_endpoint.replace("{id}", encodeURIComponent(rowId));
      await apiDelete(url);
      onBack();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setSaving(false);
    }
  }

  if (error) {
    return (
      <Stack gap="lg">
        <PageHeader title={pageLabel(page)} onBack={onBack} />
        <StateView kind="error" message={error} />
      </Stack>
    );
  }
  if (row == null) {
    return (
      <Stack gap="lg">
        <PageHeader title={pageLabel(page)} onBack={onBack} />
        <StateView kind="loading" />
      </Stack>
    );
  }

  const readonlySet = new Set(page.detail.readonly_fields || []);
  const fieldsets = page.detail.fieldsets || [];
  const hasFieldsets = fieldsets.length > 0;
  const flatEditableFields = page.detail.fields.filter((f) => !readonlySet.has(f));
  const flatReadonlyFields = (page.detail.readonly_fields || []).slice();
  const fieldTypes = page.detail.field_types || {};

  // Detail-tab slots — consumer-registered components keyed by
  // `<pageName>:detail:tab:<id>`. When any exist, the page wraps
  // its content in <Tabs> with "Detail" as the default tab; each
  // slot becomes an additional tab. Sort by id so registration
  // order doesn't shuffle the tab strip.
  const tabSlotPrefix = page.name + ":detail:tab:";
  const tabSlotEntries: [string, SlotComponent][] = Object.entries(slots || {})
    .filter(([key]) => key.startsWith(tabSlotPrefix))
    .map(([key, Comp]) => [key.slice(tabSlotPrefix.length), Comp] as [string, SlotComponent])
    .sort(([a], [b]) => a.localeCompare(b));
  const hasExtraTabs = tabSlotEntries.length > 0;

  const detailBody = (
    <Stack>
      {page.inlines && page.inlines.length > 0 && onSelectInlineRow && (
        <Stack>
          {page.inlines.map((inline) => (
            <InlineSection
              key={inline.page}
              spec={spec}
              inline={inline}
              parentId={rowId}
              onSelectChild={onSelectInlineRow}
            />
          ))}
        </Stack>
      )}

      <Paper withBorder p="md" radius="md">
        <form onSubmit={handleSubmit(onSave)}>
          <Stack>
            {hasFieldsets ? (
              <FieldsetSections
                fieldsets={fieldsets}
                readonlySet={readonlySet}
                row={row}
                disabled={!page.detail.update_endpoint || !canUpdate || saving}
                register={register}
                fieldTypes={fieldTypes}
                slots={slots}
                page={page}
              />
            ) : (
              <>
                {flatEditableFields.map((f) =>
                  renderFieldInput({
                    field: f,
                    mode: "editable",
                    semType: fieldTypes[f],
                    rawValue: row[f],
                    disabled: !page.detail.update_endpoint || !canUpdate || saving,
                    register,
                    slots,
                    page,
                  }),
                )}
                {flatReadonlyFields.map((f) =>
                  renderFieldInput({
                    field: f,
                    mode: "readonly",
                    semType: fieldTypes[f],
                    rawValue: row[f],
                    disabled: true,
                    register,
                    slots,
                    page,
                  }),
                )}
              </>
            )}
            {page.detail.update_endpoint && canUpdate && (
              <Group justify="flex-end">
                <Button type="submit" loading={saving}>
                  Save
                </Button>
              </Group>
            )}
          </Stack>
        </form>
      </Paper>
    </Stack>
  );

  const titleTemplate = spec.title_templates?.[page.name];
  const resolvedTitle = titleTemplate ? formatTitle(titleTemplate, row).trim() : "";
  const headerTitle = resolvedTitle || pageLabel(page);
  const headerSubtitle = resolvedTitle ? `${pageLabel(page)} · ${rowId}` : `ID: ${rowId}`;

  return (
    <Stack gap="lg">
      <PageHeader
        title={headerTitle}
        subtitle={headerSubtitle}
        onBack={onBack}
        actions={
          <>
            {detailActions.map(([name, action]) => (
              <Button key={name} variant="light" onClick={() => setOpenAction(name)}>
                {action.label || humanizeLabel(name)}
              </Button>
            ))}
            {page.detail.delete_endpoint && canDelete && (
              <Button color="red" variant="light" onClick={onDelete} loading={saving}>
                Delete
              </Button>
            )}
          </>
        }
      />

      {detailActions.map(([name, action]) => (
        <ActionModal
          key={name}
          action={action}
          actionName={name}
          open={openAction === name}
          onClose={() => setOpenAction(null)}
          onSuccess={() => setReloadTick((t) => t + 1)}
          // DETAIL-target actions act on the current row only.
          selectedIds={[rowId]}
        />
      ))}

      {hasExtraTabs ? (
        <Tabs defaultValue="__detail">
          <Tabs.List>
            <Tabs.Tab value="__detail">Detail</Tabs.Tab>
            {tabSlotEntries.map(([id]) => (
              <Tabs.Tab key={id} value={id}>
                {humanizeTabId(id)}
              </Tabs.Tab>
            ))}
          </Tabs.List>
          <Tabs.Panel value="__detail" pt="md">
            {detailBody}
          </Tabs.Panel>
          {tabSlotEntries.map(([id, Comp]) => (
            <Tabs.Panel key={id} value={id} pt="md">
              <Comp row={row} rowId={rowId} page={page} spec={spec} />
            </Tabs.Panel>
          ))}
        </Tabs>
      ) : (
        detailBody
      )}
    </Stack>
  );
}

// humanizeTabId turns a slot id like "audit_log" into a tab
// label like "Audit log" — kebab-case ids work the same way.
function humanizeTabId(id: string): string {
  const spaced = id.replace(/[_-]+/g, " ").trim();
  if (!spaced) return id;
  return spaced.charAt(0).toUpperCase() + spaced.slice(1);
}

// renderFieldInput dispatches one field's input rendering.
// Lookup order:
//   1. `<page>:detail:field:<field>` slot — when the consumer
//      registered a component at the slot key, render it with
//      { field, value, mode, disabled, page, register }.
//      Slot fully owns the layout; useful for JSON editors,
//      file uploads, markdown, FK pickers.
//   2. Built-in semType dispatch — PasswordInput / TextInput
//      with the semantic type's hint.
//
// `mode` distinguishes "editable" vs "readonly" so slots can
// vary their output (e.g. show rendered markdown read-only,
// markdown editor when editable).
//
// Exported so CreatePage renders its form with the SAME field
// dispatch — a create form must honour the consumer's registered
// field slots and semantic-type widgets exactly like the edit
// form, and duplicating this would let the two drift.
export function renderFieldInput({
  field,
  mode,
  semType,
  rawValue,
  disabled,
  register,
  slots,
  page,
}: {
  field: string;
  mode: "editable" | "readonly";
  semType: string | undefined;
  rawValue: unknown;
  disabled: boolean;
  register: UseFormRegister<Record<string, unknown>>;
  slots: SlotRegistry | undefined;
  page: AdminPageSpec;
}) {
  const SlotComp = slots?.[detailFieldSlotKey(page.name, field)];
  if (SlotComp) {
    return (
      <SlotComp
        key={field}
        field={field}
        value={rawValue}
        mode={mode}
        disabled={disabled}
        page={page}
        register={register}
      />
    );
  }
  const value = displayString(rawValue);
  if (mode === "readonly") {
    if (semType === "PASSWORD") {
      // Don't render the hash — useless to display + invites
      // mistakes (e.g., screenshot leaks). Show an inert
      // placeholder so the form layout doesn't collapse.
      return (
        <TextInput key={field} label={humanizeLabel(field)} disabled value="••••••••" readOnly />
      );
    }
    return <TextInput key={field} label={humanizeLabel(field)} disabled value={value} readOnly />;
  }
  if (semType === "PASSWORD") {
    return (
      <PasswordInput
        key={field}
        label={humanizeLabel(field)}
        placeholder="Leave empty to keep current"
        // No defaultValue — passwords never round-trip from the
        // read response (the stored hash is useless to pre-fill
        // with). Empty submit = "don't change" per REV-151.
        disabled={disabled}
        {...register(field)}
      />
    );
  }
  return (
    <TextInput
      key={field}
      label={humanizeLabel(field)}
      disabled={disabled}
      defaultValue={value}
      {...register(field)}
    />
  );
}

// FieldsetSections renders the detail form's grouped sections.
// Each fieldset becomes an Accordion item when `collapsed: true`
// is set somewhere; otherwise sections render as plain stacked
// blocks with their title as a heading. readonly_fields apply
// orthogonally — a field listed in a fieldset that is ALSO in
// readonly_fields renders disabled.
function FieldsetSections({
  fieldsets,
  readonlySet,
  row,
  disabled,
  register,
  fieldTypes,
  slots,
  page,
}: {
  fieldsets: AdminFieldsetSpec[];
  readonlySet: Set<string>;
  row: Record<string, unknown>;
  disabled: boolean;
  register: UseFormRegister<Record<string, unknown>>;
  fieldTypes: Record<string, string>;
  slots: SlotRegistry | undefined;
  page: AdminPageSpec;
}) {
  const anyCollapsed = fieldsets.some((fs) => fs.collapsed);
  if (anyCollapsed) {
    // Mixed mode: render every section as an Accordion item, so
    // collapsed flags work AND the layout is uniform. Sections
    // without `collapsed: true` open by default via
    // defaultValue.
    const defaultOpen = fieldsets
      .map((fs, i) => (fs.collapsed ? null : `fs-${i}`))
      .filter((v): v is string => v != null);
    return (
      <Accordion multiple defaultValue={defaultOpen} variant="separated">
        {fieldsets.map((fs, i) => (
          <Accordion.Item key={`fs-${i}`} value={`fs-${i}`}>
            <Accordion.Control>{fs.title || "Section"}</Accordion.Control>
            <Accordion.Panel>
              <FieldsetBody
                fields={fs.fields ?? []}
                readonlySet={readonlySet}
                row={row}
                disabled={disabled}
                register={register}
                fieldTypes={fieldTypes}
                slots={slots}
                page={page}
              />
            </Accordion.Panel>
          </Accordion.Item>
        ))}
      </Accordion>
    );
  }
  // No collapsed sections — render flat with section titles.
  return (
    <Stack>
      {fieldsets.map((fs, i) => (
        <Stack key={`fs-${i}`} gap="xs">
          {fs.title && <Title order={5}>{fs.title}</Title>}
          <FieldsetBody
            fields={fs.fields ?? []}
            readonlySet={readonlySet}
            row={row}
            disabled={disabled}
            register={register}
            fieldTypes={fieldTypes}
            slots={slots}
            page={page}
          />
        </Stack>
      ))}
    </Stack>
  );
}

function FieldsetBody({
  fields,
  readonlySet,
  row,
  disabled,
  register,
  fieldTypes,
  slots,
  page,
}: {
  fields: string[];
  readonlySet: Set<string>;
  row: Record<string, unknown>;
  disabled: boolean;
  register: UseFormRegister<Record<string, unknown>>;
  fieldTypes: Record<string, string>;
  slots: SlotRegistry | undefined;
  page: AdminPageSpec;
}) {
  return (
    <Stack gap="sm">
      {fields.map((f) => {
        const isReadonly = readonlySet.has(f);
        return renderFieldInput({
          field: f,
          mode: isReadonly ? "readonly" : "editable",
          semType: fieldTypes[f],
          rawValue: row[f],
          disabled: isReadonly || disabled,
          register,
          slots,
          page,
        });
      })}
    </Stack>
  );
}
