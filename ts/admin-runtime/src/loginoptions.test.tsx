import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

// The credential form's observables. Mantine decorates labels (a
// required asterisk) and PasswordInput renders more than one node
// matching /password/i, so the form is asserted on the CONTROLS an
// operator types into rather than on label text.
function usernameField(): HTMLElement | null {
  return document.querySelector('input:not([type="password"])[required]');
}
function passwordField(): HTMLElement | null {
  return document.querySelector('input[type="password"]');
}
import { MantineProvider } from "@mantine/core";

import { Login } from "./Login";
import type { AdminSpec, AdminSignInOption } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiPost: vi.fn() };
});

// The login screen is the one place a deployment's choice of sign-in is
// visible, and the four combinations are all real:
//
//   password only          — every admin before federated sign-in
//   password + federated   — operators pick per sign-in
//   federated only         — no local credentials at all
//   neither                — refused by the parser; unreachable here
//
// Each is asserted on what the OPERATOR sees, not on a prop: a spec flag
// that renders nothing is the failure worth catching.

function loginSpec(auth: Partial<AdminSpec["auth"]>): AdminSpec {
  return {
    name: "Admin",
    schema_version: "3",
    auth: {
      login_endpoint: "/admin/api/authenticate",
      whoami_endpoint: "/admin/api/whoami",
      ...auth,
    },
    pages: {},
    overview: { widgets: [] },
  } as unknown as AdminSpec;
}

function renderLogin(auth: Partial<AdminSpec["auth"]>) {
  render(
    <MantineProvider>
      <Login spec={loginSpec(auth)} onLogin={() => {}} />
    </MantineProvider>,
  );
}

const marb: AdminSignInOption = {
  label: "Sign in with Marb",
  start_url: "/api/v1/auth/oauth/marb/authorize?redirect_after=/admin",
};
const google: AdminSignInOption = {
  label: "Sign in with Google",
  start_url: "/api/v1/auth/oauth/google/authorize",
};

afterEach(cleanup);

describe("password only (the default)", () => {
  it("renders the credential form and no federated buttons", () => {
    renderLogin({});
    expect(usernameField()).not.toBeNull();
    expect(passwordField()).not.toBeNull();
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("treats an ABSENT password_sign_in as enabled", () => {
    // A spec written before the field existed must not lose its form.
    renderLogin({ sign_in_options: undefined, password_sign_in: undefined });
    expect(usernameField()).not.toBeNull();
  });
});

describe("password + federated", () => {
  it("offers both, and each button links where the spec says", () => {
    renderLogin({ sign_in_options: [marb, google] });
    expect(usernameField()).not.toBeNull();

    const marbBtn = screen.getByRole("link", { name: marb.label });
    expect(marbBtn.getAttribute("href")).toBe(marb.start_url);
    const googleBtn = screen.getByRole("link", { name: google.label });
    expect(googleBtn.getAttribute("href")).toBe(google.start_url);
  });

  it("separates the two ways in", () => {
    renderLogin({ sign_in_options: [marb] });
    // Without a divider the button reads as part of the form.
    expect(screen.getByText(/^or$/i)).toBeTruthy();
  });
});

describe("federated only", () => {
  it("drops the credential form entirely", () => {
    renderLogin({ sign_in_options: [marb], password_sign_in: false });
    expect(usernameField()).toBeNull();
    expect(passwordField()).toBeNull();
    expect(screen.getByRole("link", { name: marb.label })).toBeTruthy();
  });

  it("does not offer a divider with nothing on the other side", () => {
    renderLogin({ sign_in_options: [marb], password_sign_in: false });
    expect(screen.queryByText(/^or$/i)).toBeNull();
  });

  it("says what to do instead of asking for credentials", () => {
    renderLogin({ sign_in_options: [marb], password_sign_in: false });
    expect(screen.queryByText(/enter your credentials/i)).toBeNull();
  });
});
