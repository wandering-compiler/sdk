"""The conformance suite's NoteService, written against the Python SDK.

Kept as small and literal as the suite allows: each method exercises one
behaviour the Go harness asserts on the wire (see conformance.proto).
"""

from __future__ import annotations

import asyncio

from w17_business import distx, for_user, on_commit, paging, principal
from w17_business.errors import Status

MSG = "conformance.v1."
# The storage connection the transactions open on (the harness registers "main").
CONN = "main"


class NoteService:
    def __init__(self, cfg, registry, clients) -> None:
        self.cfg = cfg
        self.registry = registry
        self.clients = clients
        self.storage = clients.service("conformance.v1.NoteStorage")
        self.tx = clients.distributed_transaction()
        pool = registry.pool
        self.Note = pool.message(MSG + "Note")
        self.InsertNoteRequest = pool.message(MSG + "InsertNoteRequest")
        self.CreateNoteRequest = pool.message(MSG + "CreateNoteRequest")
        self.CreateNotesRequest = pool.message(MSG + "CreateNotesRequest")
        self.CreateNotesResponse = pool.message(MSG + "CreateNotesResponse")
        self.InspectRequest = pool.message(MSG + "InspectRequest")
        self.TxErrorsResponse = pool.message(MSG + "TxErrorsResponse")
        self.WhoamiResponse = pool.message(MSG + "WhoamiResponse")

    @property
    def me(self):
        # Composition goes through the served service, so the emit fires.
        return self.registry.self("conformance.v1.NoteService")

    async def CreateNote(self, ctx, req):
        async def write(tx_ctx):
            return await self.storage.InsertNote(tx_ctx, self.InsertNoteRequest(text=req.text))

        return await distx.run(ctx, self.tx, distx.begin_request(CONN), write)

    async def CreateNotes(self, ctx, req):
        notes = []

        async def work(tx_ctx):
            for text in req.texts:
                if req.nested_connection:
                    # A nested begin on another connection cannot join.
                    await distx.run(tx_ctx, self.tx, distx.begin_request(connection_name=req.nested_connection),
                                    lambda c: self.storage.InsertNote(c, self.InsertNoteRequest(text=text)))
                notes.append(await self.me.CreateNote(tx_ctx, self.CreateNoteRequest(text=text)))
            if req.nested_fail_text:
                try:
                    await self.me.CreateNotes(tx_ctx, self.CreateNotesRequest(texts=[req.nested_fail_text], fail=True))
                except RuntimeError:
                    pass  # swallowed on purpose: the root must still refuse to commit
            if req.on_commit_text:
                text = req.on_commit_text
                detached = ctx.without_cancel()
                await on_commit(tx_ctx, lambda: self.storage.InsertNote(detached, self.InsertNoteRequest(text=text)))
            if req.pause_ms:
                await asyncio.sleep(req.pause_ms / 1000)
            if req.fail:
                raise RuntimeError("refusing after the writes")

        await distx.run(ctx, self.tx, distx.begin_request(CONN), work)
        return self.CreateNotesResponse(notes=notes)

    async def ListNotes(self, ctx, req):
        # A sibling call that must NOT see the page.
        await self.storage.Inspect(ctx, self.InspectRequest())
        return await self.storage.ListNotes(paging.forward(ctx), req)

    async def Inspect(self, ctx, req):
        return await self.storage.Inspect(ctx, req)

    async def Refuse(self, ctx, req):
        raise for_user(Status.FAILED_PRECONDITION, "NOTE_LOCKED", "This note is locked.", "text",
                       dev="note 7 is locked by an export")

    async def Sleep(self, ctx, req):
        await asyncio.sleep(req.ms / 1000)
        return self.Note(text="slept")

    async def TxErrors(self, ctx, req):
        try:
            if req.case == "finished":
                h, _ = await distx.begin(ctx, self.tx, distx.begin_request(CONN))
                await h.commit(ctx)
                await h.commit(ctx)
            elif req.case == "unknown":
                h, tx_ctx = await distx.begin(ctx, self.tx, distx.begin_request(CONN))
                await self.storage.InsertNote(tx_ctx, self.InsertNoteRequest(text="outcome-unknown"))
                await h.commit(ctx)
            elif req.case == "mismatch":
                h, tx_ctx = await distx.begin(ctx, self.tx, distx.begin_request(CONN))
                try:
                    await distx.begin(tx_ctx, self.tx, distx.begin_request(connection_name="other"))
                finally:
                    await h.rollback(ctx)
            elif req.case == "rollback_only":
                async def work(tx_ctx):
                    h, _ = await distx.begin(tx_ctx, self.tx, distx.begin_request(CONN))
                    await h.rollback(tx_ctx)  # a joined rollback marks the root

                await distx.run(ctx, self.tx, distx.begin_request(CONN), work)
        except distx.DistxError as e:
            return self.TxErrorsResponse(error=type(e).__name__.removesuffix("Error"), message=str(e))
        return self.TxErrorsResponse()

    async def Whoami(self, ctx, req):
        return self.WhoamiResponse(
            user_id=principal.user_id(ctx),
            org_id=principal.scope(ctx, "org_id") or "",
            label_tenant=principal.label(ctx, "tenant") or "",
            device_id=principal.device_id(ctx) or "",
            data_scope=principal.data_scope(ctx),
            greeting=self.cfg.str("greeting"),
            limit=self.cfg.int("limit"),
            verbose=self.cfg.bool("verbose"),
            token_repr=repr(self.cfg.secret("api_token")) + ":" + str(len(self.cfg.secret("api_token").reveal())),
        )

