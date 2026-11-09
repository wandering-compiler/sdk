import type { Message } from "@bufbuild/protobuf";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Context, Metadata } from "../src/context.ts";
import { type AnyMessage, getDefaultPool } from "../src/descriptors.ts";
import * as distx from "../src/distx.ts";
import { Status, StatusError } from "../src/errors.ts";
import * as txscope from "../src/txscope.ts";
import { settled, sleep } from "./helpers.ts";

const M = (name: string, init: Record<string, unknown> = {}) => getDefaultPool().message("w17.common.distx." + name)(init);

/**
 * A coordinator whose calls can each be made to fail, recording their order
 * and the routing headers each carried. Like a real gRPC client it REFUSES a
 * call on a done context — the property the detached rollback exists for.
 */
class ScriptedTx implements distx.TxClient {
  calls: string[] = [];
  headers: Metadata[] = [];
  n = 0;
  connId: string;
  commitErr: unknown;
  rollbackErr: unknown;
  beginErr: unknown;
  commitHook: (() => Promise<void>) | undefined;
  rollbackHook: (() => Promise<void>) | undefined;

  constructor(o: { connId?: string; commitErr?: unknown; rollbackErr?: unknown; beginErr?: unknown } = {}) {
    this.connId = o.connId ?? "";
    this.commitErr = o.commitErr;
    this.rollbackErr = o.rollbackErr;
    this.beginErr = o.beginErr;
  }

  #check(ctx: Context): void {
    if (ctx.done()) throw new StatusError(Status.CANCELLED, ctx.err()!);
  }

  async begin(ctx: Context, _req: Message): Promise<{ reply: AnyMessage; header: Metadata }> {
    this.#check(ctx);
    this.calls.push("begin");
    if (this.beginErr) throw this.beginErr;
    this.n++;
    const header = new Metadata(this.connId ? { [distx.CONN_ID_HEADER]: this.connId } : undefined);
    return { reply: M("BeginResponse", { txId: `tx-${this.n}` }), header };
  }

  async commit(ctx: Context, _req: Message): Promise<unknown> {
    this.#check(ctx);
    this.calls.push("commit");
    this.headers.push(ctx.outgoing);
    await this.commitHook?.();
    if (this.commitErr) throw this.commitErr;
    return M("CommitResponse");
  }

  async rollback(ctx: Context, _req: Message): Promise<unknown> {
    this.#check(ctx);
    await this.rollbackHook?.();
    this.calls.push("rollback");
    this.headers.push(ctx.outgoing);
    if (this.rollbackErr) throw this.rollbackErr;
    return M("RollbackResponse");
  }

  seq(): string {
    return this.calls.join(",");
  }
}

const notes = (e: unknown) => (e as { notes?: string[] }).notes ?? [];

async function caught(p: Promise<unknown>): Promise<unknown> {
  try {
    await p;
  } catch (e) {
    return e;
  }
  throw new Error("expected a rejection");
}

afterEach(() => vi.restoreAllMocks());

describe("distx.run", () => {
  it("commits and returns the result", async () => {
    const tx = new ScriptedTx();
    let seen: readonly string[] = [];
    const r = await distx.run(new Context(), tx, distx.beginRequest("main"), (ctx) => {
      seen = ctx.outgoing.getAll(distx.TX_ID_HEADER);
      return 42;
    });
    expect(r).toBe(42);
    expect(tx.seq()).toBe("begin,commit");
    expect(seen).toEqual(["tx-1"]);
  });

  it("rolls back and rethrows", async () => {
    const tx = new ScriptedTx();
    await expect(distx.run(new Context(), tx, null, () => Promise.reject(new Error("boom")))).rejects.toThrow("boom");
    expect(tx.seq()).toBe("begin,rollback");
  });

  it("names a failed rollback on the function's error", async () => {
    const tx = new ScriptedTx({ rollbackErr: new StatusError(Status.INTERNAL, "db gone") });
    const e = await caught(distx.run(new Context(), tx, null, () => Promise.reject(new Error("boom"))));
    expect((e as Error).message).toBe("boom");
    expect(notes(e).some((n) => n.includes("rollback also failed"))).toBe(true);
  });

  it("the rollback is detached from a cancelled caller", async () => {
    const tx = new ScriptedTx();
    const ctx = new Context().withCancel();
    await expect(
      distx.run(ctx, tx, null, () => {
        ctx.cancel(); // the client hung up mid-transaction
        throw new Error("gone");
      }),
    ).rejects.toThrow("gone");
    expect(tx.seq()).toBe("begin,rollback"); // the rollback reached the server although the caller is gone
  });

  it("a done context refuses to commit", async () => {
    const tx = new ScriptedTx();
    const ctx = new Context().withCancel();
    await expect(distx.run(ctx, tx, null, () => void ctx.cancel())).rejects.toThrow(/not committing/);
    expect(tx.seq()).toBe("begin,rollback");
  });

  it("a commit failure is thrown and not rolled back", async () => {
    const tx = new ScriptedTx({ commitErr: new StatusError(Status.ABORTED, "serialization failure") });
    const e = await caught(distx.run(new Context(), tx, null, () => 1));
    expect((e as StatusError).code).toBe(Status.ABORTED); // surfaced: the caller retries the unit of work
    expect(tx.seq()).toBe("begin,commit");
    expect(notes(e)).toContain("distx.run: commit");
  });

  it("a begin failure does not call fn", async () => {
    const tx = new ScriptedTx({ beginErr: new StatusError(Status.UNAVAILABLE, "down") });
    let called = false;
    await expect(distx.run(new Context(), tx, null, () => void (called = true))).rejects.toThrow(StatusError);
    expect(called).toBe(false);
  });

  it("refuses nil arguments", async () => {
    await expect(distx.run(new Context(), null, null, () => 1)).rejects.toThrow(TypeError);
    await expect(distx.run(new Context(), new ScriptedTx(), null, null)).rejects.toThrow(TypeError);
  });

  it("accepts a sync fn", async () => {
    expect(await distx.run(new Context(), new ScriptedTx(), null, () => "sync")).toBe("sync");
  });
});

describe("routing headers", () => {
  it("echoes the conn id and sets the headers, never appends", async () => {
    const tx = new ScriptedTx({ connId: "replica-7" });
    const stale = new Context({ outgoing: [[distx.TX_ID_HEADER, "old"], [distx.CONN_ID_HEADER, "old-conn"]] });
    const [h, ctx] = await distx.begin(stale, tx, distx.beginRequest());
    expect(ctx.outgoing.getAll(distx.TX_ID_HEADER)).toEqual(["tx-1"]);
    expect(ctx.outgoing.getAll(distx.CONN_ID_HEADER)).toEqual(["replica-7"]);
    expect([h.connId, h.txId]).toEqual(["replica-7", "tx-1"]);
    await h.commit(new Context()); // a fresh context: the handle re-attaches its routing
    expect(tx.headers.at(-1)!.getAll(distx.CONN_ID_HEADER)).toEqual(["replica-7"]);
    expect(tx.headers.at(-1)!.getAll(distx.TX_ID_HEADER)).toEqual(["tx-1"]);
  });

  it("no conn id removes a stale one", async () => {
    const [, ctx] = await distx.begin(new Context({ outgoing: { [distx.CONN_ID_HEADER]: "stale" } }), new ScriptedTx());
    expect(ctx.outgoing.has(distx.CONN_ID_HEADER)).toBe(false);
  });
});

describe("nesting", () => {
  it("a nested run joins one transaction", async () => {
    const tx = new ScriptedTx();
    const ids: (readonly string[])[] = [];
    await distx.run(new Context(), tx, distx.beginRequest(), async (c) => {
      ids.push(c.outgoing.getAll(distx.TX_ID_HEADER));
      await distx.run(c, tx, distx.beginRequest(), (c2) => void ids.push(c2.outgoing.getAll(distx.TX_ID_HEADER)));
    });
    expect(tx.seq()).toBe("begin,commit");
    expect(ids).toEqual([["tx-1"], ["tx-1"]]);
  });

  it("a joined rollback makes the root rollback-only", async () => {
    const tx = new ScriptedTx();
    await expect(
      distx.run(new Context(), tx, distx.beginRequest(), async (c) => {
        await expect(distx.run(c, tx, distx.beginRequest(), () => Promise.reject(new Error("inner refuses")))).rejects.toThrow();
        // swallowed: the caller carries on as if nothing happened
      }),
    ).rejects.toThrow(distx.RollbackOnlyError);
    expect(tx.seq()).toBe("begin,rollback");
  });

  it.each([
    [distx.beginRequest({ connectionName: "audit" }), distx.beginRequest({ connectionName: "main" })],
    [
      distx.beginRequest({ isolation: distx.Isolation.SERIALIZABLE }),
      distx.beginRequest({ connectionName: "", isolation: distx.Isolation.READ_COMMITTED }),
    ],
  ])("a nested mismatch is refused (%#)", async (inner, outer) => {
    const tx = new ScriptedTx();
    await expect(distx.run(new Context(), tx, outer, (c) => distx.begin(c, tx, inner))).rejects.toThrow(distx.NestedMismatchError);
  });

  it("the same isolation, or none, joins", async () => {
    const tx = new ScriptedTx();
    await distx.run(new Context(), tx, distx.beginRequest({ isolation: distx.Isolation.SERIALIZABLE }), async (c) => {
      const [h] = await distx.begin(c, tx, distx.beginRequest({ isolation: distx.Isolation.SERIALIZABLE }));
      expect(h.joined).toBe(true);
      const [h2] = await distx.begin(c, tx, distx.beginRequest());
      expect(h2.joined).toBe(true);
    });
    expect(tx.seq()).toBe("begin,commit");
  });
});

describe("the handle", () => {
  it("is finished after one commit", async () => {
    const tx = new ScriptedTx();
    const [h] = await distx.begin(new Context(), tx);
    await h.commit(new Context());
    await expect(h.commit(new Context())).rejects.toThrow(/already committed/);
    await expect(h.rollback(new Context())).rejects.toThrow(distx.HandleFinishedError);
    expect(tx.seq()).toBe("begin,commit");
  });

  it("a failed attempt does not use it up", async () => {
    const tx = new ScriptedTx({ commitErr: new StatusError(Status.FAILED_PRECONDITION, "no") });
    const [h] = await distx.begin(new Context(), tx);
    await expect(h.commit(new Context())).rejects.toThrow(StatusError);
    tx.commitErr = undefined;
    // The coordinator answered (FAILED_PRECONDITION means "did not commit"): usable again.
    await h.rollback(new Context());
  });

  it("a commit lost in transport, then rolled back, is an ordinary failure", async () => {
    const tx = new ScriptedTx({ commitErr: new StatusError(Status.UNAVAILABLE, "reset") });
    const ran: number[] = [];
    const [h, ctx] = await distx.begin(new Context(), tx);
    await distx.onCommit(ctx, () => ran.push(1));
    const e = await caught(h.commit(new Context()));
    expect((e as StatusError).code).toBe(Status.UNAVAILABLE);
    expect(notes(e).some((n) => n.includes("did not take effect"))).toBe(true);
    expect(tx.seq()).toBe("begin,commit,rollback");
    expect(ran).toEqual([]);
  });

  it("a commit of unknown outcome runs none of the parked work", async () => {
    const tx = new ScriptedTx({
      commitErr: new StatusError(Status.DEADLINE_EXCEEDED, "lost"),
      rollbackErr: new StatusError(Status.NOT_FOUND, "no such tx"),
    });
    const ran: number[] = [];
    const [h, ctx] = await distx.begin(new Context(), tx);
    await distx.onCommit(ctx, () => ran.push(1));
    await expect(h.commit(new Context())).rejects.toThrow(distx.CommitOutcomeUnknownError);
    expect(ran).toEqual([]); // work parked on a commit of unknown outcome must not run
    expect(tx.seq()).toBe("begin,commit,rollback");
  });

  it("a caller cancelled mid-commit with an unknown outcome uses the handle up", async () => {
    // Cancelling the caller cancels the commit call (CANCELLED), and the
    // settling rollback fails too: the commit may have landed, so the handle
    // must refuse another one.
    const tx = new ScriptedTx({ commitErr: new StatusError(Status.CANCELLED, "context canceled"), rollbackErr: new StatusError(Status.NOT_FOUND, "no such tx") });
    const [h] = await distx.begin(new Context(), tx);
    await expect(h.commit(new Context())).rejects.toThrow(distx.CommitOutcomeUnknownError);
    tx.commitErr = undefined;
    await expect(h.commit(new Context())).rejects.toThrow(/outcome unknown/);
    expect(tx.seq()).toBe("begin,commit,rollback");
  });

  it("a second commit while the outcome is being settled is refused once it settles", async () => {
    const tx = new ScriptedTx({ commitErr: new StatusError(Status.CANCELLED, "context canceled") });
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    tx.rollbackHook = () => gate;
    const [h] = await distx.begin(new Context(), tx);
    const first = caught(h.commit(new Context()));
    await sleep(10); // now inside the settle
    const second = caught(h.commit(new Context()));
    release();
    expect(((await first) as StatusError).code).toBe(Status.CANCELLED);
    expect(await second).toBeInstanceOf(distx.HandleFinishedError);
    expect(tx.seq()).toBe("begin,commit,rollback");
  });
});

describe("onCommit", () => {
  it("runs in order after the commit, never after a rollback", async () => {
    const tx = new ScriptedTx();
    const seen: string[] = [];
    await distx.run(new Context(), tx, null, async (c) => {
      await distx.onCommit(c, () => seen.push("a"));
      expect(await distx.onCommit(c, async () => void seen.push("b"))).toBe(txscope.Outcome.DEFERRED);
      expect(seen).toEqual([]);
    });
    expect(seen).toEqual(["a", "b"]);
    seen.length = 0;
    await expect(
      distx.run(new Context(), tx, null, async (c) => {
        await distx.onCommit(c, () => seen.push("x"));
        throw new Error("no");
      }),
    ).rejects.toThrow("no");
    expect(seen).toEqual([]);
  });

  it("outside a transaction runs now", async () => {
    const seen: number[] = [];
    expect(await distx.onCommit(new Context(), () => seen.push(1))).toBe(txscope.Outcome.NO_SCOPE);
    expect(seen).toEqual([1]);
  });

  it("a failure after the commit does not fail the commit", async () => {
    const seen: string[] = [];
    await distx.run(new Context(), new ScriptedTx(), null, async (c) => {
      await distx.onCommit(c, () => {
        throw new Error("hook");
      });
      await distx.onCommit(c, () => seen.push("still runs"));
    });
    expect(seen).toEqual(["still runs"]);
  });

  it("in a joined run waits for the outer commit", async () => {
    for (const outerFails of [false, true]) {
      const tx = new ScriptedTx();
      const seen: string[] = [];
      await distx
        .run(new Context(), tx, null, async (c) => {
          await distx.run(c, tx, null, (c2) => distx.onCommit(c2, () => seen.push("inner")));
          expect(seen).toEqual([]);
          if (outerFails) throw new Error("outer refuses");
        })
        .catch(() => undefined);
      expect(seen).toEqual(outerFails ? [] : ["inner"]);
    }
  });

  it("after the transaction ended", async () => {
    const tx = new ScriptedTx();
    let kept!: Context;
    await distx.run(new Context(), tx, null, (c) => void (kept = c));
    const seen: number[] = [];
    expect(await distx.onCommit(kept, () => seen.push(1))).toBe(txscope.Outcome.ALREADY_COMMITTED);
    expect(seen).toEqual([1]);
    // And a begin on a context whose transaction ended opens a new one.
    const [h] = await distx.begin(kept, tx);
    expect(h.joined).toBe(false);
  });
});

// --- after the commit: the request may be gone ------------------------------

describe("work after the commit", () => {
  it("a cancelled caller does not cut off the parked work", async () => {
    // The client hangs up while the first hook runs: storage has committed,
    // so every hook (and the events among them) must still run to its end.
    const tx = new ScriptedTx();
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    let started = false;
    const ran: string[] = [];
    const ctx = new Context().withCancel();
    const run = distx.run(ctx, tx, null, async (c) => {
      await distx.onCommit(c, async () => {
        started = true;
        await gate;
        ran.push("slow");
      });
      await distx.onCommit(c, () => ran.push("next"));
    });
    expect(await settled(() => started)).toBe(true);
    ctx.cancel();
    release();
    await run;
    expect(await settled(() => ran.length === 2)).toBe(true);
    expect(ran).toEqual(["slow", "next"]);
    expect(await txscope.drain(1000)).toBe(true);
  });

  it("one hook cannot stop the rest; each failure is reported", async () => {
    const reported: string[] = [];
    vi.spyOn(console, "error").mockImplementation((...a: unknown[]) => void reported.push(a.join(" ")));
    const ran: string[] = [];
    await distx.run(new Context(), new ScriptedTx(), null, async (c) => {
      await distx.onCommit(c, () => Promise.reject(new Error("async failure")));
      await distx.onCommit(c, () => {
        throw new TypeError("sync failure");
      });
      await distx.onCommit(c, () => ran.push("last"));
    });
    expect(ran).toEqual(["last"]);
    const parked = reported.filter((r) => r.includes("parked on committed transaction tx-1"));
    expect(parked.length).toBe(2);
  });

  it("the drain waits for work that outlived its request", async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const done: number[] = [];
    void txscope.shielded(gate.then(() => void done.push(1)));
    expect(await txscope.drain(50)).toBe(false); // still running
    release();
    expect(await txscope.drain(1000)).toBe(true);
    expect(done).toEqual([1]);
  });

  it("the drain waits for work that starts while it waits", async () => {
    // Work started while the drain already waits (a hook of the first piece
    // starting the next) must be awaited too: one snapshot taken at the start
    // would miss it.
    const done: string[] = [];
    void txscope.shielded(
      (async () => {
        await sleep(50);
        void txscope.shielded(
          (async () => {
            await sleep(100);
            done.push("second");
          })(),
        );
        done.push("first");
      })(),
    );
    expect(await txscope.drain(2000)).toBe(true);
    expect(done).toEqual(["first", "second"]);
  });

  it("each hook's failure is caught on its own", async () => {
    const [, scope] = txscope.newScope(new Context());
    const ran: number[] = [];
    scope.park(() => Promise.reject(new Error("own")));
    scope.park(() => ran.push(1));
    const failures = await scope.runCommitted();
    expect(ran).toEqual([1]);
    expect(failures.length).toBe(1);
  });
});
