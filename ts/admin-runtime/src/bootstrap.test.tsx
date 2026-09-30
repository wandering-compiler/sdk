import { afterEach, describe, expect, it, vi } from "vitest";
import { act } from "@testing-library/react";
import { useMantineColorScheme } from "@mantine/core";

import { bootstrap } from "./bootstrap";
import { SPEC_SCHEMA_VERSION } from "./types";
import { wireIsPB } from "./wire";
import type { AdminSpec } from "./types";

// bootstrap() is the SPA entry the generated `spa/src/main.tsx` calls, and until
// 2026-09-29 NOTHING executed it: 0 of 32 statements, measured once the coverage
// instrument stopped lying about this package
// (docs/decisions/which-components-belong-in-the-gate.md). It was quoted in that
// record as "the only component that meets the rule, at 100/100/100" while carrying
// no test at all.
//
// The App shell is MOCKED. This file is about the three decisions bootstrap itself
// makes — refuse a mismatched spec, resolve the wire before anything fetches, mount
// the provider tree — not about what App renders, which has its own tests and would
// drag a router and two fetches in here.
//
// The stand-in calls a Mantine hook on purpose. Mantine's hooks THROW
// "[@mantine/core] MantineProvider was not found in tree" (measured), so mounting it
// at all is what says bootstrap wired the provider — without the hook the mock is a
// bare div and would render happily outside any provider, which is what the first
// cut of this file claimed to be testing and was not.
vi.mock("./App", () => ({
  App: (props: { spec: AdminSpec; slots: Record<string, unknown> }) => {
    useMantineColorScheme();
    return (
      <div
        data-testid="app-mounted"
        data-pages={Object.keys(props.spec.pages ?? {}).join(",")}
        data-slots={Object.keys(props.slots).join(",")}
      />
    );
  },
}));

function spec(over: Partial<AdminSpec> = {}): AdminSpec {
  return {
    name: "Admin",
    schema_version: SPEC_SCHEMA_VERSION,
    auth: { login_endpoint: "/admin/api/login", whoami_endpoint: "/admin/api/whoami" },
    pages: { Wallets: { name: "Wallets", list: { endpoint: "/l", columns: [] } } },
    ...over,
  } as unknown as AdminSpec;
}

// Every mount goes through act(), and not for tidiness: createRoot().render() is
// CONCURRENT, so without flushing it React finishes the work after the test has
// returned — and on the last test of the file that is after the jsdom environment is
// gone. It surfaced as three `ReferenceError: window is not defined` uncaught
// exceptions and exit code 1 while all six tests reported passing, which is a shape
// worth naming: the count was green and the run was red.
async function boot(opts: Parameters<typeof bootstrap>[0]): Promise<void> {
  await act(async () => {
    bootstrap(opts);
    // One microtask, so React's concurrent work is flushed INSIDE act. Without the
    // await the arrow has nothing to await and act's sync overload returns a
    // non-thenable — two lint rules disagree about the shorter spellings, and this is
    // the one that is also true: the flush is what the test needs.
    await Promise.resolve();
  });
}

function root(): HTMLElement {
  const el = document.createElement("div");
  document.body.appendChild(el);
  return el;
}

afterEach(() => {
  document.body.replaceChildren();
  vi.unstubAllGlobals();
});

describe("bootstrap — the version gate", () => {
  // The whole point of the branch: an operator who rebuilt the spec against one
  // runtime and serves it from another sees WHICH two versions disagree, rather
  // than a blank page.
  it("refuses a spec built for another runtime, and names both versions", async () => {
    const el = root();
    await boot({ spec: spec({ schema_version: "999" }), root: el });

    const pre = el.querySelector("pre");
    expect(pre, "no error UI was mounted for a mismatched spec").not.toBeNull();
    expect(pre?.textContent).toContain("admin runtime version mismatch");
    expect(pre?.textContent).toContain("spec.schema_version = 999");
    expect(pre?.textContent).toContain(`runtime SPEC_SCHEMA_VERSION = ${SPEC_SCHEMA_VERSION}`);
  });

  // ...and it must NOT go on to mount. A version gate that renders its complaint
  // and then boots anyway is not a gate, and the `return` is the only thing
  // stopping it.
  it("does not mount the app when the versions disagree", async () => {
    const el = root();
    await boot({ spec: spec({ schema_version: "999" }), root: el });

    expect(el.querySelector('[data-testid="app-mounted"]')).toBeNull();
  });

  // The security claim written at that code: "Build the node with textContent (not
  // innerHTML) so the interpolated spec/runtime versions can never inject markup —
  // this is an error path, but the spec is external input". Nothing enforced it.
  //
  // `schema_version` reaches the page from a build artefact the console generated,
  // so this is defence in depth rather than a live hole — which is exactly the kind
  // of promise that rots unnoticed, because nothing breaks when it stops being true.
  it("cannot be made to inject markup through the version it reports", async () => {
    const el = root();
    const payload = '<img src=x onerror="globalThis.__pwned = true">';
    await boot({ spec: spec({ schema_version: payload }), root: el });

    expect(el.querySelector("img"), "the spec's version was parsed as markup").toBeNull();
    // ⚠️ Deliberately NO `expect(globalThis.__pwned).toBeUndefined()`. It was here and
    // it cannot fail: jsdom builds the <img> but never loads it, so `onerror` does not
    // run even when the markup IS injected (measured). An assertion that cannot fail
    // reads as extra safety and is the opposite. The node test above is the one with
    // force — `innerHTML` in place of `textContent` reddens it.
    //
    // Present as TEXT, which is the whole point: the operator still sees what the
    // spec claimed, they just cannot be attacked by it.
    expect(el.querySelector("pre")?.textContent).toContain(payload);
  });
});

describe("bootstrap — the happy path", () => {
  // Mounted through the real createRoot, so this also covers the provider tree: a
  // missing MantineProvider throws out of Mantine's own hooks, which is how the
  // mock-App assertion below can stand in for "the tree is wired".
  it("mounts the app and hands it the spec", async () => {
    const el = root();
    await boot({ spec: spec(), root: el });

    const mounted = el.querySelector('[data-testid="app-mounted"]');
    expect(mounted, "the app never mounted for a matching spec").not.toBeNull();
    expect(mounted!.getAttribute("data-pages")).toBe("Wallets");
  });

  // `slots={opts.slots || {}}` has two arms and only the fallback was exercised, so a
  // bootstrap that dropped the caller's registry on the floor passed every test here.
  // The slot registry is how a project injects its own widgets; losing it is silent.
  it("passes the caller's slot registry through", async () => {
    const el = root();
    await boot({ spec: spec(), root: el, slots: { "overview:widget:spend": () => null } });

    expect(el.querySelector('[data-testid="app-mounted"]')?.getAttribute("data-slots")).toBe(
      "overview:widget:spend",
    );
  });

  // Resolved BEFORE anything fetches — the comment at the call says so, and the
  // observable is `wireIsPB()`, which is what every request reads. Asserting on
  // that rather than on a spy is the difference between "configureWire was called"
  // and "the wire the caller will use is the right one".
  //
  // ⚠️ CODECS ARE PASSED, and that is the whole force of this test. Without them
  // `configureWire` returns false for any spec — so the first cut of this test, which
  // passed none, could not fail: it would have held for an implementation that ignored
  // the spec and hardcoded protobuf. With codecs in hand, `false` here can only mean
  // the spec's own answer was read.
  it("leaves the wire on JSON when the spec asked for JSON, codecs or not", async () => {
    await boot({
      spec: spec({ wire: "json" }),
      root: root(),
      codecs: { "/l": { decode: () => ({}), encode: () => new Uint8Array() } },
    });
    expect(wireIsPB()).toBe(false);
  });

  // A spec that asked for protobuf still gets JSON with no codecs in hand —
  // configureWire's own degradation. Both halves are here because the pair is what
  // says bootstrap passes the spec's answer through instead of hardcoding one.
  it("turns protobuf on only when codecs came with it", async () => {
    await boot({ spec: spec({ wire: "pb" }), root: root() });
    expect(wireIsPB(), "protobuf with no codecs — nothing could be decoded").toBe(false);

    await boot({
      spec: spec({ wire: "pb" }),
      root: root(),
      codecs: { "/l": { decode: () => ({}), encode: () => new Uint8Array() } },
    });
    expect(wireIsPB()).toBe(true);
  });
});
