import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MantineProvider } from "@mantine/core";

import { DetailPage } from "./DetailPage";
import { majorTextToMinor, minorToMajorText, moneyInputScale } from "./moneyInput";
import type { AdminPageSpec, AdminSpec } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiGet: vi.fn(), apiPatch: vi.fn() };
});
const { apiGet, apiPatch } = await import("./api");

// A MONEY value is an int64 of MINOR units. The edit form used to register it
// as a plain text box over the raw integer: 123.45 showed as "12345", typing
// "123" (meaning 123) stored 1.23, and "123,45" was PATCHed as a string the
// server could not parse. The money input shows major units and submits the
// minor units they mean.

describe("money input conversion", () => {
  it("shows minor units as major units, exactly", () => {
    expect(minorToMajorText("12345", 2)).toBe("123.45");
    expect(minorToMajorText("-5", 2)).toBe("-0.05");
    expect(minorToMajorText("0", 2)).toBe("0.00");
    expect(minorToMajorText("-0", 2)).toBe("0.00");
    expect(minorToMajorText("12345678", 4)).toBe("1234.5678");
    expect(minorToMajorText("700", 0)).toBe("700");
    // Past 2^53: every digit survives.
    expect(minorToMajorText("9223372036854775807", 2)).toBe("92233720368547758.07");
    expect(minorToMajorText(undefined, 2)).toBe("");
    expect(minorToMajorText("abc", 2)).toBe("abc");
  });

  it("reads a typed amount back into minor units with either separator", () => {
    expect(majorTextToMinor("123,45", 2)).toBe("12345");
    expect(majorTextToMinor("123.45", 2)).toBe("12345");
    expect(majorTextToMinor("123", 2)).toBe("12300");
    expect(majorTextToMinor("1 234,5", 2)).toBe("123450");
    expect(majorTextToMinor("1 234,50", 2)).toBe("123450");
    expect(majorTextToMinor("-0,05", 2)).toBe("-5");
    expect(majorTextToMinor("-0", 2)).toBe("0");
    expect(majorTextToMinor(",5", 2)).toBe("50");
    expect(majorTextToMinor("92233720368547758.07", 2)).toBe("9223372036854775807");
    expect(majorTextToMinor("", 2)).toBe("");
    expect(majorTextToMinor("7", 0)).toBe("7");
  });

  it("refuses what is not an amount instead of rounding it into one", () => {
    expect(majorTextToMinor("1.234,50", 2)).toBeNull();
    expect(majorTextToMinor("12.345", 2)).toBeNull();
    expect(majorTextToMinor("1,5", 0)).toBeNull();
    expect(majorTextToMinor("12a", 2)).toBeNull();
    expect(majorTextToMinor("-", 2)).toBeNull();
    expect(majorTextToMinor(".", 2)).toBeNull();
  });

  it("finds the scale only on a money slot", () => {
    expect(
      moneyInputScale({ msgid: "{value}", slots: [{ name: "value", preset: "money", scale: 4 }] }),
    ).toBe(4);
    expect(
      moneyInputScale({ msgid: "{value}", slots: [{ name: "value", preset: "money", scale: 0 }] }),
    ).toBe(0);
    expect(
      moneyInputScale({ msgid: "{value}", slots: [{ name: "value", preset: "number" }] }),
    ).toBeUndefined();
    expect(moneyInputScale(undefined)).toBeUndefined();
  });
});

const spec = {} as AdminSpec;

function page(): AdminPageSpec {
  return {
    name: "Invoices",
    detail: {
      read_endpoint: "/admin/api/detail/Invoices/{id}",
      update_endpoint: "/admin/api/detail/Invoices/{id}",
      fields: ["title", "price"],
      field_formats: {
        price: {
          msgid: "{value}",
          slots: [{ name: "value", preset: "money", scale: 2, currency: "CZK" }],
        },
      },
    },
  } as unknown as AdminPageSpec;
}

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
  vi.mocked(apiPatch).mockReset();
  vi.mocked(apiGet).mockResolvedValue({ title: "A", price: "12345" });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("a MONEY field on an editable form", () => {
  function renderDetail() {
    render(
      <MantineProvider>
        <DetailPage spec={spec} page={page()} rowId="1" onBack={vi.fn()} />
      </MantineProvider>,
    );
  }

  it("shows major units and PATCHes the minor units a typed '200,5' means", async () => {
    vi.mocked(apiPatch).mockResolvedValue({ title: "A", price: "20050" });
    renderDetail();
    const input = await waitFor(() => screen.getByDisplayValue("123.45"));

    await userEvent.clear(input);
    await userEvent.type(input, "200,5");
    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => expect(apiPatch).toHaveBeenCalled());
    expect(vi.mocked(apiPatch).mock.calls[0][1]).toEqual({ title: "A", price: "20050" });
  });

  it("re-seeds from a save response that stored something else", async () => {
    // The server normalised the amount: the input follows the stored value.
    vi.mocked(apiPatch).mockResolvedValue({ title: "A", price: "20000" });
    renderDetail();
    const input = await waitFor(() => screen.getByDisplayValue("123.45"));
    await userEvent.clear(input);
    await userEvent.type(input, "200,5");
    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(screen.getByDisplayValue("200.00")).toBeTruthy());
  });

  it("submits an untouched amount as the minor units it came as", async () => {
    vi.mocked(apiPatch).mockResolvedValue({ title: "A", price: "12345" });
    renderDetail();
    await waitFor(() => expect(screen.getByDisplayValue("123.45")).toBeTruthy());
    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(apiPatch).toHaveBeenCalled());
    expect(vi.mocked(apiPatch).mock.calls[0][1]).toEqual({ title: "A", price: "12345" });
  });

  it("never turns text that is not an amount into one", async () => {
    vi.mocked(apiPatch).mockResolvedValue({ title: "A", price: "12345" });
    renderDetail();
    const input = await waitFor(() => screen.getByDisplayValue("123.45"));
    await userEvent.clear(input);
    await userEvent.type(input, "12.345");
    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(apiPatch).toHaveBeenCalled());
    // Passed through as typed, for the server to refuse — not rounded to 1235.
    expect(vi.mocked(apiPatch).mock.calls[0][1]).toEqual({ title: "A", price: "12.345" });
  });
});
