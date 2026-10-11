# Data layer

The runtime data packages that generated services link against: database session
settings, boot-time readiness, distributed and nested transactions, SQL shape helpers,
paging, bounded materialization, SQLite collation, blob and Redis entity storage, column
encryption and secrets. PostgreSQL, MySQL and SQLite are all reachable from this area;
the tests listed here run against in-process SQLite or fakes unless stated otherwise.
Packages: `go/lib/dbsession`, `go/lib/dbwait`, `go/service/tx/distx`,
`go/service/tx/txregistry`, `go/service/tx/txscope`, `go/service/tx/grpcrollback`,
`go/lib/dqlbind`, `go/lib/paging`, `go/lib/listsort`, `go/lib/materialize`,
`go/lib/sqlitecollate`, `go/lib/kvfs`, `go/lib/kvhash`, `go/lib/kvposture`,
`go/lib/fieldcrypt`, `go/service/secret`.

## Features

| ID | Feature | Code |
|---|---|---|
| D1 | Per-dialect session pins | `go/lib/dbsession/dbsession.go` |
| D2 | Wait for the database at boot | `go/lib/dbwait/dbwait.go` — `Ping` |
| D3 | Transaction lifecycle (`distx.Run`) | `go/service/tx/distx/run.go` — `Run` |
| D4 | Nested transactions | `go/service/tx/distx/client.go` — `Begin`, `TxHandle`; `go/service/tx/txscope/txscope.go` |
| D5 | Transaction registry and coordinator server | `go/service/tx/txregistry/memory.go` — `Memory`; `go/service/tx/txregistry/txregistry.go` — `AdoptTx`, `AdoptTxIsolated`; `go/service/tx/distx/server.go` — `Server` |
| D6 | After-commit work (emit deferral) | `go/service/tx/txregistry/txregistry.go` — `DeferUntilCommit`; `go/service/tx/txscope/txscope.go` |
| D7 | Auto-rollback interceptor | `go/service/tx/grpcrollback/grpcrollback.go` — `Interceptor` |
| D8 | Runtime SQL shape and binding helpers | `go/lib/dqlbind/dqlbind.go` |
| D9 | Cursor (keyset) paging, limit, totals | `go/lib/paging/cursor.go`, `keyset.go`, `metadata.go` |
| D10 | Admin column sort selector | `go/lib/listsort/metadata.go` |
| D11 | Bounded app-side materialization | `go/lib/materialize/guard.go` — `Guard`; `go/lib/materialize/batch.go` — `FetchBatches` |
| D12 | SQLite Unicode collation and stamp | `go/lib/sqlitecollate/sqlitecollate.go` — `Register`; `stamp.go` — `EnsureStamp`, `VerifyStamp` |
| D13 | Blob store for uploads | `go/lib/kvfs/kvfs.go` — `BuildKey`; `go/lib/kvfs/local_fs/local_fs.go`; `go/lib/kvfs/memory` |
| D14 | Redis HASH entity codec | `go/lib/kvhash/kvhash.go` |
| D15 | Redis eviction check | `go/lib/kvposture/kvposture.go` |
| D16 | Column encryption (`CRYPTED_SECRET`) | `go/lib/fieldcrypt/fieldcrypt.go`, `shared.go` — `Init` |
| D17 | Secret values and sources | `go/service/secret/secret.go` — `Secret`; `source.go` — `Resolver`; `go/service/secret/sopsenv` |

## D1. Per-dialect session pins

**Code.** `go/lib/dbsession/dbsession.go` — the SQLite, MySQL and PostgreSQL pin lists and their DSN appliers.

**Purpose.** One declaration of the connection settings the emitted SQL depends on for its
meaning. The migration applier, the dev-database tooling and every generated runtime read
it, so all of them open the database the same way.

**Configuration.** No environment variables.

| Dialect | Pins | Applier rule |
|---|---|---|
| SQLite | `_pragma=case_sensitive_like(1)`, `_pragma=foreign_keys(1)`, `_time_format=sqlite` | appends any pin whose exact text is absent |
| MySQL | `sql_mode=REPLACE(@@sql_mode,'NO_BACKSLASH_ESCAPES','')`, `parseTime=true`, `collation=utf8mb4_0900_as_cs` | appends a pin unless the operator already set that parameter |
| PostgreSQL | `standard_conforming_strings=on`, `search_path=public` | as MySQL; URL (`postgres://…?k=v`) and keyword (`host=… user=…`) forms |

**Shares.** The "applier and runtime must agree" contract with D12; the migration tooling; generated DSN builders.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D1.1 | Applying a pin list twice MUST be idempotent. | no test |
| D1.2 | MySQL: a parameter the operator named MUST keep the operator's value. | `TestApplyMySQLDSNParams_KeepsAnOperatorCollation` |
| D1.3 | MySQL: a bare DSN MUST receive every pin, including the collation. | `TestMySQLDSNParams_PinsCollation`, `TestApplyMySQLDSNParams_AddsCollationToABareDSN` |
| D1.4 | PostgreSQL: a parameter the operator named MUST keep the operator's value, in URL and keyword form. | no test |
| D1.5 | The applier MUST recognise an operator parameter by its name, not by matching text elsewhere in the DSN. | no test |
| D1.6 | SQLite: foreign keys MUST be enforced, LIKE MUST be case-sensitive, and `time.Time` MUST be stored in a format SQLite date functions parse. | no test |
| D1.7 | MySQL: backslash doubling MUST stay correct, DATETIME MUST scan into `time.Time`, and literal comparisons MUST be case- and accent-sensitive. | no test (needs a MySQL server) |
| D1.8 | PostgreSQL: single-quote-doubling escaping MUST stay correct and bare table names MUST resolve in `public`. | no test (needs a PostgreSQL server) |
| D1.9 | The helpers MUST NOT log the DSN; they only return strings. | no test |

## D2. Wait for the database at boot

**Code.** `go/lib/dbwait/dbwait.go` — `Ping(ctx, p, label, budget, logf)`.

**Purpose.** Retry the initial ping instead of exiting, so a database that is still starting does not cause a container restart.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `budget` | — | `<= 0` → 60 s |
| backoff | first attempt immediate, then 100 ms × 1.5 per attempt, capped at 2 s and at the remaining budget | — |
| `logf` | — | `nil` silences logging |
| `p` | required | `nil` → error |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D2.1 | A database that is already up MUST cost one ping and no sleep. | `TestPing_ReadyDatabaseIsImmediate` |
| D2.2 | A database that comes up within the budget MUST be accepted. | `TestPing_RetriesUntilTheDatabaseComesUp` |
| D2.3 | Each attempt MUST be bounded by the overall deadline, so a hanging dial cannot outlive the budget. | no test |
| D2.4 | Exactly one "not ready" and one "accepted after" log line MUST be written, not one per attempt. | `TestPing_AnnouncesOnceNotPerAttempt` |
| D2.5 | The final error MUST name the label, the time waited, the attempt count and the last driver error, and wrap it for `errors.Is`. | `TestPing_GivesUpAndSaysWhat` |
| D2.6 | Cancelling the parent context MUST stop the wait immediately. | `TestPing_CancelledContextStopsTheWait` |

**Deliberate limits.** Boot only; a database lost mid-life is handled by `database/sql` pool reconnects.

## D3. Transaction lifecycle (`distx.Run`)

**Code.** `go/service/tx/distx/run.go` — `Run`.

**Purpose.** Run a function inside a distributed transaction held by a storage binary: commit on success, roll back otherwise.

**Model.** A storage binary holds an in-memory registry of open transactions (D5) and exposes
Begin/Commit/Rollback over gRPC (`distx.Server`). A business-tier caller uses `distx.Run` or
`distx.Begin`; the returned context carries `w17-tx-id` (and `w17-conn-id` when a routing proxy
minted one) as outgoing metadata. A generated storage handler adopts that transaction, or opens
its own per-method transaction when no header is present.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D3.1 | MUST commit when the function returns nil and roll back when it returns an error. | `TestRunCommitsWhenTheFunctionSucceeds`, `TestRunRollsBackWhenTheFunctionFails` |
| D3.2 | MUST roll back on panic and then re-panic; the panic is never swallowed. | `TestRunRollsBackOnPanicAndKeepsPanicking` |
| D3.3 | Rollback MUST run on a context detached from cancellation, so a caller that hung up still releases its transaction. | `TestRunRollsBackEvenWhenTheCallersContextIsDead` |
| D3.4 | MUST NOT commit when the caller's context is already done (it rolls back instead), identically on network and in-process transports. | `TestRunDoesNotCommitOnBehalfOfAVanishedCaller` |
| D3.5 | A commit error MUST be returned and labelled as a commit failure. | `TestRunReturnsTheCommitError` |
| D3.6 | When both the function and the rollback fail, the function's error MUST stay the `errors.Is` target and the rollback failure MUST be appended as text. | `TestRunKeepsTheFunctionErrorAndNamesAFailedRollback` |
| D3.7 | A nil client or nil function MUST be an error. | `TestRunRefusesNils` |
| D3.8 | The function MUST NOT be called when Begin fails. | `TestRunDoesNotCallTheFunctionWhenBeginFails` |

**Deliberate limits.**
- No automatic retry on serialization failures or deadlocks; such errors surface to the caller, and inside a distributed transaction they discard it (D7).

## D4. Nested transactions

**Code.** `go/service/tx/distx/client.go` — `Begin`, `TxHandle`, `ErrRollbackOnly`, `ErrNestedMismatch`; `go/service/tx/txscope/txscope.go` — `Detach`.

**Purpose.** A Begin inside an active root transaction joins it instead of opening a second one; `txscope` is the in-process face of the root (joins, rollback-only marking, after-commit hooks).

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `distx.ConnIDHeader` | `w17-conn-id` | must equal the routing proxy's constant |
| nested `tx_timeout_ms` | ignored; the root's bound governs | — |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D4.1 | A Begin on a context whose root is active MUST join it without an RPC; the nested Commit is a no-op and the nested Rollback marks the root rollback-only. | `TestNested_InnerRunJoinsTheOuterTransaction`, `TestNested_ThreeLevelsAreOneTransaction` |
| D4.2 | After any nested rollback, the root Commit MUST roll back the whole transaction and return `ErrRollbackOnly`. | `TestNested_InnerFailureSwallowedRollsBackAndSaysSo`, `TestNested_InnerPanicRollsBackAndStillPanics`, `TestNested_OneParallelFailureMakesTheRootRollBack`, `TestCommit_RollbackOnlyRollsBackOnADoneContext` |
| D4.3 | A join MUST require the same `connection_name` and an unspecified or identical isolation; otherwise `ErrNestedMismatch` and the outer transaction is untouched. | `TestNested_AnotherConnectionIsRefusedAndTheOuterIsUntouched`, `TestNested_IsolationMustMatchOrBeUnspecified` |
| D4.4 | A nested request's timeout MUST NOT cut the root short. | `TestNested_AnInnerTimeoutDoesNotCutTheRootShort` |
| D4.5 | Routing headers MUST be set, never appended: a context carries exactly one `w17-tx-id`. | `TestNested_AJoinRestoresHeadersTheCallerOverwrote` |
| D4.6 | A composed (single-process) binary MUST mean the same as a split one: the in-process hop hides the scope. | `TestComposed_AHopInsideATransactionMeansWhatTheWireMeans` |
| D4.7 | A finished transaction's context MUST open a fresh transaction, not join the finished one. | `TestNested_AFinishedTransactionsContextOpensAFreshOne` |
| D4.8 | Parallel joins under one root MUST be one transaction. | `TestNested_ParallelJoinsAndEmitsUnderOneRoot` |

**Deliberate limits.**
- No savepoints: any nested failure dooms the whole root transaction.

## D5. Transaction registry and coordinator server

**Code.** `go/service/tx/txregistry/memory.go` — `Memory`, `WithFinishWait`; `go/service/tx/txregistry/txregistry.go` — `AdoptTx`, `AdoptTxIsolated`, `IsolationFor`, `HeaderName`; `go/service/tx/distx/server.go` — `Server`, `WithDefaultTxTimeout`, `DefaultOrphanTimeout`.

**Purpose.** An in-memory `tx_id → *sql.Tx` map with one `*sql.DB` per declared connection, served over gRPC, that generated storage handlers adopt.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `BeginRequest.tx_timeout_ms` | `0` → server default | negative → `InvalidArgument` |
| `WithDefaultTxTimeout(d)` | `DefaultOrphanTimeout` = 5 min | `0` disables the orphan guard |
| `BeginRequest.isolation` | unspecified → driver default | unknown values → driver default |
| `BeginRequest.connection_name` | — | unknown → `ErrUnknownConnection` → `InvalidArgument` |
| `WithFinishWait(d)` | 30 s; bounds both "second adopter waits for the first" and "Commit/Rollback waits for a running adopter" | `<= 0` → do not wait |
| `txregistry.HeaderName` | `w17-tx-id` | — |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D5.1 | Transaction ids MUST be random UUIDv4. | no test |
| D5.2 | Adoption on another connection MUST fail with `ErrConnectionMismatch` and an unknown id with `ErrUnknownTxID`; neither may silently open a fresh transaction. | `txregistry_test.go`, `adopt_unknown_txid_test.go` |
| D5.3 | The adoption lease MUST be exclusive: a second adopter waits (bounded) for the first, Commit/Rollback wait (bounded) for a running adopter, so a method is never split; on expiry `ErrTxBusy` with the transaction intact and retryable. | `TestAdoptedHandler_FinisherCannotSplitAMethod`, `TestFinisher_WaitsForTheAdopterToRelease`, `TestSecondAdopter_WaitsForTheFirst` |
| D5.4 | A waiting finisher MUST block new adopters, so it cannot starve. | no test |
| D5.5 | One `*sql.Tx` MUST NOT be used from two goroutines at once. | `TestMemory_ConcurrentBeginCommit`, `TestMemory_ConcurrentDoubleCommit_OneWinner`, `TestMemory_ConcurrentLookupVsCommit` |
| D5.6 | With a timeout, a watcher MUST drain and roll back the entry when the deadline fires, even under a held lease. | `TestTimeoutWatcher_DrainsUnderALease`, `TestMemory_Begin_TxTimeout_*`, `TestServer_Begin_DefaultTxTimeout_OrphanCleanup`, `TestServer_Begin_DefaultTxTimeout_OptOut`, `TestServer_Begin_CallerTimeoutOverridesDefault` |
| D5.7 | A failed Commit MUST roll back to release the connection and drop the deferred work. | `outcome_test.go` |
| D5.8 | Recently drained ids (last 256) MUST remember why they are gone, so a later NotFound names the cause. | `outcome_test.go` |
| D5.9 | `AdoptTxIsolated` MUST refuse a transaction whose isolation is weaker than, or unknown relative to, the method's declared level (`ErrIsolationTooWeak`). | no test |
| D5.10 | Isolation levels MUST be honoured on PostgreSQL and MySQL. | no test (needs a server) |
| D5.11 | A transaction id MUST be adoptable only by trusted internal callers; the storage gRPC port is an internal endpoint. | no test |

**Deliberate limits.**
- In-memory registry: a restart loses every open transaction, and the coordinator's Commit gets NotFound without a cause.
- One instance per domain; multi-replica routing relies on an external proxy honouring `w17-conn-id`.
- Isolation support depends on the driver; SQLite accepts only its default (serializable) level.
- With the orphan guard disabled and no caller timeout, an abandoned transaction holds its connection until process exit.

## D6. After-commit work (emit deferral)

**Code.** `go/service/tx/txregistry/txregistry.go` — `DeferUntilCommit`; `go/service/tx/txregistry/memory.go` — `Memory.Commit`; `go/service/tx/txscope/txscope.go`.

**Purpose.** Work such as event emission runs only after the write it announces has committed.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D6.1 | `DeferUntilCommit` MUST run the work now when no transaction is involved, park it until commit when one is, and drop it (with an operator report) when the transaction ended without committing. | `oncommit_test.go`, `phantom_emit_test.go` |
| D6.2 | In-process hooks MUST run once, in registration order, after a successful commit; a panicking hook is recovered and reported and does not stop the others or undo the commit. | `txscope_test.go`, `TestEmit_APanickingHookAfterCommitDoesNotUndoTheCommit` |
| D6.3 | A panic in a registry after-commit hook MUST NOT escape `Commit`, skip later hooks, or report failure for a committed transaction. | no test |
| D6.4 | Nothing MUST be announced for a rolled-back or cancelled root, or for a commit the server refused. | `TestEmit_NothingIsAnnouncedForARolledBackTransaction`, `TestEmit_ACancelledRootAnnouncesNothing`, `TestEmit_ACommitTheServerRefusesDropsTheParkedWork` |

**Deliberate limits.**
- Publication after commit is at-most-once: there is no transactional outbox, so a crash between the database commit and the publish loses the event.

## D7. Auto-rollback interceptor

**Code.** `go/service/tx/grpcrollback/grpcrollback.go` — `Interceptor`.

**Purpose.** Roll back the caller's distributed transaction whenever any unary handler inside it fails.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D7.1 | A failing unary handler whose request carries `w17-tx-id` MUST trigger a rollback of that transaction; the handler's own error is returned unchanged. | `grpcrollback_test.go` |
| D7.2 | A failed participant MUST doom the transaction regardless of concurrent participants or the state of the failing call's context. | no test |
| D7.3 | Control-plane calls (Begin, Commit, Rollback) MUST NOT trigger it. | `controlplane_test.go` |
| D7.4 | The recorded cause MUST name only the method and status code, never the handler's error text. | `cause_test.go` |

**Deliberate limits.**
- Any failure inside a transaction discards it, including a benign `NotFound` read; a coordinator cannot tolerate a failed call and continue.
- Streaming RPCs are not covered.

## D8. Runtime SQL shape and binding helpers

**Code.** `go/lib/dqlbind/dqlbind.go` — `ExpandIn`, `ExpandValuesPG`, `ExpandValuesQM`, `scanSQL`, `Arr`, `Bytes`, `NullIfEmpty`, `EnumFieldOrNull`, `TimestampOrNull`, `DedupKeys`.

**Purpose.** Runtime rewrites the generator cannot do statically, and value coercions at the bind boundary.

**Behaviour.**

| Function | Behaviour |
|---|---|
| `ExpandIn(sql, occurrence, n)` | MySQL/SQLite: replaces the n-th `?` placeholder with `?, ?, …`; `n == 0` → `NULL`; `n == 1` → unchanged; missing occurrence → unchanged (the driver reports the argument mismatch). Callers expand in descending binding order. PostgreSQL uses `= ANY($n)` instead. |
| `ExpandValuesPG`, `ExpandValuesQM` | Multi-row INSERT: clone the single `VALUES (…)` tuple `n` times; PostgreSQL renumbers `$k`. |
| `scanSQL` | The one byte scanner: single-quoted literals (with `''` escapes) are data, never placeholders. |
| `Arr`, `Bytes` | nil repeated/bytes → non-nil empty, so NOT NULL array/bytea columns store empty, not NULL. |
| `NullIfEmpty` | `""` or empty `[]byte` → SQL NULL. |
| `EnumFieldOrNull` | Unset optional enum at any depth, including an absent parent message → NULL; set → `int32`. |
| `TimestampOrNull` | nil Timestamp → NULL, never year 1. |
| `DedupKeys` | Deduplicates KV multi-fetch keys, first occurrence wins; no allocation without duplicates. |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D8.1 | Placeholder counting MUST ignore string literals; a `?` or `$k` inside a literal is never expanded or renumbered. | `TestNthPlaceholder`, `literalscan_test.go`, `TestExpandIn_LiveSQLite_LiteralQuestionMark` |
| D8.2 | Literal bytes MUST be copied verbatim into every cloned VALUES tuple. | `TestExpandValuesPG`, `TestExpandValuesQM` |
| D8.3 | The helpers MUST insert only `?`, `$k`, `NULL`, `, ` and copies of the input, never caller data; all values travel as bind parameters. | `TestExpandIn`, `TestExpandValuesPG`, `TestExpandValuesQM` |
| D8.4 | KV multi-fetch multiplicity MUST equal SQL's. | `TestDedupKeys` |
| D8.5 | `NullIfEmpty` and `TimestampOrNull` MUST bind NULL for empty values. | `TestNullIfEmpty`, `TestTimestampOrNull` |
| D8.6 | `Arr` and `Bytes` MUST NOT bind NULL into NOT NULL array or bytea columns. | no test |
| D8.7 | `EnumFieldOrNull` MUST bind NULL for an unset enum under an absent parent message. | no test |
| D8.8 | Expanded SQL MUST execute correctly on MySQL. | no test (needs a server) |

**Deliberate limits.**
- The scanner understands only single-quoted literals; it relies on the emitter never producing double-quoted strings, dollar-quoted bodies, comments or backslash-escaped quotes.
- The list length is not capped here; request validation upstream bounds it.

## D9. Cursor (keyset) paging, limit, totals

**Code.** `go/lib/paging/cursor.go` — `EncodeCursor`, `DecodeCursor`, `PageCursor`; `go/lib/paging/keyset.go` — `ClampLimit`; `go/lib/paging/metadata.go`.

**Purpose.** Opaque, tamper-proof page cursors at the REST boundary, plus the metadata that carries boundaries, limit and the cached total from the gateway to storage.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `<PREFIX>_CURSOR_SECRET` | unset → random per-boot key and a warning; cursors then do not survive a restart or validate across replicas (decode fails closed with `ErrCursorMalformed`) | HMAC key is SHA-256 of the secret |
| `ClampLimit(requested, def, max)` | `0` → `def` | above `max` (when `max > 0`) → `max` |
| metadata keys | `x-w17-paging-boundaries`, `x-w17-paging-limit`, `x-w17-paging-total` | — |

**Wire format.** `base64url(PageCursor) "." base64url(HMAC-SHA256)`. `PageCursor` carries the
serialized request, boundary values, total, direction, schema version and sort selector.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D9.1 | A forged or tampered cursor MUST be rejected by a constant-time MAC check before any of its bytes reach the request. | `TestDecodeCursor_RejectsForgedCursor`, `TestDecodeCursor_RejectsTamperedPayload` |
| D9.2 | A cursor from another build or another endpoint MUST fail with `ErrCursorExpired`. | `cursor_test.go` (schema-version rejection) |
| D9.3 | On resume the request MUST be replaced by the minted one; filters cannot change mid-session and the cursor's sort selector is authoritative. | no test |
| D9.4 | An empty boundary oneof MUST be rejected as malformed on both the cursor and metadata paths, without panicking. | `metadata_test.go`, `TestScalarOf_*` |
| D9.5 | `uint64` keys above `MaxInt64` MUST round-trip without sign wrap. | `keyset_uint64_test.go` |
| D9.6 | COUNT(*) MUST run on the first page only; resumed pages reuse the total cached in the signed cursor. | `metadata_test.go` |
| D9.7 | Empty boundaries MUST produce an empty token (no next/previous link). | `cursor_test.go` |
| D9.8 | Paging metadata MUST NOT be forwarded hop-to-hop by the principal relay. | no test in this package (see the principal relay tests) |
| D9.9 | The limit MUST be clamped to the configured maximum. | `TestClampLimit` |
| D9.10 | Each resumed page MUST re-apply authorization and data scopes from the caller's verified identity, never from request fields carried in the cursor. | no test |
| D9.11 | No caller MUST receive an unbounded result set. | no test |
| D9.12 | Keyset pages MUST stay stable under concurrent inserts (requires a unique ORDER BY tiebreak, emitted by the generator). | no test |
| D9.13 | The cursor secret MUST be read from the environment. | `TestCursorKeyFromEnv` |

**Deliberate limits.**
- Cursors are integrity-protected, not confidential: list request fields are visible to anyone who sees the URL.
- Cursors carry no expiry; they stay valid until the key or schema changes, and the cached total can be stale.

## D10. Admin column sort selector

**Code.** `go/lib/listsort/metadata.go`.

**Purpose.** Carry `?sort_by=&sort_dir=` from the admin gateway to storage as a small integer
index in metadata `x-w17-sort-by`: `0` default, `2i+1` ascending, `2i+2` descending.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D10.1 | An unknown field MUST map to `0` (default order). | no test |
| D10.2 | A malformed or negative header MUST map to `0`. | no test |
| D10.3 | Only an index travels, never a column name. | no test |

**Deliberate limits.**
- Storage evaluates the sort through a `CASE` expression in ORDER BY, which cannot use an index: every sorted admin page sorts the whole filtered set.
- An index beyond the column set falls back to the default order.

## D11. Bounded app-side materialization

**Code.** `go/lib/materialize/guard.go` — `NewGuard`, `Guard`; `go/lib/materialize/batch.go` — `FetchBatches`, `effectiveParallel`.

**Purpose.** Runtime safety for narrow-then-fetch plans: a row counter with a hard ceiling and a bounded-parallel batch fetcher.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `NewGuard(limit)` | — | `0` or a nil `*Guard` → unbounded but still counted |
| `batchSize` | — | `< 1` → one window |
| `maxParallel` | — | clamped to `[1, MaxOpenConnections-1]` when the pool size is finite |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D11.1 | Crossing the ceiling MUST return `RESOURCE_EXHAUSTED` naming the ceiling; the count is shared across goroutines. | `TestGuard_CrossingLimit_ResourceExhausted`, `TestGuard_Concurrent` |
| D11.2 | Parallel and sequential runs MUST return identical results in window order. | `TestFetchBatches_ParallelPreservesOrder` |
| D11.3 | The first error, or a recovered worker panic, MUST cancel the shared context, stop launching and discard partial results. | `TestFetchBatches_ErrorAborts`, `TestFetchBatches_ParallelPanicBecomesError` |
| D11.4 | Parent cancellation MUST surface as an error, never as a partial result. | `TestFetchBatches_PreCancelledContextNoLaunch` |
| D11.5 | A single call MUST NOT take the whole pool (one connection is reserved). | `TestEffectiveParallel*` |
| D11.6 | Inside a transaction, fetch windows MUST run sequentially on that transaction. | no test |

**Deliberate limits.**
- The ceiling counts rows, not bytes.
- Pool headroom is per call; many concurrent requests can still use the whole pool between them.
- Narrow and fetch are separate statements; on separate pooled connections they see different snapshots.

## D12. SQLite Unicode collation and stamp

**Code.** `go/lib/sqlitecollate/sqlitecollate.go` — `Register`, `Compare`; `go/lib/sqlitecollate/stamp.go` — `EnsureStamp`, `VerifyStamp`; `go/lib/sqlitecollate/fingerprint.go`.

**Purpose.** Register `W17_UNICODE` (UCA root, accent- and case-sensitive) and Unicode-aware
`upper()`/`lower()` on every SQLite connection, and stamp which collator built a database's indexes.

**Configuration.** `Register()` once per process before opening connections (idempotent).
`EnsureStamp` on the write side (migration applier), `VerifyStamp` on the read side (runtime).
Stamp table: `w17_collation`.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D12.1 | `'Foo' != 'foo'`, `'a' != 'á'`, ordering follows UCA root (as MySQL `utf8mb4_0900_as_cs`), and `upper('café') = 'CAFÉ'`. | `TestRegister_UnicodeOrderingAndEquality`, `TestRegister_UnicodeUpperLower`, `TestCompare`, `TestFoldCase_TypeHandling` |
| D12.2 | Comparisons MUST be safe for concurrent use. | no test |
| D12.3 | A database built with different Unicode tables MUST be reported with both fingerprints and a fix; an unstamped database is accepted. | no test in this package (exercised by the SQLite migration driver) |
| D12.4 | The fingerprint MUST derive from behaviour, not from a module version. | `TestFingerprint_StableAndOrderingDerived` |
| D12.5 | Equality and UNIQUE behaviour for canonically equivalent (NFC/NFD) strings and for strings differing only in ignorable characters MUST match PostgreSQL's deterministic collations. | no test |

**Deliberate limits.**
- SQLite does not order like PostgreSQL, which follows the database locale.
- `EnsureStamp` is check-then-insert without a transaction; two appliers on a fresh database at once can collide on the stamp row.

## D13. Blob store for uploads

**Code.** `go/lib/kvfs/kvfs.go` — `BuildKey`, `Reader`; `go/lib/kvfs/local_fs/local_fs.go` — `New`, `EnsureRoot`, `PutFromTempFile`; `go/lib/kvfs/memory`.

**Purpose.** Driver interface for uploaded files: atomic publish from a temp file, streaming and seekable reads, stat, delete, hash-sharded keys.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `BuildKey(bucketPath, objectKey, expectedObjects, maxPerBucket)` | `<bucket>/<hh>/<hh>/…/<objectKey>`; depth `ceil(log256(expected/maxPerBucket))`, `0` when flat | capped at 16 |
| `local_fs.New(root)`, `EnsureRoot(root)` | root created with mode 0755; published objects 0644; staging under `<root>/.w17-staging` | — |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D13.1 | Readers MUST NOT observe a partially written object (same-filesystem rename; the cross-device fallback copies into a temp file in the destination directory, then renames). | `TestPutFromTempFile_CrossFS_ReaderNeverSeesPartialBody` |
| D13.2 | A failed cross-device publish MUST NOT truncate or delete an existing object. | `TestPutFromTempFile_CrossFS_CopyErrorPreservesPublishedObject` |
| D13.3 | Keys resolving outside the root MUST be refused: reads report `ErrNotFound`, writes and deletes error. | `TestLocalFS_PathTraversalContained` |
| D13.4 | A key MUST NOT reach outside the root through a symlink. | no test |
| D13.5 | An empty key or `.` MUST be refused on read and delete paths. | no test |
| D13.6 | Delete MUST be idempotent; Open, Stat and OpenSeekable of a missing key MUST return `ErrNotFound`. | `local_fs_test.go`, `memory/*_test.go` |
| D13.7 | Keys MUST NOT carry a leading `/`, so upload and download paths agree across drivers. | `kvfs_test.go` |
| D13.8 | The published mode MUST NOT depend on which publish branch ran. | `TestPutFromTempFile_PublishedModeIsBranchIndependent` |
| D13.9 | Readers (business tier) MUST receive only the `Reader` half; only the gateway writes. | no test |

**Deliberate limits.**
- No `fsync` before or after rename; durability across power loss is not guaranteed.
- `local_fs` ignores the context (file I/O is not cancellable).
- The memory driver reads whole files into memory (test and development use).

## D14. Redis HASH entity codec

**Code.** `go/lib/kvhash/kvhash.go`.

**Purpose.** Flatten a flat proto message into `HSET` field/value pairs and back. The stored layout is a public wire contract.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D14.1 | Field name MUST be the proto field name, with fixed text encodings per kind (bool `true`/`false`, decimal integers, `FormatFloat 'g' -1`, base64 bytes, enum by label with numeric fallback, Timestamp RFC 3339 nano UTC, Duration via protojson). | `TestMarshalUnmarshalRoundTrip`, `kvhash_extra_test.go` |
| D14.2 | Unset presence-bearing fields MUST be omitted so that null and zero stay distinct across a round trip. | `TestMarshalEntity_Proto3OptionalPresence` |
| D14.3 | Repeated, map, nested (non-WKT) and group fields MUST be refused on marshal and unmarshal. | refusal tests in `kvhash_test.go`, `kvhash_extra_test.go` |
| D14.4 | Durations MUST round-trip exactly across the `durationpb` range; an out-of-range Duration is refused on write. | `TestKvHash_Duration*` |
| D14.5 | A value that could not be read back (such as a Timestamp outside the RFC 3339 range) MUST be refused on write. | no test |
| D14.6 | Clearing an optional field on update MUST remove the stored value. | no test |

**Deliberate limits.** Missing hash fields read as proto defaults; unknown hash fields are ignored.

## D15. Redis eviction check

**Code.** `go/lib/kvposture/kvposture.go` — `WarnIfEvicting`.

**Purpose.** Warn at boot when a Redis that stores entities would evict keys without a TTL (`allkeys-*`).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D15.1 | The check MUST only warn and never block boot; an unreadable policy logs at debug level only. | no test |
| D15.2 | A nil client MUST be a no-op. | `TestWarnIfEvicting_NilClientIsSafe` |
| D15.3 | Policy matching MUST be case- and space-insensitive. | `TestEvictsUntaggedKeys` |

**Deliberate limits.** Persistence (RDB/AOF) and `maxmemory` are not checked.

## D16. Column encryption (`CRYPTED_SECRET`)

**Code.** `go/lib/fieldcrypt/fieldcrypt.go` — `Parse`, `FromEnv`, `Keyring.EncryptDeterministic`, `Keyring.Decrypt`, `DevKeys`; `go/lib/fieldcrypt/shared.go` — `Init`.

**Purpose.** Encrypt column values so that the database alone, without the key, yields nothing. Deterministic mode for `unique` columns, randomised otherwise.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `W17_FIELD_KEYS=1:<b64 32 bytes>,2:<…>` | highest version encrypts, all versions decrypt | empty → error; versions must be positive and unique; keys exactly 32 bytes (std, raw or URL base64) |
| `Init()` | installs the process keyring at boot | an error must fail the boot; shared helpers used before `Init` return a clear error |
| `DevKeys` | a fixed development keyring | development only |

**Construction.** HKDF-SHA256 derives an AES-256-GCM key and a separate MAC key from each
configured key. Randomised mode uses a 96-bit random nonce; deterministic mode derives the
nonce from an HMAC of the plaintext. Stored form: `w17c.<ver>.<r|d>.<base64url(nonce||ct||tag)>`.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D16.1 | Randomised and deterministic values MUST round-trip, including the empty string; decryption is mode-agnostic. | `TestRandomised_*`, `TestDeterministic_*`, `TestDecrypt_IsModeAgnostic`, `TestRoundTrip_TheEmptyStringIsAValue` |
| D16.2 | A plaintext row MUST yield `ErrNotEncrypted`, distinct from a tampered or wrong-key value (authentication error). | `TestDecrypt_ATamperedValueIsDistinguishedFromPlaintext`, `TestDecrypt_AWrongKeyDoesNotSilentlyProduceGarbage` |
| D16.3 | An unknown key version MUST yield `ErrUnknownKeyVersion` naming the versions held. | `fieldcrypt_test.go` |
| D16.4 | After rotation, old versions MUST stay decryptable and new writes MUST use the newest key. | `TestRotation_OldValuesStayReadableAndNewOnesUseTheNewestKey` (randomised mode) |
| D16.5 | Deterministic equality lookups and UNIQUE enforcement MUST keep working across a key rotation. | no test |
| D16.6 | Error messages MUST NOT include plaintext or key material. | `TestDecrypt_*` |
| D16.7 | A missing or malformed keyring MUST fail the boot, not the first query. | `TestParse_*`, `shared_test.go` |
| D16.8 | `DevKeys` MUST be refused outside development. | no test |

**Deliberate limits.**
- No protection against a compromised application host, which holds both data and key.
- Deterministic mode reveals equality of values by design.

## D17. Secret values and sources

**Code.** `go/service/secret/secret.go` — `Secret`, `New`; `go/service/secret/source.go` — `Resolver`, `NewResolver`; `go/service/secret/sopsenv/sopsenv.go` — `Rekey`.

**Purpose.** `Secret[T]` makes accidental printing of a secret redact; `Resolver` loads declared secrets from the environment, an age-encrypted file or a plain dotenv file.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `W17_SECRETS_MODE` | `auto` | `plain`, `age`; anything else → error |
| `W17_SECRETS_FILE` | `.secrets` | unreadable → boot error |
| `W17_SECRETS_AGE_FILE` | `.secrets.age` | corrupt → boot error |
| `W17_SECRETS_AGE_KEY`, `W17_SECRETS_AGE_KEY_FILE` | fall back to `SOPS_AGE_KEY`, `SOPS_AGE_KEY_FILE` | bad key, or `age` mode without a key → boot error |

Precedence: environment, then age file, then plain file.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| D17.1 | Every fmt verb, `String`, `GoString`, JSON and text marshalling and slog `LogValue` MUST emit `***` for a `Secret`. | `TestFmtVerbsRedact`, `TestJSONMarshalRedacts`, `TestSlogRedacts` |
| D17.2 | A `Secret` in an exported struct field MUST redact when the struct is printed. | `TestStructRedacts` |
| D17.3 | A `Secret` in an unexported struct field MUST redact when the struct is printed or logged. | no test |
| D17.4 | Unmarshalling MUST load the value while it stays unprintable. | `TestUnmarshalLoadsButStaysRedacted` |
| D17.5 | Construction-time failures MUST fail the boot; lookups never fail. | `source_test.go`, `coverage_extra_test.go` |
| D17.6 | Source precedence MUST be environment, then age file, then plain file; the `SOPS_*` key variables are honoured as fallbacks. | `TestResolver_EnvWins`, `TestResolver_AgeRoundTripAndPrecedence`, `TestConfigFromEnv_SOPSInterop` |
| D17.7 | `sopsenv` MUST be byte-compatible with the sops dotenv format, bind each value to its key name as associated data, and verify the whole-file MAC before returning values. | `TestRoundTrip`, `TestDecrypt_RefusesTampering` |
| D17.8 | `Rekey` MUST refuse a tampered file and open the result for the new key only. | `TestRekey_KeepsValuesOpensForTheNewKeyOnly` |

**Deliberate limits.**
- Plain dotenv values are taken literally: no quote stripping, inline comments or `export` prefix (`KEY="x"` yields `"x"` with the quotes).
- `Resolver.Lookup` returns a plain `string`; wrapping it in `Secret` is the caller's job.
