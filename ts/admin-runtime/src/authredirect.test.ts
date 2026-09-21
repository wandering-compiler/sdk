import { afterEach, describe, expect, it } from "vitest";
import { clearToken, consumeRedirectToken, getToken } from "./auth";

// The last mile of a federated sign-in: the identity provider sent the
// browser back here and the gateway attached the session to the URL
// fragment. These pin what the SPA does with it.
//
// The fragment is also this SPA's ROUTER, which is why the order matters
// and why the token has to leave the URL rather than merely be read.

function setHash(h: string) {
  window.history.replaceState(null, "", "/admin/" + h);
}

afterEach(() => {
  clearToken();
  window.history.replaceState(null, "", "/admin/");
});

describe("consumeRedirectToken", () => {
  it("stores the token the gateway put in the fragment", () => {
    setHash("#token=ey.JWT.value");
    expect(consumeRedirectToken()).toBe(true);
    expect(getToken()).toBe("ey.JWT.value");
  });

  it("removes the credential from the URL", () => {
    setHash("#token=ey.JWT.value");
    consumeRedirectToken();
    // Left in place it would sit in the address bar, in the user's
    // history, and in anything they paste to a colleague.
    expect(window.location.hash).not.toContain("ey.JWT.value");
    expect(window.location.href).not.toContain("ey.JWT.value");
  });

  it("does not push a history entry — Back must not walk onto the credential", () => {
    setHash("#token=abc");
    const before = window.history.length;
    consumeRedirectToken();
    expect(window.history.length).toBe(before);
  });

  it("keeps anything else that rode along in the fragment", () => {
    setHash("#token=abc&next=%2Fusers");
    expect(consumeRedirectToken()).toBe(true);
    expect(getToken()).toBe("abc");
    expect(window.location.hash).toContain("next=");
    expect(window.location.hash).not.toContain("abc");
  });

  it("reads a non-default parameter name when asked", () => {
    setHash("#session=xyz");
    expect(consumeRedirectToken("session")).toBe(true);
    expect(getToken()).toBe("xyz");
  });

  describe("leaves an ordinary route alone", () => {
    // The collision this whole function exists to avoid: the router
    // uses the same slot. A route must survive untouched, and must not
    // be mistaken for a sign-in.
    for (const hash of ["#/", "#/list/users", "#/detail/users/42", "", "#"]) {
      it(JSON.stringify(hash), () => {
        setHash(hash);
        const before = window.location.hash;
        expect(consumeRedirectToken()).toBe(false);
        expect(getToken()).toBeNull();
        expect(window.location.hash).toBe(before);
      });
    }
  });

  it("ignores an empty token rather than storing one", () => {
    setHash("#token=");
    expect(consumeRedirectToken()).toBe(false);
    // An empty credential stored is worse than none: every later
    // request would send `Bearer ` and 401 with no clue why.
    expect(getToken()).toBeNull();
  });
});
