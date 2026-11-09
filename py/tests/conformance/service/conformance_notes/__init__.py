"""The conformance suite's NoteService, written against the Python SDK.

Kept as small and literal as the suite allows: each method exercises one
behaviour the Go harness asserts on the wire (see conformance.proto).
"""

from __future__ import annotations

import asyncio

from grpclib.exceptions import GRPCError

from w17_business import distx, files, for_user, on_commit, paging, principal
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
        self.EchoResponse = pool.message(MSG + "EchoResponse")
        self.AttachFileResponse = pool.message(MSG + "AttachFileResponse")
        self.GetEchoRequest = pool.message("clients.echo.GetEchoRequest")
        # The companion's clients: an egress client and two file stores.
        self.echo = clients.egress("echo")
        self.files = clients.store("notes-files")
        self.media = clients.store("notes-media")

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
        # Sibling calls that must NOT see the page, nor move it: one before the
        # forwarded call and one after it, so a relay that ignored the forward
        # mark is caught whichever position (first or last) would win.
        await self.storage.Inspect(ctx, self.InspectRequest())
        resp = await self.storage.ListNotes(paging.forward(ctx), req)
        await self.storage.Inspect(ctx, self.InspectRequest())
        return resp

    async def Inspect(self, ctx, req):
        if req.stale_outgoing:
            # A context reused from another request: the relay must replace
            # this whole set with the verified incoming principal.
            ctx = ctx.with_outgoing(ctx.outgoing.merged({
                "x-w17-user": ["stale-user"],
                "x-w17-scope-org_id": ["stale-org"],
                "x-w17-audience": ["stale-audience"],
                "w17-label-tenant": ["stale-tenant"],
                "x-w17-scope-stale_only": ["leak"],
            }))
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
                # Work parked on the transaction, which a commit of unknown
                # outcome must not run: a composed method's event, and a hook.
                await self.me.CreateNote(tx_ctx, self.CreateNoteRequest(text="outcome-unknown-composed"))
                detached = ctx.without_cancel()
                await on_commit(tx_ctx, lambda: self.storage.InsertNote(
                    detached, self.InsertNoteRequest(text="after-outcome-unknown")))
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


    async def Echo(self, ctx, req):
        # Inside a transaction: the companion call must not carry it.
        async def call(tx_ctx):
            return await self.echo.GetEcho(tx_ctx, self.GetEchoRequest(id=req.id))

        reply = await distx.run(ctx, self.tx, distx.begin_request(CONN), call)
        return self.EchoResponse(message=reply.message)

    async def AttachFile(self, ctx, req):
        if req.case == "read_only_delete":
            try:
                await self.media.delete(ctx, "any")
            except PermissionError as e:
                return self.AttachFileResponse(refused=str(e))
            return self.AttachFileResponse()
        body = bytes(i % 251 for i in range(req.size))
        if req.part:
            data = body

            def parts():
                for i in range(0, len(data), req.part):
                    yield data[i:i + req.part]

            body_arg = parts()
        else:
            body_arg = body

        async def work(tx_ctx):
            handle = await self.files.put(tx_ctx, "conformance.v1.NoteFile.file", body_arg)
            info = await self.files.stat(tx_ctx, handle)
            equal = await self.files.read(tx_ctx, handle) == body
            await self.files.delete(tx_ctx, handle)
            try:
                await self.files.read(tx_ctx, handle)
                gone = False
            except GRPCError as e:
                gone = files.is_not_found(e)
            return self.AttachFileResponse(
                handle=handle, stat_size=info.size, mod_time_ms=int(info.mod_time.timestamp() * 1000),
                read_back_equal=equal, not_found_after_delete=gone)

        return await distx.run(ctx, self.tx, distx.begin_request(CONN), work)
