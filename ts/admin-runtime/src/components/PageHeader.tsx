// PageHeader — the consistent title block every page renders at
// the top of the content area: an optional back link, the page
// title, an optional dimmed subtitle, and a right-aligned action
// slot. Centralising it keeps title sizing / spacing identical
// across Overview / List / Detail.

import type { ReactNode } from "react";
import { Button, Group, Stack, Text, Title } from "@mantine/core";

import { IconArrowLeft } from "../icons";

export interface PageHeaderProps {
  title: string;
  subtitle?: string;
  actions?: ReactNode;
  onBack?: () => void;
}

export function PageHeader({ title, subtitle, actions, onBack }: PageHeaderProps) {
  return (
    <Group justify="space-between" align="flex-end" wrap="nowrap" gap="md">
      <Stack gap={4} style={{ minWidth: 0 }}>
        {onBack && (
          <Button
            variant="subtle"
            color="gray"
            size="compact-sm"
            leftSection={<IconArrowLeft size={15} />}
            onClick={onBack}
            style={{ alignSelf: "flex-start", marginLeft: -8 }}
          >
            Back
          </Button>
        )}
        <Title order={2} lineClamp={1} style={{ letterSpacing: "-0.02em" }}>
          {title}
        </Title>
        {subtitle && (
          <Text c="dimmed" fz="sm" lineClamp={1}>
            {subtitle}
          </Text>
        )}
      </Stack>
      {actions && (
        <Group gap="xs" wrap="wrap" justify="flex-end">
          {actions}
        </Group>
      )}
    </Group>
  );
}
