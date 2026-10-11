# Tooling

The tools a generated project uses to bring its stores up to date (schema and data migrations,
fixtures), to run its end-to-end and load suites, to mint development certificates, and to
digest plugin trees. Packages: `go/tooling/migrate` (with `migratecli`, `factory`, `binroots` and
the dialect appliers under `internal/`), `go/tooling/datamigrate`, `go/tooling/fingerprint`,
`go/tooling/e2e/runner`, `go/tooling/e2e/internal/runtime`, `go/tooling/e2e/stress`,
`go/tooling/certgen`, `go/tooling/pathguard`, `go/tooling/plugindigest`.

Database-backed tests for Postgres and MySQL run only under the `dockertest` build tag or with
`W17_PG_DSN` set; Redis and NATS tests use embedded servers.

```
<binary> migrate apply|status|fetch|rollback      <binary> fixtures apply
 └─ migratecli.Dispatch → Main / Fixtures
     ├─ LoadLockView(w17/lock.yaml) → connection, pinned target id, target hash
     ├─ owned connections → warn about unpinned ones → DSNs from W17_TARGET_<CONN>
     ├─ artefacts: --migrations DIR  |  --fetch (gRPC, held in memory)
     └─ migrate.Run / RunRollback / Plan
          Plan: load *.json → recompute content hash → walk chain back from pinned target
                → applied-head cutoff → squash-adopt decision
          Run:  extension preflight → schema prep → per migration:
                take run lock → re-read head under lock
                → fake | adopt | drift gate → apply (or resume post-tx on Postgres)

generated e2e test → runner.RunScenario(steps, callers, opts)
  per step: expand input → pick token → subscribe events (before the call)
            → REST / MCP / SSE call → expect | expect_error | expect_transport_error
            | expect_stream → await events
```

## Features

| ID | Feature | Code |
|---|---|---|
| T1 | Command dispatch of built-in roots | `go/tooling/migrate/migratecli/migratecli.go` — `Dispatch`; `go/tooling/migrate/binroots/binroots.go` — `Reserved` |
| T2 | `migrate apply` (CLI) | `go/tooling/migrate/migratecli/migratecli.go` — `Main`, `parseFlags`, `warnUnpinned`, `dsnSpecs`; `go/tooling/migrate/lockview.go` — `LoadLockView`, `TargetEnvVar` |
| T3 | `migrate status` | `go/tooling/migrate/migratecli/migratecli.go` — `reportStatus` |
| T4 | Artefact fetch (`migrate fetch`, `--fetch`) | `go/tooling/migrate/migratecli/fetch.go` — `fetchMigrations`, `transportFor`, `consoleTLSConfig`, `bearerCreds`; `go/tooling/migrate/lockview.go` — `MigrationSetFS`; `go/tooling/migrate/orchestrator.go` — `WriteMigration` |
| T5 | `migrate rollback` (CLI) | `go/tooling/migrate/migratecli/migratecli.go` |
| T6 | `fixtures apply` | `go/tooling/migrate/migratecli/fixtures.go` — `Fixtures`, `refuseStaleRender`; `go/tooling/migrate/fixtureseed.go` |
| T7 | Content hash | `go/tooling/migrate/content_hash.go` — `ContentHash`, `ContentHashWithPreflight`, `ContentHashMatches` |
| T8 | Chain selection | `go/tooling/migrate/orchestrator.go` — `chainFromTarget` |
| T9 | Artefact sources | `go/tooling/migrate/orchestrator.go` — `migrationsFSOf` |
| T10 | Plan and squash adopt | `go/tooling/migrate/orchestrator.go` — `Plan` |
| T11 | Apply run: ordering of side effects | `go/tooling/migrate/orchestrator.go` — `Run`, `runApplierCache`, `applyOrResume`; `go/tooling/migrate/extpreflight.go`; `go/tooling/migrate/schemaprep.go` |
| T12 | Fake apply | `go/tooling/migrate/orchestrator.go` — `applyFake`; `go/tooling/migrate/content_hash.go` — `preflightBound` |
| T13 | Drift gate | `go/tooling/migrate/pre_fingerprint.go` — `checkPreFingerprint` |
| T14 | Rollback planning and execution | `go/tooling/migrate/orchestrator.go` — `PlanRollback`, `RunRollback` |
| T15 | Dialect appliers and run locks | `go/tooling/migrate/internal/{postgres,mysql,sqlite,redis,nats,s3}/`; `go/tooling/migrate/factory/factory.go` — `FromTargets`, `SeedCandidate`; `go/tooling/migrate/internal/dsnredact` |
| T16 | Data migrations | `go/tooling/datamigrate/`; `go/tooling/migrate/internal/redis/datamigrate.go`; `go/tooling/migrate/internal/s3/datamigrate.go` |
| T17 | Dev apply, store state, snapshots, render provenance | `go/tooling/migrate/dev_apply.go` — `DevApply`; `store_state.go` — `StoreSchemaStateOf`; `snapshot.go`; `renderprov.go` — `StaleRenders` |
| T18 | Fingerprint and observation | `go/tooling/fingerprint/` — `Format`, `FingerprintHex`, `Observe` |
| T19 | E2E scenario and step contract | `go/tooling/e2e/runner/scenario.go` — `RunScenario` |
| T20 | E2E auth token threading | `go/tooling/e2e/runner/scenario.go`, `caller.go`, `stream.go` |
| T21 | E2E inputs, captures, generators | `go/tooling/e2e/internal/runtime/` — `context.go`, `interpolate.go`, `generator.go` |
| T22 | E2E matchers | `go/tooling/e2e/internal/runtime/matcher.go` |
| T23 | E2E REST caller | `go/tooling/e2e/runner/caller.go` — `ResolveREST`, `Call` |
| T24 | E2E MCP caller | `go/tooling/e2e/runner/mcp.go` |
| T25 | E2E streams and events | `go/tooling/e2e/runner/stream.go`, `events.go` — `Await` |
| T26 | Stress | `go/tooling/e2e/stress/` — `engine.go`, `report.go` — `Check` |
| T27 | Dev PKI | `go/tooling/certgen/certgen.go` — `EnsureCerts` |
| T28 | Path guard | `go/tooling/pathguard/pathguard.go` — `Join`, `Contains`, `ValidateRel` |
| T29 | Plugin digest | `go/tooling/plugindigest/plugindigest.go` — `Of`, `Check` |

## T1. Command dispatch of built-in roots

**Code.** `go/tooling/migrate/migratecli/migratecli.go` — `Dispatch`; `go/tooling/migrate/binroots/binroots.go` — `Reserved`

**Purpose.** The one entry a generated `main` calls: answers `migrate`, `fixtures` and help, and
refuses unknown bare words so that a mistyped verb never starts the server.

**Configuration.**

| Setting | Behaviour |
|---|---|
| `argv[0]` = `migrate` / `fixtures` | handled |
| `-h`, `--help`, `help` | usage printed, reported as handled |
| any other bare word | error "unknown command" |
| leading `-` | falls through to the generated main |
| no arguments | falls through; the server starts |

**Shares.** `binroots.Reserved`; the `health` command of `go/service/healthcheck`.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T1.1 | An unknown verb MUST NOT start the server. | `TestDispatch_UnknownVerbIsRefused` |
| T1.2 | `--help` MUST NOT bind a port. | `TestDispatch_HelpDoesNotStartAServer` |
| T1.3 | Flags MUST fall through to the generated main. | `TestDispatch_FlagsStillFallThrough` |
| T1.4 | Every root listed in `binroots.Reserved` MUST be answered by the binary; a root the generated main relies on (such as `health`) MUST NOT be refused as unknown, and `Reserved` MUST list only roots that exist. | `TestDispatch_AnswersForEveryReservedRoot` (covers `migrate` and `fixtures` only) |

## T2. `migrate apply` (CLI)

**Code.** `go/tooling/migrate/migratecli/migratecli.go` — `Main`, `parseFlags`, `warnUnpinned`, `dsnSpecs`; `go/tooling/migrate/lockview.go` — `LoadLockView`, `TargetEnvVar`

**Purpose.** Apply every pending migration up to each owned connection's pinned target.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `--lock` | `w17/lock.yaml` | — |
| `--migrations` | `w17/migrations` | — |
| `--fetch` | off | see T4 |
| `--console ADDR` | `W17_CONSOLE_ADDR` | flag overrides the variable |
| `--dry-run` | off | refused together with `--fake` |
| `--fake` | off | see T12 |
| `--log-format` | `text` | `text` or `json`; anything else refused |
| `--parallel N` | the data migration's own setting | `<= 0` keeps the migration's own setting |
| `--allow-no-dsn` | — | `fixtures apply` only; on `apply`, `status`, `rollback` and `fetch` refused with a message naming `fixtures apply` |
| DSN | `W17_TARGET_<CONN>` (connection name upper-cased, `-` → `_`) | no flag accepts a DSN or a token |
| Timeout | 30 minutes, fixed | — |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T2.1 | A served connection without a pin MUST be named in a warning before DSNs are resolved or artefacts fetched. | `TestApplyAndStatus_WarnAboutAServedConnectionWithNoPin`, `TestUnpinned_IsReportedBeforeDSNsAndFetching` |
| T2.2 | A pinned connection without a DSN MUST fail, naming the variable. | `TestDsnSpecs_MissingDSNNamesTheVariable` |
| T2.3 | An unpinned connection MUST NOT require a DSN. | `TestDsnSpecs_UnpinnedConnectionNeedsNoDSN` |
| T2.4 | No flag MUST accept a DSN or a token. | `TestFlags_OfferNoWayToPassADSN` |
| T2.5 | A bundle that matches no lock connection MUST fail instead of reporting "nothing pending". | `TestBuildConfig_NoMatchingConnectionIsAnError` |
| T2.6 | Fetched artefacts MUST pass the same loader and hash check as artefacts on disk. | `TestMigrationSetFS_RefusesATamperedBody` |
| T2.7 | Two distinct connections MUST NOT resolve to the same DSN variable. | no test |
| T2.8 | A connection listed twice in the lock MUST be refused by planning and rollback planning before any applier is used. | `TestRun_RefusesAConnectionListedTwice` |
| T2.9 | Every flag a verb accepts MUST take effect on that verb. | `TestParseFlags_AllowNoDSNOnlyOnFixtures` (`--allow-no-dsn` only) |
| T2.10 | `--allow-no-dsn` on any verb other than `fixtures apply` MUST be refused with a message naming where it applies. | `TestParseFlags_AllowNoDSNOnlyOnFixtures` |

**Deliberate limits.**
- Unpinned connections are warned about, not refused: a KV connection can stay unpinned for good.

## T3. `migrate status`

**Code.** `go/tooling/migrate/migratecli/migratecli.go` — `reportStatus`

**Purpose.** Report pending migrations and change nothing.

**Configuration.** The flags of `migrate apply`, plus `--exit-code` (pending migrations give a
non-zero exit). Timeout 5 minutes.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T3.1 | `--exit-code` MUST turn pending migrations into a non-zero exit. | `TestReportStatus_ExitCode` |
| T3.2 | `status` MUST NOT write to the database, alter the ledger, or take locks that block on an in-flight migration. | no test |

## T4. Artefact fetch (`migrate fetch`, `--fetch`)

**Code.** `go/tooling/migrate/migratecli/fetch.go` — `fetchMigrations`, `transportFor`, `consoleTLSConfig`, `caPool`, `bearerCreds`; `go/tooling/migrate/lockview.go` — `MigrationSetFS`; `go/tooling/migrate/orchestrator.go` — `WriteMigration`

**Purpose.** Download migration artefacts from the console. `migrate fetch` writes them under
`--migrations`; `apply --fetch` keeps them in memory.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `W17_CONSOLE_ADDR` | required | `grpcs://` or bare `host:port` → TLS 1.2+; `grpc://` → plaintext |
| `W17_TOKEN` | none (no bearer sent) | — |
| `W17_CONSOLE_ORG` | none | sent as `w17-org` metadata |
| `W17_CONSOLE_CA` | system roots | additional PEM roots; an unreadable file leaves verification against system roots, which then fails |
| `W17_CONSOLE_TLS_SKIP_VERIFY` | off | `true` (case-insensitive) |
| Response size | 256 MiB maximum | — |

Unpinned connections are not requested; when every connection is unpinned the console is not
contacted.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T4.1 | The default transport MUST be TLS with system roots. | no test |
| T4.2 | The bearer MUST NOT be sent in clear by default. | no test |
| T4.3 | An `Unauthenticated` reply MUST produce a message naming `W17_TOKEN`. | no test |
| T4.4 | Fetched artefacts MUST be written only under `--migrations`: a connection name or migration id that is not one plain path segment (no separator, `:`, `.`/`..` or control character) MUST be refused before anything is written, and every write MUST pass the path guard (T28). | `TestWriteMigration_RefusesPathShapedNames`, `TestWriteMigration_PlainNamesLandUnderRoot` |
| T4.6 | An in-memory fetch (`--fetch`) MUST apply the same segment rule to connection names and ids as a fetch to disk. | `TestWriteMigration_RefusesPathShapedNames` |
| T4.5 | A fetched set MUST contain only the requested connections, and a duplicate `(connection, id)` MUST be refused. | no test |

## T5. `migrate rollback` (CLI)

**Code.** `go/tooling/migrate/migratecli/migratecli.go`

**Purpose.** Roll back applied migrations down to a named id.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `--to ID` | required | `--to=""` explicitly means "roll back everything" |
| `--dry-run` | off | prints `down_pre_tx`, then `down_sql` |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T5.1 | Rollback without `--to` MUST be refused. | `TestRollback_RefusesWithoutTo` |
| T5.2 | An explicit empty `--to` MUST be accepted. | `TestRollback_ExplicitEmptyToIsAccepted` |
| T5.3 | `--dry-run` MUST print the pre-transaction half before the down SQL. | `TestRunRollback_DryRunPrintsPreTx` |
| T5.4 | A non-empty `--to` that is not the id of a migration on one of the connections being rolled back MUST be refused, planning nothing. | `TestPlanRollback_RefusesAnUnknownTarget` |

## T6. `fixtures apply`

**Code.** `go/tooling/migrate/migratecli/fixtures.go` — `Fixtures`, `refuseStaleRender`; `go/tooling/migrate/fixtureseed.go` — `WriteFixtureSeed`; `go/tooling/migrate/internal/postgres/postgres.go` — `ExecSeed`

**Purpose.** Seed console-rendered fixture statements into one owned relational connection.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `--fixtures DIR` | `w17/fixtures` | — |
| `--fetch` | off | — |
| `--group` | default group | `.` and a trailing `/` normalise to the default group |
| `--domain`, `--connection` | none | `--connection` required when two connections can seed |
| `--dry-run` | off | — |
| `--allow-stale` | off | accepts a render older than its fixture |
| `--allow-no-dsn` | off | accepts a run with no DSN anywhere; accepted by `fixtures apply` only (T2.10) |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T6.1 | A whole group MUST apply in one transaction; a failing statement MUST leave nothing behind. | `TestFixturesApply_TheWholeGroupIsOneUnitOfWork`, `TestExecSeed_APartialFixtureIsRolledBack` |
| T6.2 | Values MUST be bound as parameters, never interpolated. | `TestExecSeed_AppliesParameterizedValues` |
| T6.3 | A render older than its fixture MUST be refused unless `--allow-stale`. | `TestFixturesApply_RefusesARenderOlderThanItsFixture` |
| T6.4 | No DSN anywhere MUST be an error unless `--allow-no-dsn`. | `TestFixturesApply_NoDSNAnywhereIsAnError` |
| T6.5 | Two seedable connections MUST require `--connection`. | `TestFixturesApply_TwoSeedableConnectionsMustBeDisambiguated` |
| T6.6 | Only connections whose applier can seed MUST be nominated as seed candidates. | no test |
| T6.7 | An unknown `fixtures` subcommand MUST print the fixtures usage. | no test |

**Deliberate limits.**
- Groups are a naming convention, not a safety gate; nothing refuses seeding a production store.
- "Nothing to seed" is a success (exit 0).
- Seeding does not check that the schema is at the pinned head; the stale-render check compares
  fixtures with their render, not with the database.

## T7. Content hash

**Code.** `go/tooling/migrate/content_hash.go` — `ContentHash`, `ContentHashWithPreflight`, `ContentHashMatches`

**Purpose.** A keyless integrity hash shared by console and client. It covers `up_sql`,
`up_post_tx`, `down_pre_tx`, `down_sql`, `prev`, `supersedes` (order-sensitive), `adopt_sql`,
`adopt_preflight_sql` (an `adoptpre` segment, written only when the preflight is non-empty) and
the sorted, deduplicated `required_extensions` projection of `manifest_json`. The encoding is
length-prefixed and version-tagged (`v1` when none of prev, supersedes, adopt, preflight or
extensions is set, `v2` otherwise). `ContentHashWithPreflight` is the full formula; `ContentHash`
is the same formula without a preflight, so a migration without one has the same digest under
either. An artefact verifies against the full formula or the formulas accepted before (without
the preflight, and without extensions); only `--fake` requires the preflight to be bound (T12).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T7.1 | Editing any hashed field of any chained artefact MUST be refused. | `TestPlan_TamperedIntermediateIsRefused`, `TestRun_TamperedAdoptFieldsAreRefused`, `TestRun_TamperedSupersedesAloneIsRefused`, `TestRun_TamperedAdoptSqlAloneIsRefused`, `TestPlan_DiskTamperedRefuses` |
| T7.2 | The declared extension set MUST be bound by the hash. | `TestContentHash_BindsTheExtensionSet` |
| T7.3 | Every artefact field MUST be either hashed or explicitly excused with a reason. | `TestContentHash_EveryMigrationFieldIsHashedOrExcused` |
| T7.4 | Any artefact content that is executed or that changes what is executed MUST be covered by the integrity check before it runs. | `TestContentHash_EveryMigrationFieldIsHashedOrExcused`, `TestFake_RefusesAPreflightTheDigestDoesNotCover`, `TestFake_BoundPreflightCannotBeReplaced` |
| T7.5 | A migration without a preflight MUST hash identically under `ContentHash` and `ContentHashWithPreflight`, and a non-empty preflight MUST move the digest. | `TestContentHashWithPreflight_EmptyPreflightKeepsTheDigest` |

## T8. Chain selection

**Code.** `go/tooling/migrate/orchestrator.go` — `chainFromTarget`

**Purpose.** Only artefacts reachable by walking `prev_content_sha256` back from the pinned
target execute.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T8.1 | An artefact inserted off the chain MUST NOT run. | `TestPlan_InsertedMigrationIsNotOnTheChain` |
| T8.2 | A missing predecessor MUST be refused. | `TestPlan_MissingPredecessorIsRefused` |
| T8.3 | A duplicate content hash MUST be refused. | `TestPlan_DuplicateContentHashIsRefused` |
| T8.4 | Ids MUST increase along the chain. | `TestPlan_RelabelledIdIsRefused` |
| T8.5 | An unchained legacy set with pending migrations below the target MUST be refused. | `TestPlan_UnchainedArtifactSetIsRefused` |
| T8.6 | The pinned target MUST carry a hash, and the artefact MUST match it. | `TestPlan_PinnedTargetEmptyHash_Refused`, `TestPlan_TargetHashMismatch` |
| T8.7 | The walk MUST terminate on a cycle. | no test |

**Deliberate limits.**
- The lock file is trusted input; integrity of the chain is anchored on the pinned hash in the lock.
- `PlanRollback` with no pinned target selects from the whole directory. The CLI never builds an
  applier for an unpinned connection; other callers of the public `PlanRollback` must not either
  (`TestPlanRollback_NoPinnedTarget_StillWorks` records the decision).

## T9. Artefact sources

**Code.** `go/tooling/migrate/orchestrator.go` — `migrationsFSOf`

**Purpose.** Artefacts come from `MigrationsDir` or from `MigrationsFS` (embedded or in-memory).

**Configuration.** Exactly one of `MigrationsDir`, `MigrationsFS`; both or neither is an error. A
missing connection directory is an empty set. Non-`.json` files are ignored. `connection` is
filled from the directory only when the artefact leaves it empty.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T9.1 | Two sources at once MUST be refused. | `TestMigrationsFS_RefusesTwoSources` |

## T10. Plan and squash adopt

**Code.** `go/tooling/migrate/orchestrator.go` — `Plan`

**Purpose.** Per connection (in lexical order, unpinned skipped), read the applied head and
compute the pending chain entries (`id > head`). A baseline whose `supersedes` contains the head
is adopted (recorded through `adopt_sql`, no DDL).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T10.1 | Adoption MUST happen only when the head is the last superseded id; a mid-range head MUST be refused. | `TestPlan_RefusesAdoptFromAMidRangeHead` |
| T10.2 | A baseline the database already satisfies MUST be adopted without running its DDL. | `TestRun_AdoptsBaselineTheDatabaseAlreadySatisfies` |
| T10.3 | A baseline without `adopt_sql` MUST NOT be adopted. | `TestRun_BaselineWithoutAdoptSQLRefuses` |

## T11. Apply run: ordering of side effects

**Code.** `go/tooling/migrate/orchestrator.go` — `Run`, `runApplierCache`, `applyOrResume`, `closeAll`, `captureMigrationError`; `go/tooling/migrate/extpreflight.go`; `go/tooling/migrate/schemaprep.go` — `schemaPrepSQL`

**Purpose.** Apply the plan in a fixed order:
1. Dry-run prints `up_sql` and `up_post_tx` and applies nothing.
2. Extension preflight (Postgres, also through decorators via `Unwrap`) refuses before anything
   is written if a declared extension is missing.
3. Schema prep: `CREATE SCHEMA IF NOT EXISTS` for `required_schemas` (Postgres; also under
   `--fake`).
4. Per migration: the applier is cached per connection and the run lock taken at first open;
   the head is re-read under the lock once per connection, and ids at or below it are skipped as
   applied by a concurrent run.
5. `--fake` → fake apply (T12); adopt → `adopt_sql`; otherwise drift gate (T13), then apply or
   resume the post-transaction half.
6. A failure aborts the run, logs one structured line, and is reported to the error tracker when
   one is initialised.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T11.1 | Dry-run MUST print both halves and apply nothing. | `TestRun_DryRunPrintsSQL`, `TestRun_DryRunPrintsPostTx` |
| T11.2 | A missing extension MUST be refused before anything is written, also behind a decorator. | `TestPreflightExtensions_RefusesBeforeAnythingIsWritten`, `TestPreflightExtensions_SurvivesADecorator` |
| T11.3 | Schema names MUST be quoted with embedded quotes doubled. | `TestSchemaPrepSQL_QuotesEachName` |
| T11.4 | Work applied by a concurrent run while this one queued MUST be skipped. | `TestApply_SkipsWhatAnotherRunAppliedWhileWeQueued` |
| T11.5 | A head that moved backward under the lock MUST be refused. | `TestApply_RefusesWhenTheHeadMovedBACKWARD` |
| T11.6 | A second run against a store with a run lock MUST fail fast with "run-lock held", never wait. | `TestRun_LockHeldAbortsFailFast` |
| T11.7 | On Postgres, a migration whose in-transaction half committed but whose post-transaction half did not MUST resume only the post-transaction half. | `TestRun_ResumesPendingSkirt`, `TestPreFingerprint_DoesNotWedgeAResume` |
| T11.8 | A migration MUST NOT be applied twice within one run. | no test |
| T11.9 | The run MUST NOT report success for work not done; the closing count MUST include only migrations actually applied. | no test |
| T11.10 | Losing the run lock during a run MUST fail the run. | no test |

## T12. Fake apply

**Code.** `go/tooling/migrate/orchestrator.go` — `applyFake`; `go/tooling/migrate/content_hash.go` — `preflightBound`

**Purpose.** Bring an existing database under management: record each pending migration without
running its body, guarded by `adopt_preflight_sql`. The drift gate is skipped; the preflight is
the check, and it runs only when the artefact's digest covers it (T7).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T12.1 | An empty preflight or empty `adopt_sql` MUST be refused. | no test |
| T12.2 | Preflight and ledger write MUST run as one unit, so a failing check leaves no ledger row. | `TestFake_BoundPreflightRuns` (one body, preflight first; a failing check on a real database has no test) |
| T12.3 | `--fake` together with `--dry-run` MUST be refused. | no test |
| T12.4 | A drift refusal MUST name the fake apply as the remedy. | `TestDriftRefusal_NamesTheFakeApply` |
| T12.5 | A migration whose digest does not cover its preflight MUST be refused before anything executes; a replaced or stripped bound preflight MUST be refused as a content mismatch. | `TestFake_RefusesAPreflightTheDigestDoesNotCover`, `TestFake_BoundPreflightCannotBeReplaced`, `TestFake_BoundPreflightCannotBeStripped` |
| T12.6 | A normal apply MUST NOT require the preflight to be bound, since it never runs it. | `TestApply_UnboundPreflightStillAppliesForReal` |

**Deliberate limits.**
- An artefact minted before the preflight was bound must be fetched again from a console that
  binds it before it can be fake-applied; its normal apply is unaffected.

## T13. Drift gate

**Code.** `go/tooling/migrate/pre_fingerprint.go` — `checkPreFingerprint`

**Purpose.** Before each body, compare the target's schema fingerprint with the migration's
`-- w17:expected_pre_fingerprint:` first-line header (`--` or `#`, in `up_sql` or `up_post_tx`).

**Configuration.** The gate passes without comparing when: there is no header; the value starts
with `FAKE_`; the Postgres migration is in the pending post-transaction phase; the applier has no
fingerprint capability (Redis, NATS, S3); the fingerprint is empty.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T13.1 | A fingerprint read error MUST refuse, never count as agreement. | `TestPreFingerprint_ReadFailureIsNotAgreement` |
| T13.2 | A real expectation that differs from the target MUST refuse the migration. | no test |

**Deliberate limits.**
- The fingerprint covers tables and columns only (see T18); index, constraint and non-default
  namespace drift is not detected.

## T14. Rollback planning and execution

**Code.** `go/tooling/migrate/orchestrator.go` — `PlanRollback`, `RunRollback`; `go/tooling/migrate/applier.go`

**Purpose.** Select `id > to` that are applied (or, on Postgres, pending post-transaction above the
head), newest first, and run their down bodies verbatim. The ledger row is erased at the position
the dialect defines.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T14.1 | An in-range id not on the pinned chain MUST NOT be rolled back. | `TestRunRollback_OffChainMigrationIsNotRolledBack` |
| T14.2 | The head re-read under the lock MUST equal the planned head, and refuse only when it actually moved. | `TestRollback_RefusesOnlyWhenTheHeadActuallyMoved`; `TestRunRollback_ReVerifiesTheHeadUnderTheLock` (source-text check) |
| T14.3 | Irreversible data migrations MUST refuse to roll back. | `TestRollback_YAMLIrreversibleRefused` |
| T14.4 | A connection listed more than once MUST be refused; a non-empty rollback target MUST name a migration on the connections being rolled back (T5.4). | `TestRun_RefusesAConnectionListedTwice`, `TestPlanRollback_RefusesAnUnknownTarget` |

**Deliberate limits.**
- Recovery from a partial rollback is the operator's.

## T15. Dialect appliers and run locks

**Code.** `go/tooling/migrate/internal/postgres/postgres.go`, `internal/mysql/mysql.go`, `internal/mysql/runlock.go`, `internal/sqlite/sqlite.go`, `internal/redis/redis.go`, `internal/redis/runlock.go`, `internal/nats/nats.go`, `internal/s3/s3.go`, `internal/s3/runlock.go` (all under `go/tooling/migrate/`); `go/tooling/migrate/factory/factory.go` — `FromTargets`, `SeedCandidate`

**Purpose.** One applier per store kind. Run locks: Postgres serialises on the ledger primary key, plus an advisory lock with a phase re-check for the post-transaction half; MySQL takes `GET_LOCK` on a dedicated session, per schema; SQLite relies on the ledger primary key; Redis uses `SET NX` on `w17:migrate:runlock` (TTL 5 min, heartbeat 60 s); S3 uses a conditional PUT of `w17-migrate-runlock.json`, stale after 3 min.

| Capability | Postgres | MySQL | SQLite | Redis | NATS | S3 |
|---|---|---|---|---|---|---|
| Apply | body's own `BEGIN…COMMIT`, one simple-protocol exec | body through the pool; DDL commits implicitly | one dedicated connection, `foreign_keys=ON` | commands line by line | commands line by line | commands line by line |
| Post-transaction resume | yes | no (warned at run time) | no (warned) | — | — | — |
| Fingerprint / Observe | yes / yes | yes / no | yes / no | schemaless | schemaless | schemaless |
| Seed | yes | no | no | no | no | no |
| Ledger | `w17_migrations` | `w17_migrations` | `w17_migrations` | hash `w17:migrations` | KV bucket `w17-migrations` | objects `w17-migrations/<id>.json` |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T15.1 | Redis and S3 run locks MUST be mutually exclusive. | `TestRunLock_MutualExclusion`, `TestS3RunLock_MutualExclusion` |
| T15.2 | A stale lock MUST be taken over; a refreshed lock MUST NOT be. | `TestRunLock_StaleTakeoverViaTTL`, `TestS3RunLock_StaleTakeover_ZombieReleaseNoOp`, `TestS3RunLock_RefreshPreventsTakeover` |
| T15.3 | Release MUST be owner-checked. | `TestRunLock_ReleaseIsOwnerChecked`, `TestS3RunLock_StaleTakeover_ZombieReleaseNoOp` |
| T15.4 | The MySQL run lock MUST survive an idle server `wait_timeout`. | `TestRunLock_SurvivesAnIdleServerTimeout` (`dockertest`) |
| T15.5 | SQLite foreign-key violations after a table rebuild MUST be caught. | `TestApply_RebuildFKViolation_Caught` |
| T15.6 | Two appliers MUST NOT run the same non-idempotent body against one store concurrently. | no test |
| T15.7 | Postgres and MySQL apply, rollback, applied head, phase and the post-transaction advisory lock MUST behave as specified against a real database. | no test |
| T15.8 | Errors MUST NOT contain credentials: a DSN named in an error (unknown scheme, unparseable URL, duplicate or empty `--target` entry) has its password and password-like parameters masked, and the error keeps its context. | `TestErrors_NeverPrintTheDSNPassword`, `TestString_MasksCredentials`, `TestErr_RedactsAURLError` |
| T15.9 | A command line the applier does not recognise MUST fail the migration, not be skipped. | no test |
| T15.10 | Wipe MUST return the store to the empty state, including namespaces created by schema prep, and MUST NOT change session settings that outlive it. | no test |
| T15.11 | NATS DSN user info is not used. | `TestDSNToServer_DropsUserInfo` |

**Deliberate limits.**
- NATS accepts only the `nats://` scheme, without credentials in the DSN.
- Redis wipe is `FLUSHDB` on the logical database.

## T16. Data migrations

**Code.** `go/tooling/datamigrate/` — `marshal.go`, `validate.go`, `json.go`, `transform.go`; `go/tooling/migrate/internal/redis/datamigrate.go`; `go/tooling/migrate/internal/s3/datamigrate.go`

**Purpose.** YAML bodies (recognised by a first non-comment line `version:`) that rewrite stored
values: `ADD_FIELD_DEFAULT`, `REMOVE_FIELD`, `RENAME_FIELD`, `TRANSFORM_FIELD` (Starlark).

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `version` | — | only `1` |
| `encoding` | — | `json` or `protobuf` (needs descriptor and message) |
| `parallel` | 4 | `>= 0`; the CLI `--parallel` wins |
| Unknown YAML fields | ignored | — |
| Starlark | `json` module only, 1 000 000 steps per call, no file system | — |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T16.1 | Starlark MUST be bounded by the step limit and MUST NOT reach the file system. | `TestTransformVM_StepLimit`, `TestTransformVM_NoFileSystemAccess` |
| T16.2 | An op-granularity cursor MUST resume after a crash. | `TestApply_YAMLResumability_Live` |
| T16.3 | An interrupted `TRANSFORM_FIELD` MUST be refused on resume, not re-run. | `TestApply_YAMLTransformResumeRefused_Live` |
| T16.4 | Key TTL and S3 object metadata MUST be preserved. | `TestApply_YAMLPreservesTTL_Live`, `TestApply_YAMLPreservesObjectMetadata` |
| T16.5 | `RENAME_FIELD` MUST NOT overwrite a differing destination. | no test |
| T16.6 | Inverse ops MUST be derived; `REMOVE_FIELD` and `TRANSFORM_FIELD` MUST be irreversible. | `TestInverseOperations_*`, `TestRollback_YAMLIrreversibleRefused` |
| T16.7 | A stored value that is not a JSON object MUST be refused with an error. | no test |
| T16.8 | Each key MUST be transformed at most once per run. | no test |
| T16.9 | Refusal messages MUST name only flags that exist; the irreversible-rollback refusal names the migration and a rollback target (`--to`) that keeps it applied. | `TestRollback_YAMLIrreversibleRefused` (Redis and S3) |
| T16.10 | Running into the apply timeout MUST leave a state that a later run can resume or refuse with a clear remedy. | no test |
| T16.11 | A stored JSON `null` MUST be treated like an empty value: every op is a no-op on it and none panics. | `TestJSONApplyOp_NullValueIsANoOp` |

**Deliberate limits.**
- Values are rewritten per key with GET then SET, not compare-and-set; run a data migration while
  no application writes that keyspace.
- NATS has no data-migration path.
- An irreversible data migration has no override flag; the way past it is a rollback target
  that keeps it applied.

## T17. Dev apply, store state, snapshots, render provenance

**Code.** `go/tooling/migrate/dev_apply.go` — `DevApply`; `go/tooling/migrate/store_state.go` — `StoreSchemaStateOf`; `go/tooling/migrate/snapshot.go`, `go/tooling/migrate/internal/*/snapshot.go`; `go/tooling/migrate/renderprov.go` — `StaleRenders`

**Purpose.** `DevApply` runs a dev plan per migration with `CONCURRENTLY` stripped and the baseline
recorded inside the schema transaction. `StoreSchemaStateOf` reports Unknown (no fingerprint),
Empty (fingerprint of the empty schema) or Populated. Snapshots dump and restore a store.
`StaleRenders` compares fixture digests with the render manifest.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T17.1 | The baseline MUST be recorded inside the schema transaction; a `COMMIT` inside a literal MUST be ignored. | `TestDevApplySQL_RecordsTheBaselineInsideTheSchemaTransaction`, `TestDevApplySQL_IgnoresACommitInsideALiteral` |
| T17.2 | Schema and extension names MUST be quoted with embedded quotes escaped. | no test |
| T17.3 | Statement rewriting MUST NOT alter text inside dollar-quoted bodies and MUST recognise `COMMIT` in any case. | no test |
| T17.4 | Postgres and MySQL snapshot tools MUST receive a URL DSN's password through the environment, not argv. | `TestSnapshotter_ConnFlagsAndChildEnv` |
| T17.5 | A missing fixture source root MUST read as "cannot tell", not stale. | `TestStaleRenders_CannotTellIsNotStale` |

## T18. Fingerprint and observation

**Code.** `go/tooling/fingerprint/fingerprint.go` — `Format`, `FingerprintHex`; `go/tooling/fingerprint/observe.go` — `Observe`

**Purpose.** The fingerprint is a sorted, `%q`-quoted, injective rendering of tables and columns
(name, type, nullability, default) of `public` (Postgres), the current database (MySQL) or `main`
(SQLite). It excludes `w17_migrations` and, on Postgres, extension-owned tables. `Observe`
(Postgres only) reads all namespaces, indexes, primary, foreign and unique keys, checks and enums.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T18.1 | The fingerprint MUST NOT depend on column or table order. | `TestFingerprintHex_IndependentOfColumnAndTableOrder` |
| T18.2 | A column change MUST change the fingerprint. | `TestFingerprintHex_DistinguishesColumnChange` |
| T18.3 | A dialect without `Observe` MUST be reported as "could not look" (the `Schemaless` marker), never as an empty schema. | no test |

**Deliberate limits.**
- Fingerprints are comparable within one dialect only; type spellings differ between dialects.
- Indexes, constraints and non-default namespaces are outside the fingerprint.

## T19. E2E scenario and step contract

**Code.** `go/tooling/e2e/runner/scenario.go` — `RunScenario`, `SetFormat`

**Purpose.** Run codegen-baked steps in order with one capture scope; the first failure aborts.
`Repeat` runs a step N times. Output is a text checklist or NDJSON, always to stdout.

**Configuration.** Each step carries exactly one of `expect`, `expect_error`,
`expect_transport_error`, `expect_stream` (with `expect_error` for a refused stream). The spec
parser enforces this; streams are checked again at run time.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T19.1 | The first failing step MUST abort the scenario. | no test |
| T19.2 | A stream step with an invalid expectation shape MUST be refused at run time. | no test |

## T20. E2E auth token threading

**Code.** `go/tooling/e2e/runner/scenario.go`, `go/tooling/e2e/runner/caller.go`, `go/tooling/e2e/runner/stream.go`

**Purpose.** When an endpoint requires auth, carries no credential in its URL and the step sets no
`Credential`, `Anonymous` or `TokenFrom`, the step takes the `auth.token` capture. `Credential`
replaces the bearer entirely. `Anonymous` sends the call with no credential at all. `TokenFrom`
names the capture to present in place of `auth.token` (a second principal, or the session token
named on purpose). The three are mutually exclusive. MCP sends the token as an HTTP bearer.
Streams refuse a `Credential`.

**Configuration.**

| Step field | Default | Invalid or edge values |
|---|---|---|
| `Anonymous` | false | combined with `Credential` or `TokenFrom` → error |
| `TokenFrom` | empty (use `auth.token`) | unbound or empty capture → the step fails before sending |
| implicit `auth.token` with `expect_error: UNAUTHENTICATED` | — | the step fails before sending and asks for `Anonymous` or `TokenFrom` |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T20.1 | With no `auth.token` bound, an auth-required step MUST fail before any request is sent. | `TestRunScenario_MissingAuthToken` |
| T20.2 | `Credential` MUST replace the bearer. | `TestCredentialReplacesTheBearer` |
| T20.3 | Static headers MUST NOT set `Authorization`. | `TestAHeaderMapStillCannotOverrideAuthorization` |
| T20.4 | A capability-link step MUST NOT borrow the scenario token. | `TestRunStep_CapabilityLink_DoesNotBorrowScenarioToken` |
| T20.5 | A step MUST be able to call an auth-required endpoint with no credential, so a signed-out refusal can be asserted. | `TestAnonymousStep_SendsNoCredential` |
| T20.6 | Event subscriptions for a step that presents its own `Credential` MUST use that credential's principal. | no test |
| T20.7 | A step that expects `UNAUTHENTICATED` while carrying the session token only by default MUST fail before sending. | `TestImplicitSessionToken_CannotAssertUnauthenticated` |
| T20.8 | `TokenFrom` MUST present the named capture; an unbound or empty capture MUST fail the step before sending. | `TestTokenFrom_UsesTheNamedCapture`, `TestTokenFrom_UnboundOrEmptyFailsBeforeSending` |
| T20.9 | A sign-in that returns no token MUST fail at the capture, so no later step sends an empty or placeholder bearer. | `TestSignInWithoutToken_FailsAtTheCapture` |
| T20.10 | `Anonymous`, `TokenFrom` and `Credential` MUST be mutually exclusive. | `TestStepCredentialFlags_Contradictions` |

## T21. E2E inputs, captures, generators

**Code.** `go/tooling/e2e/internal/runtime/context.go`, `interpolate.go`, `generator.go`

**Purpose.** Input interpolation:

| Token | Meaning |
|---|---|
| `${name}` | a capture; flat key first, then dotted traversal into maps |
| `${seq}`, `${seq:<name>}` | process-global monotonic counters starting at 1 |
| `${once:<name>}` | minted once per scope from the shared counter |
| `${random:uuid\|int\|string}` | `int` in [0, 2^30); `string` is 8 hex characters |

A whole-string token keeps its native type; embedded tokens are stringified (integral floats
without exponent).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T21.1 | `${seq}` MUST be global across scopes within a process. | `TestSeqGlobalAcrossScopes` |
| T21.2 | `${once}` MUST NOT be shared between scopes. | `TestOnce_ScopesDoNotShare` |
| T21.3 | An unresolved reference MUST be an error. | `TestUnresolvedRefErrors` |
| T21.4 | An embedded numeric capture MUST render without exponent. | `TestEmbeddedNumericCaptureHasNoExponent` |

**Deliberate limits.**
- `${seq}` is unique per process only; two runner processes against one database can collide.
- `random:string` has 32 bits; collisions on unique indexes become likely after tens of
  thousands of draws.

## T22. E2E matchers

**Code.** `go/tooling/e2e/internal/runtime/matcher.go`

**Purpose.** Response assertions. A scalar matches exactly (numeric types coerced; string never
equals number); a bare map is a partial nested match. Named matchers: `eq` (deep), `regex`
(captures interpolated as literals), `count`, `num` (numeric strings accepted), `distinct`,
`empty`, `not_empty`, `unwritten` (always fails, placeholder), `capture` (with an optional inner
matcher). proto3 JSON omits an empty list, a zero scalar and an unset message, so an absent field
is read as empty: `count == 0`, `empty`, `distinct` and `eq null` accept it. A capture of an
absent field fails and binds nothing.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T22.1 | Stray keys in a matcher MUST be refused. | `TestDecodeMatcher_RefusesAStrayKeyFromAnUnquotedComma` |
| T22.2 | Vacuous counts (`>= 0`, `!= -1`, …) MUST be refused at decode. | `TestCountMatcher_RefusesAPredicateEveryCountSatisfies` |
| T22.3 | `distinct` MUST refuse a `field` that no element carries instead of counting zero. | `TestDistinctMatcher_TypoInFieldIsLoud` |
| T22.4 | `eq 0` against a field absent from proto3 JSON MUST fail with an explanation of presence. | `TestExactMatcher_ZeroAgainstAnAbsentFieldExplainsPresence` |
| T22.5 | `capture` of an absent field MUST fail and bind nothing, also with an inner matcher that accepts absence. | `TestCapture_AbsentFieldFails` |
| T22.6 | `count == 0`, `empty`, `distinct` and `eq null` MUST accept an absent field as empty. | `TestCountMatcher`, `TestEmptyMatcher`, `TestExactMatcherAbsentVsNil` (`distinct` not asserted) |
| T22.7 | Matchers within one `expect` MUST evaluate in a deterministic order, and a capture used by a sibling matcher MUST resolve. | no test |
| T22.8 | Every spec the matcher messages suggest (the `unwritten` placeholder, the vacuous-count refusal) MUST be one that decode accepts. | `TestMatcherAdvice_Decodes` |
| T22.9 | `regex` MUST stringify numbers the way interpolation does (no exponent). | no test |

**Deliberate limits.**
- The runner cannot tell a misspelt `expect` key from an omitted empty field. The code generator refuses a misspelt key against the response schema.

## T23. E2E REST caller

**Code.** `go/tooling/e2e/runner/caller.go` — `ResolveREST`, `Call`, `multipartBody`, `NewCallError`

**Purpose.** Path params are URL-escaped and removed from the body; declared query params move to
the query; for GET and DELETE every remaining scalar becomes a query param and lists repeat.
Lists in declared query params repeat the key, null query values are omitted, and numbers render
without exponent. An object bound to a declared query or path param, or a nested object on GET or
DELETE, is an error naming the field. Multipart carries the body remainder only, narrowed to
`body_field` when set. `raw_body_field` sends bytes verbatim. Status ≥ 400 becomes a `CallError` decoded from
`{error:{code,message,details}}`; binary media types yield only the `w17_wire` facts; an empty body
is `{}`.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T23.1 | Path, query and body routing MUST follow the endpoint bindings. | `TestResolveREST_*` |
| T23.2 | An error response MUST decode into the canonical envelope. | `TestNewCallError_DecodesTheCanonicalEnvelope` |
| T23.3 | Binary responses MUST yield the wire facts. | `TestDecodeWithWire` |
| T23.4 | Input that cannot be encoded for the method (such as a nested object on GET or DELETE) MUST be an error, never silently dropped. | `TestResolveREST_NestedInputOnBodylessVerbIsRefused` |
| T23.5 | Multipart MUST NOT repeat path and query bindings as form fields and MUST honour `body_field`. | `TestCall_MultipartSendsOnlyBodyFields` |
| T23.6 | Numbers routed into a path or query MUST render without exponent. | `TestResolveREST_NumbersRenderWithoutExponent` |

## T24. E2E MCP caller

**Code.** `go/tooling/e2e/runner/mcp.go`

**Purpose.** One `initialize` per caller (no auth header) and the session id reused; `tools/call`
with the bearer; JSON or SSE-framed responses; `structuredContent` preferred, else the first text
block as JSON. A JSON-RPC error carrying `rpc error: code = X desc = Y` becomes a `CallError`
with a `SCREAMING_SNAKE` code; anything else (HTTP ≥ 400, `isError: true`) is a transport error.
Multipart is refused; static headers are ignored.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T24.1 | MCP calls and status mapping MUST behave as described. | `TestMCPCall_*`, `TestGrpcStatusFromMCPError*` |
| T24.2 | A non-status error MUST stay a transport failure. | `TestGrpcStatusFromMCPError_NonStatusErrorsStayTransportFailures` |

**Deliberate limits.**
- An MCP refusal delivered as HTTP 401/403 or as `isError` can be asserted only with
  `expect_transport_error`, not `expect_error`.

## T25. E2E streams and events

**Code.** `go/tooling/e2e/runner/stream.go`, `go/tooling/e2e/runner/events.go` — `Await`

**Purpose.** Server-stream endpoints are opened (GET, SSE) and frames matched in order; unless
`allow_more`, the stream must close after the declared frames. A non-200 open or an `error` frame
is a `CallError`. Event subscriptions open before the call, one per topic; `Await` skips other
topics and times out after 5 s by default.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T25.1 | Stream frames MUST match in order, and the close and error rules MUST hold. | `TestExpectStream_*` |
| T25.2 | Event awaits MUST honour topic, timeout and error frames. | `TestAwaitEvent_*` |
| T25.3 | An awaited event MUST be attributable to this step's call, not an earlier step's. | no test |
| T25.4 | A suite MUST be able to assert that an event is not delivered (for example to another tenant). | no test |
| T25.5 | A frame whose `data:` is not JSON MUST fail the step, not yield empty data. | no test |

**Deliberate limits.**
- Client-streaming and bidirectional (WebSocket) streams are not supported.

## T26. Stress

**Code.** `go/tooling/e2e/stress/engine.go`, `go/tooling/e2e/stress/report.go` — `Check`

**Purpose.** Setup steps through the runner, then a REST load phase over `fasthttp` in `pool`
(round-robin or random) or `sequence` mode, by count or duration, with warm-up, ramp and think
time. Teardown failure is non-fatal. The report gives p50/p95/p99/max and checks thresholds.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `Thresholds.ErrorRate` | `0` with `ErrorRateSet` false → unset | with `ErrorRateSet` true, `0` means no errors allowed |
| any threshold | none | a run that measured no requests fails every configured threshold |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T26.1 | A run that measured no requests MUST fail its configured thresholds. | `TestCheck_ZeroMeasuredRequestsFails` |
| T26.2 | `error_rate: 0` MUST mean "no errors allowed" when `ErrorRateSet` marks it configured. | `TestCheck_ErrorRateZeroWhenSetAllowsNoErrors` |
| T26.3 | The load phase MUST NOT send the bearer to capability-link endpoints. | no test |

**Deliberate limits.**
- Load success is status < 400; response bodies are not checked.
- Without `ErrorRateSet`, a zero `ErrorRate` reads as unset, so existing threshold literals keep their meaning.

## T27. Dev PKI

**Code.** `go/tooling/certgen/certgen.go` — `EnsureCerts`; `go/tooling/certgen/lock_unix.go`

**Purpose.** Mint a development CA (`ca.crt`, `ca.key`) and server leaf (`server.crt`,
`server.key`) in a directory, idempotently. A leaf that no longer chains to the CA, covers the
requested names or is valid is re-minted.

**Configuration.** ECDSA P-256; 128-bit positive serials; 10-year validity; CA with
`MaxPathLenZero` and KeyUsage CertSign|CRLSign; leaf with ServerAuth and ClientAuth and SANs = the
given names plus `localhost`, `127.0.0.1`, `::1`. Keys `0600`, certificates `0644`, a new
directory `0700` with a `.gitignore` ignoring everything.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T27.1 | Only missing files, and a leaf T27.6 re-signs, MUST be written; a second run MUST change nothing. | `TestEnsureCertsIdempotent` |
| T27.2 | A half-present CA or leaf MUST be refused, never overwritten. | `TestEnsureCA_HalfPresent_*`, `TestEnsureLeaf_HalfPresent_*` |
| T27.3 | A reused CA MUST be a valid, unexpired signing CA whose key matches. | `TestValidateReusableCA` |
| T27.4 | Concurrent runs MUST produce a consistent pair (serialised by `flock` on unix). | `TestEnsureCerts_ConcurrentMint_ConsistentPair` |
| T27.5 | Serials MUST never be zero. | `TestRandSerialFrom_NeverZero` |
| T27.6 | An existing leaf MUST be re-signed when it is not signed by the current CA, misses a requested SAN, is outside its validity window or does not match its key. | `TestEnsureLeaf_ReMintedAfterCAReMint`, `TestEnsureLeaf_ReMintedForANewSAN`, `TestEnsureLeaf_ReMintedWhenExpired` (key mismatch not asserted) |
| T27.7 | An unreadable leaf MUST be refused, not overwritten. | `TestEnsureLeaf_UnreadableIsRefused` |

**Deliberate limits.**
- Development only; no scheduled renewal (a leaf is re-minted only when `EnsureCerts` runs). Permissions of an existing directory are not tightened. The lock
  is a no-op outside unix.

## T28. Path guard

**Code.** `go/tooling/pathguard/pathguard.go` — `Join`, `Contains`, `ValidateRel`

**Purpose.** Lexical containment of relative paths under a base.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T28.1 | `Join` MUST reject empty and absolute paths, `..` escapes and a path resolving to the base itself. | `TestJoin` |
| T28.2 | `Contains` MUST check an already-joined path. | `TestContains` |
| T28.3 | Control characters MUST be rejected. | `TestControlCharsRejected` |
| T28.4 | `ValidateRel` MUST allow `.`. | `TestValidateRel` |

**Deliberate limits.**
- Purely lexical: symlinks are not resolved.

## T29. Plugin digest

**Code.** `go/tooling/plugindigest/plugindigest.go` — `Of`, `Check`

**Purpose.** The canonical digest of a plugin tree, pinned in the lock and bound by the plugin
signature. Length-prefixed, versioned, over sorted paths; covers each regular file's path,
executable bit and content. Skipped: directories named `.git`, `node_modules`, `.w17dev` at any
depth; the path `src/gen`; files named `.DS_Store`; the root `plugin.sig` only.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| T29.1 | Shifting bytes across file boundaries MUST change the digest. | `TestOf_ShiftedBoundariesDoNotCollide` |
| T29.2 | The executable bit MUST be covered. | `TestOf_ExecutableBitIsCovered` |
| T29.3 | Regenerated code under `src/gen` MUST NOT change the digest. | `TestOf_IgnoresRegeneratedPb` |
| T29.4 | Only the root `plugin.sig` MUST be excluded; a signature file below the root MUST be covered. | `TestSignatureFileIsNotPartOfTheDigest`, `TestASignatureFileBelowTheRootIsStillCovered` |
| T29.5 | Symlinks and other non-regular files MUST be refused. | no test |
| T29.6 | `Check` MUST require 64 lower-case hex characters. | no test |
| T29.7 | Every file a consumer compiles or loads from a plugin tree MUST be covered by the digest or regenerated from covered input. | no test |
