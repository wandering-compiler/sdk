# Business SDKs (Python, TypeScript, Rust)

The SDKs a business tier is written with in a language other than Go: `py/` (the
`w17_business` package, served with grpclib), `ts/business/` (`@w17/business`, grpc-js) and
`rs/w17-business/` (the `w17-business` crate, tonic, with the `w17-business-build` helper in
`rs/w17-business/build/`). Each serves the domain's business services from its descriptor set
and manifest, talks to the storage tier and the business companion through the same wire as a
Go tier, and carries the Go runtime's contracts across: the manifest rules, the principal
relay, distributed transactions, events after commit, the drain. A runtime change to
`distx`, `txscope`, `principal`, `eventbus` or the error mapping in Go carries the same change
in the three SDKs, and a row here.

One guarantee is one row, with a column per language naming the test that proves it in that
SDK, and a column for the conformance case that proves it over the wire. The conformance suite
is one Go harness (storage with the distributed-transaction coordinator, NATS, Go's own
subscriber, the business companion with a fake upstream) run against each tier, in three
identical copies: `py/tests/conformance/`, `ts/business/tests/conformance/`,
`rs/w17-business/tests/conformance/` (`make conformance-py`, `conformance-ts`,
`conformance-rs`; the `run.sh` beside each adds the boot checks named `run.sh` below).

Where the unit tests live:

| Language | Tests | Command |
|---|---|---|
| Python | `py/tests/test_*.py` (`test_distx`, `test_server`, `test_runtime`, `test_config`, `test_events`, `test_principal`, `test_companion`) | `make test-py` |
| TypeScript | `ts/business/tests/*.test.ts` (test titles in quotes) | `make test-ts-business` |
| Rust | `rs/w17-business/src/tests/*.rs` (`wire`, `context`, `config`, `manifest`, `events`, `misc`), `rs/w17-business/build/src/lib.rs` | `make test-rs-business` |

CI (`.github/workflows/ci.yml`) runs the Python and the TypeScript unit tests and conformance
suites (jobs `py-business`, `ts-business`); the Rust business SDK's run with `make` only.

A cell that reads **no test** is a statement about coverage in that SDK. A dash (—) means the
row does not apply to that language, with the reason.

## Features

| ID | Feature | Code: Python under `py/src/w17_business/`; TypeScript under `ts/business/src/`; Rust under `rs/w17-business/src/` |
|---|---|---|
| B1 | Manifest | `manifest.py` — `from_dict`; `manifest.ts` — `fromObject`; `manifest.rs` — `from_value` |
| B2 | Configuration and boot | `config.py`, `runtime.py`; `config.ts`, `runtime.ts`; `config.rs`, `runtime.rs` |
| B3 | Serving and error mapping | `server.py` — `Registry`, `_handler`, `_to_status`; `server.ts` — `Registry`, `handler`; `server.rs` — `Registry`, `Served::handle` |
| B4 | Context: deadline and cancellation | `context.py` — `Context`; `context.ts` — `Context`; `ctx.rs` — `Ctx` |
| B5 | Distributed transactions | `distx.py`; `distx.ts`; `distx.rs` |
| B6 | Work after commit | `distx.py` — `on_commit`, `txscope.py`; `distx.ts` — `onCommit`, `txscope.ts`; `lib.rs` — `on_commit`, `txscope.rs` |
| B7 | Events | `events.py`; `events.ts`; `events.rs`, `metadata.rs` |
| B8 | Principal relay and page position | `principal.py`, `paging.py`; `principal.ts`, `paging.ts`; `principal.rs`, `paging.rs` |
| B9 | Clients of storage and of the tier itself | `client.py` — `Conn`, `ServiceClient`; `client.ts` — `Conn`, `ServiceClient`; `channel.rs` — `Outbound` |
| B10 | Drain | `runtime.py` — `App`; `runtime.ts` — `App`; `runtime.rs` |
| B11 | Business companion clients | `files.py`, `egress.py`; `files.ts`, `egress.ts`; `files.rs`, `egress.rs` |

## B1. Manifest

**Purpose.** Read the bundle's `w17-business.json` (written by codegen) into the services,
config keys, storage, events and companion the tier serves, holding it to the same rules the
Go business companion holds it to, so the two agree on which manifests boot.

**Configuration.** `format` 1 or 2 (2 adds the `companion` section); `language` must name the
SDK. Keys the Go companion decodes are read with Go's rules: a string key takes a string (null
is ""), `domain` and `env_prefix` are required. Every other key is held to its JSON type all
the same: lists are lists (null is empty, a null element an empty object), booleans are `true`
or `false` (`"false"` is refused), `shutdown_timeout_seconds` is a number (absent, null, zero or
negative is 10 s). Unknown keys are kept, not refused.

**Shares.** The descriptor set the manifest names is what the server registers against (B3).

**Guarantees.**

| ID | Guarantee | Python | TypeScript | Rust | Conformance |
|---|---|---|---|---|---|
| B1.1 | The keys the Go companion decodes (`format`, `domain`, `env_prefix`, `bundle`, `descriptor_set`) MUST be read with its rules, the same 12-row refusal table in each SDK, and a null MUST take Go's default. | `test_manifest_top_level_is_read_with_gos_rules`, `test_manifest_top_level_nulls_take_gos_defaults` | "the top level is read with Go's rules: %j", "top-level nulls take Go's defaults" | `the_top_level_is_read_with_gos_rules`, `top_level_nulls_take_gos_defaults` | — |
| B1.2 | Every other key MUST be held to its JSON type the same way in the three SDKs: `shutdown_timeout_seconds` a number, `required`/`secret`/`client_streaming`/`server_streaming` a boolean, `services`, `methods`, `config`, `events.emits`, `fields`, `invalidates` and `plugin_hooks` a list of objects; a value of another type is a `ManifestError` naming the key, never a crash, a default or a silently empty list. | `test_manifest_top_level_is_read_with_gos_rules` (its last four rows) | "the top level is read with Go's rules: %j" (its last four rows) | `the_top_level_is_read_with_gos_rules` (its last four rows) | — |
| B1.3 | A `format` the SDK does not read, or a `language` that is not the SDK's, MUST stop the boot naming what it reads. | `test_manifest_format_and_language_are_checked`, `test_format_refusals` | "format and language are checked", "1.0 written in the file is not format 1" | `format_and_language_are_checked` | — |
| B1.4 | The `companion` section MUST be refused where the Go companion refuses it (types, ranges, case-folded keys, a placement on an unknown connection, a file service other than `W17Files`). | `test_companion_refusals_match_the_go_companion`, `test_companion_types_match_the_go_companion` | "refuses what the Go companion refuses", "refuses %s.%s = %s as the Go companion does" | `the_companion_section_is_read_as_go_reads_it` | — |
| B1.5 | A Rust binary MUST refuse to serve a descriptor set other than the one it was compiled from (its SHA-256 is compiled in by `w17-business-build`). | — (reads the set at run time) | — (reads the set at run time) | `the_boot_refuses`, `the_descriptor_set_compiles_without_protoc` | `run.sh`: "a binary refuses a descriptor set it was not compiled from" |

**Deliberate limits.**
- An unknown top-level key is ignored (kept in `extra`), so a manifest written by a newer
  codegen boots on an older SDK unless it changes a key the SDK reads.

## B2. Configuration and boot

**Purpose.** Resolve the manifest's config keys from the environment and the secrets chain,
build the clients, register the services, and refuse to start on anything that would fail
later.

**Configuration.** Each key's variable is `<ENV_PREFIX>_<NAME>` unless the manifest names one;
a secret resolves through the environment, then the age-encrypted file, then the plain file
(`W17_SECRETS_MODE`).

**Guarantees.**

| ID | Guarantee | Python | TypeScript | Rust | Conformance |
|---|---|---|---|---|---|
| B2.1 | A required key that is missing MUST stop the boot naming the key and its variable; a value that does not parse as its type MUST stop it naming the variable. | `test_missing_required_key_stops_the_boot`, `test_required_key_missing_names_key_and_var`, `test_bad_value_stops_the_boot` | "a required key missing names the key and the variable", "a missing required key stops it", "a bad value stops it" | `required_and_secret_keys`, `a_bad_value_names_its_variable` | `run.sh`: "missing required config key stops the boot naming NOTES_API_TOKEN" |
| B2.2 | A secret value MUST resolve environment first, then the age file, then the plain file, and MUST NOT print (repr, string, debug form). | `test_chain_env_then_age_then_plain`, `test_secret_never_prints` | "chains env, then the age file, then the plain file", "a secret never prints" | `the_secrets_chain`, `required_and_secret_keys` | — |
| B2.3 | A dotenv file MUST parse as Go's loader parses it. | `test_dotenv_parse_matches_go` | "parses dotenv like Go" | `dotenv_as_go_parses_it` | — |
| B2.4 | A service the manifest does not list, a missing method, a service registered twice and a plugin hook without a handler MUST stop the boot. | `test_registering_a_service_the_manifest_does_not_list_is_refused`, `test_missing_method_stops_registration`, `test_unknown_or_duplicate_service`, `test_a_plugin_hook_without_a_handler_stops_the_boot` | "registering a service the manifest does not list is refused", "a missing method stops it", "an unknown or duplicate service", "a plugin hook without a handler stops it" | `a_service_the_manifest_does_not_list_is_refused`, `the_boot_refuses` | — |
| B2.5 | A bad internal-TLS setting or an unreadable CA MUST stop the boot, and a client MUST trust only the configured CA (the system roots without one, as Go). | `test_bad_internal_tls_stops_the_boot`, `test_client_trusts_only_the_configured_ca`, `test_client_without_a_ca_uses_the_system_roots_like_go`, `test_mutual_tls_round_trip` | "a bad internal TLS value stops the start", "a client trusts only the configured CA", "without a CA a client uses the system roots, like Go", "mutual TLS end to end: the tier, the probe, and a client without a pair" | **no test** | — |

## B3. Serving and error mapping

**Purpose.** Serve each registered method through the SDK's middleware (context, span, emit,
error mapping) over the wire and through the in-process `self` channel alike.

**Guarantees.**

| ID | Guarantee | Python | TypeScript | Rust | Conformance |
|---|---|---|---|---|---|
| B3.1 | A handler's methods MUST be found by the exact RPC name (`CreateNote`, not `create_note`/`createNote`). | `test_snake_case_is_not_the_rpc_name` | "camelCase is not the RPC name" | — (the generated tonic trait names them) | — |
| B3.2 | A unary or client-streaming method MUST be asynchronous and a server-streaming one an async generator: Python refuses anything else at registration; TypeScript refuses a function of the wrong declared kind at registration and checks what a plain function (a decorator's or proxy's wrapper) returns on each call — a promise, or an async iterable — answering INTERNAL otherwise. | `test_sync_method_is_refused`, `test_server_streaming_is_served_and_cannot_emit` | "a method that is not async answers INTERNAL on its call; an async generator is refused", "a wrapped method is served; one that returns no promise or stream answers INTERNAL" | — (the trait's signatures) | — |
| B3.3 | An unexpected exception or panic MUST answer INTERNAL `"<method>: internal error"`, its text reported and never sent to the caller. | `test_an_unexpected_exception_is_internal_and_leaks_nothing` | "an unexpected error is INTERNAL and leaks nothing", "a handler that throws something unprintable still answers INTERNAL, traced or not" | `the_tier_over_the_wire` (the `panic` cases) | — |
| B3.4 | A `for_user` refusal MUST leave prefixed with the method (Go's operator prefix), its `w17.ErrorDetail` intact. | `test_error_detail_and_operator_prefix` | "an error detail and the operator's prefix" | `for_user_carries_the_detail_and_takes_the_method_prefix`, `the_tier_over_the_wire` | `TestErrorDetail` |
| B3.5 | A handler's own `UNAVAILABLE` MUST go out after response headers carrying `w17-answered`, so a w17 client does not run the handler again. | `test_a_handlers_unavailable_is_answered_and_not_replayed` | "a handler's UNAVAILABLE is answered after headers and not replayed" | `the_tier_over_the_wire` | — |
| B3.6 | Every method shape MUST be served, and an emit or invalidate on a streaming method MUST stop the registration. | `test_server_streaming_is_served_and_cannot_emit` | "every kind is served, and none can emit" | **no test** (`Registry` refuses it; `the_tier_over_the_wire` serves client and server streams) | — |
| B3.7 | A served method MUST refuse a received message past 4 MiB with RESOURCE_EXHAUSTED before buffering it (grpc-go's limit): Python reads the length prefix through `_grpclib.bound_received`; TypeScript has grpc-js's default receive limit; Rust's emitting methods read the request through a 4 MiB bound (the generated server bounds the rest). | `test_a_request_message_past_4_mib_is_refused_before_it_is_buffered` | **no test** (grpc-js's default) | `an_emitting_methods_request_past_the_message_limit_is_refused` | — |
| B3.8 | An emitting method's request MUST be read within the call's deadline, and an OK call whose request or response frame cannot be read (a compressed or truncated frame) MUST report that its event was not published, never skip it in silence. | — (the request is decoded by grpclib) | — (the request is decoded by grpc-js) | `an_emitting_methods_request_body_is_bound_by_the_deadline`, `event_frames_name_what_cannot_be_read` | — |

**Deliberate limits.**
- Python's client side (a response from storage or the companion) has no receive limit:
  grpclib offers no seam there, and the peers are tiers of the same deployment.
- No SDK compresses a message or accepts a compressed one.

## B4. Context: deadline and cancellation

**Purpose.** One immutable request context carrying the incoming metadata, the deadline, the
cancellation and the transaction, which every call a handler makes is built from.

**Guarantees.**

| ID | Guarantee | Python | TypeScript | Rust | Conformance |
|---|---|---|---|---|---|
| B4.1 | The caller's deadline MUST reach storage and end the call `DEADLINE_EXCEEDED` when it passes. | `test_deadline_propagates_and_expires` | "the deadline propagates and expires", "a passed deadline reads as the deadline in the handler, not as a cancel" | `the_tier_over_the_wire` | `TestDeadline` |
| B4.2 | `err()` MUST keep the first cause, as Go's `ctx.Err()` does: a cancel before the deadline stays "canceled" after the deadline passes, and a context cancelled after its deadline passed (the server ending a call that ran out of time) reads "deadline exceeded". | `test_a_cancel_before_the_deadline_stays_a_cancel_after_it_passes` | "a cancel before the deadline stays a cancel after it passes, as Go keeps the first", "a parent's deadline reaches a child cancelled with it" | `a_cancel_before_the_deadline_stays_a_cancel_after_it_passes` | — |
| B4.3 | `without_cancel` MUST keep the metadata and drop the deadline and the cancellation; a child's cancel MUST NOT reach its parent. | `test_rollback_is_detached_from_a_cancelled_caller` | "deadline, cancel and withoutCancel" | `deadlines_cancellation_and_without_cancel` | — |

## B5. Distributed transactions

**Purpose.** `distx.run` / `begin` against the storage tier's coordinator, with Go's
semantics: one root, joined nested runs, a detached rollback, the outcome-unknown probe.

**Shares.** The routing headers `w17-tx-id` and `w17-conn-id` (the storage proxy's, data.md
D5.15–D5.16) on every storage call made with the transaction's context.

**Guarantees.**

| ID | Guarantee | Python | TypeScript | Rust | Conformance |
|---|---|---|---|---|---|
| B5.1 | `run` MUST commit when the function returns and roll back when it fails, re-raising its error, which names a rollback that failed too. | `test_run_commits_and_returns_the_result`, `test_run_rolls_back_and_reraises`, `test_run_names_a_failed_rollback_on_the_functions_error` | "commits and returns the result", "rolls back and rethrows", "names a failed rollback on the function's error" | `the_tier_over_the_wire` | `TestRollbackDropsWritesEventsAndOnCommit` |
| B5.2 | The rollback MUST be detached from the caller's cancellation and deadline: a caller that hangs up or runs out of time mid-transaction leaves no open transaction (Rust: a dropped handler future rolls back through its drop guard, data.md D3.13). | `test_rollback_is_detached_from_a_cancelled_caller` | "the rollback is detached from a cancelled caller" | `the_tier_over_the_wire` (the `hang` case) | `TestACallerDeadlineInsideATransactionRollsItBack` |
| B5.3 | A caller whose context is done when the function returns MUST get no commit: the transaction is rolled back and the refusal names a rollback that failed too ("rollback also failed: …"), as Go's `Run`. | `test_a_done_context_refuses_to_commit`, `test_a_done_context_names_a_failed_rollback` | "a done context refuses to commit", "a done context names a failed rollback" | `the_tier_over_the_wire` (the `done` case) | — |
| B5.4 | A nested run MUST join the open transaction (one begin, one commit); a joined rollback MUST make the root roll back and answer rollback-only; a begin on another connection or at another explicit isolation MUST be refused. | `test_nested_run_joins_one_transaction`, `test_joined_rollback_makes_the_root_rollback_only`, `test_nested_mismatch`, `test_nested_same_isolation_or_unspecified_joins` | "a nested run joins one transaction", "a joined rollback makes the root rollback-only", "the same isolation, or none, joins" | `the_tier_over_the_wire` | `TestNestedTransactionHoldsEventsUntilCommit`, `TestRollbackOnly`, `TestNestedMismatch` |
| B5.5 | The routing headers MUST be set, never appended: the Begin response's `w17-conn-id` is echoed, and a stale one is removed when there is none. | `test_conn_id_is_echoed_and_headers_are_set_not_appended`, `test_no_conn_id_removes_a_stale_one` | "echoes the conn id and sets the headers, never appends", "no conn id removes a stale one" | `the_tier_over_the_wire` | `TestHeaderForwarding` |
| B5.6 | A second commit or rollback on a finished root MUST be refused without an RPC; a failed attempt MUST NOT use the handle up. | `test_handle_finished`, `test_a_failed_attempt_does_not_use_the_handle_up` | "is finished after one commit", "a failed attempt does not use it up" | **no test** | `TestHandleFinished` |
| B5.7 | A commit that failed in transport MUST be probed with a detached rollback: rolled back, it is an ordinary failure; gone, the outcome is unknown, and none of the parked work runs. | `test_commit_lost_in_transport_then_rolled_back_is_an_ordinary_failure`, `test_commit_outcome_unknown`, `test_cancel_mid_commit_with_unknown_outcome_uses_the_handle_up` | "a commit lost in transport, then rolled back, is an ordinary failure", "a commit of unknown outcome runs none of the parked work", "a caller cancelled mid-commit with an unknown outcome uses the handle up" | `the_tier_over_the_wire` (the `COMMIT_UNAVAILABLE` cases; data.md D3.14) | `TestCommitOutcomeUnknown` |
| B5.8 | A bidirectional-streaming method MUST NOT open a transaction: `begin` and `run` answer FAILED_PRECONDITION before any coordinator call, as Go does. | `test_a_bidi_method_cannot_open_a_transaction` | "a bidi method cannot open a transaction" | **no test** (`distx::begin` checks `Ctx::is_bidi`) | — |

## B6. Work after commit

**Purpose.** `on_commit` / `onCommit`: a side effect that must not outrun the write it
describes; the event middleware parks its publish the same way.

**Guarantees.**

| ID | Guarantee | Python | TypeScript | Rust | Conformance |
|---|---|---|---|---|---|
| B6.1 | Inside a transaction the work MUST run in order after the root commits and never after a rollback; inside a joined run it waits for the OUTER commit. | `test_on_commit_order_and_discard`, `test_on_commit_in_a_joined_run_waits_for_the_outer_commit` | "runs in order after the commit, never after a rollback", "in a joined run waits for the outer commit" | `parked_work_runs_after_commit_and_never_after_rollback`, `the_tier_over_the_wire` | `TestOnCommitRunsAfterCommit`, `TestRollbackDropsWritesEventsAndOnCommit` |
| B6.2 | Outside a transaction, or after its commit, the work MUST run before `on_commit` returns and its failure MUST reach the caller (Rust: `Err` with the error or the panic text), as Go's `OnCommit` runs it on the calling goroutine. | `test_on_commit_outside_a_transaction_runs_now`, `test_after_the_transaction_ended` | "outside a transaction runs now", "after the transaction ended" | `on_commit_outside_a_transaction_runs_now_and_returns_its_failure` | — |
| B6.3 | A failure (or panic) of parked work MUST be reported per piece and never fail the committed call; the pieces after it still run. | `test_on_commit_failure_after_commit_does_not_fail_the_commit`, `test_one_hook_cannot_stop_the_rest`, `test_a_hooks_own_cancellation_is_still_caught_per_hook` | "a failure after the commit does not fail the commit", "one hook cannot stop the rest; each failure is reported", "each hook's failure is caught on its own" | `the_tier_over_the_wire` (the `panic-hook` case; data.md D6.5) | — |
| B6.4 | Parked work MUST NOT be cut off by the caller's cancellation, and the drain MUST wait for it (B10). | `test_a_cancelled_caller_does_not_cut_off_the_parked_work`, `test_drain_waits_for_work_that_outlived_its_request` | "a cancelled caller does not cut off the parked work", "the drain waits for work that outlived its request" | `the_drain_sends_goaway_and_lets_the_call_in_flight_finish` | — |

**Deliberate limits.**
- At-most-once, as in Go (data.md D6): a crash between the commit and the work loses it.

## B7. Events

**Purpose.** Build Go's envelope for an emitting method from its request and response, and
publish it after success (parked until commit inside a transaction) to the channel's bus.

**Guarantees.**

| ID | Guarantee | Python | TypeScript | Rust | Conformance |
|---|---|---|---|---|---|
| B7.1 | The envelope MUST be Go's shape (field numbers, the payload bindings, `metadata` field 50001) and readable by Go's subscriber. | `test_envelope_is_the_go_shape`, `test_payload_from_an_unset_submessage_reads_zero` | "is Go's shape", "is written in field-number order, as Go marshals it", "a payload read through an unset submessage reads zero" | `the_envelope_is_built_from_request_and_response`, `a_binding_reads_nested_oneof_repeated_map_optional_and_enum_fields` | `TestEventAfterSuccessCarriesTheRequestMetadata` |
| B7.2 | The envelope's metadata MUST carry the request's application keys and never a credential (`authorization`, cookies, API keys, secret-named keys) or a transaction key (`w17-tx-id`, `w17-conn-id`); a gateway-owned key keeps its first value, any other its last. | `test_credentials_are_stripped`, `test_tx_keys_never_ride_an_envelope`, `test_metadata_first_for_gateway_owned_last_for_others` | "transaction keys never ride an envelope", "gateway-owned keys keep their first value, the others their last" | `credential_keys`, `context_data_as_go_flattens_it` | `TestEventAfterSuccessCarriesTheRequestMetadata` |
| B7.3 | An event MUST leave only after success, through `self` too; a direct call on the handler object skips it (Go's rule); a rolled-back transaction drops it; nested emits wait for the root commit. | `test_emit_after_success_and_through_self`, `test_direct_call_skips_the_emit_self_does_not`, `test_rolled_back_transaction_drops_its_events`, `test_nested_emits_wait_for_the_root_commit` | "emits after success, through self too", "a direct call skips the emit; self does not", "a rolled-back transaction drops its events", "nested emits wait for the root commit" | `the_tier_over_the_wire` | `TestNestedTransactionHoldsEventsUntilCommit`, `TestRollbackDropsWritesEventsAndOnCommit` |
| B7.4 | The NATS and Redis publishers MUST write what Go's write (stream, subject, entry fields, the domain stamp), each channel only to its declared bus, and a publish MUST never fail the call (bounded by the publish timeout). | `test_nats_publisher_stream_and_subject_are_gos`, `test_redis_publisher_writes_what_go_writes`, `test_router_sends_each_channel_to_its_declared_bus_only`, `test_fire_never_raises`, `test_a_hanging_broker_is_bounded_by_the_publish_timeout` | "the stream and the subject are Go's", "writes what Go writes", "each channel goes to its declared bus only", "fire never throws", "a hanging broker is bounded by the publish timeout" | `nats_publish_carries_the_domain_header`, `redis_entry_carries_the_domain_field`, `the_router_sends_each_channel_to_its_bus` | `TestEventAfterSuccessCarriesTheRequestMetadata` |
| B7.5 | Invalidate events MUST be bare and carry the request's labels. | `test_invalidate_events_are_bare_and_labelled` | "invalidate events are bare and labelled" | `invalidates_carry_the_labels` | — |

**Deliberate limits.**
- The envelope's `metadata` is the incoming request's metadata as each gRPC library hands it to
  the server, so its transport keys differ per language: a Go envelope carries `:authority`,
  `content-type` and `user-agent`; a Python one none of them (grpclib leaves them out of
  `stream.metadata`); TypeScript and Rust ones `user-agent` only (Rust also drops `te`,
  `content-type`, `grpc-*` and `accept-encoding`). The application keys — the principal, the
  labels, `x-w17-*`, the author's own — are the same in all four. A subscriber MUST NOT read a
  transport key from an envelope.

## B8. Principal relay and page position

**Purpose.** Relay the gateway-verified principal to every outgoing call, exactly Go's
`isRelayedKey` set, and relay a business method's page position from one forwarded storage
call.

**Guarantees.**

| ID | Guarantee | Python | TypeScript | Rust | Conformance |
|---|---|---|---|---|---|
| B8.1 | Exactly the gateway's stamps MUST be relayed (`x-w17-user`, `-data-scope`, `-device`, `-dpop-jkt`, `-audience`, `-membership-kinds`, `-org-from`, `-host`, `x-w17-scope-*`, `w17-label-*`), the whole set replacing the outgoing one with every value kept; nothing without an incoming principal. | `test_forward_relays_exactly_the_gateway_stamps`, `test_forward_replaces_the_whole_relayed_set_and_keeps_other_outgoing_keys`, `test_forward_keeps_every_value_of_a_multivalued_key`, `test_forward_without_incoming_principal_returns_ctx_unchanged` | "relays exactly the gateway stamps", "replaces the whole relayed set and keeps other outgoing keys", "keeps every value of a multivalued key", "without an incoming principal returns ctx unchanged" | `the_relay_replaces_the_outgoing_principal` | `TestHeaderForwarding` |
| B8.2 | The principal readers MUST fail closed (no principal, a malformed envelope, a device id that is not exactly one value); `act_as` MUST replace the principal on both sides and refuse malformed input. | `test_device_id_needs_exactly_one_value`, `test_act_as_replaces_the_principal_on_both_sides`, `test_act_as_refuses_malformed_input` | "deviceId needs exactly one value", "replaces the principal on both sides", "refuses malformed input (%j)" | `principal_readers_fail_closed`, `act_as_stamps_the_outgoing_side_only` | `TestPrincipalReadersAndConfig` |
| B8.3 | `paging.forward` MUST copy the page request onto the one forwarded call as a set, and only that call's page position MUST answer as the method's own. | `test_paging_forward_copies_incoming_onto_outgoing_only`, `test_paging_forward_replaces_stale_outgoing_as_a_set`, `test_paging_forward_reaches_the_one_call`, `test_paging_forward_relays_the_page_position` | "copies incoming onto outgoing only", "replaces stale outgoing keys as a set", "paging.forward reaches the one call", "paging.forward relays the page position" | `paging_is_forwarded_as_one_set` | `TestPagingThroughABusinessMethod` |

## B9. Clients of storage and of the tier itself

**Purpose.** Call storage (and the tier's own services through `self`) with the context's
deadline, principal and transaction, and retry only what never reached a server.

**Guarantees.**

| ID | Guarantee | Python | TypeScript | Rust | Conformance |
|---|---|---|---|---|---|
| B9.1 | A trailers-only `UNAVAILABLE` MUST be retried (three attempts); a status after response headers MUST NOT be (gRPC A6). | `test_client_retries_unavailable_three_times`, `test_a_handlers_unavailable_is_answered_and_not_replayed` | "the client retries a trailers-only UNAVAILABLE, three attempts", "a handler's UNAVAILABLE is answered after headers and not replayed" | `the_tier_over_the_wire` (data.md D3.14) | — |
| B9.2 | A call on a done context MUST be refused before it is sent; a request without a context MUST be refused (Rust: FAILED_PRECONDITION). | `test_client_refuses_a_done_context` | "the client refuses a done context", "cancelling the context cancels the call" | `the_tier_over_the_wire` | — |
| B9.3 | Unreachable storage MUST answer `UNAVAILABLE`; `self` MUST hand the callee the caller's context unchanged. | `test_unreachable_storage_is_unavailable`, `test_self_hands_the_callee_the_callers_context_unchanged` | "unreachable storage is UNAVAILABLE", "hands the callee the caller's context unchanged" | `the_tier_over_the_wire` | — |
| B9.4 | A bidirectional-streaming call MUST send its request before it reads a reply (Python: grpclib refuses a read before the request). | `test_a_bidi_method_cannot_open_a_transaction` (drives the SDK's bidi client) | "every kind is served, and none can emit" | **no test** | — |

## B10. Drain

**Purpose.** On SIGTERM: refuse new calls `UNAVAILABLE`, let calls in flight and the work after
commit they started finish within one budget (the manifest's `shutdown_timeout_seconds`), then
close the channels and the bus.

**Guarantees.**

| ID | Guarantee | Python | TypeScript | Rust | Conformance |
|---|---|---|---|---|---|
| B10.1 | A draining server MUST refuse a new call `UNAVAILABLE` and let the calls in flight finish. | `test_draining_refuses_new_calls`, `test_the_drain_waits_for_in_flight_calls` | "draining refuses new calls", "the drain waits for in-flight calls" | `the_drain_sends_goaway_and_lets_the_call_in_flight_finish` | — |
| B10.2 | The drain MUST wait for work after commit that outlived its request, including work that starts while it waits, before the bus closes, and the whole drain MUST stay within one budget. | `test_drain_waits_for_work_that_outlived_its_request`, `test_drain_waits_for_work_that_starts_while_it_waits`, `test_shutdown_drains_work_started_by_closing_the_server_before_closing_the_bus`, `test_the_whole_drain_stays_within_one_budget` | "the drain waits for work that outlived its request", "the drain waits for work that starts while it waits", "the drain waits for work the server's close started, before the bus closes", "the whole drain stays within one budget, however much of it hangs" | **no test** | — |

## B11. Business companion clients

**Purpose.** Reach the egress clients and file stores the business companion serves, without
the storage transaction or the page position.

**Guarantees.**

| ID | Guarantee | Python | TypeScript | Rust | Conformance |
|---|---|---|---|---|---|
| B11.1 | A companion call MUST relay the principal and MUST NOT carry `w17-tx-id`, `w17-conn-id` or `x-w17-paging-*`; it MUST NOT be retried. | `test_companion_pairs_strip_the_transaction_and_paging_headers`, `test_egress_call_relays_the_principal_and_nothing_transactional`, `test_the_companion_connection_does_not_retry` | "the companion metadata loses the transaction and paging headers", "an egress call relays the principal and nothing transactional", "the companion connection does not retry" | **no test** | `TestCompanionEgressCall` |
| B11.2 | An egress error MUST pass through with its `ErrorInfo`; the caller's deadline MUST reach the upstream. | `test_egress_errors_pass_through_with_their_error_info` | "egress errors pass through with their ErrorInfo" | **no test** | `TestCompanionEgressErrorPassesThrough`, `TestCompanionEgressDeadline` |
| B11.3 | A file put MUST re-chunk its body and send the end marker only after the whole body; a body that fails or stalls MUST store nothing. | `test_file_round_trip`, `test_put_bodies_are_rechunked`, `test_put_refusals` | "a file round trip", "put bodies are re-chunked", "put refusals" | `a_put_is_header_64k_chunks_and_the_end`, `a_failed_body_never_sends_the_end` | `TestCompanionFileRoundTrip`, `TestCompanionFileRefusals`, `TestCompanionFailedBodyStoresNothing`, `TestCompanionStalledBodyStoresNothing` |

**Deliberate limits.**
- The conformance suite proves the drain (B10) and a streaming method's refusals (B3.6, B5.8)
  per language only, through the unit tests above; it has no SIGTERM case and no streaming
  method in its service.
