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

// A realm with a second factor answers a password sign-in with a challenge, not a
// session. The login page asks for the code and completes the sign-in at the
// spec's mfa_verify_endpoint — the admin's own route to the realm's VerifyMfa.

function spec(verify: boolean, dpop = false): AdminSpec {
  return {
    name: "Admin",
    schema_version: "3",
    auth: {
      login_endpoint: "/admin/api/authenticate",
      whoami_endpoint: "/admin/api/whoami",
      ...(verify ? { mfa_verify_endpoint: "/admin/api/authenticate/verify" } : {}),
      ...(dpop ? { dpop: true } : {}),
    },
    pages: {},
    overview: { widgets: [] },
  } as unknown as AdminSpec;
}

function renderLogin(s: AdminSpec, onLogin = vi.fn()) {
  render(
    <MantineProvider>
      <Login spec={s} onLogin={onLogin} />
    </MantineProvider>,
  );
  return onLogin;
}

async function signIn(user: ReturnType<typeof userEvent.setup>) {
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

describe("a challenged sign-in", () => {
  it("asks for the code and completes the sign-in with it", async () => {
    vi.mocked(apiPost)
      .mockResolvedValueOnce({ mfa_required: true, challenge_id: "ch-1" })
      .mockResolvedValueOnce({ token: "tok-after-code" });
    const onLogin = renderLogin(spec(true));
    const user = userEvent.setup();

    await signIn(user);
    expect(onLogin, "the password alone signed in").not.toHaveBeenCalled();
    expect(getToken()).toBeNull();

    await user.type(await screen.findByLabelText(/code/i), " 123 456 ");
    await user.click(screen.getByRole("button", { name: /verify/i }));

    expect(vi.mocked(apiPost).mock.calls[1]).toEqual([
      "/admin/api/authenticate/verify",
      { challenge_id: "ch-1", code: "123456" },
      undefined,
      undefined,
    ]);
    expect(getToken()).toBe("tok-after-code");
    expect(onLogin).toHaveBeenCalledOnce();
  });

  // The challenge belongs to the key that passed the password: the code goes
  // out as a mint on the same key, never re-sent on a fresh one.
  it("verifies on the same device key", async () => {
    vi.mocked(apiPost)
      .mockResolvedValueOnce({ mfa_required: true, challenge_id: "ch-2" })
      .mockResolvedValueOnce({ token: "tok" });
    renderLogin(spec(true, true));
    const user = userEvent.setup();

    await signIn(user);
    await user.type(await screen.findByLabelText(/code/i), "654321");
    await user.click(screen.getByRole("button", { name: /verify/i }));

    expect(vi.mocked(apiPost).mock.calls[0][3]).toEqual({ mint: true, resign: true });
    expect(vi.mocked(apiPost).mock.calls[1][3]).toEqual({ mint: true });
  });

  it("shows a wrong code and lets the person start over", async () => {
    vi.mocked(apiPost)
      .mockResolvedValueOnce({ mfa_required: true, challenge_id: "ch-3" })
      .mockRejectedValueOnce(new Error("The code is not right."));
    const onLogin = renderLogin(spec(true));
    const user = userEvent.setup();

    await signIn(user);
    await user.type(await screen.findByLabelText(/code/i), "000000");
    await user.click(screen.getByRole("button", { name: /verify/i }));

    expect(await screen.findByText("The code is not right.")).toBeTruthy();
    expect(onLogin).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /start over/i }));
    expect(
      document.querySelector('input[type="password"]'),
      "back to the password form",
    ).toBeTruthy();
  });

  // An admin generated without the second step cannot finish the sign-in, and
  // says so rather than "login response missing 'token' field".
  it("says so when this admin cannot ask for the code", async () => {
    vi.mocked(apiPost).mockResolvedValueOnce({ mfa_required: true, challenge_id: "ch-4" });
    renderLogin(spec(false));
    const user = userEvent.setup();

    await signIn(user);

    expect(await screen.findByText(/signs in with a second factor/i)).toBeTruthy();
    expect(screen.queryByLabelText(/code/i)).toBeNull();
  });

  // An enrolment challenge sent no code: there is nothing to type, so the
  // page does not ask for one.
  it("does not ask for a code an enrolment challenge never sent", async () => {
    vi.mocked(apiPost).mockResolvedValueOnce({
      mfa_required: true,
      challenge_id: "ch-5",
      enrolment_required: true,
    });
    const onLogin = renderLogin(spec(true));
    const user = userEvent.setup();

    await signIn(user);

    expect(await screen.findByText(/set up an authenticator/i)).toBeTruthy();
    expect(screen.queryByLabelText(/code/i)).toBeNull();
    expect(onLogin).not.toHaveBeenCalled();
  });
});
