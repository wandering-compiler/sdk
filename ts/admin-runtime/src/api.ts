// fetch wrapper — threads the auth header + decodes JSON or
// raises a typed AdminApiError. Walking-skeleton iter-1
// shape; iter-2 swaps for the REV-141 generated client when
// the admin runtime + client integration lands.

import { authHeader } from "./auth";

export class AdminApiError extends Error {
  status: number;
  body: unknown;
  constructor(status: number, body: unknown, message: string) {
    super(message);
    this.status = status;
    this.body = body;
  }
}

export async function apiGet<T>(endpoint: string): Promise<T> {
  return apiCall<T>("GET", endpoint);
}

export async function apiPost<T>(endpoint: string, body: unknown): Promise<T> {
  return apiCall<T>("POST", endpoint, body);
}

export async function apiPatch<T>(endpoint: string, body: unknown): Promise<T> {
  return apiCall<T>("PATCH", endpoint, body);
}

export async function apiDelete<T>(endpoint: string): Promise<T> {
  return apiCall<T>("DELETE", endpoint);
}

async function apiCall<T>(method: string, endpoint: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
  };
  const auth = authHeader();
  if (auth) {
    headers["Authorization"] = auth;
  }

  const res = await fetch(endpoint, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });

  let parsed: unknown = null;
  const text = await res.text();
  if (text.length > 0) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = text;
    }
  }

  if (!res.ok) {
    throw new AdminApiError(res.status, parsed, `${method} ${endpoint} failed: HTTP ${res.status}`);
  }
  return parsed as T;
}

// formatTitle resolves a title format string against a record.
// `"{first_name} {last_name}"` + `{first_name: "Ada", last_name:
// "Lovelace"}` → `"Ada Lovelace"`. Missing fields render as the
// empty string (per docs/specs/admin/pages.md §Title resolution).
export function formatTitle(template: string, row: Record<string, unknown>): string {
  return template.replace(/\{([^}]+)\}/g, (_, ref: string) => {
    const path = ref.split(".");
    let cur: unknown = row;
    for (const p of path) {
      if (cur && typeof cur === "object" && p in (cur as Record<string, unknown>)) {
        cur = (cur as Record<string, unknown>)[p];
      } else {
        return "";
      }
    }
    if (cur == null) return "";
    return displayString(cur);
  });
}

/**
 * displayString renders an unknown wire value for UI display: null/undefined →
 * "", objects/arrays → JSON (never "[object Object]"), primitives → String().
 * Centralises the per-component renderCell helpers + ad-hoc String() coercions.
 */
export function displayString(v: unknown): string {
  switch (typeof v) {
    case "string":
      return v;
    case "object":
      return v === null ? "" : JSON.stringify(v);
    case "undefined":
      return "";
    default:
      // number | boolean | bigint | symbol | function — never "[object Object]".
      return String(v);
  }
}
