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

// ActionModal had ONE of its five functions covered: everything from `reset` through
// `handleSubmit` was unreached, so nothing had ever SUBMITTED this modal. What that
// leaves untested is not cosmetic — the file carries two written contracts about the
// request it builds, and a destructive-action confirmation that has to come back.
//
// Rendered through the real Mantine Modal (a portal, so queries go through `screen`
// rather than a container) because the confirm gate and the submit form are two
// render branches of the same component and the operator reaches the second through
// the first.

function spec(over: Partial<AdminActionSpec> = {}): AdminActionSpec {
  return {
    endpoint: "/admin/api/action/Wallets/freeze",
    target: "LIST",
    fields: [],
    ...over,
  } as unknown as AdminActionSpec;
}

function renderModal(
  over: Partial<AdminActionSpec> = {},
  selectedIds: string[] = ["r1", "r2"],
  handlers: { onClose?: () => void; onSuccess?: () => void } = {},
) {
  const onClose = vi.fn(handlers.onClose);
  const onSuccess = vi.fn(handlers.onSuccess);
  render(
    <MantineProvider>
      <ActionModal
        action={spec(over)}
        actionName="freeze"
        open
        onClose={onClose}
        onSuccess={onSuccess}
        selectedIds={selectedIds}
      />
    </MantineProvider>,
  );
  return { onClose, onSuccess };
}

function submitButton() {
  return screen.getByRole("button", { name: /^freeze$/i });
}

beforeEach(() => {
  vi.mocked(apiPost).mockReset();
  vi.mocked(apiPost).mockResolvedValue({});
});

afterEach(cleanup);

describe("the request ActionModal builds", () => {
  it("sends the selected ids for a row action", async () => {
    renderModal();
    await userEvent.click(submitButton());

    expect(apiPost).toHaveBeenCalledWith("/admin/api/action/Wallets/freeze", {
      ids: ["r1", "r2"],
    });
  });

  // The file's own reasoning, and the reason this is asserted on the BODY rather
  // than on "a request went out": "A PAGE action operates on no rows, so it sends no
  // `ids`. Sending an empty array instead would be worse than sending nothing: the
  // field would exist, and a request shaped like a selection that is empty is the
  // exact thing the backend refuses."
  //
  // So the assertion is that the key is ABSENT, not that it is empty. `toEqual({})`
  // says exactly that — `{ ids: [] }` fails it.
  it("sends no ids at all for a page action", async () => {
    renderModal({ target: "PAGE" }, []);
    await userEvent.click(submitButton());

    expect(apiPost).toHaveBeenCalledWith("/admin/api/action/Wallets/freeze", {});
  });

  // `if (f in extras) body[f] = extras[f]` — a field the operator never touched is
  // OMITTED rather than sent empty. That is the difference between "leave this alone"
  // and "set this to the empty string", and only the caller's own key check makes it.
  it("omits an extra field the operator never touched", async () => {
    renderModal({ fields: ["reason", "note"] });
    await userEvent.type(screen.getByLabelText(/reason/i), "fraud");
    await userEvent.click(submitButton());

    expect(apiPost).toHaveBeenCalledWith("/admin/api/action/Wallets/freeze", {
      ids: ["r1", "r2"],
      reason: "fraud",
    });
  });

  // The spec contract declares `fields` present and the generator emits `[]`, but an
  // older bundle can carry JSON null — "a TypeError the moment the modal opens",
  // which the `?? []` exists to stop. Nothing was asserting it.
  it("opens against a bundle whose fields are null", async () => {
    renderModal({ fields: null as unknown as string[] });
    await userEvent.click(submitButton());

    expect(apiPost).toHaveBeenCalledWith("/admin/api/action/Wallets/freeze", {
      ids: ["r1", "r2"],
    });
  });
});

describe("what ActionModal does with the answer", () => {
  it("reports success to the page and closes", async () => {
    const { onClose, onSuccess } = renderModal();
    await userEvent.click(submitButton());

    expect(onSuccess, "the list was never told to refetch").toHaveBeenCalled();
    expect(onClose).toHaveBeenCalled();
  });

  // A failed action must leave the modal STANDING — with the reason, and with the
  // typed extras intact, because the operator's next move is to fix one of them and
  // try again. Closing on failure would throw the input away.
  it("keeps the modal open with the reason when the action fails", async () => {
    vi.mocked(apiPost).mockRejectedValue(new Error("wallet is already frozen"));
    const { onClose, onSuccess } = renderModal({ fields: ["reason"] });

    await userEvent.type(screen.getByLabelText(/reason/i), "fraud");
    await userEvent.click(submitButton());

    expect(await screen.findByText("wallet is already frozen")).toBeTruthy();
    expect(onSuccess, "a failed action told the list it had succeeded").not.toHaveBeenCalled();
    expect(onClose, "a failed action closed the modal and lost the input").not.toHaveBeenCalled();
    // Plain `.value`: this package installs no jest-dom matchers — vitest.setup.ts
    // wires jsdom polyfills Mantine needs (matchMedia, ResizeObserver,
    // scrollIntoView) and nothing else. So `toHaveValue` fails as "Invalid Chai
    // property", which reads like a broken assertion rather than a missing matcher.
    expect(screen.getByLabelText<HTMLInputElement>(/reason/i).value).toBe("fraud");
  });

  it("reports a rejection that is not an Error at all", async () => {
    vi.mocked(apiPost).mockRejectedValue("gateway exploded");
    renderModal();

    await userEvent.click(submitButton());

    expect(await screen.findByText("gateway exploded")).toBeTruthy();
  });
});

describe("closing it", () => {
  // `handleClose` is `reset(); onClose();` and the reset is the load-bearing half: a
  // modal reopened after a failed attempt must not still be showing the old reason,
  // or the operator reads a stale error as the result of the run they just started.
  it("clears the error it was showing, so a reopen starts clean", async () => {
    vi.mocked(apiPost).mockRejectedValue(new Error("wallet is already frozen"));
    const { onClose } = renderModal();

    await userEvent.click(submitButton());
    expect(await screen.findByText("wallet is already frozen")).toBeTruthy();

    await userEvent.click(screen.getByRole("button", { name: /cancel/i }));

    expect(onClose).toHaveBeenCalled();
    expect(
      screen.queryByText("wallet is already frozen"),
      "the reason survived the close — a reopen would show the previous run's error",
    ).toBeNull();
  });
});

describe("the sentence that says what it will apply to", () => {
  // Two msgids rather than one, and the file says why: "this translator has no plural
  // form — `t()` substitutes, it does not count". So singular and plural are separate
  // strings and picking between them is this component's job, not the catalogue's.
  it("says one row in the singular", () => {
    renderModal({}, ["r1"]);
    // The whole sentence, because the count is substituted INTO it — a query for the
    // fragment alone matches no node, the text being one node.
    expect(screen.getByText("Will apply to 1 selected row.")).toBeTruthy();
  });

  it("says more than one in the plural", () => {
    renderModal({}, ["r1", "r2"]);
    expect(screen.getByText("Will apply to 2 selected rows.")).toBeTruthy();
  });

  it("says this page for a page action, which counts nothing", () => {
    renderModal({ target: "PAGE" }, []);
    expect(screen.getByText(/will apply to this page/i)).toBeTruthy();
  });
});

describe("the confirmation gate", () => {
  it("asks before it will submit anything", () => {
    renderModal({ confirm: "This freezes every selected wallet." });

    expect(screen.getByText("This freezes every selected wallet.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^freeze$/i })).toBeNull();
    expect(apiPost).not.toHaveBeenCalled();
  });

  it("submits once the operator has continued", async () => {
    renderModal({ confirm: "This freezes every selected wallet." });

    await userEvent.click(screen.getByRole("button", { name: /continue/i }));
    await userEvent.click(submitButton());

    expect(apiPost).toHaveBeenCalledWith("/admin/api/action/Wallets/freeze", {
      ids: ["r1", "r2"],
    });
  });

  // `reset()` puts `confirmed` back to `!action.confirm`, which is what makes a
  // destructive action ask AGAIN the next time it is opened. Without it the modal
  // would remember the confirmation for the rest of the session and a second click
  // on the button would submit immediately.
  it("asks again after a successful run", async () => {
    renderModal({ confirm: "This freezes every selected wallet." });

    await userEvent.click(screen.getByRole("button", { name: /continue/i }));
    await userEvent.click(submitButton());
    expect(apiPost).toHaveBeenCalledTimes(1);

    expect(
      screen.getByText("This freezes every selected wallet."),
      "the confirmation was remembered — the next run would submit without asking",
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^freeze$/i })).toBeNull();
  });
});

describe("with nothing selected", () => {
  // The button is not rendered at all rather than rendered disabled: an empty ids[]
  // reaches the storage method as `id = ANY('{}')` and matches no row, so a request
  // that looks like work and does none is the thing being prevented.
  it("refuses a row action and says why", () => {
    renderModal({}, []);

    expect(screen.getByText(/select at least one row/i)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^freeze$/i })).toBeNull();
  });

  // ...and a PAGE action is exempt, because it never had a selection to be missing.
  // Gating it on one "would make the button permanently dead".
  it("still offers a page action, which never had a selection", () => {
    renderModal({ target: "PAGE" }, []);

    expect(screen.queryByText(/select at least one row/i)).toBeNull();
    expect(submitButton()).toBeTruthy();
  });
});
