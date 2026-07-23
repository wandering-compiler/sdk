import { describe, expect, it } from "vitest";

import { monogramColor, monogramLetter } from "./components/NavMonogram";

// The monogram's whole job is stable, recognisable per-page badges: the
// letter + colour must be a deterministic function of the label so a page
// keeps the same badge across renders and reloads.
describe("monogramLetter", () => {
  it("takes the first alphanumeric char, upper-cased", () => {
    expect(monogramLetter("Users")).toBe("U");
    expect(monogramLetter("org memberships")).toBe("O");
    expect(monogramLetter("  spaced")).toBe("S");
    expect(monogramLetter("2fa devices")).toBe("2");
  });

  it("skips leading non-alphanumerics", () => {
    expect(monogramLetter("#tags")).toBe("T");
    expect(monogramLetter("__internal")).toBe("I");
  });

  it("falls back to a bullet when there is no alphanumeric char", () => {
    expect(monogramLetter("")).toBe("•");
    expect(monogramLetter("---")).toBe("•");
  });
});

describe("monogramColor", () => {
  it("is deterministic for a given label", () => {
    expect(monogramColor("Users")).toBe(monogramColor("Users"));
    expect(monogramColor("Organizations")).toBe(monogramColor("Organizations"));
  });

  it("only ever returns a registered Mantine palette colour", () => {
    const palette = new Set([
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
    ]);
    for (const label of [
      "Users",
      "Roles",
      "Sessions",
      "Organizations",
      "OrgMemberships",
      "",
      "x",
    ]) {
      expect(palette.has(monogramColor(label))).toBe(true);
    }
  });

  it("spreads distinct labels across more than one colour", () => {
    const labels = ["Users", "Roles", "Sessions", "Organizations", "OrgMemberships", "Tasks"];
    const colors = new Set(labels.map(monogramColor));
    expect(colors.size).toBeGreaterThan(1);
  });
});
