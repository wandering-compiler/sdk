/**
 * Distributed transactions — the TypeScript twin of sdk/go/service/tx/distx.
 *
 * The transaction lives in the storage server (`W17DistributedTransaction`
 * Begin / Commit / Rollback). Every storage call that carries `w17-tx-id`
 * adopts it; `w17-conn-id`, when the storage proxy minted one in Begin's
 * response headers, pins the call to the replica that holds it. This module
 * is the client half, with Go's semantics:
 *
 * - `run(ctx, client, beginRequest, fn)` — begin, call `fn(txCtx)`, commit
 *   when it returns, roll back when it throws (the error keeps propagating).
 * - `begin` → `TxHandle.commit` / `rollback` for the manual form.
 * - NESTED: a begin on a context that already belongs to an open transaction
 *   JOINS it — no RPC; its commit is a no-op and its rollback marks the root
 *   rollback-only. A join needs the same connection and an unspecified or
 *   equal isolation, or it is `NestedMismatchError`.
 * - The root's commit after a joined rollback rolls back and throws
 *   `RollbackOnlyError`.
 * - A second commit/rollback on a finished root handle throws
 *   `HandleFinishedError` without an RPC.
 * - A commit that fails in transport is probed with a detached rollback: if
 *   the rollback succeeds the commit did not take effect (an ordinary
 *   failure); if not, `CommitOutcomeUnknownError` — the writes may have
 *   landed, and the work parked for the commit is NOT run.
 * - Rollback is detached from the caller's cancellation/deadline; a done
 *   context refuses to commit (`run`).
 * - `ABORTED` from storage is thrown to the caller, who retries the unit of
 *   work.
 *
 * Work parked with `onCommit` (and every event emitted inside the transaction)
 * runs after the root commit, in order, and is dropped on rollback.
 */

import { create, type Message } from "@bufbuild/protobuf";
import type { Context, Metadata } from "./context.js";
import { type AnyMessage, getDefaultPool } from "./descriptors.js";
import { Status, StatusError } from "./errors.js";
import { errorText, reportError } from "./observability.js";
import * as txscope from "./txscope.js";

/** txregistry.HeaderName / distx.ConnIDHeader. */
export const TX_ID_HEADER = "w17-tx-id";
export const CONN_ID_HEADER = "w17-conn-id";

/** Bounds the rollback a commit sends to settle an ambiguous outcome (ms). */
export const OUTCOME_PROBE_TIMEOUT_MS = 10_000;

const PKG = "w17.common.distx.";

function msg(name: string, init: Record<string, unknown> = {}): AnyMessage {
  return create(getDefaultPool().schema(PKG + name), init) as AnyMessage;
}

/** `w17.common.distx.Isolation` values. */
export const Isolation = {
  UNSPECIFIED: 0,
  READ_UNCOMMITTED: 1,
  READ_COMMITTED: 2,
  REPEATABLE_READ: 3,
  SERIALIZABLE: 4,
} as const;

/** The shape of a `w17.common.distx.BeginRequest`. */
export interface BeginRequestInit {
  connectionName?: string;
  txTimeoutMs?: number | bigint;
  isolation?: number;
}

/**
 * The `BeginRequest` for `begin` / `run`. `connectionName` is the
 * connection's name in the lock (`notes-postgres`): the storage tier's
 * coordinator opens the transaction on it and refuses a name it does not
 * serve, the empty one included (`INVALID_ARGUMENT`, "unknown connection").
 */
export function beginRequest(connectionName: string | BeginRequestInit = "", txTimeoutMs: number | bigint = 0, isolation: number = Isolation.UNSPECIFIED): AnyMessage {
  const init: BeginRequestInit =
    typeof connectionName === "string" ? { connectionName, txTimeoutMs, isolation } : connectionName;
  return msg("BeginRequest", {
    connectionName: init.connectionName ?? "",
    txTimeoutMs: BigInt(init.txTimeoutMs ?? 0),
    isolation: init.isolation ?? Isolation.UNSPECIFIED,
  });
}

// --- errors ----------------------------------------------------------------

/** Base of the four transaction errors. */
export class DistxError extends Error {
  override name = "DistxError";
}

/**
 * A JOINED part of this transaction rolled back, so the root rolled the whole
 * transaction back instead of committing it.
 */
export class RollbackOnlyError extends DistxError {
  override name = "RollbackOnlyError";
  constructor(detail = "", options?: ErrorOptions) {
    super(
      "distx: a nested part of this transaction rolled back, so the transaction was rolled back instead of committed" +
        (detail ? `; ${detail}` : ""),
      options,
    );
  }
}

/**
 * A begin found a transaction open on ctx but cannot JOIN it: another
 * connection, or another isolation asked for explicitly. Refused rather than
 * opened as a second transaction, which would not be atomic with the first
 * (or could deadlock against its locks).
 */
export class NestedMismatchError extends DistxError {
  override name = "NestedMismatchError";
}

/** A second commit or rollback on a root handle whose transaction already ended through it. */
export class HandleFinishedError extends DistxError {
  override name = "HandleFinishedError";
}

/**
 * The commit call failed in transport and the transaction could no longer be
 * rolled back: the commit may have landed. Do not assume either outcome
 * (retrying a non-idempotent write could apply it twice).
 */
export class CommitOutcomeUnknownError extends DistxError {
  override name = "CommitOutcomeUnknownError";
}

/** Go's sentinel names, for code that reads like the Go guide. */
export const ErrRollbackOnly = RollbackOnlyError;
export const ErrNestedMismatch = NestedMismatchError;
export const ErrHandleFinished = HandleFinishedError;
export const ErrCommitOutcomeUnknown = CommitOutcomeUnknownError;

// --- client protocol -------------------------------------------------------

/**
 * The coordinator client distx drives. `clients.distributedTransaction()`
 * returns one; a test may fake it. `begin` returns the response and the
 * response's initial metadata (where the proxy puts `w17-conn-id`).
 */
export interface TxClient {
  begin(ctx: Context, req: Message): Promise<{ reply: AnyMessage; header: Metadata }>;
  commit(ctx: Context, req: Message): Promise<unknown>;
  rollback(ctx: Context, req: Message): Promise<unknown>;
}

const ROOT = Symbol("w17.distx.root");

function rootFrom(ctx: Context): TxHandle | undefined {
  return ctx.value<TxHandle>(ROOT);
}

/** A promise-chain mutex: one commit or rollback of a handle at a time. */
class Lock {
  #tail: Promise<void> = Promise.resolve();
  async run<T>(fn: () => Promise<T>): Promise<T> {
    const prev = this.#tail;
    let release!: () => void;
    this.#tail = new Promise((r) => (release = r));
    await prev;
    try {
      return await fn();
    } finally {
      release();
    }
  }
}

/** Add a note to an error, as Python's `add_note` (kept on `notes`). */
export function addNote(e: unknown, note: string): void {
  if (e !== null && typeof e === "object") {
    const o = e as { notes?: string[] };
    (o.notes ??= []).push(note);
  }
}

/**
 * One distributed transaction opened by `begin`. Single use: one commit OR
 * one rollback; a failed attempt does not use it up.
 */
export class TxHandle {
  #ended = "";
  readonly #lock = new Lock();

  readonly client: TxClient;
  readonly txId: string;
  /** The proxy routing token, "" when no proxy minted one. */
  readonly connId: string;
  readonly connection: string;
  readonly isolation: number;
  readonly scope: txscope.Scope | undefined;
  readonly joined: boolean;

  /** @internal */
  constructor(
    client: TxClient,
    txId: string,
    connId: string,
    connection: string,
    isolation: number,
    scope: txscope.Scope | undefined,
    joined = false,
  ) {
    this.client = client;
    this.txId = txId;
    this.connId = connId;
    this.connection = connection;
    this.isolation = isolation;
    this.scope = scope;
    this.joined = joined;
  }

  /**
   * ctx with this transaction's routing headers SET (never appended — an
   * appended second `w17-tx-id` is adopted by nobody; storage adopts the
   * first). `w17-conn-id` is removed when there is none.
   */
  attach(ctx: Context): Context {
    let out = ctx.outgoing.withSet(TX_ID_HEADER, this.txId);
    out = this.connId ? out.withSet(CONN_ID_HEADER, this.connId) : out.withSet(CONN_ID_HEADER);
    return ctx.withOutgoing(out);
  }

  #claim(): void {
    if (this.#ended) {
      throw new HandleFinishedError(
        `distx: this transaction handle was already used: transaction ${this.txId} was already ${this.#ended}`,
      );
    }
  }

  /**
   * Commit; a joined handle commits nothing (the root does).
   *
   * Runs the parked work after a successful commit (failures reported, never
   * thrown); drops it otherwise.
   */
  async commit(ctx: Context): Promise<void> {
    if (this.joined) return;
    const err = await this.#lock.run(async (): Promise<unknown> => {
      this.#claim();
      if (this.scope?.rollbackOnly) {
        this.scope.discarded();
        try {
          await this.client.rollback(this.attach(ctx.withoutCancel()), msg("RollbackRequest", { txId: this.txId }));
        } catch (rerr) {
          throw new RollbackOnlyError(`the rollback also failed: ${errorText(rerr)}`, { cause: rerr });
        }
        this.#ended = "rolled back";
        throw new RollbackOnlyError();
      }
      try {
        await this.client.commit(this.attach(ctx), msg("CommitRequest", { txId: this.txId }));
      } catch (e) {
        if (!outcomeUnknown(e)) return e;
        // Used up BEFORE the settle is awaited: nothing may commit this
        // handle again while the outcome is being settled. A settle whose
        // rollback succeeds turns this into "rolled back".
        this.#ended = "left with its outcome unknown";
        return await this.#settle(ctx, e);
      }
      this.#ended = "committed";
      return undefined;
    });
    if (this.scope === undefined) {
      if (err !== undefined) throw err;
      return;
    }
    if (err !== undefined) {
      this.scope.discarded();
      throw err;
    }
    // The transaction IS committed from here on. The parked work (the events
    // of the writes among it) runs to its end even when the caller hangs up,
    // and every failure is reported, never thrown into a commit that landed.
    const scope = this.scope;
    const detachedCtx = ctx.withoutCancel();
    const txId = this.txId;
    await txscope.shielded(
      (async () => {
        for (const failure of await scope.runCommitted()) {
          reportError(detachedCtx, new Error(`distx: work parked on committed transaction ${txId} failed: ${errorText(failure)}`));
        }
      })(),
    );
  }

  /**
   * After a commit with an unknown outcome: roll back, detached. Success
   * means the transaction was still open, so the commit never took effect;
   * failure means it is gone and the outcome is unknown.
   */
  async #settle(ctx: Context, commitErr: unknown): Promise<unknown> {
    const probe = ctx.withoutCancel().withTimeout(OUTCOME_PROBE_TIMEOUT_MS);
    try {
      await this.client.rollback(this.attach(probe), msg("RollbackRequest", { txId: this.txId }));
    } catch (rerr) {
      const pending = this.scope?.pending() ?? 0;
      reportError(
        ctx.withoutCancel(),
        new Error(
          `distx: the outcome of committing transaction ${this.txId} is unknown — the commit call failed ` +
            `(${errorText(commitErr)}) and the transaction could no longer be rolled back (${errorText(rerr)}); its writes may have ` +
            `landed, and ${pending} piece(s) of work waiting for the commit were not run`,
        ),
      );
      this.scope?.discarded();
      return new CommitOutcomeUnknownError(
        `distx: the commit's outcome is unknown: transaction ${this.txId}: ${errorText(commitErr)}`,
        { cause: commitErr },
      );
    }
    this.#ended = "rolled back";
    addNote(commitErr, `distx: the commit of transaction ${this.txId} did not take effect and it was rolled back`);
    return commitErr;
  }

  /**
   * Roll back. A joined handle marks the root rollback-only instead.
   *
   * Outlives ctx's cancellation and deadline: the commonest reason to roll
   * back is that the caller is gone, and a call refused on a done context
   * would leave the transaction held until the server times it out.
   */
  async rollback(ctx: Context): Promise<void> {
    if (this.joined) {
      this.scope?.markRollbackOnly();
      return;
    }
    await this.#lock.run(async () => {
      this.#claim();
      this.scope?.discarded();
      await this.client.rollback(this.attach(ctx.withoutCancel()), msg("RollbackRequest", { txId: this.txId }));
      this.#ended = "rolled back";
    });
  }
}

/**
 * A commit failure that does not say whether the server committed: it failed
 * in transport or was abandoned rather than answered.
 */
function outcomeUnknown(e: unknown): boolean {
  if (e instanceof StatusError) {
    return e.code === Status.UNAVAILABLE || e.code === Status.DEADLINE_EXCEEDED || e.code === Status.CANCELLED;
  }
  return false;
}

/**
 * Open (or JOIN) a distributed transaction.
 *
 * Returns the handle and a context carrying `w17-tx-id` (and `w17-conn-id`
 * when the proxy supplied one): pass that context to every storage call that
 * belongs to the transaction.
 */
export async function begin(ctx: Context, client: TxClient, req?: Message): Promise<[TxHandle, Context]> {
  const r = (req ?? beginRequest()) as AnyMessage;
  const root = rootFrom(ctx);
  if (root !== undefined && txscope.active(ctx)) return join(ctx, root, r);
  const { reply, header } = await client.begin(ctx, r);
  const [scoped, scope] = txscope.newScope(ctx);
  const connId = header.getAll(CONN_ID_HEADER).find((v) => v) ?? "";
  const h = new TxHandle(client, String(reply.txId), connId, String(r.connectionName), Number(r.isolation), scope);
  return [h, h.attach(scoped.withValue(ROOT, h))];
}

function join(ctx: Context, root: TxHandle, req: AnyMessage): [TxHandle, Context] {
  if (req.connectionName !== root.connection) {
    throw new NestedMismatchError(
      `distx: cannot join the transaction already open on this context: it is on connection ` +
        `${JSON.stringify(root.connection)} and this begin asks for ${JSON.stringify(req.connectionName)} — a transaction on another ` +
        "connection would not be atomic with it",
    );
  }
  if (req.isolation !== Isolation.UNSPECIFIED && req.isolation !== root.isolation) {
    throw new NestedMismatchError(
      `distx: cannot join the transaction already open on this context: it runs at isolation ` +
        `${root.isolation} and this begin asks for ${String(req.isolation)} — a running transaction cannot change level`,
    );
  }
  const h = new TxHandle(root.client, root.txId, root.connId, root.connection, root.isolation, root.scope, true);
  // SET the root's routing headers even when ctx already shows them.
  return [h, h.attach(ctx)];
}

/**
 * Run `fn` inside one distributed transaction and return its result.
 *
 * Commit when fn returns; roll back (detached) when it throws — the error
 * keeps propagating, with a note when the rollback failed too. A caller whose
 * context is done gets no commit. A failed commit is thrown as it is (with a
 * note naming the commit), and fn's work is lost.
 *
 * fn must use the context it receives for every write of the transaction.
 */
export async function run<T>(
  ctx: Context,
  client: TxClient | undefined | null,
  req: Message | undefined | null,
  fn: ((txCtx: Context) => T | Promise<T>) | undefined | null,
): Promise<T> {
  if (!client) throw new TypeError("distx.run: nil transaction client");
  if (!fn) throw new TypeError("distx.run: nil function — an empty transaction is a mistake, not a no-op");
  let handle: TxHandle;
  let txCtx: Context;
  try {
    [handle, txCtx] = await begin(ctx, client, req ?? undefined);
  } catch (e) {
    addNote(e, "distx.run: begin");
    throw e;
  }
  const detachedCtx = ctx.withoutCancel();
  let result: T;
  try {
    result = await fn(txCtx);
  } catch (ferr) {
    try {
      await handle.rollback(detachedCtx);
    } catch (rerr) {
      addNote(ferr, `distx.run: rollback also failed: ${errorText(rerr)}`);
    }
    throw ferr;
  }
  if (ctx.done()) {
    try {
      await handle.rollback(detachedCtx);
    } catch {
      // the refusal below is what the caller acts on
    }
    throw new DistxError(`distx.run: not committing, the caller's context is done: ${ctx.err()}`);
  }
  try {
    await handle.commit(ctx);
  } catch (cerr) {
    addNote(cerr, "distx.run: commit");
    throw cerr;
  }
  return result;
}

/**
 * Run `fn` once the transaction ctx belongs to commits; never if it rolls
 * back; at once outside a transaction. `fn` may be sync or async.
 *
 * Outside a transaction (or after its commit) fn runs now, before this
 * returns, and an error from it propagates — it is the caller's own code on
 * the caller's own path. Inside one it is parked and the result is
 * `Outcome.DEFERRED`; a failure then is reported, never thrown into the
 * commit (the transaction has landed). Inside a nested `run` that JOINED an
 * outer transaction, fn waits for the OUTER commit.
 *
 * The hook the event middleware itself uses, offered for any side effect that
 * must not outrun the write it describes (an e-mail, an external call). Go's
 * twin is `distx.OnCommit`.
 */
export async function onCommit(ctx: Context, fn: (() => unknown) | undefined | null): Promise<txscope.Outcome> {
  if (!fn) return txscope.Outcome.NO_SCOPE;
  const out = txscope.afterCommit(ctx, fn);
  if (out === txscope.Outcome.NO_SCOPE || out === txscope.Outcome.ALREADY_COMMITTED) {
    await fn();
  } else if (out === txscope.Outcome.DISCARDED) {
    reportError(ctx, "distx: onCommit work dropped: it was registered after its transaction rolled back");
  }
  return out;
}
