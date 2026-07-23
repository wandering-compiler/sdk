// ThemeToggle — cycles the Mantine color scheme through
// auto → light → dark. `auto` follows the OS `prefers-color-
// scheme`; the explicit modes override it. Mantine persists the
// choice to localStorage via its default color-scheme manager,
// so a reload keeps the user's pick.

import { ActionIcon, Tooltip, useMantineColorScheme } from "@mantine/core";

import { IconDeviceDesktop, IconMoon, IconSunHigh } from "../icons";

type Scheme = "auto" | "light" | "dark";

const ORDER: Scheme[] = ["auto", "light", "dark"];

const LABELS: Record<Scheme, string> = {
  auto: "System theme",
  light: "Light theme",
  dark: "Dark theme",
};

export function ThemeToggle() {
  const { colorScheme, setColorScheme } = useMantineColorScheme();
  const current: Scheme = colorScheme;
  const next = ORDER[(ORDER.indexOf(current) + 1) % ORDER.length];
  const label = LABELS[current];
  const Icon =
    current === "light" ? IconSunHigh : current === "dark" ? IconMoon : IconDeviceDesktop;

  return (
    <Tooltip label={`${label} — click to switch`} withArrow>
      <ActionIcon
        variant="default"
        size="lg"
        radius="md"
        aria-label={`${label}. Click to switch color theme.`}
        onClick={() => setColorScheme(next)}
      >
        <Icon size={18} />
      </ActionIcon>
    </Tooltip>
  );
}
