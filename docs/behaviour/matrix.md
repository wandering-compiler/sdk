# Feature-interaction matrix

Every pair of features whose combination changes behaviour, the behaviour the combination must have, and the test that proves it. Feature IDs refer to the area files ([server.md](server.md), [edges.md](edges.md), [data.md](data.md), [events-identity.md](events-identity.md), [tooling.md](tooling.md)); a feature without an ID is named descriptively. A pair is listed once, under the pair of areas it spans. **no test** means no test covers the combination; a test followed by a note covers only the part the note names.

129 interactions; 20 have no test, 32 more are covered only in part.

## Server × Server

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Component supervisor (S1) | gRPC service component (S5) | A clean, context-driven stop (what SIGTERM triggers) gives a `nil` result and exit status 0. | `TestGRPCComponent_CleanStopReturnsNil` (context cancel; no signal sent) |
| Ordered resource teardown (S4) | Bounded gRPC stop (S12) | Pools close only after handlers have ended; teardown after a forced stop must tolerate abandoned handlers. | `TestRun_ServeErrorStopsServerBeforeOnShutdown` (serve-error path only) |
| gRPC panic recovery (S8) | Interceptor and server-option order (S6) | Every caller-supplied unary interceptor, including one chained in through `ServerOptions`, runs inside recovery; a singular interceptor option, which would run outside it, is refused at start. | `TestGRPCComponent_RecoveryWrapsCallerInterceptors`, `TestGRPCComponent_SingularInterceptorOptionIsRefused` |
| Server reflection (S10) | Internal-mesh TLS, server side (S17) | Mesh TLS authenticates the peer but does not authorize reflection: whatever the TLS posture, reflection is served only behind a `ReflectionAuth` guard (both protocol versions) or an explicit, logged `ReflectionUnauthenticated` opt-in, and an unguarded `Reflection: true` is refused at start. | `TestRun_UnguardedReflectionIsRefused`, `TestRun_ReflectionAuthGuardsBothVersions`, `TestRun_ReflectionAuthStatusPassesThrough`, `TestGRPCComponent_UnguardedReflectionIsRefused` (guard only; not exercised under mutual TLS) |
| Component supervisor (S1) | Observability flush (S19) | The default shutdown deadline plus the telemetry flush ends before a 30 s orchestrator grace period; a second signal during the drain exits at once with status 1. | `TestDefaultShutdownDeadline_FitsTheCommonGracePeriod`, `TestRun_SecondSignalExitsImmediately` |
| Internal-mesh TLS, server side (S17) | gRPC service component (S5) | With the switch on, the listener built by `runtime.GRPCComponent` / `runtime.Serve` serves TLS and refuses a plaintext client; a misconfigured or unrecognised switch fails the start. | `TestGRPCComponent_ServesInternalTLS`, `TestGRPCComponent_InternalTLSMisconfigurationFailsStart` |
| Shutdown timeout from the environment (S14) | gRPC service component (S5) | The operator's opt-out stays unbounded in `Config.ShutdownTimeout`, where `0` means 10s. | `TestShutdownTimeout_EnvOptOutStaysUnbounded` |
| gRPC health service (S11) | Bounded gRPC stop (S12) | Health reports NOT_SERVING once the drain starts, while in-flight RPCs still finish. | `TestRun_HealthReportsNotServingOnceDrainStarts` |
| Internal-mesh TLS, server side (S17) | `health` subcommand (S16) | The gRPC check dials with the listener's posture (plaintext, server-auth TLS, mutual TLS, `terminated` as plaintext); under mutual TLS a leaf without client-auth usage is diagnosed by name before dialing. | `TestRun_AllListenersAnswer`, `TestRun_GRPCUnderInternalTLS`, `TestRun_GRPCUnderMutualTLS`, `TestRun_GRPCUnderMutualTLS_ServerOnlyLeafIsDiagnosed`, `TestRun_GRPCUnderTerminatedIsPlaintext` |
| Internal-mesh TLS, client side (S18) | `terminated` mode with a private CA variable set | The client verifies against system roots only; the private CA and client pair are ignored with a warning, in both client builders. | `TestClientTLSConfig_TerminatedUsesSystemRootsOnly`, `TestClientTLSConfig_TerminatedIgnoresPrivateCA` |
| Internal-mesh TLS (S17, S18) | Observability bootstrap (S19) | Telemetry export follows the mesh's transport-security posture (TLS with the mesh CA and client pair, system roots under `terminated`); plaintext only when the mesh is off or the operator sets `W17_OTEL_TLS=off`, which is logged while the mesh is on. | `TestOTLPExport_FollowsInternalTLS`, `TestOTLPExport_PlaintextOnlyWhenOffOrOptedOut`, `TestMustInit_RejectsUnknownOTelTLS` |
| Keepalive server enforcement (S7) | Keepalive client parameters (S7), `ServerOptions` override | Client pings are always permitted by the server's enforcement policy. | `TestDefaultClientKeepalive_PairsWithServerEnforcement` (client values against a literal) |
| In-process gRPC bridge (S21) | gRPC service component (S5) | The same tier interceptor chain runs in a composed binary and in a split deployment. | `TestConn_WithUnaryInterceptors_ChainOrder` (bridge only) |
| In-process gRPC bridge (S21) | Component supervisor shutdown (S1) | A call on a cancelled context is refused; teardown uses a fresh context. | `TestInvoke_RefusesADoneContextLikeTheWire` |
| Observability bootstrap (S19) | Panic containment and recovery (S3, S8, S21) | A panic is reported with its stack; the client sees only a generic message. | `TestRecoveryUnaryInterceptor_RecoversPanic` (client side) |
| Error and event reporting (S20) | Concurrent requests | Each reported event carries only its own request's context and breadcrumbs: the recovery interceptors, the HTTP panic middleware and MCP tool calls give each request its own scope, and without a scope no breadcrumb is recorded. | `TestReportError_DoesNotCarryAnotherRequestsBreadcrumbs`, `TestRequestScope_KeepsBreadcrumbsPerRequest`, `TestRecoveryInterceptors_GiveEachRequestItsOwnScope` |
| Observability bootstrap (S19) | Several supervisors in one process (tests, composed binaries) | One init per process; a second supervisor with the same configuration shares it and one with a different configuration is refused (`ErrHeld`), so state is never swapped under running components. | `TestRun_SecondSupervisorDoesNotSwapObservability`, `TestAcquire_HeldStateIsNotReplaced`, `TestAcquire_SameConfigSharesUntilLastRelease` |

## Server × Edges

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Component supervisor (S1) | REST and MCP HTTP transports | Every transport drains in parallel on one context, each within its own bound; drain plus resource teardown together end within `W17_SHUTDOWN_DEADLINE_SECONDS` (default 20 s) from the start of shutdown, and an overrun is returned as an error naming the deadline. | `TestRun_ShutdownDeadlineBoundsAStuckComponent`, `TestRunGraceful_ShutdownDeadlineBoundsResourceTeardown` (overall bound only; parallel drain of the HTTP transports: no test) |
| gRPC panic recovery (S8) | Authentication and ACL interceptors | Authentication runs inside recovery and before the handler. | `TestGRPCComponent_RecoveryWrapsCallerInterceptors`; the authentication and ACL interceptors are caller interceptors |
| gRPC health service (S11) | Authentication and ACL interceptors | Health is reachable without credentials. | `TestDefaultHealthServer` (status only) |
| In-process gRPC bridge (S21) | REST gateway status mapping | The same failure maps to the same HTTP status in a composed and in a split deployment. | `TestConn_ErrorsMatchTheWire` |
| In-process gRPC bridge (S21) | Per-client rate limiter | In-process callers are keyed per caller, not into one shared bucket. | no test |
| Bounded HTTP stop and drain (S13) | MCP session sweeper | `StopWith` runs the sweeper, then a bounded `Shutdown` that force-closes on deadline. | `TestServeStreamableHTTP_ForceClosesAToolCallPastTheBound`, `TestServeStreamableHTTPDrainsWithOpenStream` |
| Error and refusal reporting (S20) | Authentication and ACL refusals under load | Each refusal is recorded; the refusal log line is rate-limited per channel and the suppressed count is logged with the next written line and at shutdown. | `TestReportRefusal_RecordsWithoutRaising`, `TestReportRefusal_LogVolumeIsBounded`, `TestShutdown_ReportsPendingSuppressedLines` |
| Internal-mesh TLS switch (S16, S17, S18) | Inter-tier dial (E31) | An unrecognised `W17_INTERNAL_TLS` value is refused by the listener, the dialer and the health check, and makes `grpcx.InternalTLSEnabled` dial TLS; no side reads it as plaintext. | `TestTLSServerOption_UnrecognisedSwitchIsRefused`, `TestTLSDialOption_UnrecognisedSwitchIsRefused`, `TestRun_GRPCUnrecognisedTLSSwitchFails`, `TestInternalTLSEnabled_UnrecognisedValueFailsClosed` |
| Client dial retry (S18, E30) | Non-idempotent handlers | Only `UNAVAILABLE` is retried, so a request is retried only when it never reached the server. | `TestDefaultServiceConfig_RetriesOnlyUnavailable` |
| Bounded HTTP stop and drain (S13) | `w17-events` (E18), MCP GET stream (E27), per-RPC SSE/WS (E14, E15) | `w17-events` and the MCP GET stream end at drain start, so shutdown is fast and clients reconnect to another instance; per-RPC streams end by the HTTP server's force-close. | `TestStopHTTPDrainAwareStreamEndsImmediately`, `TestW17EventsEndsOnDrain`, `w17events_drain_test.go`, `serve_shutdown_test.go` (per-RPC streams: no test) |
| Server TLS posture | MCP HTTP listener (E27) | MCP follows the same TLS posture and limits as the other surfaces. | no test |

## Server × Data

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Ordered resource teardown (S4) | Event bus publish and flush | The bus is flushed after transports drain; a flush failure is reported. | `TestRunGraceful_ResourcesCloseAfterComponentsDrain`, `TestRunGraceful_ResourceCloserErrorIsReported`, `TestGRPCComponent_ShutdownHookErrorIsReported` (generic closers, not the bus) |
| In-process gRPC bridge (S21) | Distributed transactions | A nested call does not inherit the caller's transaction. | `TestConn_NestedCall_SeesOnlyWhatAWireHopWouldDeliver` |
| Graceful shutdown, `Shutdowns` hooks (S5) | Transaction registry (D5) | Pools close after the server stops and before the orchestrator's kill deadline; open distributed transactions are not RPCs and are not drained, so process exit rolls them back and their parked after-commit work is dropped. | no test |
| Graceful shutdown | Wait for the database at boot (D2) | A SIGTERM during the boot wait MUST stop the wait through context cancellation. | `TestPing_CancelledContextStopsTheWait` |

## Server × Events and identity

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| In-process gRPC bridge (S21) | Principal propagation | The gateway stamps (user, scopes, labels) are relayed as on the wire, nothing more. | `TestConn_UnauthenticatedCaller_StaysUnauthenticated`, metadata isolation tests in `go/service/inprocgrpc` |
| In-process gRPC bridge (S21) | i18n language metadata | `w17-language` set as outgoing by the caller reaches the handler as incoming; a language present only on the caller's incoming side is not relayed, as on the wire. | `TestConn_CallerWithNoOutgoingMetadata_DoesNotLeakItsIncoming` |
| Process shutdown | Bus subscribers (I2, I4–I6) | `Close` drains every subscriber on every transport; registrations after close or drain are refused. | `TestNatsBus_SubscriberAfterCloseRefused_Embedded`, `TestRedisBus_SubscriberAfterCloseRefused_Embedded`, `TestNatsSubscriber_SubscribeAfterDrainRefused_Embedded`, `TestRedisSubscriber_SubscribeAfterDrainRefused_Embedded`, `TestMemoryBus_SubscribeAfterDrainIsRefused`, `TestMemoryBus_DispatchAfterDrainIsRefused` |
| Internal-mesh TLS | Principal read side (I10) | Backend tiers that trust the principal metadata are reachable only from authenticated tiers. | no test |

## Server × Tooling

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| `health` subcommand (S16) | Command dispatch (T1) | `health` is answered before the server starts and other words go to the next dispatcher; every reserved root is answered; a binary with migration commands must not set `Final=true` ahead of them. | `TestDispatch_FinalOwnsTheCommandTable`, `TestDispatch_AnswersForEveryReservedRoot` (`migrate`, `fixtures` only) |
| Internal-mesh TLS (server and client) | Dev PKI (T27) | The leaf chains to the current CA and covers every dial name. | `TestEnsureLeaf_ReMintedAfterCAReMint`, `TestEnsureLeaf_ReMintedForANewSAN`, `TestGRPCComponent_ServesInternalTLS` |

## Edges × Edges

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Per-client rate limiter (E26) | Proxy in front of the gateway | The key is the rightmost untrusted `X-Forwarded-For` hop when the peer is a configured proxy (none by default), else the peer; IPv6 per /64; a client MUST NOT be able to choose its bucket. | `TestForwardedPicksRightmostUntrusted`, `TestForwardedFromUnconfiguredPrivatePeerIsIgnored`, `TestForwardedUnparseableHopStopsTheWalk`, `TestLimiter_WarnsWhenEveryCallerCollapsesIntoOneBucket` |
| Per-client rate limiter (E26) | SSE and WebSocket streams (E14, E15) | Public streams are charged once per open, not per frame; authenticated streams are not limited. | `TestAdmitPublicStream_ChargesEveryStream` (partial: no per-client concurrent-stream cap test) |
| Authentication hook (E5) | Header → metadata propagation (E10) on auth-excluded routes | Public routes skip auth and carry no `x-w17-user`; no client header can reach the backend as a gateway-owned key. | no test |
| Public-method rate-limit adapters (E19) | Auth outcome / handler status | A unary public method stays charged on status ≥ 400 or when it wrote nothing, and a success returns its token; concurrent requests MUST NOT exceed the budget. | `TestAdmitPublic_ChargesFailuresPerClient`, `TestAdmitPublic_SuccessReturnsItsToken`, `TestAdmitPublic_NothingWrittenIsCharged`, `TestAdmitPublic_ConcurrentFailuresStayWithinTheBurst` |
| Stream re-auth watchdog (E17) | `w17-events` channel (E18) and per-RPC streams (E14, E15) | Every authenticated stream is re-probed every interval; only a definite refusal ends it, and an auth-backend outage does not. On `w17-events` a verdict whose labels differ from those at connect ends the stream, so the client reconnects under its current entitlement. | `TestWatchStreamAuth_DefiniteRefusalsCancel`, `TestWatchStreamAuth_FastBackendFailureIsNotARevocation`, `TestW17Events_RevokedCredentialEndsTheStream`, `TestW17Events_ChangedMembershipEndsTheStream`, `TestW17Events_SameLabelsKeepTheStream`, `TestW17Events_AuthOutageKeepsTheStream` |
| CORS (E12) | WebSocket helpers (E15) | Browser access to WebSocket routes follows the documented same-origin rule, independent of the CORS allowlist. | no test |
| Auth result cache (E6) | Stream re-auth watchdog (E17) | Revocation becomes visible within `max(reauth interval, TTL)`. | no test |
| URL-carried credentials (E7) | Request tracing (E11), pagination links (E23), browser redirects (E22) | The credential reaches auth only through the reserved slot. | no test |
| URL-carried credentials (E7) | Auth result cache (E6) | Distinct capability tokens never share a cache slot (the slot is a key header). | `TestAuth_CachedAuthFunc_DistinctHeadersDistinctSlots` (partial: no explicit two-token test) |
| Organization from the path (E8) | Auth result cache (E6) | A request with `W17-Org` bypasses the cache, so every org-scoped call makes a full auth call. | `TestAuth_CachedAuthFunc_UnknownHeaderBypassesCache` (indirect) |
| Browser stream tickets (E16) | Multiple gateway replicas | Issue and redeem happen on the same process (documented limit: per-process store); each replica's store is bounded on its own. | no test |
| Database error mapping (E33) | Model Context Protocol surface (E27) | Constraint and table names appear only in the operator message, never in a client-visible result. | `TestCallUnary_BackendErrorIsSanitised` |
| Multipart uploads (E20) | Request body limits (E1, generated `MaxBytesReader`) | The per-file cap applies here and the request cap comes from generated code; an unbounded body is never spilled to disk. | no test |
| Multipart uploads (E20) | Downloads (E21) | Both sides use the same key shape (`<sha256>-<suffix>` under the hash-sharded bucket path); authorization is per endpoint, not per object. | `TestDownloadURLRoundTrip` |
| JSON dialect (E2) | Oneof collapse and expand (E29) | Values decoded through the alias and oneof reshaping are exactly the values protojson would decode, on every surface (REST and MCP); the reshape renames keys and keeps every number exact, including 64-bit integers. | `TestExpand_KeepsNumbersExact`, `TestCollapse_KeepsNumbersExact`, `TestUnmarshalProto_KeepsSixtyFourBitNumbersExact`, `TestDecodeRequest_KeepsSixtyFourBitNumbersExact`, `TestRestoreAliasesOnRequestJSON_KeepsNumbersExact`, `TestServeStreamableHTTP_ToolArgumentsKeepSixtyFourBitNumbers` |
| MCP server (E27) | MCP auth translation (E28) per transport | stdio uses the environment identity; HTTP uses request headers only. | `TestConnInfo_HTTPCallerGetsNoEnvIdentity`, `TestDefaultAuthFunc_HTTPHasNoEnvFallback`, `TestServeStreamableHTTP_AnonymousCallerGetsNoEnvIdentity` |
| HTTP panic recovery (E13) | Public-method rate limit (E19), SSE (E14) | A panic yields a 500 envelope and an operator report; a panicking public method is still charged. | no test |
| Auth result cache (E6) | Browser stream tickets (E16) promotion | Probes after promotion keep working; the promotion marker forces a cache bypass. | `TestWatchStreamAuth_TicketStreamSurvivesTheTick` (behaviour, not cache) |
| Connection pool health watch (E32) | Long-lived callers | A pooled connection is replaced only once it is shut down; a transient failure resets its backoff and keeps the connection a caller holds. | `TestPool_HealthWatch_KeepsATransientFailureConnOpen`, `TestPool_Reconnect_KeepsATransientFailureConnOpen`, `TestPool_Reconnect_ReplacesAClosedConnection` |
| WebSocket helpers (E15) | Request body limits (E1) | A WebSocket frame may be as large as a REST body: the read limit is `MaxWebSocketMessageBytes`, which defaults to `MaxRequestBodyBytes`; a refusal's close reason always fits the close frame. | `TestAcceptWebSocket_ReadLimitIsTheRequestBodyCap`, `TestAcceptWebSocket_ReadLimitIsConfigurable`, `TestWSWriteErrorWithDetails_LongSentenceStillClosesCleanly` |
| `w17-events` channel (E18) | SSE write deadline (E14) and slow clients | A stalled client is dropped after the write deadline; the hub never blocks. | `w17events_drain_test.go` (partial: no stalled-client test) |

## Edges × Data

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Multipart uploads (E20) | Blob store (D13) | Rolling back a failed request MUST NOT remove an object another request references; every upload owns its own object. | `TestRollbackUploads_KeepsAnotherRequestsIdenticalObject`, `TestRollbackUploads_ConcurrentIdenticalUploads` |
| Database error mapping (E33) | Database outage | A transient database failure is surfaced as retryable and not reported as a fault. | no test |
| REST gateway limit | Cursor paging (D9) | The gateway clamps `?limit=` and forwards it; storage clamps the forwarded limit again to `MaxLimit` (10000) and reads `0` as absent; no caller receives an unbounded result. | `TestClampLimit`, `TestLimitFromIncomingMD_ClampsToMaxLimit` |
| REST gateway auth and data scopes | Cursor paging (D9) | Each resumed page re-authorizes from the caller's verified identity; scopes come from metadata, not from request fields in the cursor. A cursor signed with a `CallerKey` does not decode for another caller; generated code must sign with it for the binding to apply. | `TestCallerKey_ACursorDoesNotDecodeForAnotherCaller`, `TestDecodeCursor_ResumeReplacesTheRequest` (binding only; per-page re-authorization: no test) |
| REST upload and download | Blob store (D13) | The gateway writes via `BuildKey`; readers use the same key form; a published object is never seen partially written. | `kvfs_test.go`, `TestPutFromTempFile_CrossFS_ReaderNeverSeesPartialBody` |
| Error mapping and translation | SQL helpers `ExpandIn` (D8) | A list that exceeds the bind limit MUST surface as a client error (`INVALID_ARGUMENT`, detail code `TOO_MANY_VALUES`), not an internal error: the `…Checked` expansions refuse it before the statement is built, and the driver's own limit error maps the same way. | `TestExpandInChecked_RefusesPastTheBindLimit`, `TestWrap_TooManyBindValuesIsTheCallersFault` |

## Edges × Events and identity

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Error envelope (E3), refusals (E4), database error mapping (E33) | i18n locale | Detail sentences are translated with the request locale; the context-free writers answer in the source locale. | `constraint_locale_test.go`, `TestPreflightSentence_AgreesWithTheDemotionUnderALocale`, `TestWriteMissingScopeCtx_NameReachesTheOperatorOnly`, `TestWriteUploadErrorCtx_RefusalsAreCatalogSentences`, `TestAdmitPublic_RefusalIsTheCatalogSentence` (context-free paths: no test) |
| Inter-tier principal relay (E31) | Event labels and data scopes | The business tier forwards principal, scopes and labels to storage, for unary and stream calls. | `TestForwardPrincipalUnaryInterceptor` (unary only) |
| Gateway tier rendering | Catalogs and formatting (I14, I15) | Gateway-rendered messages use the caller's language and explicit format locale from outgoing metadata. | `TestLocaleFromContext_OutgoingMetadata`, `TestFormatLocaleFromContext_OutgoingMetadata` |

## Edges × Tooling

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| REST error mapping | E2E MCP caller (T24) | The same refusal code is assertable over REST and MCP where the MCP refusal carries a status. | `TestGrpcStatusFromMCPError_NonStatusErrorsStayTransportFailures` |
| SSE gateway | E2E streams (T25) | Frames arrive in order; close or an error frame ends the stream; non-JSON frames fail the step. | `TestExpectStream_*` |
| Authentication and sign-out flows | E2E auth threading (T20) | Each step uses the intended principal, including none, so a signed-out refusal can be asserted. | `TestRunScenario_MissingAuthToken`, `TestAnonymousStep_SendsNoCredential`, `TestTokenFrom_UsesTheNamedCapture`, `TestImplicitSessionToken_CannotAssertUnauthenticated` |

## Data × Data

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Nested transactions (D4) | After-commit work (D6), in-process emit | A callee's emit parks on the root scope, runs once after the root commits, and is dropped on rollback or cancellation. | `TestEmit_ANestedBusinessEmitWaitsForTheRootCommit`, `TestEmit_NothingIsAnnouncedForARolledBackTransaction`, `TestEmit_ACancelledRootAnnouncesNothing` |
| Auto-rollback interceptor (D7) | Transaction registry adoption lease (D5) | A failed participant MUST doom the transaction even while another participant holds the lease; a Commit queued before the rollback does not commit. | `TestInterceptor_DoneContextBehindASiblingStillDiscardsTheTransaction`, `TestInterceptor_ZeroFinishWaitDoomsTheTransaction`, `TestDoom_ACommitQueuedBeforeTheRollbackDoesNotCommit` |
| Auto-rollback interceptor (D7) | Coordinator server (D5) | The coordinator's Commit of a doomed transaction is `Aborted`; its Rollback succeeds. | `TestServer_DoomedTransaction_CommitAbortsAndRollbackSucceeds` |
| Auto-rollback interceptor (D7) | Transaction control plane (D5) | A failed Begin or Commit MUST NOT trigger an automatic rollback. | `controlplane_test.go` |
| Transaction lifecycle (D3) | Client disconnect or deadline | Rollback runs on a detached context; no commit is made for a caller that is gone. | `TestRunRollsBackEvenWhenTheCallersContextIsDead`, `TestRunDoesNotCommitOnBehalfOfAVanishedCaller` |
| Orphan timeout (D5) | Statement timeout | Per-method statement timeout stays below the finish wait (30 s), which stays below the orphan timeout (5 min); the watcher drains an expired entry even under a held lease. | `TestServer_Begin_DefaultTxTimeout_*`, `TestTimeoutWatcher_DrainsUnderALease` |
| Transaction isolation (D5) | Database dialect | The declared level is pinned at Begin; an adopted weaker transaction is refused. | `TestBegin_IsolationIsHonoured_Live`, `TestAdoptTxIsolated_RefusesWeakerOrUnknownLevels`, `TestAdoptTxIsolated_RegistryThatCannotReportRefuses` |
| Transaction lifecycle (D3) | Deadlock or serialization failure | The error surfaces to the caller; inside a distributed transaction it discards the transaction, and the caller restarts the whole `Run`. | `TestWrap_PgRetryable_Deadlock`, `TestWrap_PgRetryable_Serialization`, `TestWrap_MySQLRetryable`, `TestInterceptor_RollbacksOnError` |
| Cursor paging (D9) | Admin sort selector (D10) | The cursor's sort selector is authoritative; changing the sort restarts at page one. | `TestEncodeDecode_SortSelectorRoundtrip` |
| Cursor paging totals (D9) | Concurrent writes | The total is counted once per paging session and cached in the cursor. | no test |
| Bounded materialization (D11) | Adopted transaction or locked query (D5) | Fetch windows run sequentially when the request carries `w17-tx-id` or the context is marked with `WithinTx`; generated locked-query fetches must mark the context for this to apply. | `TestFetchBatches_InsideATransactionRunsSequentially` |
| Bounded materialization (D11) | Cursor paging (D9) | The guard bounds intermediate rows independently of page size. | no test |
| Column encryption (D16) | UNIQUE index and equality lookups | Deterministic mode keeps the index and `WHERE col = :x` working, including across key rotation (deterministic values stay on the lowest key version). | `TestDeterministic_StableAcrossRotation` |
| Column encryption (D16) | Cursor paging (D9) | A crypted column used as a keyset key orders by ciphertext; whether to allow it is the generator's decision. | no test |
| SQLite collation (D12) | Session pins (D1) | Every process that opens the database registers the collation and applies the same session pins; equality is deterministic and matches PostgreSQL. | `TestRegister_UnicodeOrderingAndEquality`, `TestCompare_EqualityIsDeterministic` (collation only; the pairing with session pins is not asserted) |
| Redis HASH codec (D14) | KV multi-fetch (`DedupKeys`, D8) | Multi-fetch multiplicity equals SQL `IN`. | `TestDedupKeys` |
| Redis HASH codec (D14) | Redis eviction check (D15) | Entities live on a non-evicting Redis; an `allkeys-*` policy produces a boot warning. | `TestEvictsUntaggedKeys` (classification) |

## Data × Events and identity

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| After-commit work (D6) | Event bus publish (cross-process) | A storage handler's emit is registered on commit, published after the coordinator's Commit, and dropped on rollback or drain; delivery is at-most-once. A Commit whose outcome cannot be settled returns `ErrCommitOutcomeUnknown`, does not run the parked emit and is reported. | `oncommit_test.go`, `phantom_emit_test.go`, `TestCommit_AnUnsettledTransportFailureIsReportedAsUnknown`; loss between commit and publish: no test |
| After-commit work (D6) | Broker outage (I5, I6) | An emit on the committing goroutine is bounded by `PublishTimeout` (default 2 s) for the stream check, publish and acknowledgement together; a connection closed for good fails at once. | `TestNatsBus_PublishTimeoutBoundsOutage_Embedded` (NATS only) |
| Transaction routing metadata (D4, D5) | Event bus subscriber context | Event envelopes MUST NOT carry transaction routing metadata (`w17-tx-id`, `w17-conn-id`) into a subscriber, so a subscriber is never a participant and its storage calls are not pinned to a settled transaction's connection. | `TestMetadataFromContext_DropsTheTransactionHeader`, `TestHeaderName_IsTheStrippedEnvelopeKey`, `TestConnIDHeader_IsTheStrippedEnvelopeKey` |
| Cursor paging metadata (D9) | Principal relay (`ForwardToOutgoing`) | Paging headers are never relayed hop-to-hop. | `TestForwardPrincipalUnaryInterceptor`, `TestDialOpts_StreamRelaysThePrincipalOnly` |
| Secret values (D17) | Structured logging | A `Secret` redacts on every logging path, wherever it is held. | `TestSlogRedacts`, `TestStructRedacts`, `TestUnexportedFieldRedactsInEveryVerb`, `TestUnexportedFieldRedactsInSlog` |
| Column encryption (D16) | Error layer and logs | Decryption errors carry no plaintext or key material. | `TestDecrypt_*` |
| In-process transaction scope | In-memory transport (I4) | A memory subscriber does not inherit the emitter's transaction, metadata or context values; it runs on its subscription's context. | `TestMemoryBus_HandlerContextIsNotTheEmitters` |

## Data × Tooling

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| SQLite collation stamp (D12) | Migration applier | The applier stamps; the runtime verifies and refuses a known mismatch. | tested in the SQLite migration driver, not in-package |
| Session pins (D1) | Migration applier and runtime | Applier and runtime open the database with identical pins, parsed per driver; an operator's own value is kept and logged once. | `TestApplyMySQLDSNParams_KeepsAnOperatorCollation`, `TestMySQLDSNParams_PinsCollation`, `TestApplyPGDSNParams_KeepsAnOperatorParameter`, `TestApplySQLitePragmas_OperatorPragmaWinsAndIsLogged` (shared pin functions; the two call sites are not asserted) |

## Events and identity × Events and identity

| Feature A | Feature B | Expected behaviour | Test |
|---|---|---|---|
| Event context (I8) | Principal relay into subscribers (I10) | The subscriber runs with the emitter's principal and scopes carried in `Event.Metadata`, and the metadata carries no credentials. | `TestEventContextReaders_*`, `TestMetadataFromContext_DropsCredentials` |
| Event labels (I8) | Server-sent-events hub audience (I9) | Labelled events reach only principals holding every label; unlabelled events reach nobody under isolation. | `TestEventbusEventSource_LabelFiltering`, `TestEventbusEventSource_UnlabelledEvent*`, `TestEmitInvalidate_*` |
| Principal, gateway side (I11) | Event context (I8) | A client cannot inject `x-w17-*` or `w17-label-*` keys into an event envelope. | `TestWithGatewayMD_SanitizesBothSides` |
| Principal relay (I12) | Dispatcher (I1) | An emit reached through several tiers keeps its scopes and labels. | `TestForwardToOutgoing_SecondHopKeepsTheLabel` |
| Catalogs and formatting (I14, I15) | Event context (I8) | Subscriber messages use the requester's language and format locale. | `TestMetadataFromContext_RepeatedKeysMatchTheirReaders` (metadata collapse only; rendering not asserted) |
| Server-sent-events hub (I9) | Delivery modes (I3) | In fan-out mode every replica's hub receives every event. | `TestEventbusEventSource_EveryReplicaReceivesEveryEvent` |
| Server-sent-events hub (I9) | NATS JetStream transport (I5) | The hub resumes delivery after a broker loss, including a recreated broker. | `TestNatsBus_RecoversFromStreamLoss_Embedded`, `TestNatsBus_RecoversFromBrokerRecreated_Embedded` (bus level; hub not exercised) |
| Server-sent-events hub (I9) | Slow client | A slow client loses only its own frames; other clients are unaffected. | `TestEventbusEventSource_SlowClientLosesOnlyItsOwnFrames` |
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
| Apply run (T11), Postgres applier (T15) | Concurrent appliers (several replicas at boot) | Exactly one applies; each other run either finds it applied or fails fast on the run lock, naming the run in progress (the next start finds the work done), never on what the body ran into. | `TestApply_ConcurrentMigratorsApplyOnce_Live`, `TestRunLock_SerialisesAndReportsALostSession_Live` |
| MySQL applier and run lock (T15) | Concurrent appliers | `GET_LOCK` serialises; the loser fails fast; a lost lock session fails the run. | `TestRunLock_SurvivesAnIdleServerTimeout` (lock session only) |
| Redis/S3 run lock (T15) | Long data migration (T16) | The lock is held for the whole migration; a holder that loses it stops before writing again and the run fails with `ErrLockLost`. | `TestRunLock_LostLockStopsTheNextWrite`, `TestRunLock_LeaseRunsOutWithoutARefresh`, `TestS3RunLock_LostLockStopsTheNextWrite`, `TestRun_LostRunLockStopsBeforeTheNextMigration` |
| Squash adopt (T10) | Rollback by another run while this one waits on the lock (T11) | A head that moved backward is refused. | `TestApply_RefusesWhenTheHeadMovedBACKWARD` |
| Post-transaction resume (T11) | Crash between halves on each dialect (T15) | Postgres resumes only the post-transaction half; MySQL and SQLite warn at run time that the migration is not resumable. | `TestRun_ResumesPendingSkirt` (Postgres, stub applier) |
| Drift gate (T13) | Console-emitted expectations | Every migration that carries a real expectation is compared; placeholders are skipped explicitly. | `TestPreFingerprint_SkipsFakePlaceholder`, `TestPreFingerprint_SkipsEmptyTargetFingerprint`, `TestPreFingerprint_StillRefusesRealDrift`, `TestPreFingerprint_RefusesWhenTargetDisagrees` |
| Drift gate (T13) | Fake apply (T12) | The gate is skipped and the adopt preflight is the check; only integrity-checked preflight content is executed. | `TestFake_RefusesAPreflightTheDigestDoesNotCover`, `TestFake_BoundPreflightCannotBeReplaced` (the gate skip has no test) |
| Extension preflight (T11) | Applier decorators | `Unwrap` reaches the Postgres applier through every decorator, or the run refuses. | `TestPreflightExtensions_SurvivesADecorator` |
| Schema prep (T11) | Wipe (T15) | Wipe returns the store to empty, including namespaces schema prep created. | `TestWipe_DropsDeclaredSchemasOnly_Live`, `TestWipeSQL_DropsDeclaredSchemas` |
| `apply --fetch` (T4) | Read-only container | No disk writes; the same hash check as artefacts on disk. | `TestMigrationSetFS_RefusesATamperedBody` |
| `migrate fetch` (T4) | Path guard (T28) | Every fetched artefact lands under `--migrations`. | `TestWriteMigration_RefusesPathShapedNames`, `TestWriteMigration_PlainNamesLandUnderRoot` |
| Lock file (T2) | Artefact chain (T7, T8) | The pinned hash anchors the chain; only chained, hash-matching artefacts execute; a connection listed twice is refused. | `TestPlan_TargetHashMismatch`, `TestPlan_InsertedMigrationIsNotOnTheChain`, `TestRun_RefusesAConnectionListedTwice` |
| Apply timeout (T2) | Data migration (T16) | A migration cut by the timeout leaves a state a later run resumes or refuses with a clear remedy. | `TestApply_YAMLApplyTimeoutLeavesAResumableState`, `TestApply_YAMLApplyTimeoutLeavesAResumableState_Live` |
| Fixtures (T6) | Dialect appliers (T15) | Only stores that can seed (Postgres) are offered as seed targets; the group is one transaction. | `TestFixturesApply_TheWholeGroupIsOneUnitOfWork`, `TestSeedCandidate_MatchesSeedCapable`, `TestFixturesApply_StoresThatCannotSeedAreNotCandidates` |
| Fixtures (T6) | Migrations (T11) | Seeding runs after the schema exists; the stale-render check compares fixtures with their render. | `TestFixturesApply_RefusesARenderOlderThanItsFixture` |
| E2E credential step (T20) | Event awaits (T25) | Event visibility is asserted for the credential's principal. | no test |
| E2E `${seq}` (T21) | Several runner processes on one database | Generated values stay unique per process; parallel processes need distinct data. | `TestSeqGlobalAcrossScopes` (in-process) |
| E2E matchers (T22) | Proto3 JSON zero values | An absent field is the encoding of an empty one: `count == 0`, `empty`, `distinct` and `eq null` accept it; `eq 0` explains presence; `capture` fails on it and binds nothing. | `TestExactMatcher_ZeroAgainstAnAbsentFieldExplainsPresence`, `TestCapture_AbsentFieldFails`, `TestCountMatcher`, `TestEmptyMatcher` (`distinct` not asserted) |
| Stress (T26) | E2E runner routing (T23) | Load requests are routed like runner requests (`ResolveREST`) and carry the same credentials. | no test |
| Plugin digest (T29) | Code generation | Every compiled file is covered by the digest or regenerated from covered input, provided the consumer refuses a tree that `Uncovered` reports or calls `Prune` before regenerating into it or compiling it. | `TestOf_IgnoresRegeneratedPb`, `TestUncovered_NamesEveryFileOutsideTheDigest`, `TestPrune_LeavesOnlyCoveredContent`, `TestPrune_DoesNotFollowSymlinks` |
