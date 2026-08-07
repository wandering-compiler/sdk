// Token + identity helpers. Walking-skeleton iter-1 stores the
// token in localStorage; iter-2 may swap for httpOnly cookies
// once a CSRF strategy is picked.

import type { AdminPageSpec } from "./types";

const TOKEN_KEY = "w17_admin_token";

export function getToken(): string | null {
  try {
    return window.localStorage.getItem(TOKEN_KEY);
  } catch {
    return null;
  }
}

export function setToken(token: string): void {
  try {
    window.localStorage.setItem(TOKEN_KEY, token);
  } catch {
    // localStorage blocked (private mode / DOM sandbox) — the
    // SPA degrades to in-memory only; user re-logs in on refresh.
  }
}

export function clearToken(): void {
  try {
    window.localStorage.removeItem(TOKEN_KEY);
  } catch {
    /* ignore */
  }
}

// authHeader returns the Authorization header value to send on
// every API request, or undefined when the user hasn't logged
// in yet.
export function authHeader(): string | undefined {
  const t = getToken();
  return t ? `Bearer ${t}` : undefined;
}

// hasAllPermissions reports whether the user's permission IDs
// are a superset of `required`. Both sides are the numeric
// lock-allocated IDs (whoami's `permission_ids[]` vs a page/
// action's `required_permissions[]`) — never perm-name
// strings. Empty / unset `required` returns true — endpoints
// with no perms gated land here. Empty / unset `userPerms`
// returns true only when `required` is also empty (a user with
// no perms can only see fully-open endpoints).
//
// Used by REV-150 iter-3 perm-aware hiding. The backend
// handler still enforces independently — this drives UX
// only; a malicious client that bypasses the SPA still hits
// the server's permission gate (P21).
export function hasAllPermissions(
  userPerms: number[] | undefined | null,
  required: number[] | undefined | null,
): boolean {
  if (!required || required.length === 0) return true;
  if (!userPerms || userPerms.length === 0) return false;
  const have = new Set(userPerms);
  for (const r of required) {
    if (!have.has(r)) return false;
  }
  return true;
}

// pageVisibleTo reports whether a page should appear to the user
// in the nav AND on the overview tiles. A page is visible when
// either:
//   - it has no list view (overview-style / detail-only page —
//     always shown; reached via inlines or direct URL), OR
//   - whoami's permission_ids cover the list's required perms.
//
// Shared by App's nav filter and OverviewPage's default tiles so
// the two surfaces never disagree about what a user can see. The
// detail's own update/delete buttons gate themselves separately;
// the backend enforces independently (UX hint only).
export function pageVisibleTo(page: AdminPageSpec, userPerms: number[] | undefined): boolean {
  if (!page.list) return true;
  return hasAllPermissions(userPerms, page.list.required_permissions);
}
