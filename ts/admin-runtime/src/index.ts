// Public surface of @w17/admin-runtime. The generated SPA
// scaffold (spa/src/main.tsx) imports `bootstrap` from this
// module and calls it with the embedded spec.

export { bootstrap } from "./bootstrap";
export type {
  AdminSpec,
  AdminPageSpec,
  AdminListSpec,
  AdminListPagingSpec,
  AdminDetailSpec,
  AdminFieldsetSpec,
  AdminActionSpec,
  AdminInlineSpec,
  AdminAuthSpec,
  AdminNavGroup,
  AdminOverviewSpec,
  AdminWidgetSpec,
  AdminWidgetValueSpec,
  SlotKey,
  SlotRegistry,
  SlotComponent,
  WidgetRenderer,
  BootstrapOptions,
  WhoAmIResp,
} from "./types";
export {
  SPEC_SCHEMA_VERSION,
  overviewWidgetSlotKey,
  listColumnSlotKey,
  listRowActionSlotKey,
  detailFieldSlotKey,
  detailTabSlotKey,
  globalNavItemSlotKey,
} from "./types";

// Design system — the theme tokens, label humanizer, and shared
// chrome components. Exposed so slot authors build custom widgets
// that match the runtime's look and wording.
//
// `ChartWidget` here is the code-split wrapper (components/
// ChartWidgetLazy): importing it costs nothing until it renders, and
// only then does the browser fetch @mantine/charts + recharts. That is
// what keeps a chart-free admin's bundle chart-free even though the
// component sits on the public surface.
export { theme } from "./theme";
export { humanizeLabel, pageLabel } from "./format";
export {
  Brand,
  ThemeToggle,
  PageHeader,
  StateView,
  StatCard,
  ChartWidget,
  NavMonogram,
} from "./components";
export type {
  BrandProps,
  PageHeaderProps,
  StateViewProps,
  StatCardProps,
  ChartWidgetProps,
  NavMonogramProps,
} from "./components";
