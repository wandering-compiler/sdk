import { describe, expect, it } from "vitest";

import { createDpopFetch, DpopRefusedError } from "../src/fetch";
import { MemoryKeyStore } from "../src/key";
import { accessTokenHash } from "../src/proof";
import { DEFAULT_TICKET_PATH, DpopTicketError, issueStreamTicket, withTicket } from "../src/ticket";
import { json, scriptedFetch, useNonce, verifyProof } from "./helpers";

const ORIGIN = "https://api.example.com";

function client(reply: Parameters<typeof scriptedFetch>[0]) {
  const server = scriptedFetch(reply);
  const f = createDpopFetch({
    origins: [ORIGIN],
    keyStore: new MemoryKeyStore(),
    getToken: () => "tok",
    fetch: server.fetch,
  });
  return { f, server };
}

describe("issueStreamTicket", () => {
  it("POSTs a signed, token-bound request to the default issuer path", async () => {
    const { f, server } = client(() => json(200, { ticket: "t-1", expires_in: 30 }));
    expect(await issueStreamTicket(f, `${ORIGIN}/`)).toEqual({ ticket: "t-1", expiresIn: 30 });
    const s = server.sent[0]!;
    expect(s.req.method).toBe("POST");
    expect(s.req.url).toBe(`${ORIGIN}${DEFAULT_TICKET_PATH}`);
    expect(s.req.headers.get("Authorization")).toBe("DPoP tok");
    expect(s.proof!.claims).toMatchObject({
      htm: "POST",
      htu: `${ORIGIN}/api/ws/ticket`,
      ath: await accessTokenHash("tok"),
    });
    expect(await verifyProof(s.proofRaw!)).toBe(true);
  });

  it("takes a configured path under a base with a prefix, and follows a nonce challenge", async () => {
    const { f, server } = client((_r, n) =>
      n === 1 ? useNonce("n-1") : json(200, { ticket: "t-2" }),
    );
    expect(await issueStreamTicket(f, `${ORIGIN}/app`, { path: "streams/ticket" })).toEqual({
      ticket: "t-2",
      expiresIn: 0,
    });
    expect(server.sent.map((s) => s.req.url)).toEqual([
      `${ORIGIN}/app/streams/ticket`,
      `${ORIGIN}/app/streams/ticket`,
    ]);
    expect(server.sent[1]!.proof!.claims.nonce).toBe("n-1");
  });

  it("throws DpopTicketError for a refusal or a body without a ticket", async () => {
    for (const reply of [
      () => json(401, { error: { code: "UNAUTHENTICATED" } }),
      () => json(200, { nope: true }),
      () => json(200, { ticket: "" }),
      () => new Response("not json", { status: 200 }),
    ]) {
      const { f } = client(reply);
      await expect(issueStreamTicket(f, ORIGIN)).rejects.toBeInstanceOf(DpopTicketError);
    }
  });

  it("lets a revoked device surface as DpopRefusedError", async () => {
    const { f } = client(() => json(401, { error: { details: [{ code: "DEVICE_REVOKED" }] } }));
    await expect(issueStreamTicket(f, ORIGIN)).rejects.toBeInstanceOf(DpopRefusedError);
  });
});

describe("withTicket", () => {
  it("sets the ticket parameter and keeps the others", () => {
    expect(withTicket("wss://api.example.com/w17-events?topic=a", "t+1")).toBe(
      "wss://api.example.com/w17-events?topic=a&ticket=t%2B1",
    );
    expect(withTicket("https://api.example.com/sse?ticket=old", "new", "ticket")).toBe(
      "https://api.example.com/sse?ticket=new",
    );
    expect(withTicket("https://api.example.com/sse", "x", "t")).toBe(
      "https://api.example.com/sse?t=x",
    );
  });
});
