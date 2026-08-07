import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { renderableWidgets } from "./OverviewPage";
import { overviewWidgetSlotKey } from "./types";
import type { AdminWidgetSpec, SlotRegistry } from "./types";

// Plugins reserve overview slots declaratively but the implementing JS
// is the project's to supply. A project that supplied none used to get
// a landing page full of em-dash "Not configured yet" tiles — the very
// first screen after login, looking like a broken admin. Unwired slots
// are now dropped instead; the developer hint stays on the console.
describe("renderableWidgets", () => {
  const widget = (slot: string): AdminWidgetSpec => ({ slot, size: "SMALL" });
  const registry = (...slots: string[]): SlotRegistry =>
    Object.fromEntries(slots.map((s) => [overviewWidgetSlotKey(s), () => null]));

  beforeEach(() => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("keeps widgets whose slot has a registered renderer", () => {
    const widgets = [widget("revenue"), widget("signups")];
    expect(renderableWidgets(widgets, registry("revenue", "signups"))).toEqual(widgets);
  });

  it("drops widgets with no renderer", () => {
    const kept = widget("revenue");
    const got = renderableWidgets([kept, widget("auth_user_count")], registry("revenue"));
    expect(got).toEqual([kept]);
  });

  it("drops every widget when nothing is registered — an empty overview", () => {
    expect(renderableWidgets([widget("auth_user_count")], {})).toEqual([]);
  });

  it("still warns the developer about each dropped slot", () => {
    renderableWidgets([widget("never_wired_slot")], {});
    expect(console.warn).toHaveBeenCalledWith(
      expect.stringContaining(overviewWidgetSlotKey("never_wired_slot")),
    );
  });

  it("tolerates a missing widget list", () => {
    expect(renderableWidgets(undefined as unknown as AdminWidgetSpec[], {})).toEqual([]);
  });

  it("keeps a declarative STAT widget even with no registered slot", () => {
    const stat: AdminWidgetSpec = {
      slot: "user_stats",
      size: "MEDIUM",
      kind: "STAT",
      endpoint: "/admin/api/widget/user_stats",
      values: [{ field: "total" }],
    };
    // STAT is rendered by the runtime from the spec — no slot needed.
    expect(renderableWidgets([stat], {})).toEqual([stat]);
    expect(console.warn).not.toHaveBeenCalled();
  });
});
