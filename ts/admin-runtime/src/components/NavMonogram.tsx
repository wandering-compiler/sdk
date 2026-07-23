// NavMonogram — a small tinted initial badge for a nav item.
//
// Django-style admin nav gives every model the SAME leading glyph,
// which reads as "cheap" once there are more than a couple of pages.
// A per-page monogram (the page's first letter on a stable per-page
// tint) makes each entry individually recognisable at a glance while
// staying inside the design system: the tint is one of Mantine's
// registered colours at its theme-aware `light` variant, so it adapts
// to light / dark automatically and never introduces an off-palette hue.
//
// The letter + colour are pure functions of the label, so the same
// page always gets the same badge across reloads and across the nav /
// any future breadcrumb reuse — recognition depends on stability.

import { Box } from "@mantine/core";

// A curated slice of Mantine's default palette. Every name here is a
// registered colour, so `--mantine-color-<name>-light` and
// `-light-color` CSS vars exist and are theme-aware. Ordered so that
// adjacent hash buckets land on visibly different hues.
const MONOGRAM_PALETTE = [
  "indigo",
  "teal",
  "grape",
  "orange",
  "cyan",
  "pink",
  "green",
  "violet",
  "blue",
  "lime",
] as const;

/**
 * monogramLetter picks the glyph shown in the badge: the first
 * alphanumeric character of the label, upper-cased. Falls back to a
 * bullet when the label has no alphanumeric character (e.g. a purely
 * symbolic title), so the badge is never blank.
 */
export function monogramLetter(label: string): string {
  for (const ch of label.trim()) {
    if (/[a-z0-9]/i.test(ch)) return ch.toUpperCase();
  }
  return "•";
}

/**
 * monogramColor maps a label to a stable palette colour via a small
 * string hash. Pure + deterministic: a page keeps its colour across
 * renders and reloads. Exported for unit tests (stability + range).
 */
export function monogramColor(label: string): string {
  let h = 0;
  for (let i = 0; i < label.length; i++) {
    h = (h * 31 + label.charCodeAt(i)) | 0;
  }
  return MONOGRAM_PALETTE[Math.abs(h) % MONOGRAM_PALETTE.length];
}

export interface NavMonogramProps {
  label: string;
  size?: number;
}

export function NavMonogram({ label, size = 22 }: NavMonogramProps) {
  const color = monogramColor(label);
  return (
    <Box
      w={size}
      h={size}
      aria-hidden
      style={{
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        flexShrink: 0,
        borderRadius: "var(--mantine-radius-sm)",
        background: `var(--mantine-color-${color}-light)`,
        color: `var(--mantine-color-${color}-light-color)`,
        fontSize: Math.round(size * 0.5),
        fontWeight: 700,
        lineHeight: 1,
      }}
    >
      {monogramLetter(label)}
    </Box>
  );
}
