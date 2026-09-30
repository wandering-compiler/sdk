// ActionModal — bulk-action UI for an admin list page. Closes
// the P8 actions loop on the runtime side: every LIST-target
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

import { useState } from "react";
import { Button, Group, Modal, Stack, Text, TextInput } from "@mantine/core";

import { apiPost } from "./api";
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
  };

  const handleClose = () => {
    reset();
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
      await apiPost(action.endpoint, body);
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
