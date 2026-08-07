import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { WIRE_PARAM, codecFor, configureWire, resolveWire, wireIsPB } from "./wire";
import type { WireCodecs } from "./wire";

// docs/specs/admin/architecture.md — which wire the console asks for.
//
// The server negotiates both directions either way, so everything here is
// about what the SPA requests and, more importantly, when it must NOT request
// protobuf: asking for a wire you cannot decode is worse than staying on JSON.

const codecs: WireCodecs = {
  ".probe.Thing": { encode: () => new Uint8Array(), decode: () => ({}) },
};

function setSearch(search: string) {
  window.history.replaceState({}, "", search === "" ? "/" : `/?${search}`);
}

beforeEach(() => {
  window.localStorage.clear();
  setSearch("");
});
afterEach(() => {
  window.localStorage.clear();
  setSearch("");
});

describe("resolveWire", () => {
  it("takes the spec's wire when nothing overrides it", () => {
    expect(resolveWire(true)).toBe(true);
    expect(resolveWire(false)).toBe(false);
  });

  // The affordance the lock switch would otherwise be asked for: an operator
  // reporting a bug appends this and every payload becomes readable in
  // devtools — no redeploy, no lock edit.
  it("honours ?w17wire=json and remembers it", () => {
    setSearch(`${WIRE_PARAM}=json`);
    expect(resolveWire(true)).toBe(false);
    setSearch("");
    expect(resolveWire(true)).toBe(false);
  });

  it("honours ?w17wire=pb to put it back", () => {
    setSearch(`${WIRE_PARAM}=json`);
    resolveWire(true);
    setSearch(`${WIRE_PARAM}=pb`);
    expect(resolveWire(true)).toBe(true);
    setSearch("");
    expect(resolveWire(true)).toBe(true);
  });

  // A typo must not silently change the wire — it is ignored, not read as
  // "not pb".
  it("ignores an unrecognised value", () => {
    setSearch(`${WIRE_PARAM}=protobuff`);
    expect(resolveWire(true)).toBe(true);
    expect(resolveWire(false)).toBe(false);
  });
});

describe("configureWire", () => {
  it("stays on JSON when the console has no codecs, whatever the spec asked", () => {
    configureWire(undefined, true);
    expect(wireIsPB()).toBe(false);
    configureWire({}, true);
    expect(wireIsPB()).toBe(false);
  });

  it("uses protobuf when the spec asked and codecs are present", () => {
    configureWire(codecs, true);
    expect(wireIsPB()).toBe(true);
    expect(codecFor(".probe.Thing")).toBeDefined();
  });

  // Per-CALL, not per-console: an endpoint whose message is a well-known type
  // has no codec, and must not drag the rest back onto JSON.
  it("declines a ref it has no codec for", () => {
    configureWire(codecs, true);
    expect(codecFor(".probe.Missing")).toBeUndefined();
    expect(codecFor(undefined)).toBeUndefined();
  });

  it("declines every ref once the console is on JSON", () => {
    configureWire(codecs, false);
    expect(codecFor(".probe.Thing")).toBeUndefined();
  });
});
