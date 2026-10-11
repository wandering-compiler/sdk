# Feature-interaction matrix

Every pair of features whose combination changes behaviour, the behaviour the combination must have, and the test that proves it. Feature IDs refer to the area files ([server.md](server.md), [edges.md](edges.md), [data.md](data.md), [events-identity.md](events-identity.md), [tooling.md](tooling.md)); a feature without an ID is named descriptively. A pair is listed once, under the pair of areas it spans. **no test** means no test covers the combination; a test followed by a note covers only the part the note names.

122 interactions; 59 have no test, 11 more are covered only in part.

## Server × Server

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Component supervisor (S1) | gRPC service component (S5) | A clean SIGTERM gives a `nil` result and exit status 0. | no test |
| Ordered resource teardown (S4) | Bounded gRPC stop (S12) | Pools close only after handlers have ended; teardown after a forced stop must tolerate abandoned handlers. | no test |
| gRPC panic recovery (S8) | Interceptor and server-option order (S6) | Every caller-supplied unary interceptor, including one passed through `ServerOptions`, runs inside recovery. | no test |
| Server reflection (S10) | Internal-mesh TLS, server side (S17) | Reflection is reachable only by trusted, authenticated peers. | no test |
| Internal-mesh TLS, server side (S17) | gRPC service component (S5) | With the switch on, the listener built by `runtime.GRPCComponent` / `runtime.Serve` serves TLS. | no test |
| Internal-mesh TLS, server side (S17) | `health` subcommand (S16) | The gRPC check dials with the listener's posture (plaintext, server-auth TLS, mutual TLS, `terminated`). | `TestRun_GRPCUnderInternalTLS` (server-auth TLS only) |
| Internal-mesh TLS, client side (S18) | `terminated` mode with a private CA variable set | The client verifies against system roots. | no test |
| Internal-mesh TLS (S17, S18) | Observability bootstrap (S19) | Telemetry export follows the mesh's transport-security posture. | no test |
| Keepalive server enforcement (S7) | Keepalive client parameters (S7), `ServerOptions` override | Client pings are always permitted by the server's enforcement policy. | `TestDefaultClientKeepalive_PairsWithServerEnforcement` (client values against a literal) |
| In-process gRPC bridge (S21) | gRPC service component (S5) | The same tier interceptor chain runs in a composed binary and in a split deployment. | `TestConn_WithUnaryInterceptors_ChainOrder` (bridge only) |
| In-process gRPC bridge (S21) | Component supervisor shutdown (S1) | A call on a cancelled context is refused; teardown uses a fresh context. | `TestInvoke_RefusesADoneContextLikeTheWire` |
| Observability bootstrap (S19) | Panic containment and recovery (S3, S8, S21) | A panic is reported with its stack; the client sees only a generic message. | `TestRecoveryUnaryInterceptor_RecoversPanic` (client side) |
| Error and event reporting (S20) | Concurrent requests | Each reported event carries only its own request's context and breadcrumbs. | no test |
| Observability bootstrap (S19) | Several supervisors in one process (tests, composed binaries) | One init per process; a second init must not silently swap state under running components. | no test |

## Server × Edges

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Component supervisor (S1) | REST and MCP HTTP transports | Every transport drains in parallel on one context, each within its own bound; process exit time is the largest transport bound plus teardown. | no test |
| gRPC panic recovery (S8) | Authentication and ACL interceptors | Authentication runs inside recovery and before the handler. | no test |
| gRPC health service (S11) | Authentication and ACL interceptors | Health is reachable without credentials. | `TestDefaultHealthServer` (status only) |
| In-process gRPC bridge (S21) | REST gateway status mapping | The same failure maps to the same HTTP status in a composed and in a split deployment. | no test |
| In-process gRPC bridge (S21) | Per-client rate limiter | In-process callers are keyed per caller, not into one shared bucket. | no test |
| Bounded HTTP stop and drain (S13) | MCP session sweeper | `StopWith` runs the sweeper, then a bounded `Shutdown` that force-closes on deadline. | no test |
| Error and refusal reporting (S20) | Authentication and ACL refusals under load | Each refusal is recorded, with bounded log volume. | no test |
| Client dial retry (S18, E30) | Non-idempotent handlers | Only `UNAVAILABLE` is retried, so a request is retried only when it never reached the server. | `TestDefaultServiceConfig_RetriesOnlyUnavailable` |
| Bounded HTTP stop and drain (S13) | `w17-events` (E18), MCP GET stream (E27), per-RPC SSE/WS (E14, E15) | `w17-events` and the MCP GET stream end at drain start, so shutdown is fast and clients reconnect to another instance; per-RPC streams end by the HTTP server's force-close. | `TestStopHTTPDrainAwareStreamEndsImmediately`, `TestW17EventsEndsOnDrain`, `w17events_drain_test.go`, `serve_shutdown_test.go` (per-RPC streams: no test) |
| Server TLS posture | MCP HTTP listener (E27) | MCP follows the same TLS posture and limits as the other surfaces. | no test |

## Server × Data

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Ordered resource teardown (S4) | Event bus publish and flush | The bus is flushed after transports drain; a flush failure is reported. | no test |
| In-process gRPC bridge (S21) | Distributed transactions | A nested call does not inherit the caller's transaction. | `TestConn_NestedCall_SeesOnlyWhatAWireHopWouldDeliver` |
| Graceful shutdown, `Shutdowns` hooks (S5) | Transaction registry (D5) | Pools close after the server stops and before the orchestrator's kill deadline; open distributed transactions are not RPCs and are not drained, so process exit rolls them back and their parked after-commit work is dropped. | no test |
| Graceful shutdown | Wait for the database at boot (D2) | A SIGTERM during the boot wait MUST stop the wait through context cancellation. | `TestPing_CancelledContextStopsTheWait` |

## Server × Events and identity

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| In-process gRPC bridge (S21) | Principal propagation | The gateway stamps (user, scopes, labels) are relayed as on the wire, nothing more. | `TestConn_UnauthenticatedCaller_StaysUnauthenticated`, metadata isolation tests in `go/service/inprocgrpc` |
| In-process gRPC bridge (S21) | i18n language metadata | `w17-language` set as outgoing by the caller reaches the handler as incoming; a language present only on the caller's incoming side is not relayed, as on the wire. | `TestConn_CallerWithNoOutgoingMetadata_DoesNotLeakItsIncoming` |
| Process shutdown | Bus subscribers (I2, I4–I6) | `Close` drains every subscriber on every transport; registrations after close or drain are refused. | `TestNatsBus_SubscriberAfterCloseRefused_Embedded`, `TestRedisBus_SubscriberAfterCloseRefused_Embedded`, `TestNatsSubscriber_SubscribeAfterDrainRefused_Embedded`, `TestRedisSubscriber_SubscribeAfterDrainRefused_Embedded` (memory transport: no test) |
| Internal-mesh TLS | Principal read side (I10) | Backend tiers that trust the principal metadata are reachable only from authenticated tiers. | no test |

## Server × Tooling

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| `health` subcommand (S16) | Command dispatch (T1) | `health` is answered before the server starts and other words go to the next dispatcher; every reserved root is answered; a binary with migration commands must not set `Final=true` ahead of them. | `TestDispatch_FinalOwnsTheCommandTable`, `TestDispatch_AnswersForEveryReservedRoot` (`migrate`, `fixtures` only) |
| Internal-mesh TLS (server and client) | Dev PKI (T27) | The leaf chains to the current CA and covers every dial name. | no test |

## Edges × Edges

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Per-client rate limiter (E26) | Proxy in front of the gateway | The key is the rightmost untrusted `X-Forwarded-For` hop when the peer is trusted, else the peer; IPv6 per /64; a client MUST NOT be able to choose its bucket. | `TestForwardedPicksRightmostUntrusted`, `TestLimiter_WarnsWhenEveryCallerCollapsesIntoOneBucket` (partial: client-chosen keys have no test) |
| Per-client rate limiter (E26) | SSE and WebSocket streams (E14, E15) | Public streams are charged once per open, not per frame; authenticated streams are not limited. | `TestAdmitPublicStream_ChargesEveryStream` (partial: no per-client concurrent-stream cap test) |
| Authentication hook (E5) | Header → metadata propagation (E10) on auth-excluded routes | Public routes skip auth and carry no `x-w17-user`; no client header can reach the backend as a gateway-owned key. | no test |
| Public-method rate-limit adapters (E19) | Auth outcome / handler status | A unary public method is charged on status ≥ 400; concurrent requests MUST NOT exceed the budget. | `TestAdmitPublic_ChargesFailuresPerClient` (partial: concurrency has no test) |
| Stream re-auth watchdog (E17) | `w17-events` channel (E18) and per-RPC streams (E14, E15) | Every authenticated stream is re-probed every interval; a revoked credential ends it; an auth-backend outage does not. | Re-auth unit tests (`TestWatchStreamAuth_*`); no test for the `w17-events` pairing |
| CORS (E12) | WebSocket helpers (E15) | Browser access to WebSocket routes follows the documented same-origin rule, independent of the CORS allowlist. | no test |
| Auth result cache (E6) | Stream re-auth watchdog (E17) | Revocation becomes visible within `max(reauth interval, TTL)`. | no test |
| URL-carried credentials (E7) | Request tracing (E11), pagination links (E23), browser redirects (E22) | The credential reaches auth only through the reserved slot. | no test |
| URL-carried credentials (E7) | Auth result cache (E6) | Distinct capability tokens never share a cache slot (the slot is a key header). | `TestAuth_CachedAuthFunc_DistinctHeadersDistinctSlots` (partial: no explicit two-token test) |
| Organization from the path (E8) | Auth result cache (E6) | A request with `W17-Org` bypasses the cache, so every org-scoped call makes a full auth call. | `TestAuth_CachedAuthFunc_UnknownHeaderBypassesCache` (indirect) |
| Browser stream tickets (E16) | Multiple gateway replicas | Issue and redeem happen on the same process (documented limit: per-process store). | no test |
| Database error mapping (E33) | Model Context Protocol surface (E27) | Constraint and table names appear only in the operator message, never in a client-visible result. | no test |
| Multipart uploads (E20) | Request body limits (E1, generated `MaxBytesReader`) | The per-file cap applies here and the request cap comes from generated code; an unbounded body is never spilled to disk. | no test |
| Multipart uploads (E20) | Downloads (E21) | Both sides use the same key shape; authorization is per endpoint, not per object. | `TestDownloadURLRoundTrip` |
| JSON dialect (E2) | Oneof collapse and expand (E29) | Values decoded through the alias and oneof reshaping are exactly the values protojson would decode, on every surface (REST and MCP). | no test |
| MCP server (E27) | MCP auth translation (E28) per transport | stdio uses the environment identity; HTTP uses request headers only. | no test |
| HTTP panic recovery (E13) | Public-method rate limit (E19), SSE (E14) | A panic yields a 500 envelope and an operator report; a panicking public method is still charged. | no test |
| Auth result cache (E6) | Browser stream tickets (E16) promotion | Probes after promotion keep working; the promotion marker forces a cache bypass. | `TestWatchStreamAuth_TicketStreamSurvivesTheTick` (behaviour, not cache) |
| Connection pool health watch (E32) | Long-lived callers | Failed connections are replaced without closing a connection a caller still holds. | no test |
| `w17-events` channel (E18) | SSE write deadline (E14) and slow clients | A stalled client is dropped after the write deadline; the hub never blocks. | `w17events_drain_test.go` (partial: no stalled-client test) |

## Edges × Data

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Multipart uploads (E20) | Blob store (D13) | Rolling back a failed request MUST NOT remove an object another request references. | no test |
| Database error mapping (E33) | Database outage | A transient database failure is surfaced as retryable and not reported as a fault. | no test |
| REST gateway limit | Cursor paging (D9) | The gateway clamps `?limit=` and forwards it; storage applies it; no caller receives an unbounded result. | `TestClampLimit` (gateway side) |
| REST gateway auth and data scopes | Cursor paging (D9) | Each resumed page re-authorizes from the caller's verified identity; scopes come from metadata, not from request fields in the cursor. | no test |
| REST upload and download | Blob store (D13) | The gateway writes via `BuildKey`; readers use the same key form; a published object is never seen partially written. | `kvfs_test.go`, `TestPutFromTempFile_CrossFS_ReaderNeverSeesPartialBody` |
| Error mapping and translation | SQL helpers `ExpandIn` (D8) | A list that exceeds the driver's variable limit MUST surface as a client error, not an internal error. | no test |

## Edges × Events and identity

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Error envelope (E3), refusals (E4), database error mapping (E33) | i18n locale | Detail sentences are translated with the request locale; the context-free writers answer in the source locale. | `constraint_locale_test.go`, `TestPreflightSentence_AgreesWithTheDemotionUnderALocale` (context-free paths: no test) |
| Inter-tier principal relay (E31) | Event labels and data scopes | The business tier forwards principal, scopes and labels to storage, for unary and stream calls. | `TestForwardPrincipalUnaryInterceptor` (unary only) |
| Gateway tier rendering | Catalogs and formatting (I14, I15) | Gateway-rendered messages use the caller's language and explicit format locale from outgoing metadata. | `TestLocaleFromContext_OutgoingMetadata` (language); format locale: no test |

## Edges × Tooling

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| REST error mapping | E2E MCP caller (T24) | The same refusal code is assertable over REST and MCP where the MCP refusal carries a status. | `TestGrpcStatusFromMCPError_NonStatusErrorsStayTransportFailures` |
| SSE gateway | E2E streams (T25) | Frames arrive in order; close or an error frame ends the stream; non-JSON frames fail the step. | `TestExpectStream_*` |
| Authentication and sign-out flows | E2E auth threading (T20) | Each step uses the intended principal, including none, so a signed-out refusal can be asserted. | `TestRunScenario_MissingAuthToken` (missing-token refusal only) |

## Data × Data

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Nested transactions (D4) | After-commit work (D6), in-process emit | A callee's emit parks on the root scope, runs once after the root commits, and is dropped on rollback or cancellation. | `TestEmit_ANestedBusinessEmitWaitsForTheRootCommit`, `TestEmit_NothingIsAnnouncedForARolledBackTransaction`, `TestEmit_ACancelledRootAnnouncesNothing` |
| Auto-rollback interceptor (D7) | Transaction registry adoption lease (D5) | A failed participant MUST doom the transaction even while another participant holds the lease. | no test |
| Auto-rollback interceptor (D7) | Transaction control plane (D5) | A failed Begin or Commit MUST NOT trigger an automatic rollback. | `controlplane_test.go` |
| Transaction lifecycle (D3) | Client disconnect or deadline | Rollback runs on a detached context; no commit is made for a caller that is gone. | `TestRunRollsBackEvenWhenTheCallersContextIsDead`, `TestRunDoesNotCommitOnBehalfOfAVanishedCaller` |
| Orphan timeout (D5) | Statement timeout | Per-method statement timeout stays below the finish wait (30 s), which stays below the orphan timeout (5 min); the watcher drains an expired entry even under a held lease. | `TestServer_Begin_DefaultTxTimeout_*`, `TestTimeoutWatcher_DrainsUnderALease` |
| Transaction isolation (D5) | Database dialect | The declared level is pinned at Begin; an adopted weaker transaction is refused. | no test |
| Transaction lifecycle (D3) | Deadlock or serialization failure | The error surfaces to the caller; inside a distributed transaction it discards the transaction, and the caller restarts the whole `Run`. | no test |
| Cursor paging (D9) | Admin sort selector (D10) | The cursor's sort selector is authoritative; changing the sort restarts at page one. | `TestEncodeDecode_SortSelectorRoundtrip` |
| Cursor paging totals (D9) | Concurrent writes | The total is counted once per paging session and cached in the cursor. | no test |
| Bounded materialization (D11) | Adopted transaction or locked query (D5) | Fetch windows run sequentially on the transaction. | no test |
| Bounded materialization (D11) | Cursor paging (D9) | The guard bounds intermediate rows independently of page size. | no test |
| Column encryption (D16) | UNIQUE index and equality lookups | Deterministic mode keeps the index and `WHERE col = :x` working, including across key rotation. | no test |
| Column encryption (D16) | Cursor paging (D9) | A crypted column used as a keyset key orders by ciphertext; whether to allow it is the generator's decision. | no test |
| SQLite collation (D12) | Session pins (D1) | Every process that opens the database registers the collation and applies the same session pins; equality matches PostgreSQL. | `TestRegister_UnicodeOrderingAndEquality` (ordering) |
| Redis HASH codec (D14) | KV multi-fetch (`DedupKeys`, D8) | Multi-fetch multiplicity equals SQL `IN`. | `TestDedupKeys` |
| Redis HASH codec (D14) | Redis eviction check (D15) | Entities live on a non-evicting Redis; an `allkeys-*` policy produces a boot warning. | `TestEvictsUntaggedKeys` (classification) |

## Data × Events and identity

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| After-commit work (D6) | Event bus publish (cross-process) | A storage handler's emit is registered on commit, published after the coordinator's Commit, and dropped on rollback or drain; delivery is at-most-once. | `oncommit_test.go`, `phantom_emit_test.go`; loss between commit and publish: no test |
| Transaction routing metadata (D4, D5) | Event bus subscriber context | Event envelopes MUST NOT carry transaction routing metadata into a subscriber, so a subscriber is never a participant. | `TestHeaderName_IsTheStrippedEnvelopeKey` (transaction id) |
| Cursor paging metadata (D9) | Principal relay (`ForwardToOutgoing`) | Paging headers are never relayed hop-to-hop. | no test in the data packages (relay tests live in `go/lib/principal`) |
| Secret values (D17) | Structured logging | A `Secret` redacts on every logging path, wherever it is held. | `TestSlogRedacts`, `TestStructRedacts` (exported fields) |
| Column encryption (D16) | Error layer and logs | Decryption errors carry no plaintext or key material. | `TestDecrypt_*` |
| In-process transaction scope | In-memory transport (I4) | A memory subscriber does not inherit the emitter's transaction. | no test |

## Data × Tooling

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| SQLite collation stamp (D12) | Migration applier | The applier stamps; the runtime verifies and refuses a known mismatch. | tested in the SQLite migration driver, not in-package |
| Session pins (D1) | Migration applier and runtime | Applier and runtime open the database with identical pins. | `TestApplyMySQLDSNParams_KeepsAnOperatorCollation`, `TestMySQLDSNParams_PinsCollation` (MySQL collation) |

## Events and identity × Events and identity

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Event context (I8) | Principal relay into subscribers (I10) | The subscriber runs with the emitter's principal and scopes carried in `Event.Metadata`, and the metadata carries no credentials. | `TestEventContextReaders_*` (scopes and labels); credentials: no test |
| Event labels (I8) | Server-sent-events hub audience (I9) | Labelled events reach only principals holding every label; unlabelled events reach nobody under isolation. | `TestEventbusEventSource_LabelFiltering`, `TestEventbusEventSource_UnlabelledEvent*`, `TestEmitInvalidate_*` |
| Principal, gateway side (I11) | Event context (I8) | A client cannot inject `x-w17-*` or `w17-label-*` keys into an event envelope. | `TestWithGatewayMD_SanitizesBothSides` |
| Principal relay (I12) | Dispatcher (I1) | An emit reached through several tiers keeps its scopes and labels. | `TestForwardToOutgoing_SecondHopKeepsTheLabel` |
| Catalogs and formatting (I14, I15) | Event context (I8) | Subscriber messages use the requester's language and format locale. | no test |
| Server-sent-events hub (I9) | Delivery modes (I3) | In fan-out mode every replica's hub receives every event. | `TestEventbusEventSource_EveryReplicaReceivesEveryEvent` |
| Server-sent-events hub (I9) | NATS JetStream transport (I5) | The hub resumes delivery after a broker loss, including a recreated broker. | no test |
| Server-sent-events hub (I9) | Slow client | A slow client loses only its own frames; other clients are unaffected. | no test |
| Server-sent-events hub (I9) | Client reconnect | The client reconnects automatically; events emitted during the gap are not replayed (deliberate). | no test |
| Permission checks (I13) | Multi-domain auth response | A permission check uses the lock of the handler's own domain. | no test |
| Handler panic recovery (I2) | Broker retry (I5, I6) and Observer (I7) | A handler panic is recovered, retried and dead-lettered at the cap; an Observer panic does not crash the emitter. | `TestRecoverHandler_PanicBecomesError` (handler); Observer: no test |

## Events and identity × Tooling

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Event fan-out with tenant scoping | E2E event awaits (T25) | A suite can assert that tenant B does not receive tenant A's event, and an await matches only this step's event. | no test |

## Tooling × Tooling

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Apply run (T11), Postgres applier (T15) | Concurrent appliers (several replicas at boot) | Exactly one applies; the others skip under the head re-read or fail on the ledger key without reporting the deploy as failed. | no test |
| MySQL applier and run lock (T15) | Concurrent appliers | `GET_LOCK` serialises; the loser fails fast; a lost lock session fails the run. | `TestRunLock_SurvivesAnIdleServerTimeout` (lock session only) |
| Redis/S3 run lock (T15) | Long data migration (T16) | The lock is held for the whole migration; a holder that loses it stops before writing again. | no test |
| Squash adopt (T10) | Rollback by another run while this one waits on the lock (T11) | A head that moved backward is refused. | `TestApply_RefusesWhenTheHeadMovedBACKWARD` |
| Post-transaction resume (T11) | Crash between halves on each dialect (T15) | Postgres resumes only the post-transaction half; MySQL and SQLite warn at run time that the migration is not resumable. | `TestRun_ResumesPendingSkirt` (Postgres, stub applier) |
| Drift gate (T13) | Console-emitted expectations | Every migration that carries a real expectation is compared; placeholders are skipped explicitly. | no test |
| Drift gate (T13) | Fake apply (T12) | The gate is skipped and the adopt preflight is the check; only integrity-checked preflight content is executed. | no test |
| Extension preflight (T11) | Applier decorators | `Unwrap` reaches the Postgres applier through every decorator, or the run refuses. | `TestPreflightExtensions_SurvivesADecorator` |
| Schema prep (T11) | Wipe (T15) | Wipe returns the store to empty, including namespaces schema prep created. | no test |
| `apply --fetch` (T4) | Read-only container | No disk writes; the same hash check as artefacts on disk. | `TestMigrationSetFS_RefusesATamperedBody` |
| `migrate fetch` (T4) | Path guard (T28) | Every fetched artefact lands under `--migrations`. | no test |
| Lock file (T2) | Artefact chain (T7, T8) | The pinned hash anchors the chain; only chained, hash-matching artefacts execute. | `TestPlan_TargetHashMismatch`, `TestPlan_InsertedMigrationIsNotOnTheChain` |
| Apply timeout (T2) | Data migration (T16) | A migration cut by the timeout leaves a state a later run resumes or refuses with a clear remedy. | no test |
| Fixtures (T6) | Dialect appliers (T15) | Only stores that can seed are offered as seed targets; the group is one transaction. | `TestFixturesApply_TheWholeGroupIsOneUnitOfWork` (Postgres) |
| Fixtures (T6) | Migrations (T11) | Seeding runs after the schema exists; the stale-render check compares fixtures with their render. | `TestFixturesApply_RefusesARenderOlderThanItsFixture` |
| E2E credential step (T20) | Event awaits (T25) | Event visibility is asserted for the credential's principal. | no test |
| E2E `${seq}` (T21) | Several runner processes on one database | Generated values stay unique per process; parallel processes need distinct data. | `TestSeqGlobalAcrossScopes` (in-process) |
| E2E matchers (T22) | Proto3 JSON zero values | Absent and zero are distinguished: `eq 0` explains presence; `count`, `empty` and `capture` fail on an absent field. | `TestExactMatcher_ZeroAgainstAnAbsentFieldExplainsPresence` (`eq` only) |
| Stress (T26) | E2E runner routing (T23) | Load requests are routed like runner requests (`ResolveREST`) and carry the same credentials. | no test |
| Plugin digest (T29) | Code generation | Every compiled file is covered by the digest or regenerated from covered input. | `TestOf_IgnoresRegeneratedPb` (skip rule only) |
