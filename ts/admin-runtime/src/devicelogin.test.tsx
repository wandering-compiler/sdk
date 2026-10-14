import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MantineProvider } from "@mantine/core";

import { App } from "./App";
import { Login } from "./Login";
import { configureAuth, getToken } from "./auth";
import { federatedStartURL } from "./oauth";
import type { AdminSpec } from "./types";
import { IDBFactory } from "fake-indexeddb";

// jsdom cannot navigate to another document; a fragment-only change is the
// one navigation it performs. So the real start URL is recorded and the
// browser is sent to a fragment instead.
const started = vi.hoisted(() => [] as string[]);
vi.mock("./oauth", async () => {
  const actual = await vi.importActual<typeof import("./oauth")>("./oauth");
  return {
    ...actual,
    federatedStartURL: async (...args: Parameters<typeof actual.federatedStartURL>) => {
      started.push(await actual.federatedStartURL(...args));
      return "#/oauth-started";
    },
  };
});

// The screens of device identity: the sign-in form as a mint call, the
// public-computer choice, a revoked device sending the user back to sign
// in, and a federated sign-in coming back with a code. Every request goes
// through the real api.ts to a scripted fetch, so what is asserted is what
// a server would receive.

const EXCHANGE = "/api/v1/auth/oauth/exchange";

function makeSpec(auth: Partial<AdminSpec["auth"]> = {}): AdminSpec {
  return {
    name: "Admin",
    schema_version: "3",
    auth: {
      login_endpoint: "/admin/api/authenticate",
      whoami_endpoint: "/admin/api/whoami",
      ...auth,
    },
    pages: {
      Wallets: { name: "Wallets", list: { endpoint: "/admin/api/list/Wallets", columns: [] } },
    },
    overview: { widgets: [] },
  } as unknown as AdminSpec;
}

interface Sent {
  path: string;
  auth: string | null;
  proof: string | null;
  body: string;
}

function server(reply: (s: Sent) => Response) {
  const sent: Sent[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const req = new Request(
        typeof input === "string" ? new URL(input, "http://localhost:3000") : input,
        init,
      );
      const s: Sent = {
        path: new URL(req.url).pathname,
        auth: req.headers.get("Authorization"),
        proof: req.headers.get("DPoP"),
        body: await req.text(),
      };
      sent.push(s);
      return reply(s);
    }),
  );
  return sent;
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

async function typeCredentials() {
  const user = userEvent.setup();
  await user.type(await screen.findByLabelText(/^Username/), "alice");
  await user.type(screen.getByLabelText(/^Password/), "hunter2");
  return user;
}

beforeEach(() => {
  // A browser's key store; without IndexedDB every token is tab-only.
  vi.stubGlobal("indexedDB", new IDBFactory());
  window.localStorage.clear();
  window.sessionStorage.clear();
  window.history.replaceState(null, "", "/admin/");
});

afterEach(() => {
  cleanup();
  configureAuth(makeSpec().auth);
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  window.localStorage.clear();
  window.sessionStorage.clear();
  window.history.replaceState(null, "", "/admin/");
});

describe("Login with device identity", () => {
  it("signs in with a mint proof and no token, and keeps a public-computer session tab-only", async () => {
    const spec = makeSpec({ dpop: true });
    configureAuth(spec.auth);
    window.localStorage.setItem("w17_admin_token", "stale");
    const sent = server(() => json(200, { token: "t-1", public_computer: true }));
    const onLogin = vi.fn();
    render(
      <MantineProvider>
        <Login spec={spec} onLogin={onLogin} />
      </MantineProvider>,
    );
    const user = await typeCredentials();
    await user.click(screen.getByLabelText("This is a public computer"));
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(onLogin).toHaveBeenCalled());

    expect(sent[0].auth).toBeNull();
    expect(sent[0].proof).not.toBeNull();
    expect(JSON.parse(sent[0].body)).toEqual({
      username: "alice",
      password: "hunter2",
      public_computer: true,
    });
    expect(window.sessionStorage.getItem("w17_admin_token")).toBe("t-1");
    expect(window.localStorage.getItem("w17_admin_token")).toBeNull();
  });

  it("keeps a public-computer session tab-only even when the answer does not echo it", async () => {
    const spec = makeSpec({ dpop: true });
    configureAuth(spec.auth);
    server(() => json(200, { token: "t-1" }));
    const onLogin = vi.fn();
    render(
      <MantineProvider>
        <Login spec={spec} onLogin={onLogin} />
      </MantineProvider>,
    );
    const user = await typeCredentials();
    await user.click(screen.getByLabelText("This is a public computer"));
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(onLogin).toHaveBeenCalled());
    expect(window.sessionStorage.getItem("w17_admin_token")).toBe("t-1");
    expect(window.localStorage.getItem("w17_admin_token")).toBeNull();
  });

  it("sends no public_computer when the box is not ticked", async () => {
    const spec = makeSpec({ dpop: true });
    configureAuth(spec.auth);
    const sent = server(() => json(200, { token: "t-1" }));
    const onLogin = vi.fn();
    render(
      <MantineProvider>
        <Login spec={spec} onLogin={onLogin} />
      </MantineProvider>,
    );
    const user = await typeCredentials();
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(onLogin).toHaveBeenCalled());
    expect(JSON.parse(sent[0].body)).toEqual({ username: "alice", password: "hunter2" });
    expect(window.localStorage.getItem("w17_admin_token")).toBe("t-1");
  });

  it("signs in on a fresh key when the stored one was revoked", async () => {
    const spec = makeSpec({ dpop: true });
    configureAuth(spec.auth);
    const sent = server(() =>
      sent.length === 1
        ? json(401, {
            error: {
              code: "UNAUTHENTICATED",
              message: "revoked",
              details: [{ code: "DEVICE_REVOKED" }],
            },
          })
        : json(200, { token: "t-1" }),
    );
    const onLogin = vi.fn();
    render(
      <MantineProvider>
        <Login spec={spec} onLogin={onLogin} />
      </MantineProvider>,
    );
    const user = await typeCredentials();
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(onLogin).toHaveBeenCalled());
    expect(sent).toHaveLength(2);
    expect(sent[1].proof).not.toBe(sent[0].proof);
    expect(getToken()).toBe("t-1");
  });

  it("offers no public-computer box, and signs in with Bearer, on an admin without it", async () => {
    const spec = makeSpec();
    configureAuth(spec.auth);
    window.localStorage.setItem("w17_admin_token", "stale");
    const sent = server(() => json(200, { token: "t-1" }));
    const onLogin = vi.fn();
    render(
      <MantineProvider>
        <Login spec={spec} onLogin={onLogin} />
      </MantineProvider>,
    );
    expect(screen.queryByLabelText("This is a public computer")).toBeNull();
    const user = await typeCredentials();
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(onLogin).toHaveBeenCalled());
    expect(sent[0].proof).toBeNull();
    expect(sent[0].auth).toBe("Bearer stale");
    expect(JSON.parse(sent[0].body)).toEqual({ username: "alice", password: "hunter2" });
  });

  it("starts a federated sign-in with a PKCE challenge when the admin redeems codes", async () => {
    const spec = makeSpec({
      dpop: true,
      oauth_exchange_endpoint: EXCHANGE,
      sign_in_options: [{ label: "Sign in with Acme", start_url: "/oauth/acme/authorize" }],
    });
    configureAuth(spec.auth);
    started.length = 0;
    render(
      <MantineProvider>
        <Login spec={spec} onLogin={() => {}} />
      </MantineProvider>,
    );
    const user = userEvent.setup();
    await user.click(screen.getByLabelText("This is a public computer"));
    // A button, not a link: a link to the bare start_url opened in a new
    // tab would reach the plugin without a challenge.
    expect(screen.queryByRole("link", { name: "Sign in with Acme" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Sign in with Acme" }));
    await waitFor(() => expect(window.location.hash).toBe("#/oauth-started"));
    const target = new URL(started[0]);
    expect(target.pathname).toBe("/oauth/acme/authorize");
    expect(target.searchParams.get("code_challenge")).toMatch(/^[A-Za-z0-9_-]{43}$/);
    const pending = JSON.parse(window.sessionStorage.getItem("w17_admin_oauth") ?? "{}") as {
      public_computer?: boolean;
    };
    expect(pending.public_computer).toBe(true);
  });

  it("starts one federated sign-in at a time", async () => {
    const spec = makeSpec({
      oauth_exchange_endpoint: EXCHANGE,
      sign_in_options: [{ label: "Sign in with Acme", start_url: "/oauth/acme/authorize" }],
    });
    configureAuth(spec.auth);
    vi.spyOn(crypto.subtle, "digest").mockReturnValue(new Promise<ArrayBuffer>(() => {}));
    render(
      <MantineProvider>
        <Login spec={spec} onLogin={() => {}} />
      </MantineProvider>,
    );
    const button = screen.getByRole("button", { name: "Sign in with Acme" });
    await userEvent.setup().click(button);
    await waitFor(() => expect((button as HTMLButtonElement).disabled).toBe(true));
  });

  it("explains that a sign-in needs HTTPS where there is no WebCrypto", async () => {
    const spec = makeSpec({
      oauth_exchange_endpoint: EXCHANGE,
      sign_in_options: [{ label: "Sign in with Acme", start_url: "/oauth/acme/authorize" }],
    });
    configureAuth(spec.auth);
    vi.stubGlobal("crypto", { getRandomValues: crypto.getRandomValues.bind(crypto) });
    render(
      <MantineProvider>
        <Login spec={spec} onLogin={() => {}} />
      </MantineProvider>,
    );
    await userEvent.setup().click(screen.getByRole("button", { name: "Sign in with Acme" }));
    expect(
      await screen.findByText("This browser cannot start a secure sign-in here — it needs HTTPS"),
    ).toBeTruthy();
    expect(window.sessionStorage.getItem("w17_admin_oauth")).toBeNull();
  });

  it("says why a federated sign-in could not start", async () => {
    const spec = makeSpec({
      oauth_exchange_endpoint: EXCHANGE,
      password_sign_in: false,
      sign_in_options: [{ label: "Sign in with Acme", start_url: "/oauth/acme/authorize" }],
    });
    configureAuth(spec.auth);
    vi.spyOn(crypto.subtle, "digest").mockRejectedValue(new Error("no WebCrypto here"));
    render(
      <MantineProvider>
        <Login spec={spec} onLogin={() => {}} />
      </MantineProvider>,
    );
    await userEvent.setup().click(screen.getByRole("button", { name: "Sign in with Acme" }));
    expect(await screen.findByText("no WebCrypto here")).toBeTruthy();
    expect(window.location.hash).not.toBe("#/oauth-started");
  });
});

describe("App with device identity", () => {
  function renderApp(spec: AdminSpec) {
    render(
      <MantineProvider>
        <App spec={spec} slots={{}} />
      </MantineProvider>,
    );
  }

  it("sends the user back to sign in when the device was revoked, and says why", async () => {
    window.localStorage.setItem("w17_admin_token", "tok");
    server(() =>
      json(401, {
        error: {
          code: "UNAUTHENTICATED",
          message: "revoked",
          details: [{ code: "DEVICE_REVOKED" }],
        },
      }),
    );
    renderApp(makeSpec({ dpop: true }));
    expect(await screen.findByText("This device was signed out. Sign in again.")).toBeTruthy();
    expect(getToken()).toBeNull();
  });

  it("redeems a #code= the callback sent back, and signs the user in", async () => {
    const spec = makeSpec({ dpop: true, oauth_exchange_endpoint: EXCHANGE });
    configureAuth(spec.auth);
    await federatedStartURL("/oauth/acme/authorize");
    window.history.replaceState(null, "", "/admin/#code=one-time");
    const sent = server((s) =>
      s.path === EXCHANGE
        ? json(200, { token: "t-1" })
        : json(200, { user_id: "u", username: "alice" }),
    );
    renderApp(spec);
    expect(await screen.findByRole("button", { name: "Sign out" })).toBeTruthy();
    expect(window.location.href).not.toContain("one-time");
    expect(sent.map((s) => s.path)).toEqual([EXCHANGE, "/admin/api/whoami"]);
    expect(JSON.parse(sent[0].body)).toMatchObject({ code: "one-time" });
    expect(sent[0].auth).toBeNull();
    expect(sent[1].auth).toBe("DPoP t-1");
  });

  it("shows why a code could not be redeemed", async () => {
    window.history.replaceState(null, "", "/admin/#code=one-time");
    const sent = server(() => json(200, {}));
    renderApp(makeSpec({ dpop: true, oauth_exchange_endpoint: EXCHANGE }));
    expect(await screen.findByText(/started in another tab/)).toBeTruthy();
    expect(sent).toHaveLength(0);
  });

  it("drops a #token= and asks the user to sign in again", async () => {
    window.history.replaceState(null, "", "/admin/#token=ey.leaked");
    server(() => json(200, {}));
    renderApp(makeSpec({ dpop: true }));
    expect(
      await screen.findByText("This sign-in link cannot be used here. Sign in again."),
    ).toBeTruthy();
    expect(getToken()).toBeNull();
    expect(window.location.href).not.toContain("ey.leaked");
  });
});
