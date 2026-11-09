import { describe, expect, it, afterEach } from "vitest";
import { Context, Metadata } from "../src/context.ts";
import * as paging from "../src/paging.ts";
import * as principal from "../src/principal.ts";
import { msg } from "./helpers.ts";

function envelope(uid = "u-1", kw: Record<string, unknown> = {}): string {
  const AuthResp = msg("AuthResp");
  return Buffer.from(AuthResp.toBinary(AuthResp({ userId: uid, ...kw }))).toString("base64");
}

afterEach(() => principal.setAuthRespType(undefined));

// --- forwardToOutgoing: the exact key set, replace semantics ---------------

describe("forwardToOutgoing", () => {
  it("relays exactly the gateway stamps", () => {
    const incoming: Record<string, string> = {
      "x-w17-user": envelope(),
      "x-w17-scope-org_id": "o-1",
      "w17-label-tenant": "t-1",
      "x-w17-device": "dev",
      "x-w17-dpop-jkt": "jkt",
      "x-w17-data-scope": "org",
      "x-w17-audience": "public",
      "x-w17-membership-kinds": "member",
      "x-w17-org-from": "host",
      "x-w17-host": "example.test",
      // never relayed:
      authorization: "Bearer secret",
      "x-w17-paging-limit": "5",
      "w17-language": "cs",
      traceparent: "00-abc-def-01",
      "w17-tx-id": "tx-9",
      "x-w17-surface": "rest",
      "x-custom": "c",
    };
    const out = principal.forwardToOutgoing(new Context({ incoming })).outgoing;
    const relayed = [
      "x-w17-user", "x-w17-scope-org_id", "w17-label-tenant", "x-w17-device", "x-w17-dpop-jkt",
      "x-w17-data-scope", "x-w17-audience", "x-w17-membership-kinds", "x-w17-org-from", "x-w17-host",
    ];
    expect([...out.keys()].sort()).toEqual([...relayed].sort());
    for (const k of relayed) expect(out.getAll(k)).toEqual([incoming[k]]);
  });

  it("replaces the whole relayed set and keeps other outgoing keys", () => {
    const ctx = new Context({
      incoming: { "x-w17-user": envelope("new"), "x-w17-scope-org_id": "o-new" },
      outgoing: {
        "x-w17-user": "stale",
        "x-w17-scope-org_id": "o-stale",
        "x-w17-scope-team_id": "t-stale", // outgoing-only relayed key: dropped
        "w17-label-x": "stale",
        "w17-tx-id": "tx-1", // not relayed: kept
        "x-w17-paging-limit": "3", // not relayed: kept
      },
    });
    const out = principal.forwardToOutgoing(ctx).outgoing;
    expect(out.getAll("x-w17-user")).toEqual([envelope("new")]);
    expect(out.getAll("x-w17-scope-org_id")).toEqual(["o-new"]);
    expect(out.has("x-w17-scope-team_id")).toBe(false);
    expect(out.has("w17-label-x")).toBe(false);
    expect(out.getAll("w17-tx-id")).toEqual(["tx-1"]);
    expect(out.getAll("x-w17-paging-limit")).toEqual(["3"]);
  });

  it("without an incoming principal returns ctx unchanged", () => {
    const ctx = new Context({ incoming: { "w17-language": "cs" }, outgoing: { "x-w17-user": "gateway-stamped" } });
    expect(principal.forwardToOutgoing(ctx)).toBe(ctx);
  });

  it("is idempotent and never duplicates", () => {
    const ctx = new Context({ incoming: { "x-w17-user": envelope(), "x-w17-scope-org_id": "o" } });
    const once = principal.forwardToOutgoing(ctx);
    const twice = principal.forwardToOutgoing(once);
    expect(twice.outgoing.equals(once.outgoing)).toBe(true);
    expect(twice.outgoing.getAll("x-w17-scope-org_id")).toEqual(["o"]);
  });

  it("keeps every value of a multivalued key", () => {
    const ctx = new Context({ incoming: [["w17-label-a", "1"], ["w17-label-a", "2"]] });
    expect(principal.forwardToOutgoing(ctx).outgoing.getAll("w17-label-a")).toEqual(["1", "2"]);
  });

  it("metadata keys are case-insensitive", () => {
    const md = new Metadata({ "X-W17-User": "e" });
    expect(md.getAll("x-w17-user")).toEqual(["e"]);
    expect(principal.isRelayedKey("x-w17-user")).toBe(true);
  });

  it("a derived context never changes the one it came from", () => {
    const ctx = new Context({ incoming: { "x-w17-user": envelope() } });
    principal.forwardToOutgoing(ctx);
    expect(ctx.outgoing.size).toBe(0);
  });
});

// --- readers ------------------------------------------------------------------

describe("readers", () => {
  it("userId: the fast path, then the envelope", () => {
    principal.setAuthRespType(msg("AuthResp"));
    expect(principal.userId(new Context({ incoming: { "x-w17-scope-user_id": "fast" } }))).toBe("fast");
    expect(principal.userId(new Context({ incoming: { "x-w17-user": envelope("env") } }))).toBe("env");
    principal.setAuthRespType(undefined);
    expect(principal.userId(new Context({ incoming: { "x-w17-user": envelope("x") } }), msg("AuthResp"))).toBe("x");
  });

  it.each([
    [{}],
    [{ "x-w17-user": "" }],
    [{ "x-w17-user": "!!not base64!!" }],
    [{ "x-w17-user": Buffer.from([0xff, 0xff, 0xff]).toString("base64") }],
  ])("userId fails closed (%j)", (incoming) => {
    principal.setAuthRespType(msg("AuthResp"));
    expect(() => principal.userId(new Context({ incoming }))).toThrow(principal.NoPrincipalError);
  });

  it("an empty user id is no principal", () => {
    principal.setAuthRespType(msg("AuthResp"));
    expect(() => principal.userId(new Context({ incoming: { "x-w17-user": envelope("") } }))).toThrow(principal.NoPrincipalError);
  });

  it("envelope decodes the whole AuthResp", () => {
    const who = principal.envelope(new Context({ incoming: { "x-w17-user": envelope("u", { scopes: { org_id: "o" } }) } }), msg("AuthResp"));
    expect(who.userId).toBe("u");
    expect(who.scopes.org_id).toBe("o");
  });

  it("scope, label, device, data scope", () => {
    const ctx = new Context({
      incoming: { "x-w17-scope-org_id": "o", "w17-label-kind": "bot", "x-w17-device": "d", "x-w17-data-scope": "org" },
    });
    expect(principal.scope(ctx, "org_id")).toBe("o");
    expect(principal.scope(ctx, "team_id")).toBeUndefined();
    expect(principal.label(ctx, "kind")).toBe("bot");
    expect(principal.label(ctx, "other")).toBeUndefined();
    expect(principal.deviceId(ctx)).toBe("d");
    expect(principal.dataScope(ctx)).toBe("org");
  });

  it("deviceId needs exactly one value", () => {
    expect(principal.deviceId(new Context({ incoming: [["x-w17-device", "a"], ["x-w17-device", "b"]] }))).toBeUndefined();
  });

  it.each([
    [[], ""],
    [["all"], "all"],
    [["weird"], "owner"],
    [["org", "all"], "owner"],
  ])("data scope %j reads %j (unknown or several is the narrowest)", (vals, want) => {
    const ctx = new Context({ incoming: (vals).map((v) => ["x-w17-data-scope", v] as const) });
    expect(principal.dataScope(ctx)).toBe(want);
  });
});

// --- actAs --------------------------------------------------------------------

describe("actAs", () => {
  it("replaces the principal on both sides", () => {
    const ctx = new Context({
      incoming: { "x-w17-user": envelope("caller"), "x-w17-scope-org_id": "o-caller", "w17-language": "cs" },
      outgoing: { "x-w17-scope-team_id": "t", "w17-tx-id": "tx" },
    });
    const acted = principal.actAs(ctx, msg("AuthResp")({ userId: "person" }), { org_id: "o-person" });
    expect(acted.incoming.has("x-w17-user")).toBe(false);
    expect(acted.incoming.getAll("w17-language")).toEqual(["cs"]);
    const out = acted.outgoing;
    expect(out.getAll("x-w17-scope-org_id")).toEqual(["o-person"]);
    expect(out.has("x-w17-scope-team_id")).toBe(false);
    expect(out.getAll("w17-tx-id")).toEqual(["tx"]);
    // A later forward cannot put the caller back.
    expect(principal.forwardToOutgoing(acted).outgoing.equals(out)).toBe(true);
    const decoded = msg("AuthResp").fromBinary(Buffer.from(out.first("x-w17-user")!, "base64"));
    expect(decoded.userId).toBe("person");
  });

  it.each([[{ "": "v" }], [{ org_id: "" }], [{ " org_id": "v" }]])("refuses malformed input (%j)", (scopes) => {
    expect(() => principal.actAs(new Context(), msg("AuthResp")({ userId: "u" }), scopes)).toThrow(TypeError);
    expect(() => principal.actAs(new Context(), msg("AuthResp")({ userId: "" }))).toThrow(TypeError);
  });
});

it("gateway-owned keys", () => {
  for (const k of ["x-w17-anything", "W17-LABEL-x", "w17-tx-id", "w17-conn-id"]) expect(principal.isGatewayOwnedKey(k)).toBe(true);
  for (const k of ["w17-org", "w17-tx-id-hint", "authorization"]) expect(principal.isGatewayOwnedKey(k)).toBe(false);
});

// --- paging.forward -----------------------------------------------------------

describe("paging.forward", () => {
  it("copies incoming onto outgoing only", () => {
    const ctx = new Context({
      incoming: { [paging.BOUNDARIES_MD_KEY]: "c", [paging.LIMIT_MD_KEY]: "25", [paging.TOTAL_MD_KEY]: "1", "x-w17-user": "e" },
    });
    const out = paging.forward(ctx).outgoing;
    expect(out.toObject()).toEqual({ [paging.BOUNDARIES_MD_KEY]: ["c"], [paging.LIMIT_MD_KEY]: ["25"], [paging.TOTAL_MD_KEY]: ["1"] });
    expect(ctx.outgoing.size).toBe(0); // the original context is untouched
    expect(paging.forwarded(paging.forward(ctx))).toBe(true);
    expect(paging.forwarded(ctx)).toBe(false);
  });

  it("replaces stale outgoing keys as a set", () => {
    const ctx = new Context({
      incoming: { [paging.LIMIT_MD_KEY]: "50" },
      outgoing: { [paging.BOUNDARIES_MD_KEY]: "stale", [paging.LIMIT_MD_KEY]: "10", "w17-language": "cs" },
    });
    const out = paging.forward(ctx).outgoing;
    expect(out.getAll(paging.LIMIT_MD_KEY)).toEqual(["50"]);
    expect(out.has(paging.BOUNDARIES_MD_KEY)).toBe(false);
    expect(out.getAll("w17-language")).toEqual(["cs"]);
  });

  it("does nothing when absent", () => {
    const ctx = new Context({ incoming: { "x-w17-user": "e" }, outgoing: { [paging.LIMIT_MD_KEY]: "10" } });
    expect(paging.forward(ctx)).toBe(ctx);
  });

  it("relays a forwarded call's position only", () => {
    const [ctx, sink] = paging.withResponseHeaders(new Context({ incoming: { [paging.LIMIT_MD_KEY]: "2" } }));
    const header = new Metadata({ [paging.NEXT_MD_KEY]: "n", [paging.PREV_MD_KEY]: "p", [paging.TOTAL_MD_KEY]: "3", other: "x" });
    paging.relayPosition(ctx, header);
    expect(sink.size).toBe(0);
    paging.relayPosition(paging.forward(ctx), header);
    expect(Object.fromEntries(sink)).toEqual({ [paging.NEXT_MD_KEY]: ["n"], [paging.PREV_MD_KEY]: ["p"], [paging.TOTAL_MD_KEY]: ["3"] });
  });
});

// --- the context itself ---------------------------------------------------------

describe("context", () => {
  it("deadline, cancel and withoutCancel", async () => {
    const parent = new Context().withCancel();
    const child = parent.withTimeout(50);
    expect(child.done()).toBe(false);
    const fired: string[] = [];
    child.onDone(() => fired.push(child.err()!));
    await new Promise((r) => setTimeout(r, 80));
    expect(child.err()).toBe("context deadline exceeded");
    expect(fired).toEqual(["context deadline exceeded"]);
    const c2 = parent.withCancel();
    parent.cancel();
    expect(c2.err()).toBe("context canceled");
    const free = c2.withoutCancel();
    expect([free.done(), free.deadline]).toEqual([false, undefined]);
    expect(child.withTimeout(10_000).deadline).toBe(child.deadline); // the earlier deadline wins
  });

  it("signal aborts when the context ends", () => {
    const ctx = new Context().withCancel();
    const s = ctx.signal;
    expect(s.aborted).toBe(false);
    ctx.cancel();
    expect(s.aborted).toBe(true);
  });
});
