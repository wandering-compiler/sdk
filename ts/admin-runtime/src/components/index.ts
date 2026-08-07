// The admin runtime's shared component kit. Small on purpose —
// the chrome primitives every page composes from. Re-exported on
// the package surface (index.ts) so slot authors build with the
// same pieces.

export { Brand } from "./Brand";
export type { BrandProps } from "./Brand";
export { ThemeToggle } from "./ThemeToggle";
export { PageHeader } from "./PageHeader";
export type { PageHeaderProps } from "./PageHeader";
export { StateView } from "./StateView";
export type { StateViewProps } from "./StateView";
export { StatCard } from "./StatCard";
export type { StatCardProps } from "./StatCard";
// `ChartWidget` on the package surface is the LAZY wrapper, and this
// barrel must never re-export ./ChartWidget itself: a static re-export
// is a static import as far as the bundler is concerned, and it would
// pull @mantine/charts + recharts (~450 kB raw / ~126 kB gzip) into
// the entry chunk of every admin — including the ones with no chart —
// via anything that touches this barrel. Slot authors get the same
// component under the same name and the same props; it just fetches
// its chart chunk on first render. See ./ChartWidgetLazy.
export { ChartWidgetLazy as ChartWidget } from "./ChartWidgetLazy";
export type { ChartWidgetProps } from "./ChartWidgetLazy";
export { NavMonogram, monogramLetter, monogramColor } from "./NavMonogram";
export type { NavMonogramProps } from "./NavMonogram";
