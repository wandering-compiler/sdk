// StateView — the shared loading / empty / error placeholder.
// Replaces the ad-hoc "<Loader/> Loading…" and bare red text
// each page grew its own copy of, so every non-content state
// reads the same.

import { Center, Loader, Paper, Stack, Text, ThemeIcon } from "@mantine/core";

import { IconAlertTriangle, IconInbox } from "../icons";
import { useT } from "../i18n";

export interface StateViewProps {
  kind: "loading" | "empty" | "error";
  title?: string;
  message?: string;
}

export function StateView({ kind, title, message }: StateViewProps) {
  const t = useT();
  if (kind === "loading") {
    return (
      <Center mih={220}>
        <Stack align="center" gap="sm">
          <Loader />
          <Text c="dimmed" fz="sm">
            {message ?? t("Loading…")}
          </Text>
        </Stack>
      </Center>
    );
  }

  const isError = kind === "error";
  return (
    <Paper withBorder p="xl" radius="md">
      <Stack align="center" gap="xs" py="md">
        <ThemeIcon size={46} radius="xl" variant="light" color={isError ? "red" : "gray"}>
          {isError ? <IconAlertTriangle size={24} /> : <IconInbox size={24} />}
        </ThemeIcon>
        <Text fw={600}>
          {title ?? (isError ? t("Something went wrong") : t("Nothing here yet"))}
        </Text>
        {message && (
          <Text c="dimmed" fz="sm" ta="center" maw={440} style={{ overflowWrap: "anywhere" }}>
            {message}
          </Text>
        )}
      </Stack>
    </Paper>
  );
}
