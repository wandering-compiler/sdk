// StatCard — a metric tile for the overview grid: an uppercase
// label, a large value, and an optional hint line. The default
// (unconfigured) overview widget renders as one of these with an
// em-dash value, so an admin that hasn't wired real widgets yet
// still shows a clean dashboard instead of developer jargon.

import type { ReactNode } from "react";
import { Paper, Stack, Text } from "@mantine/core";

export interface StatCardProps {
  label: string;
  value: ReactNode;
  hint?: string;
}

export function StatCard({ label, value, hint }: StatCardProps) {
  return (
    <Paper withBorder p="lg" radius="md" h="100%">
      <Stack gap={6}>
        <Text fz="xs" fw={600} tt="uppercase" c="dimmed" style={{ letterSpacing: "0.04em" }}>
          {label}
        </Text>
        <Text fz={30} fw={700} lh={1.1} style={{ letterSpacing: "-0.02em" }}>
          {value}
        </Text>
        {hint && (
          <Text fz="xs" c="dimmed">
            {hint}
          </Text>
        )}
      </Stack>
    </Paper>
  );
}
