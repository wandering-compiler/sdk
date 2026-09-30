import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MantineProvider } from "@mantine/core";

import { Login } from "./Login";
import { clearToken, getToken } from "./auth";
import type { AdminSpec } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiPost: vi.fn() };
});
const { apiPost } = await import("./api");

// The sign-in FAILURE path, which had no test at all: lines 70-73, 76-77 and
// 138-146 of Login.tsx were the only uncovered statements in the file, and between
// them they are every way signing in can go wrong — a rejected credential, a 200
// carrying no token, and the Alert that tells the operator either happened.
//
// loginoptions.test.tsx covers which ways IN a deployment offers. This covers what
// happens when the one it offered does not work, which is the half a login screen
// is judged on.

function loginSpec(): AdminSpec {
  return {
    name: "Admin",
    schema_version: "3",
    auth: {
      login_endpoint: "/admin/api/authenticate",
      whoami_endpoint: "/admin/api/whoami",
    },
    pages: {},
    overview: { widgets: [] },
  } as unknown as AdminSpec;
}

function renderLogin(onLogin = vi.fn()) {
  render(
    <MantineProvider>
      <Login spec={loginSpec()} onLogin={onLogin} />
    </MantineProvider>,
  );
  return onLogin;
}

async function signIn() {
  const user = userEvent.setup();
  await user.type(document.querySelector('input:not([type="password"])[required]')!, "root");
  await user.type(document.querySelector('input[type="password"]')!, "hunter2");
  await user.click(screen.getByRole("button", { name: /sign in/i }));
}

beforeEach(() => {
  clearToken();
  vi.mocked(apiPost).mockReset();
});

afterEach(() => {
  cleanup();
  clearToken();
});

describe("the sign-in failure path", () => {
  // The operator has to be told WHY, and the server's sentence is the only thing
  // that can say it — a generic "sign-in failed" would make a wrong password
  // indistinguishable from an unreachable console.
  it("shows the server's refusal instead of swallowing it", async () => {
    vi.mocked(apiPost).mockRejectedValue(new Error("invalid credentials"));
    const onLogin = renderLogin();

    await signIn();

    expect(await screen.findByText("invalid credentials")).toBeTruthy();
    expect(screen.getByText(/sign-in failed/i)).toBeTruthy();
    expect(onLogin, "a rejected sign-in called onLogin anyway").not.toHaveBeenCalled();
  });

  // The button has to come back. `finally { setSubmitting(false) }` is what makes a
  // failed attempt retryable, and a login screen stuck in its loading state after a
  // typo is a support ticket.
  it("re-enables the form so the credential can be retyped", async () => {
    vi.mocked(apiPost).mockRejectedValue(new Error("invalid credentials"));
    renderLogin();

    await signIn();
    await screen.findByText("invalid credentials");

    const button = screen.getByRole("button", { name: /sign in/i });
    expect(
      button.getAttribute("data-loading"),
      "the button stayed in its loading state",
    ).toBeNull();
  });

  // A 200 with no token is the shape that matters most here: the request SUCCEEDED,
  // so every `catch` in the file is bypassed, and without the explicit check the
  // screen would call setToken(undefined) and onLogin() — logging the operator into
  // a session that does not exist, which then fails on the next request instead of
  // this one.
  //
  // THREE assertions, and each needs its own disarm to prove, because vitest reports
  // the first failure and the later ones would otherwise be unproven decoration:
  //
  //   disarm the `if (!resp.token)`     -> the message never appears (assertion 1)
  //   delete the `return` after it      -> onLogin fires (assertion 2)
  //   setToken BEFORE the return        -> getToken is 'undefined' (assertion 3)
  //
  // That last one is worth reading twice. MEASURED: the disarm fails with
  // `expected 'undefined' to be null` — `setToken(undefined)` stores the STRING
  // "undefined". READ from auth.ts: `authHeader()` is `t ? \`Bearer ${t}\` : undefined`,
  // and that string is truthy, so every later request would carry
  // `Authorization: Bearer undefined`. Which is why the assertion is `toBeNull()` and
  // not `toBeFalsy()` — a truthiness check is exactly what fails to see this.
  it("refuses a successful response that carries no token", async () => {
    vi.mocked(apiPost).mockResolvedValue({});
    const onLogin = renderLogin();

    await signIn();

    expect(await screen.findByText(/missing 'token' field/)).toBeTruthy();
    expect(onLogin, "a tokenless 200 logged the operator in").not.toHaveBeenCalled();
    expect(getToken(), "a tokenless 200 stored something as the session").toBeNull();
  });

  // `err instanceof Error ? err.message : String(err)` — the else arm. A transport
  // that rejects with a string (or anything else) must still produce a sentence
  // rather than "[object Object]" or an empty Alert.
  it("reports a rejection that is not an Error at all", async () => {
    vi.mocked(apiPost).mockRejectedValue("gateway exploded");
    renderLogin();

    await signIn();

    expect(await screen.findByText("gateway exploded")).toBeTruthy();
  });

  // The Alert is conditional (`{error && …}`), so its ABSENCE before a failed
  // attempt is part of the contract: a login screen that opens already complaining
  // is a different bug and this is the assertion that would catch it.
  it("shows nothing before an attempt is made", () => {
    renderLogin();
    // No waitFor: Login renders synchronously and nothing has been requested, so
    // there is nothing to wait FOR. A waitFor around a negative passes on its first
    // check anyway, which makes it read as patience it does not have.
    expect(screen.queryByText(/sign-in failed/i)).toBeNull();
  });
});
