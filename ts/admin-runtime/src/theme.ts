// Admin design tokens. A single Mantine theme drives every
// screen so the runtime reads as one system (not a pile of
// default-styled components). Consumed by bootstrap()'s
// MantineProvider; exported so slot authors can reuse the same
// tokens in custom widgets.
//
// Deliberately dependency-free: system font stacks (no web-font
// fetch → works offline / behind strict CSP), Mantine's built-in
// `indigo` accent, generous radii, and per-component defaults so
// buttons/inputs/cards look consistent without prop repetition.

import { createTheme } from "@mantine/core";

const SANS = [
  "-apple-system",
  "BlinkMacSystemFont",
  "Segoe UI",
  "Roboto",
  "Helvetica",
  "Arial",
  "sans-serif",
  "Apple Color Emoji",
  "Segoe UI Emoji",
].join(", ");

const MONO = [
  "ui-monospace",
  "SFMono-Regular",
  "Menlo",
  "Monaco",
  "Consolas",
  "Liberation Mono",
  "monospace",
].join(", ");

export const theme = createTheme({
  primaryColor: "indigo",
  // Slightly brighter accent in dark mode so it doesn't muddy.
  primaryShade: { light: 6, dark: 5 },
  autoContrast: true,
  defaultRadius: "md",
  fontFamily: SANS,
  fontFamilyMonospace: MONO,
  cursorType: "pointer",
  headings: {
    fontFamily: SANS,
    fontWeight: "650",
  },
  components: {
    Button: { defaultProps: { radius: "md" } },
    Paper: { defaultProps: { radius: "md" } },
    Card: { defaultProps: { radius: "md" } },
    TextInput: { defaultProps: { radius: "md" } },
    PasswordInput: { defaultProps: { radius: "md" } },
    NumberInput: { defaultProps: { radius: "md" } },
    Select: { defaultProps: { radius: "md" } },
    Modal: {
      defaultProps: {
        radius: "md",
        centered: true,
        overlayProps: { backgroundOpacity: 0.55, blur: 3 },
      },
    },
  },
});
