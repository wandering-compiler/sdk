// Generic detail view — react-hook-form rendering an
// arbitrary record's fields per spec. Walking-skeleton iter-1
// handles flat fields + readonly_fields; iter-2 lifts
// fieldsets rendering + per-field UI overrides.

import { useEffect, useState } from "react";
import {
  Accordion,
  Button,
  Group,
  MultiSelect,
  Paper,
  PasswordInput,
  Select,
  Stack,
  Tabs,
  TextInput,
  Title,
} from "@mantine/core";
import { Control, Controller, useForm, UseFormRegister } from "react-hook-form";

import { ActionModal } from "./ActionModal";
import { InlineSection } from "./InlineSection";
import { PageHeader, StateView } from "./components";
import { humanizeLabel, pageLabel } from "./format";
import { apiDelete, apiGet, apiPatch, displayString, formatTitle } from "./api";
import { hasAllPermissions } from "./auth";
import { detailFieldSlotKey } from "./types";
import { formatCellValue, formatContextFor } from "./cellFormat";
import { resolveChoiceLabel, translateChoiceLabel } from "./choiceLabel";
import { identityTranslate, useT } from "./i18n";
import type { Translate } from "./i18n";
import type { FormatContext, FormatTemplate } from "./cellFormat";
import type { SlotComponent } from "./types";
import type {
  AdminActionSpec,
  AdminChoicesSpec,
  AdminFieldsetSpec,
  AdminPageSpec,
  AdminSpec,
  SlotRegistry,
  WhoAmIResp,
} from "./types";

/**
 * What a DISPLAY-ONLY field needs to render its value the way the list
 * renders the same one.
 *
 * Its presence is the decision: a field whose value the form can submit gets
 * none, because that value round-trips through the input and a grouped
 * "1 234,50" would be PATCHed back as the literal string the user never typed.
 * `format` may still be absent inside it — a text field is display-only and
 * has nothing to format.
 */
export interface FieldDisplay {
  format?: FormatTemplate;
  ctx: FormatContext;
}

/**
 * One field's rendered value. Formats when the caller said the value is
 * display-only AND the compiler resolved a format for it; otherwise the raw
 * rendering stands, exactly as it did before formats existed.
 */
function renderFieldValue(raw: unknown, display: FieldDisplay | undefined, t: Translate): string {
  if (!display) return displayString(raw);
  const formatted = formatCellValue(raw, display.format, display.ctx, t);
  return formatted !== undefined ? formatted : displayString(raw);
}

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

  const { register, handleSubmit, reset, control } = useForm<Record<string, unknown>>();
  const t = useT();

  useEffect(() => {
    let cancelled = false;
    const url = page.detail.read_endpoint.replace("{id}", encodeURIComponent(rowId));
    apiGet<Record<string, unknown>>(url, { response: page.detail.read_response_ref })
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
    // read_response_ref is spec data, constant for the life of the page; it
    // is in the closure but not a reason to refetch.
    // eslint-disable-next-line react-hooks/exhaustive-deps
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
      const updated = await apiPatch<Record<string, unknown>>(url, payload, {
        request: page.detail.update_request_ref,
        response: page.detail.update_response_ref,
      });
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
    if (!window.confirm(t("Delete this row?"))) return;
    setSaving(true);
    setError(null);
    try {
      const url = page.detail.delete_endpoint.replace("{id}", encodeURIComponent(rowId));
      await apiDelete(url, { response: page.detail.delete_response_ref });
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
  const fieldFormats = page.detail.field_formats || {};
  const formatCtx = formatContextFor(spec);
  // A value the form can SUBMIT renders RAW. It round-trips through the
  // input and back out on save, so a grouped "1 234,50" or a day-first date
  // would be PATCHed as the literal string the user never typed. Formatting
  // therefore applies exactly where the value is display-only: a readonly
  // field, or any field on a form that cannot be submitted at all (no update
  // endpoint, or no permission to use it).
  const canSubmit = Boolean(page.detail.update_endpoint) && canUpdate;
  const displayFor = (field: string, readonly: boolean): FieldDisplay | undefined =>
    readonly || !canSubmit ? { format: fieldFormats[field], ctx: formatCtx } : undefined;

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
                control={control}
                fieldTypes={fieldTypes}
                displayFor={displayFor}
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
                    display: displayFor(f, false),
                    choices: page.detail.field_choices?.[f],
                    t,
                    register,
                    control,
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
                    display: displayFor(f, true),
                    choices: page.detail.field_choices?.[f],
                    t,
                    register,
                    control,
                    slots,
                    page,
                  }),
                )}
              </>
            )}
            {page.detail.update_endpoint && canUpdate && (
              <Group justify="flex-end">
                <Button type="submit" loading={saving}>
                  {t("Save")}
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
                {action.label ? t(action.label) : humanizeLabel(name)}
              </Button>
            ))}
            {page.detail.delete_endpoint && canDelete && (
              <Button color="red" variant="light" onClick={onDelete} loading={saving}>
                {t("Delete")}
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
            <Tabs.Tab value="__detail">{t("Detail")}</Tabs.Tab>
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
  display,
  choices,
  t,
  register,
  control,
  slots,
  page,
}: {
  field: string;
  mode: "editable" | "readonly";
  semType: string | undefined;
  rawValue: unknown;
  disabled: boolean;
  // Present only when this field's value is DISPLAY-ONLY. Absent means the
  // form can submit it, and the raw value is what must round-trip. See
  // DetailPage's `displayFor`.
  display?: FieldDisplay;
  // This field's enum catalogue (`detail.field_choices[field]`), when it has
  // one. Drives BOTH directions: the label instead of the wire number when
  // showing, a <Select> instead of a free-text box when editing.
  choices?: AdminChoicesSpec;
  // The tree's translator. See the note at the top of the body.
  t?: Translate;
  register: UseFormRegister<Record<string, unknown>>;
  // react-hook-form's control, needed only for the choices <Select>: Mantine's
  // Select is controlled and calls onChange with a VALUE, not an event, so it
  // cannot be driven by register()'s event-shaped handler. Optional so a
  // caller with no enum-bound field need not thread one — such a field then
  // falls through to the text input, which is what it did before.
  control?: Control<Record<string, unknown>>;
  slots: SlotRegistry | undefined;
  page: AdminPageSpec;
}) {
  // renderFieldInput is a plain function invoked during a component's render,
  // not a component itself, so it cannot call useT — the translator arrives as
  // a parameter, the way `display` does. Optional so a caller with nothing to
  // translate (and the tests) need not thread one.
  const tr = t ?? identityTranslate;
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
  const hasChoices = !!choices && !!choices.values?.length;
  // An enum's label wins over a display format when a field somehow carries
  // both — same precedence chartData applies, and for the same reason: the
  // format lowers the WIRE value, and for an enum the wire value is the one
  // thing the reader cannot use.
  const value = hasChoices
    ? displayString(translateChoiceLabel(resolveChoiceLabel(choices, rawValue), tr))
    : renderFieldValue(rawValue, display, tr);
  // `display` present means the value is display-only — either the field is
  // readonly, or the whole form is unsubmittable. Such a field renders
  // UNREGISTERED, and it has to: a registered input is owned by
  // react-hook-form, whose `reset(row)` puts the raw value back over whatever
  // `defaultValue` said, so a formatted value would appear for one frame and
  // then vanish. Not registering it is also the honest shape — a value the
  // form will never submit is not part of the form.
  if (mode === "readonly" || display) {
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
        placeholder={tr("Leave empty to keep current")}
        // No defaultValue — passwords never round-trip from the
        // read response (the stored hash is useless to pre-fill
        // with). Empty submit = "don't change" per REV-151.
        disabled={disabled}
        {...register(field)}
      />
    );
  }
  if (hasChoices && control) {
    // `choices` is already narrowed here — hasChoices is a const alias of a
    // check on it, which TS follows into this branch and into the closures
    // below. No assertion needed.
    const spec = choices;
    const repeated = spec.carrier?.startsWith("repeated") ?? false;
    // A form value arrives typed as unknown, so String() on it is how
    // "[object Object]" reaches the screen. Only a primitive can match a
    // catalogue token; anything else is not a value this Select can show.
    const token = (v: unknown): string | null =>
      typeof v === "number" || typeof v === "string" ? String(v) : null;
    const selected = new Set(
      (Array.isArray(rawValue) ? rawValue : [rawValue])
        .map(token)
        .filter((s): s is string => s != null),
    );
    // A deprecated member is hidden so nobody PICKS it — but it stays in the
    // list when it is what this row already HOLDS. Dropping it outright would
    // render the field blank and let a save silently rewrite a value the
    // operator never touched.
    const data = spec.values
      .filter((c) => !c.deprecated || selected.has(String(c.value)))
      .map((c) => ({ value: String(c.value), label: String(tr(c.label)) }));
    // The <Select> speaks strings; the wire does not. An int32-carrier enum
    // must go back as the NUMBER it came as, so the token is mapped through
    // the catalogue rather than submitted as typed — otherwise the PATCH
    // sends "1" where the API expects 1.
    const toWire = (token: string | null): unknown => {
      if (token == null) return null;
      const hit = spec.values.find((c) => String(c.value) === token);
      return hit ? hit.value : token;
    };
    return (
      <Controller
        key={field}
        name={field}
        control={control}
        defaultValue={rawValue}
        render={({ field: bound }) =>
          repeated ? (
            <MultiSelect
              label={humanizeLabel(field)}
              disabled={disabled}
              data={data}
              value={(Array.isArray(bound.value) ? bound.value : [])
                .map(token)
                .filter((s): s is string => s != null)}
              onChange={(tokens) => bound.onChange(tokens.map(toWire))}
              clearable
              searchable
            />
          ) : (
            <Select
              label={humanizeLabel(field)}
              disabled={disabled}
              data={data}
              value={token(bound.value)}
              onChange={(token) => bound.onChange(toWire(token))}
              clearable
              searchable
            />
          )
        }
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
  control,
  fieldTypes,
  displayFor,
  slots,
  page,
}: {
  fieldsets: AdminFieldsetSpec[];
  readonlySet: Set<string>;
  row: Record<string, unknown>;
  disabled: boolean;
  register: UseFormRegister<Record<string, unknown>>;
  control?: Control<Record<string, unknown>>;
  fieldTypes: Record<string, string>;
  displayFor: (field: string, readonly: boolean) => FieldDisplay | undefined;
  slots: SlotRegistry | undefined;
  page: AdminPageSpec;
}) {
  const t = useT();
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
            <Accordion.Control>{fs.title ? t(fs.title) : t("Section")}</Accordion.Control>
            <Accordion.Panel>
              <FieldsetBody
                fields={fs.fields ?? []}
                readonlySet={readonlySet}
                row={row}
                disabled={disabled}
                register={register}
                control={control}
                fieldTypes={fieldTypes}
                displayFor={displayFor}
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
          {fs.title && <Title order={5}>{t(fs.title)}</Title>}
          <FieldsetBody
            fields={fs.fields ?? []}
            readonlySet={readonlySet}
            row={row}
            disabled={disabled}
            register={register}
            control={control}
            fieldTypes={fieldTypes}
            displayFor={displayFor}
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
  control,
  fieldTypes,
  displayFor,
  slots,
  page,
}: {
  fields: string[];
  readonlySet: Set<string>;
  row: Record<string, unknown>;
  disabled: boolean;
  register: UseFormRegister<Record<string, unknown>>;
  control?: Control<Record<string, unknown>>;
  fieldTypes: Record<string, string>;
  displayFor: (field: string, readonly: boolean) => FieldDisplay | undefined;
  slots: SlotRegistry | undefined;
  page: AdminPageSpec;
}) {
  const t = useT();
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
          display: displayFor(f, isReadonly),
          choices: page.detail.field_choices?.[f],
          t,
          register,
          control,
          slots,
          page,
        });
      })}
    </Stack>
  );
}
