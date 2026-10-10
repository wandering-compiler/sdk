// ActionModal — bulk-action UI for an admin list page. Closes
// the admin actions loop on the runtime side: every LIST-target
// action declared in admin_spec.json renders as a button above
// the list; click opens a Mantine modal collecting the action's
// declared extras + posts to the action endpoint.
//
// Walking-skeleton iter-2 scope:
//   - LIST-target actions only (DETAIL-target parked next lift)
//   - Explicit selection only: the action posts the picked rows'
//     ids. There is no "all filtered rows" mode — nothing
//     implements it (an empty ids[] reaches the storage method as
//     `id = ANY('{}')`, matching no row), so the modal refuses to
//     submit an empty selection and the button stays disabled.
//   - Every extra field renders as a plain TextInput; iter-3
//     introspects the proto descriptor for typed inputs.
//   - An action that declares a `result` shows the named response
//     fields after it succeeds, in this same modal, until the operator
//     closes it. A SECRET result is shown once: the values live only in
//     this component's state — never browser storage, never a log — and
//     closing the modal discards them (ActionResultView below).

import { useState } from "react";
import { Button, Code, CopyButton, Group, Modal, Stack, Text, TextInput } from "@mantine/core";

import { apiPost, displayString } from "./api";
import { humanizeLabel } from "./format";
import { useT } from "./i18n";
import type { AdminActionSpec } from "./types";

export interface ActionModalProps {
  action: AdminActionSpec;
  actionName: string;
  open: boolean;
  onClose: () => void;
  onSuccess: () => void;
  // Selected row IDs. Empty array = "apply to all filtered rows".
  // Walking-skeleton iter-2 always sends [] (selection UI parked).
  selectedIds: string[];
}

export function ActionModal({
  action,
  actionName,
  open,
  onClose,
  onSuccess,
  selectedIds,
}: ActionModalProps) {
  const [extras, setExtras] = useState<Record<string, string>>({});
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [confirmed, setConfirmed] = useState(!action.confirm);
  // The declared response fields of a SUCCEEDED action, as display text, in
  // declared order. Non-null = the action ran and the modal now shows its
  // result instead of its form. Held here and nowhere else, on purpose: for a
  // secret result this state IS the only copy the browser has.
  const [result, setResult] = useState<Array<[string, string]> | null>(null);

  // The spec contract declares `fields` unconditionally present, and the
  // generator now emits `[]` for an action with no extras. Older bundles
  // (and hand-written specs) can still carry JSON null there, which is not
  // iterable — a TypeError the moment the modal opens. Read through one
  // guarded binding rather than trusting the wire.
  const fields = action.fields ?? [];

  const reset = () => {
    setExtras({});
    setSubmitting(false);
    setError(null);
    setConfirmed(!action.confirm);
    setResult(null);
  };

  const handleClose = () => {
    // Closing a shown result is when the page refreshes: the action already
    // succeeded, and refreshing earlier could replace the page (a failed
    // reload renders an error view) and unmount this modal — taking a
    // one-time value with it before the operator copied it.
    const ranWithResult = result !== null;
    reset();
    if (ranWithResult) onSuccess();
    onClose();
  };

  // Declared before handleSubmit reads it: a PAGE action is a different
  // request shape, not a variation on an empty selection.
  const isPageAction = action.target === "PAGE";

  const handleSubmit = async () => {
    setError(null);
    setSubmitting(true);
    try {
      // A PAGE action operates on no rows, so it sends no `ids`. Sending an
      // empty array instead would be worse than sending nothing: the field
      // would exist, and a request shaped like a selection that is empty is
      // the exact thing the backend refuses.
      const body: Record<string, unknown> = isPageAction ? {} : { ids: selectedIds };
      for (const f of fields) {
        if (f in extras) body[f] = extras[f];
      }
      const resp = await apiPost<Record<string, unknown> | null>(action.endpoint, body);
      if (action.result) {
        setSubmitting(false);
        setResult(resultValues(action.result.fields, resp));
        return;
      }
      reset();
      onSuccess();
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setSubmitting(false);
    }
  };

  const t = useT();
  const label = action.label ? t(action.label) : humanizeLabel(actionName);
  // No selection = nothing to act on. Say so and gate the modal, rather
  // than posting an empty ids[] the backend now (correctly) rejects.
  //
  // …unless this is a PAGE action, which never had a selection to be missing.
  // Gating it on one would make the button permanently dead.
  const hasSelection = isPageAction || selectedIds.length > 0;
  // These three feed `t("Will apply to {target}.")`, so leaving them raw produced a
  // HALF-translated sentence — worse than an untranslated one, because it reads as
  // a bug rather than as a missing catalogue. Neither the JSX-text check nor the
  // prop check could see them: they are plain literals in a ternary.
  //
  // Two msgids rather than one with a suffix, because this translator has no
  // plural form — `t()` substitutes, it does not count.
  const targetText = isPageAction
    ? t("this page")
    : selectedIds.length === 1
      ? t("{count} selected row", { count: selectedIds.length })
      : selectedIds.length > 0
        ? t("{count} selected rows", { count: selectedIds.length })
        : t("no rows");

  if (result !== null) {
    return (
      <Modal opened={open} onClose={handleClose} title={label} centered>
        <ActionResultView values={result} secret={!!action.result?.secret} onClose={handleClose} />
      </Modal>
    );
  }

  return (
    <Modal opened={open} onClose={handleClose} title={label} centered>
      <Stack>
        <Text size="sm" c="dimmed">
          {t("Will apply to {target}.", { target: targetText })}
        </Text>

        {!hasSelection && (
          <>
            <Text size="sm" c="red">
              {t("Select at least one row to run this action.")}
            </Text>
            <Group justify="flex-end">
              <Button variant="subtle" onClick={handleClose}>
                {t("Close")}
              </Button>
            </Group>
          </>
        )}

        {hasSelection && action.confirm && !confirmed && (
          <>
            <Text>{t(action.confirm)}</Text>
            <Group justify="flex-end">
              <Button variant="subtle" onClick={handleClose}>
                {t("Cancel")}
              </Button>
              <Button color="orange" onClick={() => setConfirmed(true)}>
                {t("Continue")}
              </Button>
            </Group>
          </>
        )}

        {hasSelection && confirmed && (
          <>
            {fields.map((f) => (
              <TextInput
                key={f}
                label={humanizeLabel(f)}
                value={extras[f] || ""}
                // Read the value BEFORE the updater. React nulls a synthetic event's
                // `currentTarget` once the handler returns, and a functional updater
                // runs later — so `e.currentTarget.value` INSIDE it threw
                // "Cannot read properties of null (reading 'value')" on the first
                // keystroke. Every extra field of every action was unusable, and
                // nothing noticed because nothing had ever typed into one: this file
                // had one of its five functions covered.
                onChange={(e) => {
                  const value = e.currentTarget.value;
                  setExtras((prev) => ({ ...prev, [f]: value }));
                }}
                disabled={submitting}
              />
            ))}
            {error && (
              <Text c="red" size="sm">
                {error}
              </Text>
            )}
            <Group justify="flex-end">
              <Button variant="subtle" onClick={handleClose} disabled={submitting}>
                {t("Cancel")}
              </Button>
              <Button onClick={handleSubmit} loading={submitting}>
                {label}
              </Button>
            </Group>
          </>
        )}
      </Stack>
    </Modal>
  );
}

// resultValues reads the declared fields out of an action's JSON response, by
// proto name (the admin wire's keys), in declared order. A field the response
// does not carry — proto3 omits a zero value — reads as "".
function resultValues(
  fields: string[],
  resp: Record<string, unknown> | null,
): Array<[string, string]> {
  return fields.map((f) => [f, displayString(resp?.[f])]);
}

interface ActionResultViewProps {
  values: Array<[string, string]>;
  secret: boolean;
  onClose: () => void;
}

// ActionResultView shows a succeeded action's declared response fields, each
// with a copy button.
//
// A SECRET result says it is shown once, and that is a promise this component
// keeps by having nowhere else to put the values: they arrive as props from
// ActionModal's state, are rendered, and go when the modal closes. Nothing
// here writes storage, logs, or hands them to anything that might (the
// actionmodal tests spy on all three).
function ActionResultView({ values, secret, onClose }: ActionResultViewProps) {
  const t = useT();
  return (
    <Stack>
      {secret && (
        <Text size="sm" fw={500} c="orange">
          {t(
            "Shown only once. Copy it now: nothing keeps it, and closing this dialog discards it.",
          )}
        </Text>
      )}
      {values.map(([field, value]) => (
        <Stack key={field} gap={4}>
          <Text size="sm" fw={500}>
            {humanizeLabel(field)}
          </Text>
          <Group gap="xs" wrap="nowrap" align="flex-start">
            <Code
              block
              style={{ flex: 1, wordBreak: "break-all" }}
              data-testid={`action-result-${field}`}
            >
              {value}
            </Code>
            <CopyButton value={value}>
              {({ copied, copy }) => (
                <Button
                  size="xs"
                  variant="light"
                  onClick={copy}
                  aria-label={t("Copy {field}", { field: humanizeLabel(field) })}
                >
                  {copied ? t("Copied") : t("Copy")}
                </Button>
              )}
            </CopyButton>
          </Group>
        </Stack>
      ))}
      <Group justify="flex-end">
        <Button onClick={onClose}>{t("Close")}</Button>
      </Group>
    </Stack>
  );
}
