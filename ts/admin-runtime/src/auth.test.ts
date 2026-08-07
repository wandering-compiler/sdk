import { afterEach, describe, expect, it, vi } from "vitest";
import { authHeader, clearToken, getToken, hasAllPermissions, setToken } from "./auth";

// The token helpers are the admin SPA's identity layer. Their contract
// is "never throw" — localStorage can be blocked (private mode, DOM
// sandbox), so every accessor swallows the error and degrades. These
// tests pin both the happy path (jsdom localStorage round-trips) and
// the swallow-and-degrade guards.

afterEach(() => {
  window.localStorage.clear();
  vi.restoreAllMocks();
});

describe("token storage round-trip", () => {
  it("set → get returns the stored token; clear removes it", () => {
    expect(getToken()).toBeNull();
    setToken("abc123");
    expect(getToken()).toBe("abc123");
    clearToken();
    expect(getToken()).toBeNull();
  });
});

describe("localStorage failure is swallowed (never throws)", () => {
  // jsdom's localStorage methods can't be spied in place (the spy
  // silently no-ops, so the happy path runs instead of the catch).
  // Replace the whole accessor with a throwing stub to actually drive
  // the swallow-and-degrade guards. `installThrowingStorage` returns a
  // restore fn.
  function installThrowingStorage(): () => void {
    const original = Object.getOwnPropertyDescriptor(window, "localStorage");
    const thrower = () => {
      throw new Error("blocked (private mode / sandbox)");
    };
    Object.defineProperty(window, "localStorage", {
      configurable: true,
      value: { getItem: thrower, setItem: thrower, removeItem: thrower },
    });
    return () => {
      if (original) Object.defineProperty(window, "localStorage", original);
    };
  }

  it("getToken returns null when getItem throws", () => {
    const restore = installThrowingStorage();
    try {
      expect(getToken()).toBeNull();
    } finally {
      restore();
    }
  });

  it("setToken does not throw when setItem throws", () => {
    const restore = installThrowingStorage();
    try {
      expect(() => setToken("x")).not.toThrow();
    } finally {
      restore();
    }
  });

  it("clearToken does not throw when removeItem throws", () => {
    const restore = installThrowingStorage();
    try {
      expect(() => clearToken()).not.toThrow();
    } finally {
      restore();
    }
  });
});

describe("authHeader", () => {
  it("returns a Bearer header when a token is present", () => {
    setToken("tok");
    expect(authHeader()).toBe("Bearer tok");
  });

  it("returns undefined when no token is stored", () => {
    expect(authHeader()).toBeUndefined();
  });
});

describe("hasAllPermissions", () => {
  it.each<{
    name: string;
    perms: number[] | null;
    req: number[] | null | undefined;
    want: boolean;
  }>([
    { name: "empty required → true (open endpoint)", perms: [], req: [], want: true },
    { name: "null required → true", perms: [1], req: null, want: true },
    { name: "undefined required → true", perms: [1], req: undefined, want: true },
    { name: "no perms but perms required → false", perms: [], req: [1], want: false },
    { name: "null perms but perms required → false", perms: null, req: [1], want: false },
    { name: "superset → true", perms: [1, 2, 3], req: [1, 3], want: true },
    { name: "missing one perm → false", perms: [1, 2], req: [1, 26], want: false },
    { name: "exact match → true", perms: [1], req: [1], want: true },
  ])("$name", ({ perms, req, want }) => {
    expect(hasAllPermissions(perms, req)).toBe(want);
  });
});
