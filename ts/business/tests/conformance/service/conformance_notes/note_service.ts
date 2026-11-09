// The conformance suite's NoteService, written against the TypeScript SDK.
//
// Kept as small and literal as the suite allows: each method exercises one
// behaviour the Go harness asserts on the wire (see conformance.proto). It is
// author code: erasable TypeScript run by Node's type stripping, no build.

import { distx, forUser, onCommit, paging, principal, Status } from "@w17/business";
import type {
  AnyMessage,
  Clients,
  Config,
  Context,
  MessageType,
  Registry,
  Served,
  ServiceClient,
  TxCoordinator,
} from "@w17/business";

const MSG = "conformance.v1.";
// The storage connection the transactions open on (the harness registers "main").
const CONN = "main";

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export class NoteService {
  cfg: Config;
  registry: Registry;
  storage: ServiceClient;
  tx: TxCoordinator;
  M: (name: string) => MessageType;

  constructor(cfg: Config, registry: Registry, clients: Clients) {
    this.cfg = cfg;
    this.registry = registry;
    this.storage = clients.service("conformance.v1.NoteStorage");
    this.tx = clients.distributedTransaction();
    this.M = (name) => registry.pool.message(MSG + name);
  }

  // Composition goes through the served service, so the emit fires.
  get me(): Served {
    return this.registry.self("conformance.v1.NoteService");
  }

  async CreateNote(ctx: Context, req: AnyMessage): Promise<AnyMessage> {
    return distx.run(ctx, this.tx, distx.beginRequest(CONN), (txCtx) =>
      this.storage.InsertNote(txCtx, this.M("InsertNoteRequest")({ text: req.text })),
    );
  }

  async CreateNotes(ctx: Context, req: AnyMessage): Promise<AnyMessage> {
    const notes: AnyMessage[] = [];
    await distx.run(ctx, this.tx, distx.beginRequest(CONN), async (txCtx) => {
      for (const text of req.texts as string[]) {
        if (req.nestedConnection) {
          // A nested begin on another connection cannot join.
          await distx.run(txCtx, this.tx, distx.beginRequest({ connectionName: req.nestedConnection }), (c) =>
            this.storage.InsertNote(c, this.M("InsertNoteRequest")({ text })),
          );
        }
        notes.push(await this.me.CreateNote(txCtx, this.M("CreateNoteRequest")({ text })));
      }
      if (req.nestedFailText) {
        try {
          await this.me.CreateNotes(txCtx, this.M("CreateNotesRequest")({ texts: [req.nestedFailText], fail: true }));
        } catch {
          // swallowed on purpose: the root must still refuse to commit
        }
      }
      if (req.onCommitText) {
        const text: string = req.onCommitText;
        const detached = ctx.withoutCancel();
        await onCommit(txCtx, () => this.storage.InsertNote(detached, this.M("InsertNoteRequest")({ text })));
      }
      if (req.pauseMs) await sleep(req.pauseMs);
      if (req.fail) throw new Error("refusing after the writes");
    });
    return this.M("CreateNotesResponse")({ notes });
  }

  async ListNotes(ctx: Context, req: AnyMessage): Promise<AnyMessage> {
    // Sibling calls that must NOT see the page, nor move it: one before the
    // forwarded call and one after it, so a relay that ignored the forward
    // mark is caught whichever position (first or last) would win.
    await this.storage.Inspect(ctx, this.M("InspectRequest")());
    const resp = await this.storage.ListNotes(paging.forward(ctx), req);
    await this.storage.Inspect(ctx, this.M("InspectRequest")());
    return resp;
  }

  async Inspect(ctx: Context, req: AnyMessage): Promise<AnyMessage> {
    if (req.staleOutgoing) {
      // A context reused from another request: the relay must replace this
      // whole set with the verified incoming principal.
      ctx = ctx.withOutgoing(
        ctx.outgoing.merged({
          "x-w17-user": ["stale-user"],
          "x-w17-scope-org_id": ["stale-org"],
          "x-w17-audience": ["stale-audience"],
          "w17-label-tenant": ["stale-tenant"],
          "x-w17-scope-stale_only": ["leak"],
        }),
      );
    }
    return this.storage.Inspect(ctx, req);
  }

  async Refuse(_ctx: Context, _req: AnyMessage): Promise<AnyMessage> {
    throw forUser(Status.FAILED_PRECONDITION, "NOTE_LOCKED", "This note is locked.", "text", "note 7 is locked by an export");
  }

  async Sleep(_ctx: Context, req: AnyMessage): Promise<AnyMessage> {
    await sleep(req.ms);
    return this.M("Note")({ text: "slept" });
  }

  async TxErrors(ctx: Context, req: AnyMessage): Promise<AnyMessage> {
    try {
      if (req.case === "finished") {
        const [h] = await distx.begin(ctx, this.tx, distx.beginRequest(CONN));
        await h.commit(ctx);
        await h.commit(ctx);
      } else if (req.case === "unknown") {
        const [h, txCtx] = await distx.begin(ctx, this.tx, distx.beginRequest(CONN));
        await this.storage.InsertNote(txCtx, this.M("InsertNoteRequest")({ text: "outcome-unknown" }));
        // Work parked on the transaction, which a commit of unknown outcome
        // must not run: a composed method's event, and a hook.
        await this.me.CreateNote(txCtx, this.M("CreateNoteRequest")({ text: "outcome-unknown-composed" }));
        const detached = ctx.withoutCancel();
        await onCommit(txCtx, () =>
          this.storage.InsertNote(detached, this.M("InsertNoteRequest")({ text: "after-outcome-unknown" })),
        );
        await h.commit(ctx);
      } else if (req.case === "mismatch") {
        const [h, txCtx] = await distx.begin(ctx, this.tx, distx.beginRequest(CONN));
        try {
          await distx.begin(txCtx, this.tx, distx.beginRequest({ connectionName: "other" }));
        } finally {
          await h.rollback(ctx);
        }
      } else if (req.case === "rollback_only") {
        await distx.run(ctx, this.tx, distx.beginRequest(CONN), async (txCtx) => {
          const [h] = await distx.begin(txCtx, this.tx, distx.beginRequest(CONN));
          await h.rollback(txCtx); // a joined rollback marks the root
        });
      }
    } catch (e) {
      if (e instanceof distx.DistxError) {
        return this.M("TxErrorsResponse")({ error: e.name.replace(/Error$/, ""), message: e.message });
      }
      throw e;
    }
    return this.M("TxErrorsResponse")();
  }

  async Whoami(ctx: Context, _req: AnyMessage): Promise<AnyMessage> {
    const token = this.cfg.secret("api_token");
    return this.M("WhoamiResponse")({
      userId: principal.userId(ctx),
      orgId: principal.scope(ctx, "org_id") ?? "",
      labelTenant: principal.label(ctx, "tenant") ?? "",
      deviceId: principal.deviceId(ctx) ?? "",
      dataScope: principal.dataScope(ctx),
      greeting: this.cfg.str("greeting"),
      limit: BigInt(this.cfg.int("limit")),
      verbose: this.cfg.bool("verbose"),
      tokenRepr: `${String(token)}:${token.reveal().length}`,
    });
  }
}
