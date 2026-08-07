// fetch wrapper — threads the auth header + decodes JSON or
// raises a typed AdminApiError. Walking-skeleton iter-1
// shape; iter-2 swaps for the REV-141 generated client when
// the admin runtime + client integration lands.

import { authHeader } from "./auth";
import { MIME_PROTOBUF, codecFor } from "./wire";
import type { WireCodec } from "./wire";

export class AdminApiError extends Error {
  status: number;
  body: unknown;
  constructor(status: number, body: unknown, message: string) {
    super(message);
    this.status = status;
    this.body = body;
  }
}

/**
 * A per-field validation failure, from the error envelope's `details` array.
 * Mirrors restgw's `FieldError`.
 */
export interface AdminFieldError {
  field: string;
  code?: string;
  message: string;
}

/** The decoded error envelope. Absent parts read as "" / []. */
export interface AdminErrorEnvelope {
  code: string;
  message: string;
  details: AdminFieldError[];
}

// readErrorEnvelope decodes the `{error:{code,message,details}}` body that
// restgw writes for EVERY failed admin call (sdk/go/lib/restgw errorEnvelope).
// Three shapes are accepted so a host that re-maps the admin API through its
// own error envelope degrades instead of breaking: the canonical nested one, a
// flat `{code,message}`, and `{error:"text"}` with a bare string.
//
// A body that is not one of those — a proxy's HTML page, a raw text 502 —
// yields empty strings deliberately. Such a body is not the server's message
// and must not be pasted into the UI; callers fall back to the status line.
export function readErrorEnvelope(body: unknown): AdminErrorEnvelope {
  const outer = body as Record<string, unknown> | null | undefined;
  const wrapped = outer?.error;
  if (typeof wrapped === "string") {
    return { code: "", message: wrapped, details: [] };
  }
  const inner = (wrapped ?? outer) as Record<string, unknown> | null | undefined;
  return {
    code: typeof inner?.code === "string" ? inner.code : "",
    message: typeof inner?.message === "string" ? inner.message : "",
    details: readFieldErrors(inner?.details),
  };
}

function readFieldErrors(raw: unknown): AdminFieldError[] {
  if (!Array.isArray(raw)) return [];
  const out: AdminFieldError[] = [];
  for (const entry of raw) {
    const d = entry as Record<string, unknown> | null;
    const message = typeof d?.message === "string" ? d.message : "";
    const field = typeof d?.field === "string" ? d.field : "";
    if (message === "" && field === "") continue;
    out.push({
      field,
      message,
      ...(typeof d?.code === "string" ? { code: d.code } : {}),
    });
  }
  return out;
}

/**
 * The request body as bytes or JSON text.
 *
 * `undefined` stays `undefined` — a GET/DELETE has no body, and sending an
 * empty one changes what the server sees.
 */
function encodeBody(body: unknown, codec: WireCodec | undefined): BodyInit | undefined {
  if (body === undefined) return undefined;
  if (!codec) return JSON.stringify(body);
  const bytes = codec.encode(body);
  // Copy into a fresh ArrayBuffer: a codec may hand back a view over a larger
  // pooled buffer, and fetch would send the whole thing.
  return bytes.slice().buffer;
}

/**
 * The response body, decoded by whatever the server actually sent.
 *
 * The content type decides, NOT what was asked for: a server that ignored the
 * `Accept` (or a proxy that rewrote it) still has to be read correctly. And
 * ERRORS are always JSON — `restgw.WriteError` does not negotiate — so an
 * error envelope parses on the JSON path even on a protobuf-negotiated call.
 * That is why this checks the header rather than the codec.
 */
async function decodeBody(res: Response, codec: WireCodec | undefined): Promise<unknown> {
  const contentType = res.headers.get("Content-Type") ?? "";
  if (codec && contentType.includes(MIME_PROTOBUF)) {
    const buf = await res.arrayBuffer();
    if (buf.byteLength === 0) return null;
    try {
      return codec.decode(new Uint8Array(buf));
    } catch {
      // A payload the codec cannot read is not something to render as a row.
      // Surface it the way a malformed JSON body is surfaced: as the failure
      // it is, with the status the caller already checks.
      return null;
    }
  }
  const text = await res.text();
  if (text.length === 0) return null;
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

/**
 * serverMessage renders a failed call's envelope as one human-readable line —
 * the top-level message, with any per-field `details` appended as
 * `"message (field: reason, field2: reason2)"`. Returns "" when the body
 * carries no usable message, which is the caller's signal to fall back.
 */
export function serverMessage(body: unknown): string {
  const { message, details } = readErrorEnvelope(body);
  // Never empty: readFieldErrors drops an entry only when BOTH parts are
  // missing, so each surviving detail renders to something.
  const fields = details.map((d) => (d.field === "" ? d.message : `${d.field}: ${d.message}`));
  if (fields.length === 0) return message;
  const joined = fields.join(", ");
  return message === "" ? joined : `${message} (${joined})`;
}

/**
 * `wire` names the message refs this call carries, so the binary wire can be
 * used when a codec exists for them. Omitting it (or naming a ref with no
 * codec) falls back to JSON for that call alone — the console does not need
 * every endpoint to be codec-backed before any of them can be.
 */
export interface WireRefs {
  request?: string;
  response?: string;
}

export async function apiGet<T>(endpoint: string, wire?: WireRefs): Promise<T> {
  return apiCall<T>("GET", endpoint, undefined, wire);
}

export async function apiPost<T>(endpoint: string, body: unknown, wire?: WireRefs): Promise<T> {
  return apiCall<T>("POST", endpoint, body, wire);
}

export async function apiPatch<T>(endpoint: string, body: unknown, wire?: WireRefs): Promise<T> {
  return apiCall<T>("PATCH", endpoint, body, wire);
}

export async function apiDelete<T>(endpoint: string, wire?: WireRefs): Promise<T> {
  return apiCall<T>("DELETE", endpoint, undefined, wire);
}

async function apiCall<T>(
  method: string,
  endpoint: string,
  body?: unknown,
  wire?: WireRefs,
): Promise<T> {
  // Per-CALL negotiation, not per-console: a codec exists for a message ref or
  // it does not, and an endpoint whose request or response is a well-known
  // type has none. Deciding here means one such endpoint does not force the
  // whole console back onto JSON.
  const reqCodec = body === undefined ? undefined : codecFor(wire?.request);
  const respCodec = codecFor(wire?.response);

  // Content-Type stays on every request, body or not — that is what this
  // surface has always sent, and narrowing it to bodied requests would be a
  // behaviour change this feature has no reason to make.
  const headers: Record<string, string> = {
    "Content-Type": reqCodec ? MIME_PROTOBUF : "application/json",
  };
  // Ask for protobuf only when this call can actually decode it. An `Accept`
  // the caller cannot honour would get exactly what it asked for.
  if (respCodec) {
    headers["Accept"] = MIME_PROTOBUF;
  }
  const auth = authHeader();
  if (auth) {
    headers["Authorization"] = auth;
  }

  const res = await fetch(endpoint, {
    method,
    headers,
    body: encodeBody(body, reqCodec),
  });

  const parsed = await decodeBody(res, respCodec);

  if (!res.ok) {
    // Prefer the SERVER's reason — every admin error is a
    // `{error:{code,message}}` envelope, and "permission denied on
    // users.delete" tells the operator what "HTTP 403" cannot. The status line
    // stays as the fallback for a body with no message in it (a proxy page, an
    // empty response), where it is all we know.
    const reason = serverMessage(parsed);
    const message = reason !== "" ? reason : `${method} ${endpoint} failed: HTTP ${res.status}`;
    throw new AdminApiError(res.status, parsed, message);
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
