// OverviewPage — landing dashboard.
//
// Two modes, decided by whether the admin declared any overview
// widgets:
//   - widgets declared → render ONLY those. A DECLARATIVE widget
//     (STAT, CHART) is rendered by the runtime straight from the
//     spec — it names a data route plus the fields to read, and needs
//     no project JS at all. A CUSTOM widget is dispatched by slot id
//     to a developer-provided renderer (the JS escape hatch).
//   - none declared → render the DEFAULT group tiles: a card per nav
//     group listing its pages as quick-links, with a row count on any
//     page whose list is paged (the count is free — a paged list
//     already computes COUNT(*) for its pager total). This replaces the
//     old "empty overview" first screen with something oriented.
//
// A CUSTOM widget whose slot has no registered renderer is dropped
// (warned once) rather than rendered as a broken placeholder.

import { useEffect, useState } from "react";
import { Anchor, Badge, Grid, Group, Loader, Paper, SimpleGrid, Stack, Text } from "@mantine/core";

import { PageHeader } from "./components";
// Imported from its own module (not the ./components barrel) so the
// code-split is visible where it is used: ChartWidgetLazy fetches
// @mantine/charts + recharts on demand, keeping ~450 kB of chart
// library out of the bundle of every admin that declares no CHART.
import { ChartWidgetLazy } from "./components/ChartWidgetLazy";
import { apiGet } from "./api";
import { humanizeLabel } from "./format";
import { overviewWidgetSlotKey } from "./types";
import type {
  AdminSpec,
  AdminOverviewSpec,
  AdminWidgetSpec,
  SlotRegistry,
  WhoAmIResp,
} from "./types";
import {
  defaultTileGroups,
  hasDeclaredWidgets,
  type OverviewCountRequest,
  type OverviewTileGroup,
  type OverviewTilePage,
} from "./overviewTiles";

export interface OverviewPageProps {
  spec: AdminSpec;
  overview?: AdminOverviewSpec;
  slots: SlotRegistry;
  whoami: WhoAmIResp | null;
  onSelectPage?: (pageName: string) => void;
}

export function OverviewPage({ spec, overview, slots, whoami, onSelectPage }: OverviewPageProps) {
  return (
    <Stack gap="lg">
      <PageHeader title="Overview" subtitle={spec.name} />
      {hasDeclaredWidgets(spec) ? (
        <WidgetGrid overview={overview} slots={slots} />
      ) : (
        <TileGrid spec={spec} whoami={whoami} onSelectPage={onSelectPage} />
      )}
    </Stack>
  );
}

// WidgetGrid renders the declared overview widgets — STAT and CHART
// widgets from the spec, CUSTOM widgets via their registered slot. Only
// reached when hasDeclaredWidgets(spec) is true, so `overview` is
// present + non-empty.
function WidgetGrid({ overview, slots }: { overview?: AdminOverviewSpec; slots: SlotRegistry }) {
  const widgets = renderableWidgets(overview?.widgets ?? [], slots);
  if (widgets.length === 0) return null;
  return (
    <Grid gap="lg">
      {widgets.map((w) => (
        <Grid.Col
          key={w.slot}
          span={{ base: 12, xs: w.size === "LARGE" ? 12 : 6, md: widgetSpan(w) }}
        >
          <WidgetCard widget={w} slots={slots} />
        </Grid.Col>
      ))}
    </Grid>
  );
}

// TileGrid renders the default group tiles. Empty (no visible pages) →
// nothing (a bare header reads better than an empty grid).
function TileGrid({
  spec,
  whoami,
  onSelectPage,
}: {
  spec: AdminSpec;
  whoami: WhoAmIResp | null;
  onSelectPage?: (pageName: string) => void;
}) {
  const groups = defaultTileGroups(spec, whoami?.permission_ids);
  if (groups.length === 0) return null;
  return (
    <Grid gap="lg">
      {groups.map((g) => (
        <Grid.Col key={g.title} span={{ base: 12, xs: 6, md: 4 }}>
          <GroupTile group={g} onSelectPage={onSelectPage} />
        </Grid.Col>
      ))}
    </Grid>
  );
}

// GroupTile is one nav group's card: an uppercase title over its
// pages, each a link (to its list) with an optional row count.
function GroupTile({
  group,
  onSelectPage,
}: {
  group: OverviewTileGroup;
  onSelectPage?: (pageName: string) => void;
}) {
  return (
    <Paper withBorder p="lg" radius="md" h="100%">
      <Stack gap="sm">
        <Text fz="xs" fw={700} tt="uppercase" c="dimmed" style={{ letterSpacing: "0.04em" }}>
          {group.title}
        </Text>
        <Stack gap={4}>
          {group.pages.map((p) => (
            <PageTileRow key={p.name} page={p} onSelectPage={onSelectPage} />
          ))}
        </Stack>
      </Stack>
    </Paper>
  );
}

// PageTileRow is one page inside a group tile: its label (a link to the
// page's list when it has one) plus a right-aligned count badge when
// the page is paged.
function PageTileRow({
  page,
  onSelectPage,
}: {
  page: OverviewTilePage;
  onSelectPage?: (pageName: string) => void;
}) {
  const label =
    page.listEndpoint && onSelectPage ? (
      <Anchor
        component="button"
        type="button"
        fz="sm"
        onClick={() => onSelectPage(page.name)}
        style={{ textAlign: "left" }}
      >
        {page.label}
      </Anchor>
    ) : (
      <Text fz="sm">{page.label}</Text>
    );
  return (
    <Group justify="space-between" wrap="nowrap" gap="sm">
      {label}
      {page.count && <CountBadge req={page.count} />}
    </Group>
  );
}

// CountBadge fetches one page's row count off its paged list endpoint
// (GET <endpoint>?<limit>=1 → read <paging_field>.total) and shows it.
// While loading it shows a small spinner; on error it renders nothing
// (a count is orientational, not worth surfacing a failure for).
type CountState = { status: "loading" } | { status: "ok"; value: string } | { status: "error" };

function CountBadge({ req }: { req: OverviewCountRequest }) {
  const [state, setState] = useState<CountState>({ status: "loading" });
  useEffect(() => {
    let cancelled = false;
    const sep = req.endpoint.includes("?") ? "&" : "?";
    const url = `${req.endpoint}${sep}${encodeURIComponent(req.limitParam)}=1`;
    apiGet<Record<string, unknown>>(url)
      .then((resp) => {
        if (cancelled) return;
        setState({ status: "ok", value: formatCount(readTotal(resp, req.pagingField)) });
      })
      .catch(() => {
        if (!cancelled) setState({ status: "error" });
      });
    return () => {
      cancelled = true;
    };
  }, [req.endpoint, req.limitParam, req.pagingField]);
  if (state.status === "loading") return <Loader size="xs" />;
  if (state.status === "error") return null;
  return (
    <Badge variant="light" radius="sm">
      {state.value}
    </Badge>
  );
}

// readTotal pulls `<pagingField>.total` out of a list response. The
// backend marshals with EmitDefaultValues:false, so an absent paging
// object or an omitted zero total both mean 0.
function readTotal(resp: Record<string, unknown>, pagingField: string): unknown {
  const paging = resp?.[pagingField];
  if (paging && typeof paging === "object") {
    return (paging as Record<string, unknown>).total;
  }
  return undefined;
}

// formatCount renders a count total (protojson renders a uint64 as a
// STRING, so accept both string + number) with thousands separators.
// Absent/unparseable → "0" / the raw value.
export function formatCount(total: unknown): string {
  if (typeof total === "number") return Number.isFinite(total) ? total.toLocaleString() : "0";
  if (typeof total === "string") {
    const n = Number(total);
    return Number.isFinite(n) ? n.toLocaleString() : total;
  }
  return "0";
}

// renderableWidgets keeps every declarative widget (STAT, CHART) —
// those need no registered renderer, the runtime renders them from the
// spec — and every CUSTOM widget whose slot HAS a registered renderer,
// dropping (and warning once about) CUSTOM widgets whose slot is
// unwired. Exported (and free of JSX) so the drop rule is unit-testable
// without a React render harness.
export function renderableWidgets(
  widgets: AdminWidgetSpec[],
  slots: SlotRegistry,
): AdminWidgetSpec[] {
  return (widgets || []).filter((w) => {
    // Declarative — always renderable, no slot to look up.
    if (w.kind === "STAT" || w.kind === "CHART") return true;
    const key = overviewWidgetSlotKey(w.slot);
    if (slots?.[key]) return true;
    warnUnconfiguredWidget(key);
    return false;
  });
}

// widgetSpan maps AdminWidgetSpec.size to a Mantine Grid.Col span
// (1-12) at the md+ breakpoint — SMALL three-per-row, MEDIUM
// two-per-row, LARGE full-width. Narrower breakpoints stack via the
// responsive span object above.
function widgetSpan(w: AdminWidgetSpec): number {
  switch (w.size) {
    case "LARGE":
      return 12;
    case "MEDIUM":
      return 6;
    case "SMALL":
    default:
      return 4;
  }
}

// WidgetCard dispatches one widget by kind: a declarative STAT or
// CHART widget is rendered by the runtime from the spec; a CUSTOM
// widget goes to its registered slot component (which owns its own
// chrome). Only reached for widgets renderableWidgets already confirmed
// are renderable.
//
// CHART goes through the LAZY wrapper — this dispatch is the only
// place the runtime itself draws a chart, so it is also the only place
// that decides whether the chart libraries enter the entry bundle.
// Rendering ChartWidget directly here would put them back in it for
// every admin.
function WidgetCard({ widget, slots }: { widget: AdminWidgetSpec; slots: SlotRegistry }) {
  if (widget.kind === "STAT") return <StatWidget widget={widget} />;
  if (widget.kind === "CHART") return <ChartWidgetLazy widget={widget} />;
  const Renderer = slots[overviewWidgetSlotKey(widget.slot)];
  if (!Renderer) return null;
  return <Renderer widget={widget} />;
}

type StatState =
  { status: "loading" } | { status: "ok"; data: Record<string, unknown> } | { status: "error" };

// StatWidget renders a declarative aggregated-values panel: it fetches
// the widget's data route once, then shows one cell per declared value
// (the value read off the response, its label humanized from the field
// name when not overridden). Loading → a spinner; error → a quiet
// "Unavailable" (a dashboard tile shouldn't shout about a failed fetch).
function StatWidget({ widget }: { widget: AdminWidgetSpec }) {
  const [state, setState] = useState<StatState>({ status: "loading" });
  const endpoint = widget.endpoint;
  useEffect(() => {
    if (!endpoint) {
      setState({ status: "error" });
      return;
    }
    let cancelled = false;
    apiGet<Record<string, unknown>>(endpoint)
      .then((data) => {
        if (!cancelled) setState({ status: "ok", data });
      })
      .catch(() => {
        if (!cancelled) setState({ status: "error" });
      });
    return () => {
      cancelled = true;
    };
  }, [endpoint]);
  const values = widget.values ?? [];
  return (
    <Paper withBorder p="lg" radius="md" h="100%">
      <Stack gap="md">
        {widget.title && (
          <Text fw={600} fz="sm">
            {widget.title}
          </Text>
        )}
        {state.status === "loading" ? (
          <Loader size="sm" />
        ) : state.status === "error" ? (
          <Text c="dimmed" fz="sm">
            Unavailable
          </Text>
        ) : (
          <SimpleGrid cols={{ base: 2, sm: Math.min(Math.max(values.length, 1), 4) }} spacing="lg">
            {values.map((v) => (
              <Stack key={v.field} gap={2}>
                <Text fz={28} fw={700} lh={1.1} style={{ letterSpacing: "-0.02em" }}>
                  {formatCount(state.data[v.field])}
                </Text>
                <Text
                  fz="xs"
                  fw={600}
                  tt="uppercase"
                  c="dimmed"
                  style={{ letterSpacing: "0.04em" }}
                >
                  {v.label && v.label.trim() ? v.label : humanizeLabel(v.field)}
                </Text>
              </Stack>
            ))}
          </SimpleGrid>
        )}
      </Stack>
    </Paper>
  );
}

// warnUnconfiguredWidget logs the developer-facing setup hint once per
// slot key. Kept out of the rendered UI so end users never see
// framework wiring instructions.
const warnedSlots = new Set<string>();
function warnUnconfiguredWidget(key: string): void {
  if (warnedSlots.has(key)) return;
  warnedSlots.add(key);
  console.warn(
    `[admin] overview widget "${key}" has no renderer. ` +
      `Register one via bootstrap({ slots: { ["${key}"]: MyWidget } }) to show real content.`,
  );
}
