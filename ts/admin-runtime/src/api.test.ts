import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  AdminApiError,
  apiDelete,
  apiGet,
  apiPatch,
  apiPost,
  displayString,
  formatTitle,
} from "./api";
import { setToken } from "./auth";

// The fetch wrapper threads the auth header, encodes the body, and
// decodes JSON-or-text into a typed result / AdminApiError. These tests
// stub global fetch and assert the request shape + every response-decode
// branch (ok/non-ok, JSON/text/empty body).

function mockFetch(status: number, bodyText: string) {
  const fn = vi.fn(() =>
    Promise.resolve(new Response(bodyText, { status, statusText: `HTTP ${status}` })),
  );
  vi.stubGlobal("fetch", fn);
  return fn;
}

// fetch's recorded call args, typed as [url, init] — the mock.calls
// tuple is `any[]` to TS, so narrow it once here instead of asserting
// at every call site.
function lastCall(fn: ReturnType<typeof mockFetch>): [string, RequestInit] {
  const calls = fn.mock.calls as unknown as Array<[string, RequestInit]>;
  return calls[calls.length - 1];
}

afterEach(() => {
  vi.unstubAllGlobals();
  window.localStorage.clear();
});

describe("apiCall request shape", () => {
  beforeEach(() => window.localStorage.clear());

  it("GET sends no body and JSON content-type, omits auth when logged out", async () => {
    const fetchFn = mockFetch(200, JSON.stringify({ ok: true }));
    const out = await apiGet<{ ok: boolean }>("/x");
    expect(out).toEqual({ ok: true });
    const [url, init] = lastCall(fetchFn);
    expect(url).toBe("/x");
    expect(init.method).toBe("GET");
    expect(init.body).toBeUndefined();
    expect((init.headers as Record<string, string>)["Content-Type"]).toBe("application/json");
    expect((init.headers as Record<string, string>)["Authorization"]).toBeUndefined();
  });

  it("attaches the Bearer header when a token is present", async () => {
    setToken("tok");
    const fetchFn = mockFetch(200, "");
    await apiGet("/me");
    const [, init] = lastCall(fetchFn);
    expect((init.headers as Record<string, string>)["Authorization"]).toBe("Bearer tok");
  });

  it.each([
    { fn: apiPost, method: "POST" },
    { fn: apiPatch, method: "PATCH" },
  ])("$method serializes the body as JSON", async ({ fn, method }) => {
    const fetchFn = mockFetch(200, "{}");
    await fn("/r", { a: 1 });
    const [, init] = lastCall(fetchFn);
    expect(init.method).toBe(method);
    expect(init.body).toBe(JSON.stringify({ a: 1 }));
  });

  it("DELETE sends no body", async () => {
    const fetchFn = mockFetch(200, "");
    await apiDelete("/r/1");
    const [, init] = lastCall(fetchFn);
    expect(init.method).toBe("DELETE");
    expect(init.body).toBeUndefined();
  });
});

describe("response decoding", () => {
  it("empty body → null", async () => {
    mockFetch(200, "");
    expect(await apiGet("/x")).toBeNull();
  });

  it("non-JSON body falls back to the raw text", async () => {
    mockFetch(200, "plain text");
    expect(await apiGet("/x")).toBe("plain text");
  });

  it("non-ok status throws AdminApiError carrying status + parsed body", async () => {
    mockFetch(404, JSON.stringify({ error: "nope" }));
    await expect(apiGet("/missing")).rejects.toMatchObject({
      status: 404,
      body: { error: "nope" },
    });
    // the thrown value is an AdminApiError instance with the formatted message
    try {
      mockFetch(500, "boom");
      await apiGet("/x");
      expect.unreachable();
    } catch (e) {
      expect(e).toBeInstanceOf(AdminApiError);
      expect((e as AdminApiError).message).toContain("HTTP 500");
      expect((e as AdminApiError).body).toBe("boom");
    }
  });
});

describe("formatTitle", () => {
  it("substitutes flat fields", () => {
    expect(
      formatTitle("{first_name} {last_name}", { first_name: "Ada", last_name: "Lovelace" }),
    ).toBe("Ada Lovelace");
  });

  it("walks dotted paths into nested objects", () => {
    expect(formatTitle("{account.name}", { account: { name: "Acme" } })).toBe("Acme");
  });

  it("missing field / broken path / null value → empty segment", () => {
    expect(formatTitle("[{missing}]", {})).toBe("[]");
    expect(formatTitle("[{a.b}]", { a: 5 })).toBe("[]"); // a is not an object
    expect(formatTitle("[{x}]", { x: null })).toBe("[]");
  });

  it("renders nested object values as JSON, not [object Object]", () => {
    expect(formatTitle("{meta}", { meta: { k: 1 } })).toBe('{"k":1}');
  });
});

describe("displayString", () => {
  it.each([
    { v: "hi", want: "hi" },
    { v: null, want: "" },
    { v: undefined, want: "" },
    { v: 42, want: "42" },
    { v: true, want: "true" },
    { v: { k: 1 }, want: '{"k":1}' },
    { v: [1, 2], want: "[1,2]" },
    { v: 10n, want: "10" },
  ])("renders $v", ({ v, want }) => {
    expect(displayString(v)).toBe(want);
  });
});
