import { describe, expect, it } from "vitest";
import {
  SPEC_SCHEMA_VERSION,
  detailFieldSlotKey,
  detailTabSlotKey,
  globalNavItemSlotKey,
  listColumnSlotKey,
  listRowActionSlotKey,
  overviewWidgetSlotKey,
} from "./types";

// The slot-key builders are the single source of truth for the wire
// strings that match registered custom-JS slots to render sites. A
// drift between a builder here and its consumer (ListPage/DetailPage/…)
// silently disables an override, so these pin the exact format.
describe("slot key builders", () => {
  it("schema version is the documented constant", () => {
    expect(SPEC_SCHEMA_VERSION).toBe("3");
  });

  it.each([
    { name: "overview widget", got: overviewWidgetSlotKey("sales"), want: "overview:widget:sales" },
    {
      name: "list column",
      got: listColumnSlotKey("UserAdmin", "status"),
      want: "UserAdmin:list:column:status",
    },
    {
      name: "detail field",
      got: detailFieldSlotKey("UserAdmin", "bio"),
      want: "UserAdmin:detail:field:bio",
    },
    {
      name: "list row action",
      got: listRowActionSlotKey("UserAdmin"),
      want: "UserAdmin:list:row-action",
    },
    {
      name: "detail tab",
      got: detailTabSlotKey("UserAdmin", "audit_log"),
      want: "UserAdmin:detail:tab:audit_log",
    },
    { name: "global nav item", got: globalNavItemSlotKey(), want: "global:nav:item" },
  ])("$name → $want", ({ got, want }) => {
    expect(got).toBe(want);
  });
});
