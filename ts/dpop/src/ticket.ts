// Stream tickets: a browser cannot put headers on `new WebSocket()` or
// `EventSource`, so it swaps a signed request for a one-shot ticket and
// appends that to the stream URL. On a DPoP gateway the ticket is bound to
// the thumbprint of the key that signed the request.

/** The gateway's default ticket issuer path. */
export const DEFAULT_TICKET_PATH = "/api/ws/ticket";

/** A one-shot stream ticket. */
export interface StreamTicket {
  ticket: string;
  /** Seconds the ticket stays redeemable, as the issuer reported. */
  expiresIn: number;
}

/** Options of {@link issueStreamTicket}. */
export interface StreamTicketOptions {
  /** The issuer path, appended to the base URL. Default {@link DEFAULT_TICKET_PATH}. */
  path?: string;
}

/** The issuer answered something other than a ticket. */
export class DpopTicketError extends Error {
  readonly status: number;
  constructor(message: string, status: number) {
    super(message);
    this.name = "DpopTicketError";
    this.status = status;
  }
}

/**
 * Asks the issuer for a ticket with a signed POST. `dpopFetch` is the
 * function {@link createDpopFetch} returned (any fetch works, but only that
 * one signs); `baseUrl` is the API base (`https://api.example.com` or with a
 * path prefix). A DEVICE_REVOKED or SESSION_EXPIRED refusal surfaces as the
 * wrapper throws it; any other non-2xx answer, or a body without a ticket,
 * throws {@link DpopTicketError}.
 */
export async function issueStreamTicket(
  dpopFetch: (input: string, init?: RequestInit) => Promise<Response>,
  baseUrl: string,
  opts: StreamTicketOptions = {},
): Promise<StreamTicket> {
  const path = opts.path ?? DEFAULT_TICKET_PATH;
  const url = baseUrl.replace(/\/+$/, "") + (path.startsWith("/") ? path : `/${path}`);
  const resp = await dpopFetch(url, { method: "POST" });
  if (!resp.ok) {
    // Not awaited: a tee'd body's cancel settles only with its sibling's
    // (createDpopFetch).
    void resp.body?.cancel().catch(() => undefined);
    throw new DpopTicketError(`dpop: ticket issuer answered HTTP ${resp.status}`, resp.status);
  }
  let body: { ticket?: unknown; expires_in?: unknown };
  try {
    body = (await resp.json()) as typeof body;
  } catch {
    throw new DpopTicketError("dpop: ticket issuer answered no JSON", resp.status);
  }
  if (typeof body?.ticket !== "string" || body.ticket === "") {
    throw new DpopTicketError("dpop: ticket issuer answered no ticket", resp.status);
  }
  const expiresIn = typeof body.expires_in === "number" ? body.expires_in : 0;
  return { ticket: body.ticket, expiresIn };
}

/** Appends `?<param>=<ticket>` (default `ticket`) to a stream URL, keeping its other parameters. */
export function withTicket(streamUrl: string, ticket: string, param = "ticket"): string {
  const u = new URL(streamUrl);
  u.searchParams.set(param, ticket);
  return u.toString();
}
