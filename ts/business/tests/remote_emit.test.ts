// A business method reached over the wire inside a transaction ANOTHER process
// runs (its request carries that process's `w17-tx-id`, and no in-process
// scope is on its context) defers its event to that transaction's commit: it
// asks the coordinator (W17TransactionOutcome.Await) and publishes once on
// COMMITTED, never on NOT_COMMITTED. Over real grpc-js servers on loopback: a
// fake coordinator serving W17DistributedTransaction and W17TransactionOutcome,
// and the business service calling it.

import { Server, ServerCredentials } from "@grpc/grpc-js";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { Conn, ServiceClient, TxCoordinator, TxOutcome } from "../src/client.ts";
import { Context, type Metadata } from "../src/context.ts";
import type { AnyMessage } from "../src/descriptors.ts";
import * as distx from "../src/distx.ts";
import { Status, StatusError } from "../src/errors.ts";
import * as events from "../src/events.ts";
import type { Emit } from "../src/manifest.ts";
import { Registry } from "../src/server.ts";
import * as txscope from "../src/txscope.ts";
import { msg, pool, settled, sleep } from "./helpers.ts";

const EMIT: Emit = {
  method: "/conformance.v1.NoteService/CreateNote",
  channel: "notes",
  topic: "note.created",
  envelope: "conformance.events.Event",
  envelopeField: "v1_note_created",
  event: "conformance.v1.NoteCreated",
  fields: [
    { field: "id", source: "response", path: ["id"], value: "" },
    { field: "text", source: "request", path: ["text"], value: "" },
  ],
};

const DM = (name: string, init: Record<string, unknown> = {}) => pool().message("w17.common.distx." + name)(init);

const PENDING = 0;
const COMMITTED = 1;
const NOT_COMMITTED = 2;

interface Tx {
  state: "open" | "committed" | "not_committed";
  waiters: ((outcome: number) => void)[];
}

/**
 * The coordinator of the caller's process, as the storage bundle serves it:
 * Begin/Commit/Rollback, and Await answering PENDING once the waiter is
 * registered, then the end. A commit refused while `busy` (a call still holds
 * the transaction) settles nothing.
 */
class Coordinator {
  n = 0;
  txs = new Map<string, Tx>();
  busy = 0;
  /** What each Await call carried. */
  awaits: Metadata[] = [];
  /** "silent": never confirms the wait; "break": fails after PENDING. */
  mode: "normal" | "silent" | "break" = "normal";

  async Begin() {
    this.n++;
    const txId = `tx-${this.n}`;
    this.txs.set(txId, { state: "open", waiters: [] });
    return DM("BeginResponse", { txId });
  }

  #end(txId: string, state: "committed" | "not_committed") {
    const t = this.txs.get(txId)!;
    t.state = state;
    for (const w of t.waiters) w(state === "committed" ? COMMITTED : NOT_COMMITTED);
    t.waiters = [];
  }

  async Commit(_ctx: Context, req: AnyMessage) {
    if (this.busy > 0) {
      this.busy--;
      throw new StatusError(Status.FAILED_PRECONDITION, "distx: commit refused: a call still holds it");
    }
    this.#end(req.txId, "committed");
    return DM("CommitResponse");
  }

  async Rollback(_ctx: Context, req: AnyMessage) {
    this.#end(req.txId, "not_committed");
    return DM("RollbackResponse");
  }

  async *Await(ctx: Context, req: AnyMessage) {
    this.awaits.push(ctx.incoming);
    const t = this.txs.get(req.txId);
    if (t === undefined) throw new StatusError(Status.NOT_FOUND, `await: unknown transaction id "${req.txId}"`);
    if (t.state !== "open") {
      yield DM("AwaitResponse", { outcome: t.state === "committed" ? COMMITTED : NOT_COMMITTED });
      return;
    }
    if (this.mode === "silent") {
      await new Promise<void>((r) => ctx.onDone(r));
      return;
    }
    const ended = new Promise<number>((r) => t.waiters.push(r));
    yield DM("AwaitResponse", { outcome: PENDING });
    if (this.mode === "break") throw new StatusError(Status.INTERNAL, "the coordinator went away");
    yield DM("AwaitResponse", { outcome: await ended });
  }
}

class Notes {
  async CreateNote(_ctx: Context, req: AnyMessage) {
    return msg("Note")({ id: 7n, text: req.text });
  }
  async CreateNotes(): Promise<AnyMessage> {
    throw new StatusError(Status.UNIMPLEMENTED, "not here");
  }
  async ListNotes(): Promise<AnyMessage> {
    throw new StatusError(Status.UNIMPLEMENTED, "not here");
  }
  async Inspect(): Promise<AnyMessage> {
    throw new StatusError(Status.UNIMPLEMENTED, "not here");
  }
  async Refuse(): Promise<AnyMessage> {
    throw new StatusError(Status.UNIMPLEMENTED, "not here");
  }
  async Sleep(): Promise<AnyMessage> {
    throw new StatusError(Status.UNIMPLEMENTED, "not here");
  }
  async TxErrors(): Promise<AnyMessage> {
    throw new StatusError(Status.UNIMPLEMENTED, "not here");
  }
  async Whoami(): Promise<AnyMessage> {
    throw new StatusError(Status.UNIMPLEMENTED, "not here");
  }
  async Echo(): Promise<AnyMessage> {
    throw new StatusError(Status.UNIMPLEMENTED, "not here");
  }
  async AttachFile(): Promise<AnyMessage> {
    throw new StatusError(Status.UNIMPLEMENTED, "not here");
  }
}

async function listen(s: Server): Promise<number> {
  return new Promise<number>((res, rej) =>
    s.bindAsync("127.0.0.1:0", ServerCredentials.createInsecure(), (e, p) => (e ? rej(e) : res(p))),
  );
}

const stop = (s: Server) => new Promise<void>((r) => s.tryShutdown(() => r()));

/** A business tier whose coordinator is at `coordConn`, and a client of it. */
async function businessTier(txOutcome: TxOutcome | undefined) {
  const pub = new events.MemoryPublisher();
  const reg = new Registry(pool(), { emits: [EMIT], dispatcher: pub, ...(txOutcome !== undefined && { txOutcome }) });
  reg.register("conformance.v1.NoteService", new Notes());
  const srv = new Server();
  reg.addTo(srv);
  const conn = new Conn(`127.0.0.1:${await listen(srv)}`, { tls: false });
  return { pub, srv, conn, client: new ServiceClient(conn, "conformance.v1.NoteService", pool()) };
}

let coord: Coordinator;
let coordSrv: Server;
let coordConn: Conn;
let caller: TxCoordinator;
let biz: Awaited<ReturnType<typeof businessTier>>;
let reported: string[];

beforeAll(async () => {
  coord = new Coordinator();
  const reg = new Registry(pool());
  reg.register("w17.common.distx.W17DistributedTransaction", coord);
  reg.register("w17.common.distx.W17TransactionOutcome", coord);
  coordSrv = new Server();
  reg.addTo(coordSrv);
  coordConn = new Conn(`127.0.0.1:${await listen(coordSrv)}`, { tls: false });
  caller = new TxCoordinator(coordConn);
  biz = await businessTier(new TxOutcome(coordConn));
});

afterAll(async () => {
  biz.conn.close();
  coordConn.close();
  await Promise.all([stop(biz.srv), stop(coordSrv)]);
});

let restoreWait: (() => void) | undefined;

afterEach(() => {
  coord.busy = 0;
  coord.mode = "normal";
  coord.awaits = [];
  biz.pub.published.length = 0;
  restoreWait?.();
  restoreWait = undefined;
  vi.restoreAllMocks();
});

function captureReports() {
  reported = [];
  vi.spyOn(console, "error").mockImplementation((...a: unknown[]) => void reported.push(a.join(" ")));
}

const create = (ctx: Context, text = "remote") => biz.client.CreateNote(ctx, msg("CreateNoteRequest")({ text })) as Promise<AnyMessage>;

/** The caller's process: a transaction begun on the coordinator, its context carrying `w17-tx-id`. */
async function callerTx(): Promise<[distx.TxHandle, Context]> {
  return distx.begin(new Context(), caller, distx.beginRequest("main"));
}

describe("a method reached inside another process's transaction", () => {
  it("publishes its event once, when that transaction commits, and not before", async () => {
    captureReports();
    const [h, txCtx] = await callerTx();
    const note = await create(txCtx);
    expect(note.text).toBe("remote");
    await sleep(150);
    expect(biz.pub.published.length).toBe(0); // answered, the transaction still open
    expect(txscope.pendingCount()).toBeGreaterThan(0); // a shutdown drain waits for it
    await h.commit(new Context());
    expect(await settled(() => biz.pub.published.length === 1)).toBe(true);
    await sleep(100);
    expect(biz.pub.published.length).toBe(1);
    expect(biz.pub.published[0]![1]).toBe("note.created");
    expect(reported).toEqual([]);
  });

  it("drops its event when that transaction rolls back, silently", async () => {
    captureReports();
    const [h, txCtx] = await callerTx();
    await create(txCtx);
    await h.rollback(new Context());
    await sleep(200);
    expect(biz.pub.published.length).toBe(0);
    expect(reported).toEqual([]);
  });

  it("a commit refused while a call holds the transaction publishes nothing; the retried commit publishes once", async () => {
    captureReports();
    const [h, txCtx] = await callerTx();
    await create(txCtx);
    coord.busy = 1;
    await expect(h.commit(new Context())).rejects.toMatchObject({ code: Status.FAILED_PRECONDITION });
    await sleep(150);
    expect(biz.pub.published.length).toBe(0);
    await h.commit(new Context());
    expect(await settled(() => biz.pub.published.length === 1)).toBe(true);
    await sleep(100);
    expect(biz.pub.published.length).toBe(1);
  });

  it("a transaction that already committed publishes at once", async () => {
    const [h, txCtx] = await callerTx();
    await h.commit(new Context());
    await create(txCtx);
    expect(biz.pub.published.length).toBe(1); // before the method answered
  });

  it("a transaction that already ended without committing drops it, reported", async () => {
    captureReports();
    const [h, txCtx] = await callerTx();
    await h.rollback(new Context());
    await create(txCtx);
    await sleep(100);
    expect(biz.pub.published.length).toBe(0);
    expect(reported.some((r) => r.includes(
      `distx: dropped the deferred announcement of tx "${h.txId}": the transaction ended without committing before the emit registered`,
    ))).toBe(true);
  });

  it("an id the coordinator does not know drops it, reported", async () => {
    captureReports();
    await create(new Context({ outgoing: { "w17-tx-id": "tx-nobody" } }));
    await sleep(100);
    expect(biz.pub.published.length).toBe(0);
    expect(reported.some((r) => r.includes(
      `distx: dropped the deferred announcement of tx "tx-nobody": its coordinator could not be asked how the transaction ends (`,
    ) && r.includes("unknown transaction id"))).toBe(true);
  });

  it("a coordinator that does not confirm the wait in time drops it, reported", async () => {
    captureReports();
    restoreWait = distx.setRegisterWaitMs(100);
    coord.mode = "silent";
    const [h, txCtx] = await callerTx();
    await create(txCtx);
    await h.commit(new Context());
    await sleep(150);
    expect(biz.pub.published.length).toBe(0);
    expect(reported.some((r) => r.includes(
      `distx: dropped the deferred announcement of tx "${h.txId}": its coordinator could not be asked how the transaction ends (the coordinator did not confirm the wait within 100ms)`,
    ))).toBe(true);
  });

  it("a coordinator that stops answering mid-wait drops it, reported", async () => {
    captureReports();
    coord.mode = "break";
    const [h, txCtx] = await callerTx();
    await create(txCtx);
    expect(await settled(() => reported.some((r) => r.includes(
      `distx: dropped the deferred announcement of tx "${h.txId}": the coordinator stopped answering before the transaction ended (`,
    ) && r.includes("so whether it committed is unknown")))).toBe(true);
    await h.commit(new Context());
    await sleep(100);
    expect(biz.pub.published.length).toBe(0);
  });

  it("a wait the shutdown drain abandons is cancelled: it reports the drop and never publishes", async () => {
    captureReports();
    const [h, txCtx] = await callerTx();
    await create(txCtx);
    expect(await txscope.drain(50)).toBe(false); // the transaction is still open
    txscope.abandonPending();
    expect(distx.cancelRemoteWaits()).toBe(1);
    expect(await settled(() => reported.some((r) => r.includes(
      `distx: dropped the deferred announcement of tx "${h.txId}": the coordinator stopped answering before the transaction ended (`,
    )))).toBe(true);
    // The commit lands after the shutdown closed the dispatcher: nothing goes out.
    await h.commit(new Context());
    await sleep(150);
    expect(biz.pub.published.length).toBe(0);
    expect(distx.cancelRemoteWaits()).toBe(0);
  });

  it("echoes only the request's w17-conn-id on the Await call", async () => {
    const [h, txCtx] = await callerTx();
    const ctx = txCtx.withOutgoing(
      txCtx.outgoing.merged({ "w17-conn-id": ["c-7"], authorization: ["Bearer secret"], "x-custom": ["v"], "w17-language": ["cs"] }),
    );
    await create(ctx);
    await create(txCtx); // no routing token on this one
    await h.commit(new Context());
    expect(await settled(() => biz.pub.published.length === 2)).toBe(true);
    const sent = coord.awaits.map((md) =>
      Object.fromEntries([...md].filter(([k]) => !/^(user-agent|grpc-|content-type$|te$|:)/.test(k))),
    );
    expect(sent).toEqual([{ "w17-conn-id": ["c-7"] }, {}]);
  });
});

describe("where the remote wait does not apply", () => {
  it("a coordinator without W17TransactionOutcome: published now, reported", async () => {
    captureReports();
    const bare = new Server(); // serves nothing: every call is UNIMPLEMENTED
    const bareConn = new Conn(`127.0.0.1:${await listen(bare)}`, { tls: false });
    const old = await businessTier(new TxOutcome(bareConn));
    try {
      await old.client.CreateNote(new Context({ outgoing: { "w17-tx-id": "tx-old" } }), msg("CreateNoteRequest")({ text: "x" }));
      expect(old.pub.published.length).toBe(1);
      expect(reported.some((r) => r.includes(
        `distx: announcing inside transaction "tx-old" before it commits: its coordinator does not serve W17TransactionOutcome (upgrade the storage bundle), so the event cannot wait for the commit`,
      ))).toBe(true);
    } finally {
      old.conn.close();
      bareConn.close();
      await Promise.all([stop(old.srv), stop(bare)]);
    }
  });

  it("a tier with no coordinator configured: published now, nothing reported", async () => {
    captureReports();
    const none = await businessTier(undefined);
    try {
      await none.client.CreateNote(new Context({ outgoing: { "w17-tx-id": "tx-x" } }), msg("CreateNoteRequest")({ text: "x" }));
      expect(none.pub.published.length).toBe(1);
      expect(reported).toEqual([]);
    } finally {
      none.conn.close();
      await stop(none.srv);
    }
  });

  it("no w17-tx-id on the request: published now, the coordinator not asked", async () => {
    await create(new Context());
    expect(biz.pub.published.length).toBe(1);
    expect(coord.awaits).toEqual([]);
  });

  it("an in-process transaction still parks on its own scope, not the coordinator", async () => {
    const reg = new Registry(pool(), { emits: [EMIT], dispatcher: new events.MemoryPublisher(), txOutcome: new TxOutcome(coordConn) });
    const pub = reg.dispatcher as events.MemoryPublisher;
    reg.register("conformance.v1.NoteService", new Notes());
    const [h, txCtx] = await callerTx();
    // The incoming side names the transaction too, as a method composed
    // inside a run that itself was reached with the caller's id.
    const ctx = txCtx.withIncoming({ "w17-tx-id": h.txId });
    await reg.self("NoteService").CreateNote(ctx, msg("CreateNoteRequest")({ text: "local" }));
    expect(pub.published.length).toBe(0);
    await h.commit(new Context());
    expect(pub.published.length).toBe(1);
    expect(coord.awaits).toEqual([]);
  });
});
