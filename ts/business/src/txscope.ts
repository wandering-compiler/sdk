/**
 * The in-process face of a distributed transaction (Go's `service/tx/txscope`).
 *
 * Which transaction a context belongs to, what has to happen when it commits,
 * and whether something inside it already decided it must not. It exists for
 * COMPOSITION: a business method that calls another one through
 * `registry.self(...)` inside its own `distx.run` must JOIN the caller's
 * transaction (not open a second one), and the callee's event must wait for
 * the caller's commit.
 *
 * JavaScript is single-threaded: every state change below happens between two
 * awaits, so it is atomic with respect to other requests.
 */

import type { Context } from "./context.js";
import { errorText, reportError } from "./observability.js";

const KEY = Symbol("w17.txscope");

/** What `afterCommit` did with the work it was given. */
export const Outcome = {
  /** ctx belongs to no transaction; the caller runs the work now. */
  NO_SCOPE: 0,
  /** Runs when the root commits, never if it rolls back. */
  DEFERRED: 1,
  /** The transaction committed first; run it now. */
  ALREADY_COMMITTED: 2,
  /** The transaction rolled back first; it must not run. */
  DISCARDED: 3,
} as const;
export type Outcome = (typeof Outcome)[keyof typeof Outcome];

export type Hook = () => unknown;

/** One root transaction as seen from the process that opened it. */
export class Scope {
  #hooks: Hook[] = [];
  #rollbackOnly = false;
  ended = false;
  committed = false;

  /**
   * A joined part failed and rolled back. Without savepoints its writes
   * cannot be undone alone, so the root must not commit.
   */
  markRollbackOnly(): void {
    this.#rollbackOnly = true;
  }

  get rollbackOnly(): boolean {
    return this.#rollbackOnly;
  }

  pending(): number {
    return this.#hooks.length;
  }

  /**
   * End the scope as committed and run the parked work in order.
   *
   * An error in one piece does not stop the rest and does not escape: the
   * transaction has already committed, and an error unwinding into its
   * caller would read as a failed commit. What failed is returned for the
   * caller to report.
   */
  async runCommitted(): Promise<unknown[]> {
    if (this.ended) return [];
    this.ended = this.committed = true;
    const hooks = this.#hooks;
    this.#hooks = [];
    const failures: unknown[] = [];
    for (const fn of hooks) {
      try {
        await fn();
      } catch (e) {
        failures.push(e);
      }
    }
    return failures;
  }

  /** End the scope as rolled back and drop the parked work. */
  discarded(): void {
    this.ended = true;
    this.#hooks = [];
  }

  /** @internal */
  park(fn: Hook): Outcome {
    if (this.ended) return this.committed ? Outcome.ALREADY_COMMITTED : Outcome.DISCARDED;
    this.#hooks.push(fn);
    return Outcome.DEFERRED;
  }
}

export function newScope(ctx: Context): [Context, Scope] {
  const s = new Scope();
  return [ctx.withValue(KEY, s), s];
}

export function scopeFrom(ctx: Context): Scope | undefined {
  return ctx.value<Scope>(KEY) ?? undefined;
}

/**
 * Whether ctx belongs to a scope that has not ended — when a new transaction
 * JOINS instead of beginning.
 */
export function active(ctx: Context): boolean {
  const s = scopeFrom(ctx);
  return s !== undefined && !s.ended;
}

/** ctx with its scope hidden (Go's `txscope.Detach`). */
export function detach(ctx: Context): Context {
  if (scopeFrom(ctx) === undefined) return ctx;
  return ctx.withValue(KEY, null);
}

/** Park fn until the transaction ctx belongs to commits. */
export function afterCommit(ctx: Context, fn: Hook | undefined): Outcome {
  const s = scopeFrom(ctx);
  if (s === undefined || fn === undefined) return Outcome.NO_SCOPE;
  return s.park(fn);
}

// --- work that must outlive the request ----------------------------------------
//
// Go runs the parked work on the goroutine that commits, and a goroutine is
// not interrupted when its client hangs up. A JavaScript promise is not
// interrupted either, but nothing waits for it once its request has answered:
// the process could exit (a SIGTERM drain) with the events of a committed
// write still in flight. So that work is tracked here, and the drain waits for
// it before the process exits.

const detached = new Set<Promise<unknown>>();

/**
 * Run work to completion, tracked: `drain` waits for it. The caller awaits
 * it as usual; a caller that stops waiting (its request was cancelled) does
 * not stop it.
 */
export function shielded<T>(work: Promise<T>): Promise<T> {
  const tracked = work.finally(() => detached.delete(tracked));
  detached.add(tracked);
  // A failure nobody awaits any more is reported, never an unhandled rejection.
  tracked.catch((e: unknown) => {
    reportError(undefined, new Error(`txscope: work run after its request failed: ${errorText(e)}`));
  });
  return tracked;
}

/** How many pieces of tracked work are still running. */
export function pendingCount(): number {
  return detached.size;
}

/**
 * Wait until no work `shielded` started is left running — including work
 * started while waiting (a hook of the first piece starting the next) — or
 * until one overall deadline of timeoutMs passes. True when none is left.
 */
export async function drain(timeoutMs: number): Promise<boolean> {
  const deadline = Date.now() + Math.max(timeoutMs, 0);
  for (;;) {
    // Let work that is about to start (a shielded() not yet called) do so.
    await new Promise((r) => setImmediate(r));
    if (!detached.size) return true;
    const remaining = deadline - Date.now();
    if (remaining <= 0) return false;
    let timer: NodeJS.Timeout | undefined;
    await Promise.race([
      Promise.allSettled([...detached]),
      new Promise((r) => {
        timer = setTimeout(r, remaining);
      }),
    ]);
    clearTimeout(timer);
  }
}

/**
 * Stop waiting for the work still running after the drain. A promise cannot
 * be cancelled from outside: it is abandoned (the process exits under it).
 * Returns how many pieces were abandoned.
 */
export function abandonPending(): number {
  const n = detached.size;
  detached.clear();
  return n;
}
