// Brand — the product mark shown in the app header and login.
// A gradient tile + the admin's name. Kept tiny so it reads on
// the 56px header without crowding the nav toggle.

import { Group, Text, ThemeIcon } from "@mantine/core";

import { IconDashboard } from "../icons";

export interface BrandProps {
  name: string;
  size?: number;
}

export function Brand({ name, size = 30 }: BrandProps) {
  return (
    <Group gap="sm" wrap="nowrap" style={{ minWidth: 0 }}>
      <ThemeIcon
        size={size}
        radius="md"
        variant="gradient"
        gradient={{ from: "indigo", to: "violet", deg: 135 }}
      >
        <IconDashboard size={Math.round(size * 0.58)} />
      </ThemeIcon>
      <Text fw={700} fz="lg" lh={1.1} truncate style={{ letterSpacing: "-0.01em" }}>
        {name}
      </Text>
    </Group>
  );
}
