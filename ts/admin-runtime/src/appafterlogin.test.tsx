import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MantineProvider } from "@mantine/core";

import { App } from "./App";
import type { AdminSpec } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiGet: vi.fn(), apiPost: vi.fn() };
});
const { apiGet, apiPost } = await import("./api");

// The nav is filtered by whoami.permission_ids, so a page whose list
// declares a required permission is the ONLY page shape that can tell
// "whoami answered" apart from "whoami never ran": a page with no
// required_permissions renders either way and would make this test pass
// against the bug it guards. Both pages are present so the assertions
// can also show the filter is still doing its job.
const spec = {
  name: "Admin",
  schema_version: "3",
  auth: { login_endpoint: "/admin/api/login", whoami_endpoint: "/admin/api/whoami" },
  pages: {
    Wallets: {
      name: "Wallets",
      list: { endpoint: "/admin/api/list/Wallets", columns: [], required_permissions: [7] },
    },
    Secrets: {
      name: "Secrets",
      list: { endpoint: "/admin/api/list/Secrets", columns: [], required_permissions: [99] },
    },
  },
  overview: { widgets: [] },
} as unknown as AdminSpec;

function renderApp() {
  render(
    <MantineProvider>
      <App spec={spec} slots={{}} />
    </MantineProvider>,
  );
}

async function signIn() {
  const user = userEvent.setup();
  // Anchored + case-sensitive: both fields are `required`, so Mantine
  // appends a " *" span to the label text, and the password field's
  // visibility toggle carries aria-label "Toggle password visibility"
  // — a plain /password/i matcher hits both.
  await user.type(await screen.findByLabelText(/^Username/), "jiri");
  await user.type(screen.getByLabelText(/^Password/), "hunter2");
  await user.click(screen.getByRole("button", { name: "Sign in" }));
}

// Assertions are scoped to the <nav> the AppShell renders: the overview
// tiles name the same pages, so an unscoped getByText would match twice
// and could pass on the overview alone.
function nav() {
  return within(screen.getByRole("navigation"));
}

describe("App nav after a fresh sign-in", () => {
  afterEach(() => {
    cleanup();
    window.localStorage.clear();
    vi.resetAllMocks();
  });

  // Regression: the whoami fetch was keyed only on the whoami endpoint,
  // so it ran once — on a mount that had no token yet and short-circuited
  // to "anon". Signing in flipped the shell to "authed" with whoami still
  // null, and every perm-gated page was filtered out of the nav. Only a
  // manual reload (token already in localStorage at mount) populated it.
  it("shows the permitted page without a reload", async () => {
    vi.mocked(apiPost).mockResolvedValue({ token: "t-1" });
    vi.mocked(apiGet).mockResolvedValue({ user_id: "1", username: "jiri", permission_ids: [7] });

    renderApp();
    await signIn();

    await waitFor(() => expect(nav().getByText("Wallets")).toBeTruthy());
    // The filter still applies — this is a whoami-arrived assertion, not
    // a "nav renders everything" one.
    expect(nav().queryByText("Secrets")).toBeNull();
    expect(vi.mocked(apiGet)).toHaveBeenCalledWith("/admin/api/whoami");
  });

  // The same fetch on the reload path, so a fix that only ever repaired
  // the post-login case cannot pass by breaking the one that worked.
  it("shows the permitted page when the token is already stored at mount", async () => {
    window.localStorage.setItem("w17_admin_token", "t-1");
    vi.mocked(apiGet).mockResolvedValue({ user_id: "1", username: "jiri", permission_ids: [7] });

    renderApp();

    await waitFor(() => expect(nav().getByText("Wallets")).toBeTruthy());
    expect(nav().queryByText("Secrets")).toBeNull();
  });

  // The navbar is a fixed, viewport-height box: nav taller than the
  // viewport is simply unreachable, because the document scrollbar does
  // not cover a fixed element. Only a scroll container INSIDE the navbar
  // makes it reachable, so the assertion is that every nav entry sits in
  // one — a rendered height cannot be asserted under jsdom, which lays
  // nothing out and reports every box as zero.
  it("puts the nav entries inside a scroll container", async () => {
    window.localStorage.setItem("w17_admin_token", "t-1");
    vi.mocked(apiGet).mockResolvedValue({ user_id: "1", username: "jiri", permission_ids: [7] });

    renderApp();

    const link = await waitFor(() => nav().getByText("Wallets"));
    expect(link.closest("[data-scrollarea-viewport]")).not.toBeNull();
  });
});
