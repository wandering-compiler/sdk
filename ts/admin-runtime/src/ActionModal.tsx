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

  const handleSubmit = async () => {
    setError(null);
    setSubmitting(true);
    try {
      const body: Record<string, unknown> = { ids: selectedIds };
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

  const label = action.label || humanizeLabel(actionName);
  // No selection = nothing to act on. Say so and gate the modal, rather
  // than posting an empty ids[] the backend now (correctly) rejects.
  const hasSelection = selectedIds.length > 0;
  const targetText = hasSelection
    ? `${selectedIds.length} selected row${selectedIds.length === 1 ? "" : "s"}`
    : "no rows";

  return (
    <Modal opened={open} onClose={handleClose} title={label} centered>
      <Stack>
        <Text size="sm" c="dimmed">
          Will apply to {targetText}.
        </Text>

        {!hasSelection && (
          <>
            <Text size="sm" c="red">
              Select at least one row to run this action.
            </Text>
            <Group justify="flex-end">
              <Button variant="subtle" onClick={handleClose}>
                Close
              </Button>
            </Group>
          </>
        )}

        {hasSelection && action.confirm && !confirmed && (
          <>
            <Text>{action.confirm}</Text>
            <Group justify="flex-end">
              <Button variant="subtle" onClick={handleClose}>
                Cancel
              </Button>
              <Button color="orange" onClick={() => setConfirmed(true)}>
                Continue
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
                onChange={(e) => setExtras((prev) => ({ ...prev, [f]: e.currentTarget.value }))}
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
                Cancel
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
