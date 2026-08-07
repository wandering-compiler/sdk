import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  AdminApiError,
  apiDelete,
  apiGet,
  apiPatch,
  apiPost,
  displayString,
  formatTitle,
  readErrorEnvelope,
  serverMessage,
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

describe("readErrorEnvelope", () => {
  it("decodes the canonical nested envelope", () => {
    expect(
      readErrorEnvelope({ error: { code: "PERMISSION_DENIED", message: "missing users.delete" } }),
    ).toEqual({ code: "PERMISSION_DENIED", message: "missing users.delete", details: [] });
  });

  it("accepts a flat {code,message} and a bare {error:string}", () => {
    expect(readErrorEnvelope({ code: "NOT_FOUND", message: "no such user" })).toMatchObject({
      code: "NOT_FOUND",
      message: "no such user",
    });
    expect(readErrorEnvelope({ error: "nope" })).toEqual({
      code: "",
      message: "nope",
      details: [],
    });
  });

  it("decodes per-field details, dropping empty entries", () => {
    const env = readErrorEnvelope({
      error: {
        code: "INVALID_ARGUMENT",
        message: "validation failed",
        details: [
          { field: "email", code: "REQUIRED", message: "must be set" },
          { field: "", message: "" },
          null,
          { message: "unattributed" },
        ],
      },
    });
    expect(env.details).toEqual([
      { field: "email", code: "REQUIRED", message: "must be set" },
      { field: "", message: "unattributed" },
    ]);
  });

  it("reads nothing out of a non-envelope body", () => {
    for (const body of [null, "<html>502 Bad Gateway</html>", 7, [], { other: 1 }]) {
      expect(readErrorEnvelope(body)).toEqual({ code: "", message: "", details: [] });
    }
  });
});

describe("serverMessage", () => {
  it("returns the top-level message alone when there are no details", () => {
    expect(serverMessage({ error: { code: "UNAUTHENTICATED", message: "bad password" } })).toBe(
      "bad password",
    );
  });

  it("appends field details in parentheses", () => {
    expect(
      serverMessage({
        error: {
          message: "validation failed",
          details: [
            { field: "email", message: "must be set" },
            { field: "age", message: "must be positive" },
          ],
        },
      }),
    ).toBe("validation failed (email: must be set, age: must be positive)");
  });

  it("renders details alone when the top-level message is absent", () => {
    expect(
      serverMessage({ error: { details: [{ field: "email", message: "must be set" }] } }),
    ).toBe("email: must be set");
  });

  it("renders an unattributed detail without a field prefix", () => {
    expect(
      serverMessage({
        error: { message: "rejected", details: [{ message: "at least one field required" }] },
      }),
    ).toBe("rejected (at least one field required)");
  });

  it("is empty for a body with no message, so callers can fall back", () => {
    expect(serverMessage("<html>502</html>")).toBe("");
    expect(serverMessage(null)).toBe("");
  });
});

describe("AdminApiError message", () => {
  it("carries the server's reason, not the status line", async () => {
    mockFetch(403, JSON.stringify({ error: { code: "PERMISSION_DENIED", message: "forbidden" } }));
    await expect(apiGet("/admin/users")).rejects.toMatchObject({
      status: 403,
      message: "forbidden",
    });
  });

  it("includes validation details so a rejected form is not opaque", async () => {
    mockFetch(
      400,
      JSON.stringify({
        error: {
          code: "INVALID_ARGUMENT",
          message: "validation failed",
          details: [{ field: "email", message: "must be a valid address" }],
        },
      }),
    );
    await expect(apiPost("/admin/users", {})).rejects.toMatchObject({
      message: "validation failed (email: must be a valid address)",
    });
  });

  it("falls back to the status line when the body has no message", async () => {
    mockFetch(502, "<html>Bad Gateway</html>");
    await expect(apiGet("/admin/users")).rejects.toMatchObject({
      status: 502,
      message: "GET /admin/users failed: HTTP 502",
    });
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
