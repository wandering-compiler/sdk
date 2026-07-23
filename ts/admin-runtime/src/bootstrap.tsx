// bootstrap — the SPA entry the generated `spa/src/main.tsx`
// calls with the embedded admin_spec.json + the document root.
// Mounts the Mantine provider tree + the App shell.

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { MantineProvider } from "@mantine/core";

import "@mantine/core/styles.css";
import "@mantine/charts/styles.css";
import "mantine-datatable/styles.css";

import { App } from "./App";
import { theme } from "./theme";
import { SPEC_SCHEMA_VERSION } from "./types";
import type { BootstrapOptions } from "./types";

export function bootstrap(opts: BootstrapOptions): void {
  if (opts.spec.schema_version !== SPEC_SCHEMA_VERSION) {
    // Mount a minimal error UI so the operator sees the
    // mismatch immediately instead of staring at a blank
    // page. Boots without Mantine provider — keeps this path
    // independent of the runtime's deps in case the mismatch
    // breaks them too.
    // Build the node with textContent (not innerHTML) so the interpolated
    // spec/runtime versions can never inject markup — this is an error path,
    // but the spec is external input (js/html-constructed-from-input).
    const pre = document.createElement("pre");
    pre.style.cssText = "padding:2rem;font-family:monospace;color:#c00";
    pre.textContent =
      `admin runtime version mismatch\n` +
      `  spec.schema_version = ${opts.spec.schema_version}\n` +
      `  runtime SPEC_SCHEMA_VERSION = ${SPEC_SCHEMA_VERSION}\n\n` +
      `rebuild the admin bundle against runtime ${SPEC_SCHEMA_VERSION}`;
    opts.root.replaceChildren(pre);
    return;
  }
  const root = createRoot(opts.root);
  root.render(
    <StrictMode>
      {/* defaultColorScheme="auto" follows the OS light/dark
          preference on first load; the header toggle can override
          it and Mantine persists that choice to localStorage. */}
      <MantineProvider theme={theme} defaultColorScheme="auto">
        <App spec={opts.spec} slots={opts.slots || {}} />
      </MantineProvider>
    </StrictMode>,
  );
}
