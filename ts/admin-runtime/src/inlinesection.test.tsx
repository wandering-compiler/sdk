import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MantineProvider } from "@mantine/core";

import { InlineSection } from "./InlineSection";
import type { AdminInlineSpec, AdminSpec } from "./types";

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, apiGet: vi.fn(), apiDelete: vi.fn(), apiPost: vi.fn(), apiPatch: vi.fn() };
});
const { apiGet, apiDelete, apiPost, apiPatch } = await import("./api");

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// An inline section is the only place the admin renders a CHILD collection, and
// 418 lines of it carried no test of their own: it fetches, it deletes behind a
// confirm, it re-fetches after a mutation, and every one of those paths sets
// state the rest of the tree reads. The assertions below are about the decisions
// it makes, not its markup.

const inline: AdminInlineSpec = {
  page: "wallet_entry",
  endpoint: "/admin/api/inline/wallet/{id}/entries",
  layout: "TABULAR",
  delete_endpoint: "/admin/api/inline/wallet/{id}/entries/{child_id}",
};

const spec = (): AdminSpec =>
  ({
    name: "Admin",
    schema_version: "3",
    auth: { login_endpoint: "/login", whoami_endpoint: "/whoami" },
    pages: {
      wallet_entry: {
        name: "wallet_entry",
        list: { columns: [{ name: "id" }, { name: "amount" }] },
      },
    },
    overview: { widgets: [] },
  }) as unknown as AdminSpec;

// onSelectChild is how an inline row opens the child's own detail page. Nothing
// below asserts on it, but it is REQUIRED — vitest never typechecks, so omitting
// it ran green here and red under tsc.
const onSelectChild = vi.fn();

function renderInline(parentId = "7") {
  return render(
    <MantineProvider>
      <InlineSection
        spec={spec()}
        inline={inline}
        parentId={parentId}
        onSelectChild={onSelectChild}
      />
    </MantineProvider>,
  );
}

describe("InlineSection", () => {
  it("substitutes and ENCODES the parent id into the endpoint", async () => {
    vi.mocked(apiGet).mockResolvedValue({ items: [] });
    renderInline("a/b c");
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    // An id that is not URL-safe reaches the server as part of the path, so the
    // encoding is not cosmetic: unencoded, "a/b c" becomes two path segments and
    // the request addresses a different resource.
    expect(vi.mocked(apiGet).mock.calls[0][0]).toBe("/admin/api/inline/wallet/a%2Fb%20c/entries");
  });

  it("says so when the spec names a target page it does not carry", async () => {
    vi.mocked(apiGet).mockResolvedValue({ items: [] });
    render(
      <MantineProvider>
        <InlineSection
          spec={spec()}
          inline={{ ...inline, page: "not_in_spec" }}
          parentId="7"
          onSelectChild={onSelectChild}
        />
      </MantineProvider>,
    );
    await waitFor(() => expect(screen.getByText(/not_in_spec/)).toBeTruthy());
  });

  // ⚠️ THE ONE THAT FAILS BEFORE THE FIX IN THIS COMMIT.
  //
  // `error` is set by a failed fetch and by a failed delete, and the whole body
  // — loader, empty state, table — renders behind `!error`. Nothing ever put it
  // back to null, so one failed request blanked the section for the lifetime of
  // the page: a later successful load set `rows` and the table stayed hidden
  // behind an error that had already been superseded.
  it("recovers when a later load succeeds, instead of staying blank", async () => {
    vi.mocked(apiGet).mockRejectedValueOnce(new Error("upstream unavailable"));
    const { rerender } = renderInline("7");
    await waitFor(() => expect(screen.getByText("upstream unavailable")).toBeTruthy());

    // Re-running the effect is an ordinary path: the parent id changes whenever
    // the operator opens a different record.
    vi.mocked(apiGet).mockResolvedValue({ items: [{ id: "1", amount: "10" }] });
    rerender(
      <MantineProvider>
        <InlineSection spec={spec()} inline={inline} parentId="8" onSelectChild={onSelectChild} />
      </MantineProvider>,
    );

    await waitFor(() => expect(screen.getByText("10")).toBeTruthy());
    expect(screen.queryByText("upstream unavailable")).toBeNull();
  });

  // The other door into the same trap: a failed DELETE used to set the same
  // `error` the body renders behind, so a network hiccup on one row removed the
  // whole collection from the screen — and nothing re-fetches except a
  // SUCCESSFUL mutation, so there was no way back.
  it("keeps the collection on screen when a delete fails", async () => {
    vi.mocked(apiGet).mockResolvedValue({ items: [{ id: "1", amount: "10" }] });
    vi.mocked(apiDelete).mockRejectedValue(new Error("row is referenced"));
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    renderInline();
    await waitFor(() => expect(screen.getByText("10")).toBeTruthy());

    const del = screen.getAllByRole("button").find((b) => /delete/i.test(b.textContent || ""));
    del?.click();

    await waitFor(() => expect(screen.getByText("row is referenced")).toBeTruthy());
    expect(screen.getByText("10")).toBeTruthy();
    confirm.mockRestore();
  });

  // The half-fixed version of this component cleared loadError on a successful
  // load and left actionError standing — so a delete that failed against one
  // parent kept its message on screen next to the NEXT parent's rows, attached
  // to data it was never about.
  it("drops a stale action error when a different collection loads", async () => {
    vi.mocked(apiGet).mockResolvedValue({ items: [{ id: "1", amount: "10" }] });
    vi.mocked(apiDelete).mockRejectedValue(new Error("row is referenced"));
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    const { rerender } = renderInline("7");
    await waitFor(() => expect(screen.getByText("10")).toBeTruthy());

    screen
      .getAllByRole("button")
      .find((b) => /delete/i.test(b.textContent || ""))
      ?.click();
    await waitFor(() => expect(screen.getByText("row is referenced")).toBeTruthy());

    vi.mocked(apiGet).mockResolvedValue({ items: [{ id: "2", amount: "20" }] });
    rerender(
      <MantineProvider>
        <InlineSection spec={spec()} inline={inline} parentId="8" onSelectChild={onSelectChild} />
      </MantineProvider>,
    );

    await waitFor(() => expect(screen.getByText("20")).toBeTruthy());
    expect(screen.queryByText("row is referenced")).toBeNull();
    confirm.mockRestore();
  });

  it("asks before deleting, and does nothing when the answer is no", async () => {
    vi.mocked(apiGet).mockResolvedValue({ items: [{ id: "1", amount: "10" }] });
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderInline();
    await waitFor(() => expect(screen.getByText("10")).toBeTruthy());

    const del = screen.getAllByRole("button").find((b) => /delete/i.test(b.textContent || ""));
    del?.click();

    expect(confirm).toHaveBeenCalled();
    expect(apiDelete).not.toHaveBeenCalled();
    confirm.mockRestore();
  });
});

// The inline form's fields, which threw on the first keystroke. Same defect as
// ActionModal's — `e.currentTarget.value` read inside the `setValues` updater, where
// the event has already been recycled — and the same reason it survived: 418 lines at
// 46% coverage and nothing had ever typed in here.
//
// `create_endpoint` is what renders the Add button that opens the form; without it
// this whole surface is unreachable, which is why the spec above does not carry one.
function renderCreatable() {
  return render(
    <MantineProvider>
      <InlineSection
        spec={spec()}
        inline={{ ...inline, create_endpoint: "/admin/api/inline/wallet/{id}/entries" }}
        parentId="7"
        onSelectChild={onSelectChild}
      />
    </MantineProvider>,
  );
}

describe("the inline create form", () => {
  it("accepts a keystroke in an editable field", async () => {
    vi.mocked(apiGet).mockResolvedValue({ items: [] });
    renderCreatable();
    await waitFor(() => expect(apiGet).toHaveBeenCalled());

    await userEvent.click(screen.getByRole("button", { name: /add wallet entry/i }));
    const amount = screen.getByLabelText<HTMLInputElement>(/amount/i);
    await userEvent.type(amount, "250");

    expect(amount.value, "the field did not accept input").toBe("250");
  });

  it("sends the typed value, and only the fields that were touched", async () => {
    vi.mocked(apiGet).mockResolvedValue({ items: [] });
    vi.mocked(apiPost).mockResolvedValue({});
    renderCreatable();
    await waitFor(() => expect(apiGet).toHaveBeenCalled());

    await userEvent.click(screen.getByRole("button", { name: /add wallet entry/i }));
    await userEvent.type(screen.getByLabelText(/amount/i), "250");
    await userEvent.click(screen.getByRole("button", { name: /^(save|create|add)$/i }));

    await waitFor(() => expect(apiPost).toHaveBeenCalled());
    expect(vi.mocked(apiPost).mock.calls[0][1]).toEqual({ amount: "250" });
  });
});

// A child page with a password column, which is what turns the inline form's "plain
// TextInput per field" into a secret-handling question. The defect was the same one
// DetailPage carried and worse: the row's stored hash was seeded into an UNMASKED input
// and went back on submit as the new password.
const secretSpec = (): AdminSpec =>
  ({
    name: "Admin",
    schema_version: "3",
    auth: { login_endpoint: "/login", whoami_endpoint: "/whoami" },
    pages: {
      wallet_entry: {
        name: "wallet_entry",
        list: { columns: [{ name: "id" }, { name: "secret" }] },
        detail: { read_endpoint: "/r", fields: ["secret"], field_types: { secret: "PASSWORD" } },
      },
    },
    overview: { widgets: [] },
  }) as unknown as AdminSpec;

function renderSecretInline() {
  return render(
    <MantineProvider>
      <InlineSection
        spec={secretSpec()}
        inline={{ ...inline, update_endpoint: "/admin/api/inline/wallet/{id}/entries/{child_id}" }}
        parentId="7"
        onSelectChild={onSelectChild}
      />
    </MantineProvider>,
  );
}

describe("the inline form and a secret column", () => {
  it("never seeds the stored hash, and masks the field", async () => {
    vi.mocked(apiGet).mockResolvedValue({
      items: [{ id: "1", secret: "$2b$12$KIXQm2Q0m9jV1Q0m9jV1Qe" }],
    });
    renderSecretInline();
    await waitFor(() => expect(apiGet).toHaveBeenCalled());

    await userEvent.click(screen.getByRole("button", { name: /edit/i }));

    const pw = document.querySelector<HTMLInputElement>('input[type="password"]');
    expect(pw, "the secret rendered as a plain text input").not.toBeNull();
    expect(pw!.value, "the stored hash was seeded into the inline form").toBe("");
    expect(
      screen.queryByDisplayValue(/^\$2b\$/),
      "the hash is on the page in clear text",
    ).toBeNull();
  });

  it("submits nothing for a secret left alone", async () => {
    vi.mocked(apiGet).mockResolvedValue({
      items: [{ id: "1", secret: "$2b$12$KIXQm2Q0m9jV1Q0m9jV1Qe" }],
    });
    vi.mocked(apiPatch).mockResolvedValue({});
    renderSecretInline();
    await waitFor(() => expect(apiGet).toHaveBeenCalled());

    await userEvent.click(screen.getByRole("button", { name: /edit/i }));
    await userEvent.click(screen.getByRole("button", { name: /^(save|update)$/i }));

    await waitFor(() => expect(apiPatch).toHaveBeenCalled());
    expect(vi.mocked(apiPatch).mock.calls[0][1]).toEqual({ secret: "" });
  });
});
