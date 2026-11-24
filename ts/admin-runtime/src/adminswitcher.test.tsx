import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MantineProvider } from "@mantine/core";

import { AdminSwitcher } from "./components/AdminSwitcher";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function stubLinks(links: Record<string, string>, ok = true) {
  vi.stubGlobal(
    "fetch",
    vi.fn(() => Promise.resolve({ ok, json: () => Promise.resolve({ links }) })),
  );
}

describe("AdminSwitcher", () => {
  it("offers the admins whose address the environment gives, linking to it", async () => {
    stubLinks({ billing: "http://localhost:9091/admin" });
    render(
      <MantineProvider>
        <AdminSwitcher
          endpoint="/admin/api/links"
          links={[
            { domain: "billing", label: "Billing" },
            { domain: "crm", label: "CRM" },
          ]}
        />
      </MantineProvider>,
    );
    const button = await screen.findByRole("button", { name: "Switch admin" });
    fireEvent.click(button);
    const item = await screen.findByText("Billing");
    expect(item.closest("a")?.getAttribute("href")).toBe("http://localhost:9091/admin");
    expect(screen.queryByText("CRM")).toBeNull();
  });

  it("renders nothing when no link has an address", async () => {
    stubLinks({}, false);
    const { container } = render(
      <MantineProvider>
        <AdminSwitcher
          endpoint="/admin/api/links"
          links={[{ domain: "billing", label: "Billing" }]}
        />
      </MantineProvider>,
    );
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled());
    expect(container.querySelector("button")).toBeNull();
  });
});
