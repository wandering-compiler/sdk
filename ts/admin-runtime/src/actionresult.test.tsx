import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MantineProvider } from "@mantine/core";

import { ActionModal } from "./ActionModal";
import type { AdminActionSpec } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiPost: vi.fn() };
});
const { apiPost } = await import("./api");

// An action that declares `result` shows the named response fields after it
// succeeds — the case it exists for is a one-time secret (a registration code,
// a fresh API key) that the admin used to mint and then show nobody.
//
// The secret half is a promise about what does NOT happen, so it is asserted
// on every channel the runtime could leak through: browser storage, the
// console, and the DOM after the dialog closes. "Query cache" has no instance
// here — the runtime keeps no response cache (no react-query, no SWR); the
// only place a response lives is the state of the component that asked for it,
// which is exactly what the close-and-reopen case pins.

const CODE = "W17-REG-7f3a9c-ONE-TIME";
const RESPONSE = {
  relay: "relay-eu-1",
  code: CODE,
  expires_at: "2026-10-05T12:00:00Z",
  relay_fingerprint: "sha256:abcd",
};

function spec(over: Partial<AdminActionSpec> = {}): AdminActionSpec {
  return {
    endpoint: "/admin/api/action/Relays/issue_registration_code",
    target: "DETAIL",
    fields: [],
    result: { fields: ["code", "expires_at", "relay"], secret: true },
    ...over,
  };
}

function Harness({
  action,
  open,
  onClose,
  onSuccess,
}: {
  action: AdminActionSpec;
  open: boolean;
  onClose: () => void;
  onSuccess: () => void;
}) {
  return (
    <MantineProvider>
      <ActionModal
        action={action}
        actionName="issue_registration_code"
        open={open}
        onClose={onClose}
        onSuccess={onSuccess}
        selectedIds={["relay-id-1"]}
      />
    </MantineProvider>
  );
}

function submit() {
  return userEvent.click(screen.getByRole("button", { name: /^issue registration code$/i }));
}

const storageWrites: unknown[][] = [];
const consoleCalls: unknown[][] = [];

beforeEach(() => {
  vi.mocked(apiPost).mockReset();
  vi.mocked(apiPost).mockResolvedValue(RESPONSE);
  storageWrites.length = 0;
  consoleCalls.length = 0;
  // Storage.prototype covers localStorage AND sessionStorage.
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(function (
    this: Storage,
    ...args: unknown[]
  ) {
    storageWrites.push(args);
  });
  for (const m of ["log", "info", "debug", "warn", "error", "trace"] as const) {
    vi.spyOn(console, m).mockImplementation((...args: unknown[]) => {
      consoleCalls.push(args);
    });
  }
  try {
    window.localStorage.clear();
    window.sessionStorage.clear();
  } catch {
    // jsdom always has both; nothing to clear otherwise.
  }
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function leaked(haystack: unknown[][]): boolean {
  return haystack.some((args) =>
    args.some((a) => {
      try {
        return (typeof a === "string" ? a : (JSON.stringify(a) ?? "")).includes(CODE);
      } catch {
        return String(a).includes(CODE);
      }
    }),
  );
}

describe("an action with a result", () => {
  it("shows the declared response fields, in declared order, after it succeeds", async () => {
    const onClose = vi.fn();
    const onSuccess = vi.fn();
    render(<Harness action={spec()} open onClose={onClose} onSuccess={onSuccess} />);
    await submit();

    expect((await screen.findByTestId("action-result-code")).textContent).toContain(CODE);
    expect(screen.getByTestId("action-result-expires_at").textContent).toContain(
      "2026-10-05T12:00:00Z",
    );
    expect(screen.getByTestId("action-result-relay").textContent).toContain("relay-eu-1");
    // Only the DECLARED fields: the fingerprint is in the response, not in `fields`.
    expect(screen.queryByText("sha256:abcd")).toBeNull();
    const order = screen
      .getAllByTestId(/^action-result-/)
      .map((el) => el.getAttribute("data-testid"));
    expect(order).toEqual([
      "action-result-code",
      "action-result-expires_at",
      "action-result-relay",
    ]);

    // The modal stays open on the result: closing it is the operator's call.
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByText(/shown only once/i)).toBeTruthy();
  });

  it("copies a value to the clipboard", async () => {
    const user = userEvent.setup();
    render(<Harness action={spec()} open onClose={vi.fn()} onSuccess={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: /^issue registration code$/i }));

    await user.click(await screen.findByRole("button", { name: /copy code/i }));
    expect(await navigator.clipboard.readText()).toBe(CODE);
  });

  it("refreshes the page only when the result is closed, then closes", async () => {
    const onClose = vi.fn();
    const onSuccess = vi.fn();
    render(<Harness action={spec()} open onClose={onClose} onSuccess={onSuccess} />);
    await submit();
    await screen.findByTestId("action-result-code");
    // Refreshing now could unmount the modal (a failed reload renders an
    // error view) and lose a value the operator has not copied yet.
    expect(onSuccess).not.toHaveBeenCalled();

    await userEvent.click(screen.getByRole("button", { name: /^close$/i }));
    expect(onSuccess).toHaveBeenCalledTimes(1);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("does not say 'shown once' for a result that is not secret", async () => {
    render(
      <Harness
        action={spec({ result: { fields: ["relay"], secret: false } })}
        open
        onClose={vi.fn()}
        onSuccess={vi.fn()}
      />,
    );
    await submit();
    expect((await screen.findByTestId("action-result-relay")).textContent).toContain("relay-eu-1");
    expect(screen.queryByText(/shown only once/i)).toBeNull();
  });

  it("keeps the old contract without a result: success closes the modal", async () => {
    const onClose = vi.fn();
    const onSuccess = vi.fn();
    render(
      <Harness action={spec({ result: undefined })} open onClose={onClose} onSuccess={onSuccess} />,
    );
    await submit();
    expect(onSuccess).toHaveBeenCalledTimes(1);
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(CODE)).toBeNull();
  });
});

describe("a SECRET result is kept nowhere", () => {
  it("never reaches browser storage or the console, and is gone once the dialog closes", async () => {
    let open = true;
    const onClose = vi.fn(() => {
      open = false;
    });
    const { rerender } = render(
      <Harness action={spec()} open onClose={onClose} onSuccess={vi.fn()} />,
    );
    await submit();
    expect((await screen.findByTestId("action-result-code")).textContent).toContain(CODE);

    await userEvent.click(screen.getByRole("button", { name: /^close$/i }));
    rerender(<Harness action={spec()} open={open} onClose={onClose} onSuccess={vi.fn()} />);
    // Reopen the SAME mounted modal, as the page does on the next click: it
    // must come back as the form, not as the last result.
    rerender(<Harness action={spec()} open onClose={onClose} onSuccess={vi.fn()} />);

    expect(screen.queryByText(CODE)).toBeNull();
    expect(screen.getByRole("button", { name: /^issue registration code$/i })).toBeTruthy();
    expect(document.body.innerHTML).not.toContain(CODE);

    expect(
      leaked(storageWrites),
      `a storage write carried the secret: ${JSON.stringify(storageWrites)}`,
    ).toBe(false);
    for (const store of [window.localStorage, window.sessionStorage]) {
      for (let i = 0; i < store.length; i++) {
        const k = store.key(i) ?? "";
        expect(`${k}=${store.getItem(k)}`).not.toContain(CODE);
      }
    }
    expect(
      leaked(consoleCalls),
      `a console call carried the secret: ${JSON.stringify(consoleCalls)}`,
    ).toBe(false);
  });

  // The leak detectors above are the load-bearing half: if they could not see
  // a write, "nothing leaked" would be vacuous.
  it("the detectors do see a leak", () => {
    window.sessionStorage.setItem("probe", CODE);
    console.log("probe", { code: CODE });
    expect(leaked(storageWrites)).toBe(true);
    expect(leaked(consoleCalls)).toBe(true);
  });
});
