/**
 * The context a handler is given and passes on.
 *
 * Go's rule — "the context you were given is the one you pass on" — is what
 * keeps the caller's identity, deadline and transaction attached to every call
 * a handler makes. JavaScript has no `context.Context`, and the ambient
 * substitute (`AsyncLocalStorage`) loses its value in work started outside the
 * request without anyone noticing. So the SDK makes the context an explicit,
 * immutable value: every handler receives one, every client call takes one
 * first, and every helper that changes it (`paging.forward`,
 * `principal.actAs`, `distx.begin`) returns a NEW one rather than mutating the
 * one it was given — exactly like a Go `context.WithValue`.
 *
 * A context carries:
 *
 * - the INCOMING metadata of the request being served (what the gateway stamped);
 * - the OUTGOING metadata the next call will send (empty until something sets it);
 * - the deadline, as an absolute `performance.now()` instant in milliseconds
 *   (`undefined` = none);
 * - a cancellation flag (`cancel()`), the analogue of a Go cancel func;
 * - values keyed by private objects (the open transaction lives there).
 *
 * Durations in this SDK are milliseconds, as everywhere in JavaScript.
 */

import { performance } from "node:perf_hooks";
import * as observability from "./observability.js";

/** What a {@link Metadata} can be built from. */
export type MetadataInit =
  | Metadata
  | Readonly<Record<string, string | Uint8Array | readonly (string | Uint8Array)[]>>
  | Iterable<readonly [string, string | Uint8Array]>
  | null
  | undefined;

function asStr(v: string | Uint8Array): string {
  // grpc-js hands `-bin` keys over as Buffers; the w17 keys are all text, so
  // anything binary is carried as latin-1 rather than dropped.
  return typeof v === "string" ? v : Buffer.from(v).toString("latin1");
}

/** An already-normalised map, for the class's own copies. */
class Raw {
  readonly d: ReadonlyMap<string, readonly string[]>;
  constructor(d: ReadonlyMap<string, readonly string[]>) {
    this.d = d;
  }
}

/**
 * gRPC metadata: lower-case keys, each with one or more string values.
 *
 * Immutable, like a Go `metadata.MD` handed between contexts after a `Copy()`:
 * a helper that changes metadata builds a new value, so two contexts never
 * share a list one of them could rewrite. Keys are lower-cased on the way in,
 * because HTTP/2 and grpc-go both normalise them.
 */
export class Metadata implements Iterable<[string, readonly string[]]> {
  readonly #d: ReadonlyMap<string, readonly string[]>;

  constructor(init?: MetadataInit) {
    if (init instanceof Metadata) {
      this.#d = init.#d;
      return;
    }
    if ((init as unknown) instanceof Raw) {
      this.#d = (init as unknown as Raw).d;
      return;
    }
    const d = new Map<string, string[]>();
    const add = (k: string, v: string | Uint8Array) => {
      const key = k.toLowerCase();
      let list = d.get(key);
      if (list === undefined) {
        list = [];
        d.set(key, list);
      }
      list.push(asStr(v));
    };
    if (init == null) {
      // empty
    } else if (Symbol.iterator in init) {
      for (const [k, v] of init as Iterable<readonly [string, string | Uint8Array]>) add(k, v);
    } else {
      for (const [k, v] of Object.entries(init)) {
        if (typeof v === "string" || v instanceof Uint8Array) add(k, v);
        else for (const x of v) add(k, x);
      }
    }
    this.#d = Metadata.#clean(d);
  }

  static #clean(d: ReadonlyMap<string, readonly string[]>): Map<string, readonly string[]> {
    const out = new Map<string, readonly string[]>();
    for (const [k, v] of d) if (v.length) out.set(k, Object.freeze([...v]));
    return out;
  }

  static #of(d: ReadonlyMap<string, readonly string[]>): Metadata {
    return new Metadata(new Raw(Metadata.#clean(d)) as unknown as MetadataInit);
  }

  /** Every value of key, `[]` when absent (Go's `md.Get`). */
  getAll(key: string): readonly string[] {
    return this.#d.get(key.toLowerCase()) ?? [];
  }

  first(key: string): string | undefined {
    return this.#d.get(key.toLowerCase())?.[0];
  }

  has(key: string): boolean {
    return this.#d.has(key.toLowerCase());
  }

  get size(): number {
    return this.#d.size;
  }

  keys(): IterableIterator<string> {
    return this.#d.keys();
  }

  entries(): IterableIterator<[string, readonly string[]]> {
    return this.#d.entries();
  }

  [Symbol.iterator](): IterableIterator<[string, readonly string[]]> {
    return this.#d.entries();
  }

  /** A copy with key replaced by values (Go's `md.Set`); no values deletes it. */
  withSet(key: string, ...values: string[]): Metadata {
    const d = new Map(this.#d);
    if (values.length) d.set(key.toLowerCase(), values);
    else d.delete(key.toLowerCase());
    return Metadata.#of(d);
  }

  /** A copy without the keys pred(key) answers true for. */
  without(pred: (key: string) => boolean): Metadata {
    const d = new Map<string, readonly string[]>();
    for (const [k, v] of this.#d) if (!pred(k)) d.set(k, v);
    return Metadata.#of(d);
  }

  /** A copy with other's keys REPLACING this one's. */
  merged(other: Metadata | Readonly<Record<string, readonly string[]>> | ReadonlyMap<string, readonly string[]>): Metadata {
    const d = new Map(this.#d);
    const entries = other instanceof Metadata || other instanceof Map ? other.entries() : Object.entries(other);
    for (const [k, v] of entries as Iterable<[string, readonly string[]]>) d.set(k.toLowerCase(), [...v]);
    return Metadata.#of(d);
  }

  /** Flattened (key, value) pairs. */
  pairs(): [string, string][] {
    const out: [string, string][] = [];
    for (const [k, vs] of this.#d) for (const v of vs) out.push([k, v]);
    return out;
  }

  /** A plain object, key → values. */
  toObject(): Record<string, string[]> {
    const o: Record<string, string[]> = {};
    for (const [k, v] of this.#d) o[k] = [...v];
    return o;
  }

  equals(other: Metadata): boolean {
    if (other.size !== this.size) return false;
    for (const [k, v] of this.#d) {
      const o = other.getAll(k);
      if (o.length !== v.length || o.some((x, i) => x !== v[i])) return false;
    }
    return true;
  }

  toString(): string {
    return `Metadata(${JSON.stringify(this.toObject())})`;
  }
}

const CANCELED = "context canceled";
const DEADLINE = "context deadline exceeded";

/** A cancellation flag shared by a context and the contexts derived from it. */
class CancelNode {
  cancelled = false;
  /** When it was cancelled (`performance.now()`), and why. */
  at = 0;
  reason = CANCELED;
  readonly listeners = new Set<() => void>();
  readonly parent: CancelNode | undefined;
  constructor(parent?: CancelNode) {
    this.parent = parent;
  }

  isSet(): boolean {
    return this.cancelled || (this.parent?.isSet() ?? false);
  }

  /** The node of the chain cancelled first, if any. */
  first(): CancelNode | undefined {
    let out: CancelNode | undefined;
    for (let c: CancelNode | undefined = this; c; c = c.parent) if (c.cancelled && (out === undefined || c.at <= out.at)) out = c;
    return out;
  }

  cancel(reason = CANCELED): void {
    if (this.cancelled) return;
    this.cancelled = true;
    this.at = performance.now();
    this.reason = reason;
    for (const fn of [...this.listeners]) fn();
  }
}

/** Cancel ctx because its deadline passed (the server's 'cancelled' on DEADLINE_EXCEEDED). */
let cancelForDeadline: (ctx: Context) => void;

const INTERNAL = Symbol("w17.context.internal");
interface Internal {
  cancel: CancelNode;
  values: ReadonlyMap<unknown, unknown>;
}

/** What {@link Context} is constructed from. */
export interface ContextInit {
  incoming?: MetadataInit;
  outgoing?: MetadataInit;
  /** Absolute `performance.now()` milliseconds. */
  deadline?: number | undefined;
  /** The full gRPC method being served ("/pkg.Svc/Method"). */
  method?: string;
}

/** An immutable request context. See the module doc. */
export class Context {
  readonly incoming: Metadata;
  readonly outgoing: Metadata;
  /** Absolute `performance.now()` milliseconds; undefined = no deadline. */
  readonly deadline: number | undefined;
  /** The full gRPC method being served ("/pkg.Svc/Method"), "" outside a handler. */
  readonly method: string;
  readonly #cancel: CancelNode;
  readonly #values: ReadonlyMap<unknown, unknown>;

  constructor(init: ContextInit = {}) {
    this.incoming = new Metadata(init.incoming);
    this.outgoing = new Metadata(init.outgoing);
    this.deadline = init.deadline;
    this.method = init.method ?? "";
    const internal = (init as { [INTERNAL]?: Internal })[INTERNAL];
    this.#cancel = internal?.cancel ?? new CancelNode();
    this.#values = internal?.values ?? new Map();
  }

  #replace(kw: {
    incoming?: Metadata;
    outgoing?: Metadata;
    deadline?: number | undefined;
    hasDeadline?: boolean;
    method?: string;
    cancel?: CancelNode;
    values?: ReadonlyMap<unknown, unknown>;
  }): Context {
    const init: ContextInit & { [INTERNAL]: Internal } = {
      incoming: kw.incoming ?? this.incoming,
      outgoing: kw.outgoing ?? this.outgoing,
      deadline: kw.hasDeadline ? kw.deadline : this.deadline,
      method: kw.method ?? this.method,
      [INTERNAL]: { cancel: kw.cancel ?? this.#cancel, values: kw.values ?? this.#values },
    };
    return new Context(init);
  }

  withIncoming(md: MetadataInit): Context {
    return this.#replace({ incoming: new Metadata(md) });
  }

  withOutgoing(md: MetadataInit): Context {
    return this.#replace({ outgoing: new Metadata(md) });
  }

  /** A copy serving another method (the server sets it). */
  withMethod(method: string): Context {
    return this.#replace({ method });
  }

  withValue(key: unknown, value: unknown): Context {
    const v = new Map(this.#values);
    v.set(key, value);
    return this.#replace({ values: v });
  }

  value<T = unknown>(key: unknown): T | undefined {
    return this.#values.get(key) as T | undefined;
  }

  /** A child whose deadline is the earlier of this one's and now+ms. */
  withTimeout(ms: number): Context {
    let d = performance.now() + ms;
    if (this.deadline !== undefined && this.deadline < d) d = this.deadline;
    return this.#replace({ deadline: d, hasDeadline: true, cancel: new CancelNode(this.#cancel) });
  }

  /** A child that `cancel()` ends without ending this one. */
  withCancel(): Context {
    return this.#replace({ cancel: new CancelNode(this.#cancel) });
  }

  /**
   * Go's `context.WithoutCancel`: same metadata and values, no deadline,
   * never done. For work that must outlive the request — a rollback, a
   * commit-outcome probe, an event published after commit.
   */
  withoutCancel(): Context {
    return this.#replace({ deadline: undefined, hasDeadline: true, cancel: new CancelNode() });
  }

  cancel(): void {
    this.#cancel.cancel();
  }

  /** Milliseconds left before the deadline, undefined when there is none. */
  timeRemaining(): number | undefined {
    return this.deadline === undefined ? undefined : this.deadline - performance.now();
  }

  /** True once cancelled or past the deadline (Go's `ctx.Err() != nil`). */
  done(): boolean {
    return this.err() !== undefined;
  }

  /**
   * Why the context is done — Go's spellings — or undefined.
   *
   * Whichever came first, as Go's `ctx.Err()` keeps the first: a deadline
   * that had passed when the context was cancelled is the reason (a server
   * cancels a call whose deadline passed), and a cancel before the deadline
   * stays "canceled" after it passes.
   */
  err(): string | undefined {
    const c = this.#cancel.first();
    if (c !== undefined) {
      if (c.reason === DEADLINE || (this.deadline !== undefined && this.deadline <= c.at)) return DEADLINE;
      return CANCELED;
    }
    if (this.deadline !== undefined && performance.now() >= this.deadline) return DEADLINE;
    return undefined;
  }

  static {
    cancelForDeadline = (ctx) => ctx.#cancel.cancel(DEADLINE);
  }

  /**
   * Call fn once, when the context becomes done (at once, on a microtask, if
   * it already is). Returns the function that unregisters it.
   */
  onDone(fn0: () => void): () => void {
    // fn runs from a timer, a microtask, or whoever cancels — in a server,
    // a grpc-js 'cancelled' listener. A throw there would be an uncaught
    // exception that ends the process: it is reported instead.
    const fn = () => {
      try {
        fn0();
      } catch (e) {
        observability.reportError(this, new Error(`context: an onDone callback threw: ${e instanceof Error ? (e.stack ?? e.message) : String(e)}`, { cause: e }));
      }
    };
    if (this.done()) {
      let live = true;
      queueMicrotask(() => live && fn());
      return () => {
        live = false;
      };
    }
    let fired = false;
    const nodes: CancelNode[] = [];
    let timer: NodeJS.Timeout | undefined;
    const fire = () => {
      if (fired) return;
      fired = true;
      dispose();
      fn();
    };
    const dispose = () => {
      for (const n of nodes) n.listeners.delete(fire);
      if (timer !== undefined) clearTimeout(timer);
    };
    for (let c: CancelNode | undefined = this.#cancel; c; c = c.parent) {
      c.listeners.add(fire);
      nodes.push(c);
    }
    // setTimeout may fire a fraction of a millisecond early: re-arm until
    // the context reads as done, so fn always sees err() set.
    const arm = () => {
      const rem = this.timeRemaining();
      if (rem === undefined) return;
      timer = setTimeout(() => (this.done() ? fire() : arm()), Math.max(Math.ceil(rem), 0));
      timer.unref();
    };
    arm();
    return () => {
      fired = true;
      dispose();
    };
  }

  /** An AbortSignal that aborts when this context is done (for fetch & co). */
  get signal(): AbortSignal {
    const ac = new AbortController();
    this.onDone(() => ac.abort(new Error(this.err() ?? CANCELED)));
    return ac.signal;
  }

  toString(): string {
    return `Context(method=${JSON.stringify(this.method)}, incoming=[${[...this.incoming.keys()].join(",")}], outgoing=[${[...this.outgoing.keys()].join(",")}], deadline=${this.deadline})`;
  }
}

/**
 * @internal The server's cancel for a call grpc-js cancelled: within
 * `slackMs` of its deadline (grpc-js's timer and this context's deadline are
 * read off different clocks) it is the deadline, not a cancel.
 */
export function cancelCall(ctx: Context, slackMs = 5): void {
  const rem = ctx.timeRemaining();
  if (rem !== undefined && rem <= slackMs) cancelForDeadline(ctx);
  else ctx.cancel();
}

/** An empty context: no metadata, no deadline (Go's `context.Background()`). */
export function background(): Context {
  return new Context();
}
