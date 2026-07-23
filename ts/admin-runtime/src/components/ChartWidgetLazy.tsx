// ChartWidgetLazy — the code-split door to the CHART widget.
//
// WHY THIS FILE EXISTS: ./ChartWidget imports @mantine/charts, which
// drags recharts in behind it — measured at ~450 kB raw / ~126 kB
// gzip, roughly DOUBLING the emitted admin bundle. Most generated
// admins declare no CHART widget at all, and they must not pay for a
// chart they never draw. So the ONLY reference to ./ChartWidget in the
// eagerly-reachable module graph is the dynamic import() below:
// Rollup cuts the module graph at a dynamic import, so the chart
// libraries land in their own chunk that the browser fetches the first
// time an overview actually renders a CHART tile.
//
// The corollary is a rule, not a preference: NOTHING may `import` (or
// statically re-export) ./ChartWidget from a module the entry can
// reach. One static edge silently merges the chunk back into the
// entry and the whole saving disappears with no error and no test
// failure — only a bigger dist/. The package surface (../index.ts,
// ./index.ts) therefore exports THIS component under the public name
// `ChartWidget`; only its TYPES come from the heavy module, and types
// are erased before Rollup ever sees them.

import { Component, Suspense, lazy, type ErrorInfo, type ReactNode } from "react";
import { Loader, Paper, Stack, Text } from "@mantine/core";

import type { ChartWidgetProps } from "./ChartWidget";

export type { ChartWidgetProps } from "./ChartWidget";

const ChartWidgetImpl = lazy(async () => ({
  default: (await import("./ChartWidget")).ChartWidget,
}));

// ChartWidgetLazy renders the CHART widget with the chart libraries
// fetched on demand. Props and rendered result are identical to
// ChartWidget's — the only difference is the placeholder shown for the
// tick it takes to pull the chunk.
export function ChartWidgetLazy(props: ChartWidgetProps) {
  return (
    <ChartChunkBoundary title={props.widget.title}>
      <Suspense
        fallback={<ChartCardShell title={props.widget.title} body={<Loader size="sm" />} />}
      >
        <ChartWidgetImpl {...props} />
      </Suspense>
    </ChartChunkBoundary>
  );
}

// ChartCardShell is ChartWidget's card chrome, duplicated here on
// purpose: the placeholder must be the SAME box as the loaded widget
// (same Paper, same padding, same title line, same small Loader as
// StatWidget/CountBadge) so swapping the chunk in causes no layout
// jump — the tile is already the right size and the spinner is already
// in the right spot. Keep the two in step if either changes.
function ChartCardShell({ title, body }: { title?: string; body: ReactNode }) {
  return (
    <Paper withBorder p="lg" radius="md" h="100%">
      <Stack gap="md">
        {title && (
          <Text fw={600} fz="sm">
            {title}
          </Text>
        )}
        {body}
      </Stack>
    </Paper>
  );
}

// ChartChunkBoundary catches a FAILED chunk fetch (offline, a stale
// index.html pointing at an asset a redeploy removed). Without it,
// React.lazy rethrows to the nearest boundary above — and the admin
// has none, so one missing 450 kB file would blank the entire console.
// Instead the tile degrades to the same quiet dimmed "Unavailable"
// ChartWidget shows for a failed data fetch: a dashboard tile must not
// shout over the healthy ones beside it. A class is the only way to
// declare an error boundary; React ships no hook for it.
interface ChartChunkBoundaryProps {
  title?: string;
  children: ReactNode;
}

class ChartChunkBoundary extends Component<ChartChunkBoundaryProps, { failed: boolean }> {
  state = { failed: false };

  static getDerivedStateFromError(): { failed: boolean } {
    return { failed: true };
  }

  // The UI stays quiet, but the console must not: a chunk that will
  // not load is a deploy problem an operator can only diagnose if it
  // is written down somewhere.
  componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error("[admin] chart chunk failed to load", error, info.componentStack);
  }

  render(): ReactNode {
    if (this.state.failed) {
      return (
        <ChartCardShell
          title={this.props.title}
          body={
            <Text c="dimmed" fz="sm">
              Unavailable
            </Text>
          }
        />
      );
    }
    return this.props.children;
  }
}
