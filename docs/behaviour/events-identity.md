# Events, identity and language

How events are emitted, carried and delivered, how the verified caller travels
between tiers, how permissions are checked, and how messages are translated and
formatted. Includes the eventbus-backed server-sent-events hub, because it is the
eventbus's live fan-out consumer. Packages: `go/lib/eventbus`, `go/lib/restgw`
(event hub only), `go/lib/principal`, `go/lib/acllock`, `go/lib/i18n`,
`go/lib/typere`, `go/lib/randstr`.

## Features

| ID | Feature | Code |
|---|---|---|
| I1 | Dispatcher contract (emit side) | `go/lib/eventbus/dispatcher.go` — `Dispatcher.Dispatch`; `go/lib/eventbus/names.go` — `ValidateChannel`, `ValidateTopic` |
| I2 | Subscriber contract | `go/lib/eventbus/subscriber.go` — `SubscriberFactory`, `Subscribe`, `Drain`, `MatchGlob`, `recoverHandler` |
| I3 | Delivery modes (queue and fan-out) | `go/lib/eventbus/subscriber.go` — `DeliveryMode`, `DeliveryQueue`, `DeliveryFanOut` |
| I4 | In-memory transport | `go/lib/eventbus/memory.go` — `MemoryBus`, `MemoryBusOptions` |
| I5 | NATS JetStream transport | `go/lib/eventbus/nats.go` — `NatsBus`, `NatsBusOptions`; `go/lib/eventbus/retry.go` — `ChannelRetry`; `go/lib/eventbus/retention.go` — `StreamRetention` |
| I6 | Redis Streams transport | `go/lib/eventbus/redis.go` — `RedisBus`, `RedisBusOptions` |
| I7 | Bus observability | `go/lib/eventbus/observability.go` — `Observer`, `ObserverWithLatency`, `DefaultObserver`, `NopObserver` |
| I8 | Event context: metadata, labels, invalidate | `go/lib/eventbus/eventctx.go` — `MetadataFromContext`, `LabelsFromContext`; `go/lib/eventbus/w17invalidate.go` — `EmitInvalidate` |
| I9 | Server-sent-events fan-out hub | `go/lib/restgw/w17events_eventbus.go` — `EventbusEventSource`, `entitled`; `go/lib/restgw/w17events.go` — `HandleW17Events` |
| I10 | Principal, read side | `go/lib/principal/principal.go` — `UserID`, `Envelope`, `Scope`, `Label` |
| I11 | Principal, gateway side | `go/lib/principal/gateway.go` — `SanitizeGatewayMD`, `WithGatewayMD`; `go/lib/principal/reserved.go` — `IsGatewayOwnedKey` |
| I12 | Principal relay | `go/lib/principal/principal.go` — `ForwardToOutgoing` |
| I13 | Permission lock and checks | `go/lib/acllock/allocator.go` — `Allocate`; `go/lib/acllock/bits.go` — `HasPermission`, `HasAnyPermission`, `HasAllPermissions`, `HasAllPermissionsStrict`, `GrantAll`, `IDByString` |
| I14 | Message catalogs and translation | `go/lib/i18n/i18n.go` — `Register`, `RegisterCatalog`, `RegisterFS`, `SetDefaultLanguage`, `T`, `LocaleFromContext`; `go/lib/i18n/pocheck.go` — `ValidatePO` |
| I15 | Number and date formatting | `go/lib/i18n/formats.go`, `formatnum.go`, `formatdate.go`, `formatlocale.go` — `FormatLocaleFromContext` |
| I16 | Catalog tooling | `go/lib/i18n/extract.go`, `harvest.go` — `MarshalPO`, `MergePO`, `HarvestEnumLabels` |
| I17 | Shared value validators | `go/lib/typere/typere.go` — `UUID`, `Slug`, `URL`, `MAC`, `ValidEmail`, `FitsDecimal` |
| I18 | Random tokens | `go/lib/randstr/randstr.go` — `Token` |

---

## I1. Dispatcher contract (emit side)

**Code.** `go/lib/eventbus/dispatcher.go` — `Dispatcher.Dispatch(ctx, channel, topic, envelope)`; `go/lib/eventbus/names.go` — `ValidateChannel`, `ValidateTopic`, `ErrInvalidChannel`, `ErrInvalidTopic`

**Purpose.** The one call every generated emit helper makes. Emit is
fire-and-forget: the RPC has already answered, and generated helpers discard the
returned error.

**Configuration.** None on the interface; each transport has its own options (I4–I6).
One name rule holds on every transport, the in-memory one included:

| Name | Rule | Invalid values |
|---|---|---|
| Channel | 1–200 characters of `A–Z a–z 0–9 _ -` (`ValidateChannel`) | refused with `ErrInvalidChannel` at `Subscriber`, at `Dispatch`, and for the keys of `ChannelRetries` at construction |
| Topic | `.`-separated non-empty segments without whitespace, control characters, `*` or `>` (`ValidateTopic`) | refused with `ErrInvalidTopic` at `Dispatch`; an empty topic is invalid |

**Shares.** Every transport; the `Observer` (I7), which is the only record of a
failure; the commit hook in `go/service/tx/txregistry/txregistry.go`, which runs
the emit after the database commit.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I1.1 | A marshal or publish failure MUST be returned to the caller and reported to the configured Observer. | `TestRedisBus_DispatchError` |
| I1.2 | The publish MUST be detached from the request's cancellation (`context.WithoutCancel`), so an emit after the RPC answered still goes out; on the broker transports it is bounded by `PublishTimeout` instead (I5.15, I6.10). | `TestNatsBus_DispatchDetachesCanceledCtx_Embedded`, `TestRedisBus_DispatchDetachesCanceledCtx_Embedded`, `TestMemoryBus_ContextDetached` |
| I1.3 | The default Observer MUST log every emit failure, delivery failure and drop with channel and topic, and report it to the error reporter. Successes are silent. | `TestDefaultObserver_EmitFailureLeavesATrace`, `TestMemoryBus_ObserverNilDefaults` |
| I1.4 | An event MUST NOT be lost without a trace on any transport, in any bus state. | `TestMemoryBus_DispatchAfterDrainIsRefused`, `TestDispatchAfterClose_IsAnErrorOnEveryTransport`, `TestNatsBus_ClosedForGoodIsSaidAndFailsDispatchAtOnce` |
| I1.5 | An empty topic MUST be refused on every transport with `ErrInvalidTopic` and reported through `OnEmitFailure`. | `TestDispatch_EmptyTopicRefusedOnEveryTransport` |
| I1.6 | A channel or topic that breaks the name rule MUST be refused identically on every transport, before any broker call, at both `Subscriber` and `Dispatch`; the names generated surfaces use MUST stay valid. | `TestChannelNameRefusedOnEveryTransport` |

**Deliberate limits.**
- No transactional outbox. The event is published after the commit by the
  committing goroutine; a crash or broker outage between commit and publish loses
  it (producer side is at-most-once). The Observer line is the only record.
- Publishing runs on the committing goroutine, so a slow broker adds to commit latency, at most
  `PublishTimeout` per event on the broker transports.

---

## I2. Subscriber contract

**Code.** `go/lib/eventbus/subscriber.go` — `SubscriberFactory.Subscriber`, `Subscribe`, `Drain`, `MatchGlob`, `recoverHandler`, `recoverLoopTick`, `ErrBusClosed`, `ErrSubscriberDraining`

**Purpose.** `Subscriber(ctx, channel)` hands out one subscriber per channel;
`Subscribe(ctx, topicFilter, handler)` starts delivery; `Drain(ctx)` stops it and
waits for in-flight handlers. The generated dispatcher subscribes with `**` and
re-applies each subscription's glob itself.

**Configuration.** Glob syntax for `topicFilter`: `*` is one segment, `**` is zero
or more segments, a literal matches exactly; an empty filter matches only an
empty topic.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I2.1 | `MatchGlob` MUST implement the glob syntax above. | `TestMatchGlob` |
| I2.2 | Every transport MUST honour `topicFilter` identically, so a hand-written subscriber receives only matching topics. | `TestMemoryBus_TopicFilterMatches`, `TestTopicFilter_Memory`, `TestTopicFilter_Nats_Embedded`, `TestTopicFilter_Redis_Embedded` |
| I2.3 | `Subscriber()` after `Close` MUST return `ErrBusClosed`, checked atomically with registration. | `TestNatsBus_SubscriberAfterCloseRefused_Embedded`, `TestRedisBus_SubscriberAfterCloseRefused_Embedded` |
| I2.4 | `Subscribe()` after `Drain` MUST return `ErrSubscriberDraining`, checked atomically with registration. | `TestNatsSubscriber_SubscribeAfterDrainRefused_Embedded`, `TestRedisSubscriber_SubscribeAfterDrainRefused_Embedded`, `TestMemoryBus_SubscribeAfterDrainIsRefused` |
| I2.5 | A handler panic MUST become a delivery error (retry or dead-letter on brokers) and be reported; it MUST NOT crash the process. | `TestRecoverHandler_PanicBecomesError` |
| I2.6 | A panic in loop plumbing MUST be recovered. | `TestRecoverLoopTick_RecoversPanic` |

---

## I3. Delivery modes (queue and fan-out)

**Code.** `go/lib/eventbus/subscriber.go` — `DeliveryMode`, `DeliveryQueue`, `DeliveryFanOut`

**Purpose.** Decides how replicas of one surface share a channel.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `NatsBusOptions.Delivery`, `RedisBusOptions.Delivery` | `DeliveryQueue` (zero value): competing consumers, one message to one replica, starts from retained backlog | — |
| `DeliveryFanOut` | — | one consumer identity per process; every replica gets every message; starts at new messages only |
| `InstanceID` | empty → `hostname-pid-random` | sanitised to `[A-Za-z0-9_-]`; ignored in queue mode |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I3.1 | Queue mode: a surface deployed after its producer MUST process the retained backlog. | `TestNatsBus_StartPosition_Embedded`, `TestRedisBus_StartPosition_Embedded` |
| I3.2 | Fan-out mode: every replica MUST receive every message; history MUST NOT be replayed into a new process. | `TestNatsBus_DeliveryMode_ReplicaFanOut_Embedded`, `TestRedisBus_DeliveryMode_ReplicaFanOut_Embedded`, `TestNatsBus_StartPosition_Embedded`, `TestRedisBus_StartPosition_Embedded` |
| I3.3 | A fan-out NATS consumer MUST be ephemeral and reaped by the server 5 minutes after it stops pulling; a fan-out Redis group MUST be destroyed on `Close`. | `TestNatsBus_FanOutConsumerIsEphemeralAndReaped_Embedded`, `TestRedisBus_FanOutGroupDestroyedOnClose_Embedded` |
| I3.4 | After transient link loss shorter than the client's reconnect budget, a fan-out subscriber MUST receive the backlog since disconnect. | `TestFanOut_BacklogSinceDisconnectArrivesAfterReconnect` |

**Deliberate limits.**
- A restarted process gets a new fan-out identity starting at "new"; events emitted while it was down are not seen.
- Fan-out Redis groups of a process killed without `Close` remain (stream metadata only).
- An event hub (I9) needs fan-out; queue mode would split its audience between replicas. The generated main selects fan-out.

---

## I4. In-memory transport

**Code.** `go/lib/eventbus/memory.go` — `MemoryBus`, `MemoryBusOptions`

**Purpose.** In-process loopback for memory-transport channels; implements both
`Dispatcher` and `SubscriberFactory`. Dispatch marshals, snapshots matching
handlers and runs one goroutine per matching handler per event. Each handler runs
with the context its `Subscribe` call was given, detached from its cancellation, as
a broker delivery would; the emitter's context reaches no handler, so a subscriber
sees only what the envelope carries.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `BufferSizes map[channel]int` | absent → unbounded | `<= 0` → unbounded. Bounded: a per-channel semaphore limits concurrent handler invocations |
| `BlockTimeout` | 100 ms | `<= 0` → 100 ms. Dispatch blocks up to this per matching handler, then drops (`Drops()`, `OnDrop(buffer_full)`) |
| `Observer` | `DefaultObserver` | nil → default |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I4.1 | Round trip and topic filtering MUST work; channels MUST be isolated; several subscribers per channel MUST each receive the event. | `TestMemoryBus_RoundTrip`, `TestMemoryBus_TopicFilterMatches`, `TestMemoryBus_ChannelIsolation`, `TestMemoryBus_MultipleSubscribersPerChannel`, `TestMemoryBus_EmptyChannel` |
| I4.2 | Delivery is at-most-once with no retry; a handler error MUST be reported through `OnDeliverFailure`. | observer tests in `go/lib/eventbus/observability_test.go` |
| I4.3 | Handlers MUST survive request cancellation. | `TestMemoryBus_ContextDetached` |
| I4.4 | Drain MUST wait for in-flight handlers, bounded by its context. | `TestMemoryBus_DrainWaitsForInflight`, `TestMemoryBus_DrainTimeout` |
| I4.5 | Concurrent subscribe and dispatch MUST be race-free. | `TestMemoryBus_ConcurrentSubscribeAndDispatch` |
| I4.6 | Bounded channels MUST accept under capacity, drop after `BlockTimeout`, and be isolated per channel; unbounded is the default. | `TestMemoryBus_BufferUnboundedDefaults`, `TestMemoryBus_BufferDropsAfterTimeout`, `TestMemoryBus_BufferAcceptsUnderCapacity`, `TestMemoryBus_BufferPerChannelIsolation` |
| I4.7 | A Dispatch after Drain MUST return `ErrBusDraining` and fire `OnEmitFailure`; `OnEmitSuccess` fires only once the fan-out is admitted. | `TestMemoryBus_DispatchAfterDrainIsRefused` |
| I4.8 | `Subscribe` after Drain MUST return `ErrSubscriberDraining`, checked under the same lock as the registration. | `TestMemoryBus_SubscribeAfterDrainIsRefused` |
| I4.9 | Drain of any subscriber MUST apply to the whole bus: after it, `Dispatch` on every channel, including channels the drained subscriber never served, is refused with `ErrBusDraining`. | `TestMemoryBus_DispatchAfterDrainIsRefused` |
| I4.10 | A handler MUST run with its subscription's context and MUST NOT see the emitter's context values or its incoming or outgoing metadata (transaction id, principal, scopes). | `TestMemoryBus_HandlerContextIsNotTheEmitters` |

**Deliberate limits.**
- No ordering between events: each delivery is an independent goroutine.
- Unbounded mode (the default) has no backpressure: a slow handler under sustained emit grows goroutines.
- Bounded mode blocks the emitting (committing) goroutine for up to `BlockTimeout` × matching handlers.
- Drain is bus-wide, not per channel: draining any subscriber of a memory bus ends delivery on all its channels.

---

## I5. NATS JetStream transport

**Code.** `go/lib/eventbus/nats.go` — `NatsBus`, `NewNatsBus`, `NatsBusOptions`; `go/lib/eventbus/retry.go` — `ChannelRetry`

**Purpose.** Broker transport with at-least-once delivery and a dead-letter
stream. Channel → stream `<channel>` with subjects `<channel>.>`; topic → subject
`<channel>.<topic>`; consumer name `<DurablePrefix>-<channel>[-<filter>][-<InstanceID>]`.
In the filter part letters and digits are kept, `.` becomes `_`, and every other
byte is escaped behind `-` (`*` → `-s`, `_` → `-u`, `-` → `-d`, any other byte →
`-xHH`), so distinct filters always get distinct consumers.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `DSN` | required | must start with `nats://` or `tls://` |
| `DurablePrefix` | required | prefixes every consumer name; letters, digits, `_` and `-` only, refused at construction otherwise |
| `Delivery`, `InstanceID` | see I3 | — |
| `DefaultMaxDeliver` | 0 → 3 | negative passed to JetStream unchanged |
| `DefaultAckWait` | 0 → 30 s | negative passed to JetStream unchanged |
| `ChannelRetries map[channel]ChannelRetry` | none | a zero field inherits the default; a key that breaks the channel rule is refused at construction |
| `PublishTimeout` | 0 → 2 s (`DefaultPublishTimeout`) | one bound over the stream check, the publish and the acknowledgement |
| `Retention StreamRetention{MaxAge, MaxMsgs}` | `MaxAge` 0 → 7 days (`DefaultStreamMaxAge`), no count limit | negative `MaxAge` → no age limit; `MaxMsgs <= 0` → no count limit. Applied to every channel stream the bus ensures, oldest discarded first |
| `DLQRetention StreamRetention` | `MaxAge` 0 → 30 days (`DefaultDLQMaxAge`), no count limit | as `Retention`, for every dead-letter stream |
| `Observer` | `DefaultObserver` | nil → default |

**Behaviour.**
- Handler success → `Ack`; error → `Nak` → redelivery until `MaxDeliver`.
- On the final attempt a failed message is dead-lettered to stream `<channel>_dlq`
  (subjects `_dlq.<channel>[.<topic>]`) after claiming its terminal outcome in a
  broker-side KV bucket (`w17_eventbus_dlq_claims`, TTL 1 h), then acked.
- A handler that never completes its last attempt is dead-lettered through the
  `MAX_DELIVERIES` advisory, consumed by a queue group so one replica acts, after
  the same claim.
- DLQ broker operations retry 3 times with 200 ms backoff.
- Each subscription's topic filter is applied with `MatchGlob` to the delivered topic; a non-matching
  message is settled without invoking the handler. Each filter has its own consumer.
- A publish that finds no stream drops the cached entry, re-creates the stream and publishes once more.
- Consume errors that can mean the consumer is gone (deleted, not found, no responders, missed
  heartbeats) start one recovery per subscription: unless the consumer was reported deleted, it is
  looked up first and a transient error changes nothing; otherwise the stream, dead-letter stream
  and claim bucket are re-ensured, the consumer re-created and a new pull loop swapped into the
  subscription under the subscriber lock. Recovery retries until it succeeds or the subscriber drains.
- `Drain` stops each pull loop by draining it, not stopping it: messages already prefetched into
  the client still reach the consume callback, which Naks them, and `Drain` returns once that
  buffer is empty (bounded by its context). They go back to the broker at once instead of waiting
  out `AckWait`.
- Broker down later: the client reconnects and logs each state change; when it
  gives up, a "closed for good" line is logged and an error reported, and every later
  `Dispatch` fails at once with `ErrConnectionClosed` through `OnEmitFailure`.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I5.1 | Options MUST be validated at construction (DSN scheme, required prefix); naming MUST follow the mapping above. | `go/lib/eventbus/nats_test.go` |
| I5.2 | Delivery MUST be at-least-once: a failed handler is redelivered up to `MaxDeliver`, with per-channel retry overrides. | `TestNatsBus_HandlerErrorRedelivers_Embedded`, `TestNatsBus_ChannelRetriesOverride_Embedded` |
| I5.3 | `OnDrop(max_deliver_exceeded)` MUST fire only after the payload is in the DLQ; a failed DLQ write MUST report `OnDeliverFailure` and Nak instead. | `TestNatsBus_DLQRouting_Embedded`, `TestNatsBus_FailedDLQWriteIsAFailureNotADrop_Embedded`, `TestNatsSubscriber_RouteToDLQ_BrokerFreeGuards` |
| I5.4 | Exactly one terminal outcome per message across replicas (DLQ or success), including for slow handlers. | `TestNatsBus_MaxDeliverAdvisory_SingleReplicaRoutes_Embedded`, `TestNatsBus_SlowSuccessNotDropped_Embedded`, `TestNatsBus_SlowFailureAtCapStillDLQs_Embedded`, `TestNatsBus_AdvisoryWhileBuffered_NoDropPlusSuccess_Embedded`, `TestNatsBus_CrossReplicaAdvisory_NoDropPlusSuccess_Embedded` |
| I5.5 | Deliveries that arrive during drain MUST be Nak'd for redelivery, not dropped. | `TestNatsBus_DeliveryDuringDrainIsHandedBack_Embedded`, `TestNatsBus_DrainDuringDelivery_NoWaitGroupRace_Embedded`, `TestNatsSubscriber_AdvisoryRespectsDraining` |
| I5.6 | `Close` MUST drain all subscribers, then close the connection; it MUST be idempotent and MUST NOT produce error-level callbacks for its own close. | `TestNatsBus_CloseAtShutdownIsNotAnError`, `TestNatsBus_CloseIdempotent_Embedded` |
| I5.7 | A broker outage MUST be logged. | `TestNatsBus_ABrokerOutageIsStillHeard` |
| I5.8 | Consume-loop errors MUST be logged. | `TestNatsConsumeFailureIsLogged` |
| I5.9 | `NewNatsBus` MUST fail when the broker is unreachable at start. | `TestNewNatsBus_UnreachableBrokerFails` |
| I5.10 | After a stream or consumer is lost under a live bus (stream deleted, broker recreated without its JetStream state), emit and consume MUST recover without a process restart. | `TestNatsBus_RecoversFromStreamLoss_Embedded`, `TestNatsBus_RecoversFromBrokerRecreated_Embedded` |
| I5.11 | After the client stops reconnecting, the bus MUST log a "closed for good" line, report an error, and fail every later `Dispatch` at once through `OnEmitFailure`. | `TestNatsBus_ClosedForGoodIsSaidAndFailsDispatchAtOnce` |
| I5.12 | Channel and topic names MUST be valid on every transport or be refused before use; `DurablePrefix` and the keys of `ChannelRetries` MUST be checked at construction. | `TestChannelNameRefusedOnEveryTransport`, `TestDispatch_EmptyTopicRefusedOnEveryTransport`, `TestBusConstruction_RefusesInvalidNames` |
| I5.13 | Distinct topic filters on one channel MUST map to distinct consumer names; a plain dotted filter keeps its name. | `TestSanitizeDurableSuffix_Injective`, `TestSanitizeDurableSuffix`, `TestTopicFilter_Nats_Embedded` |
| I5.14 | Every channel stream and dead-letter stream the bus ensures MUST carry the configured retention limits, or the defaults when none are configured. | `TestNatsBus_StreamRetention_Embedded` |
| I5.15 | During a broker outage one `Dispatch` MUST fail within `PublishTimeout` rather than hold the committing goroutine for the client's own timeout. | `TestNatsBus_PublishTimeoutBoundsOutage_Embedded` |

**Deliberate limits.**
- No ordering under retry: a Nak'd message is redelivered after later ones. First deliveries within one process arrive in stream order; across queue-mode replicas there is no ordering.
- No `InProgress` heartbeat: a handler that runs past `AckWait` can execute concurrently with its redelivery. Handlers must be idempotent.
- Retention is set by the SDK, not by the operator: every time the bus ensures a stream it
  writes the configured limits (or the defaults), replacing limits set on the broker by other
  means. Set `MaxAge: -1` for no age limit.
- Retention removes messages regardless of whether a consumer has read them; a consumer that is
  down longer than `MaxAge` loses the older backlog.

---

## I6. Redis Streams transport

**Code.** `go/lib/eventbus/redis.go` — `RedisBus`, `NewRedisBus`, `RedisBusOptions`

**Purpose.** Broker transport on Redis Streams. Channel → stream key `<channel>`;
each entry `{topic, payload}`; DLQ stream `<channel>.dlq`. A consume loop reads
batches with `XREADGROUP` (block 1 s) and `XACK`s on success; failures stay pending.
A claim loop runs every `AckWait`, re-claims pending entries idle at least
`AckWait` (up to 100 per page), and either re-runs the handler or, at
`MaxDeliver`, copies to the DLQ and acks. Each subscription's topic filter is applied with
`MatchGlob`; a non-matching entry is acknowledged without invoking the handler. The catch-all
filter (`**` or empty) reads through the channel's group; a narrower filter reads through a group
of its own (`<group>-<filter>`, with the filter escaped as in I5), so that acknowledgement never
takes an entry from a sibling subscription it matches. `NewRedisBus` PINGs the broker, bounded by
a timeout, before returning. Each append trims its stream to the retention limits: age with
`MINID ~`, count with `MAXLEN ~`.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `DSN` | required | `redis://…`, parsed by `ParseURL` |
| `GroupPrefix` | required | — |
| `Delivery`, `InstanceID` | see I3 | — |
| `ConsumerName` | hostname | must be unique per process within a prefix |
| `DefaultMaxDeliver` | 0 → 3 | — |
| `DefaultAckWait` | 0 → 30 s | — |
| `ReadBatch` | 0 → 10 | — |
| `ChannelRetries`, `Observer` | as I5 | — |
| `PublishTimeout` | 0 → 2 s (`DefaultPublishTimeout`) | bounds one `XADD` and its reply |
| `Retention`, `DLQRetention` | as I5 (7 days and 30 days, no count limit) | applied on each append to the channel stream and to `<channel>.dlq` |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I6.1 | Delivery MUST be at-least-once with up to `MaxDeliver` handler attempts, then dead-letter. | `TestRedisBus_HandlerErrorClaimRedelivers_Embedded`, `TestRedisBus_DLQRouting_Embedded`, `TestRedisBus_ChannelRetriesOverride_Embedded` |
| I6.2 | The claim loop MUST NOT reclaim or dead-letter a message whose handler is still running, including queued siblings of the same batch; the guard is per consumer identity. | `TestRedisBus_BatchSiblingIsNotReclaimedWhileQueued`, `TestRedisBus_InFlightGuardIsPerConsumerIdentity_Embedded`, `TestRedisBus_InFlightAtCapNotDLQd_Embedded` |
| I6.3 | A lost group (flush or non-persistent restart) MUST be recreated at `$`; entries between loss and recreation are skipped. | `TestRedisBus_RecoversFromLostGroup_Embedded` |
| I6.4 | Drain of one subscriber MUST leave others running; `Close` MUST be idempotent. | `TestRedisBus_DrainOneOfTwoSubscribers_Embedded`, `TestRedisBus_CloseIdempotent_Embedded` |
| I6.5 | `NewRedisBus` MUST fail when the broker is unreachable at construction, with any DSN password redacted from the error. | `TestNewRedisBus_UnreachableBrokerFails` |
| I6.6 | Consume-loop and pending-scan errors MUST be logged and reported once per outage, and the recovery logged. | `TestRedisSubscriber_LoopFailuresAreReported` |
| I6.7 | `ReadBatch` and pending pages larger than 100 entries MUST be handled without loss. | `TestRedisBus_ReadBatchAbove100LosesNothing`; pending pages: holds by construction (every claim tick re-reads the pending list from its start); no test |
| I6.8 | A narrower topic filter MUST read through its own consumer group, so an entry it skips still reaches a sibling subscription on the same channel whose filter matches it. | `TestTopicFilter_Redis_Embedded` |
| I6.9 | Each append MUST trim the stream to the configured age and count limits, or to the default age when none are configured. | `TestRedisBus_StreamRetention_Embedded`, `TestRedisBus_DLQRetention_Embedded` |
| I6.10 | One `Dispatch` MUST fail within `PublishTimeout` when the broker does not answer. | `TestRedisBus_PublishTimeoutBoundsAHangingBroker` |

**Deliberate limits.**
- Ordering is sequential within one consume loop; retries come later from the claim loop, out of order.
- A failed read backs off 200 ms.
- Trimming is approximate and happens on append: a stream can hold somewhat more than its limits,
  and an idle stream keeps entries past their age until the next append. With both an age and a
  count limit, the count trim is a best-effort second command.

---

## I7. Bus observability

**Code.** `go/lib/eventbus/observability.go` — `Observer`, `ObserverWithLatency`, `DefaultObserver`, `NopObserver`

**Purpose.** Five callbacks (emit success and failure, delivery failure, drop, and
related), plus optional `ObserverWithLatency.OnDeliverComplete`, fired on success
and failure. `DefaultObserver` logs failures and drops; `NopObserver` is the
explicit silent opt-out. Callbacks run on the hot path.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I7.1 | Callbacks MUST fire for each outcome; drops MUST fire `OnDrop`. | `TestMemoryBus_ObserverCallbacks`, `TestMemoryBus_ObserverDropFires` |
| I7.2 | The latency hook MUST fire on both success and failure, and only for observers that implement it. | `TestMemoryBus_ObserverLatencyFiresOnBothPaths`, `TestMemoryBus_PlainObserverSkipsLatencyHook` |
| I7.3 | `NopObserver` MUST be silent. | `TestNopObserver_*` |
| I7.4 | A panicking Observer callback MUST NOT crash the emitting goroutine. | `TestMemoryBus_PanickingObserverDoesNotBreakEmit`, `TestNatsBus_PanickingObserverDoesNotBreakEmit_Embedded`, `TestRedisBus_PanickingObserverDoesNotBreakEmit_Embedded` |

---

## I8. Event context: metadata, labels, invalidate

**Code.** `go/lib/eventbus/eventctx.go` — `MetadataFromContext`, `LabelsFromContext`; `go/lib/eventbus/w17invalidate.go` — `EmitInvalidate`

**Purpose.** `MetadataFromContext` captures request metadata into
`Event.Metadata`, from one side (incoming if non-empty, else outgoing). The
generated subscriber dispatch re-injects it into the handler's context; this is
how the principal and data scopes cross the event hop. It carries the
gateway-stamped principal and scopes (`x-w17-*`), labels (`w17-label-*`), locale,
trace and correlation context and custom propagated headers. It drops every key
`ReservedMetadataKey` reports: the transport-internal `w17-tx-id` and
`w17-conn-id`, and credential-bearing keys. Generated subscriber dispatch filters
the replayed map with the same predicate.

On delivery the bus itself refuses an envelope whose metadata carries
`w17-tx-id` or `w17-conn-id`: every transport reads the envelope's metadata map
(field 50001, `EnvelopeMetadataField`, the slot every generated envelope uses)
without decoding the envelope type, and fails the delivery with
`ErrTransactionKeyInEnvelope` before any handler runs. The refusal holds for a
subscriber that does not use the generated dispatch and for an envelope from a
publisher outside the SDK.

Credential rule: `authorization`, `proxy-authorization`, `cookie`, `set-cookie` and
`x-w17-credential` by exact name (also behind grpc-gateway's `grpcgateway-` prefix),
plus any key with a whole `-`/`_`/`.` segment among `auth`, `authorization`,
`apikey`, `api`+`key`, `cookie`, `credential(s)`, `jwt`, `passwd`, `password`,
`secret`, `session`, `token`. The gateway-owned namespaces (`x-w17-*`,
`w17-label-*`) are exempt from the segment rule.

Repeated keys collapse to one value, chosen as the key's reader on the subscriber
side chooses: the first value for the gateway-owned namespaces, the last value for
every other key (the metadata-binding convention the locale readers follow). `LabelsFromContext`
extracts `w17-label-<name>` entries into `Event.Labels`, the event's audience.
`EmitInvalidate` dispatches an `InvalidateEvent` on the reserved topic
`w17.invalidate`, labelled from the context like any other event.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I8.1 | The gateway tier (outgoing only) and the server tier (incoming) MUST produce the same metadata and labels. | `TestEventContextReaders_*` |
| I8.2 | The transaction header and the storage routing token (`w17-tx-id`, `w17-conn-id`) MUST NOT ride an envelope; both literals are pinned against the constants of the packages that set them. | `TestMetadataFromContext_DropsTheTransactionHeader`, `TestHeaderName_IsTheStrippedEnvelopeKey`, `TestConnIDHeader_IsTheStrippedEnvelopeKey` |
| I8.3 | An emit outside a labelled request MUST be unlabelled; invalidate events MUST take labels from incoming or outgoing metadata. | `TestEmitInvalidate_*` |
| I8.4 | `Event.Metadata` MUST NOT carry a key matching the credential rule above, while principal, scope, label, locale, trace and custom keys still travel. | `TestMetadataFromContext_DropsCredentials` |
| I8.5 | Repeated keys MUST collapse as their subscriber-side readers pick (first for `x-w17-*` / `w17-label-*`, last otherwise), so a subscriber resolves the same language and format locale as the emitter. | `TestMetadataFromContext_RepeatedKeysMatchTheirReaders` |
| I8.6 | A delivery whose envelope metadata carries `w17-tx-id` or `w17-conn-id` (any letter case) MUST be refused on every transport before any handler runs, reported, and dead-lettered through the transport's normal failure path; the key as a value, in labels, or as part of a longer name MUST NOT be refused. | `TestEnvelopeGuard_Memory`, `TestEnvelopeGuard_NATS_Embedded`, `TestEnvelopeGuard_Redis_Embedded`, `TestEnvelopeGuard_RefusedEnvelopeIsDeadLettered_Redis`, `TestEnvelopeGuard_RefusedKeysAreStrippedOnEmit` |

**Deliberate limits.**
- The delivery refusal reads field 50001 only. A hand-written envelope that keeps its metadata elsewhere is not checked.
- An internal service that sends `w17-tx-id` straight to storage over gRPC is not an event; that caller is inside the service mesh, where trust between services applies, not this check.
- The credential rule is a denylist, not an allowlist, because declared metadata propagation uses arbitrary header names. A credential under a name that matches neither an exact name nor a flagged segment travels with the event.

---

## I9. Server-sent-events fan-out hub

**Code.** `go/lib/restgw/w17events_eventbus.go` — `EventbusEventSource`, `entitled`, `handleDelivery`; `go/lib/restgw/w17events.go` — `HandleW17Events`

**Purpose.** One bus subscriber per configured channel, opened lazily on the first
client with `**`, fanned out to every connected client, filtered by exact topic and
by labels.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| Principal-labels deriver | none → no isolation, every event broadcast | configured → isolation (audience rule below) |
| Per-client buffer | 64 frames | full → the frame is dropped for that client only |
| Per-write deadline | 30 s | — |

**Audience rule.** With isolation, an unlabelled event reaches nobody; a labelled
event reaches a client only if every event label is present with the same value in
the principal's labels. A client whose principal cannot be resolved receives nothing.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I9.1 | Events MUST be forwarded by topic across one or several channels, through a single shared subscriber per channel. | `go/lib/restgw/w17events_eventbus_test.go` |
| I9.2 | The audience rule MUST hold: label filtering, unlabelled denied, unresolved principal denied, broadcast without isolation. | `TestEventbusEventSource_LabelFiltering`, `TestEventbusEventSource_UnlabelledEvent*`, `go/lib/restgw/w17events_eventbus_test.go` |
| I9.3 | Every replica MUST receive every event. | `TestEventbusEventSource_EveryReplicaReceivesEveryEvent`, `go/lib/restgw/w17events_replicas_test.go` |
| I9.4 | A failed open MUST drain what it opened, bounded by 10 s, without holding the client lock. | `TestEventbusEventSource_FailedOpenDrainsWhatItOpened`, `TestEventbusEventSource_PartialOpenDrainsWithoutHoldingTheHubLock`, `TestEventbusEventSource_FailedOpenDrainIsBoundedAtTenSeconds` |
| I9.5 | Streams MUST end on client disconnect or server drain. | `TestW17EventsEndsOnDrain`, `go/lib/restgw/w17events_drain_test.go` |
| I9.6 | A transcode error MUST be logged with channel, topic and cause, reported to the error reporter, and the event dropped (skipped for that client set). | `TestEventbusEventSource_TranscodeErrorIsLogged`, `TestEventbusEventSource_TranscodeError`, `TestEventbusEventSource_TranscodeSkip` |
| I9.7 | A slow client MUST lose only its own frames; other clients MUST be unaffected. | `TestEventbusEventSource_SlowClientLosesOnlyItsOwnFrames` |
| I9.8 | A write to a stalled client MUST end within the write deadline. | `TestHandleW17Events_StalledClientEndsWithinTheWriteDeadline` |
| I9.9 | The hub MUST resume delivery after the broker loses and recreates its consumer. | `TestEventbusEventSource_ResumesAfterTheBrokerRecreatesItsConsumer` |

**Deliberate limits.**
- Deliveries are always acked; nothing is retried.
- No SSE `id:` field and no `Last-Event-ID` resume: events emitted while a browser reconnects are not replayed.
- The shared subscriptions live for the process and are never drained.

---

## I10. Principal, read side

**Code.** `go/lib/principal/principal.go` — `UserID`, `Envelope`, `Scope`, `ScopeKey`, `Label`, `LabelKey`, `ErrNoPrincipal`

**Purpose.** Reads the gateway-verified caller from incoming gRPC metadata:
`x-w17-user` (base64 of the auth response proto), `x-w17-scope-<name>` per declared
scope, and `w17-label-<name>` per broadcast label.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I10.1 | `UserID` MUST take the `x-w17-scope-user_id` fast path, else the envelope; the scope MUST win over the envelope. | `TestUserID_FromScope`, `TestUserID_FromEnvelope`, `TestUserID_ScopeWinsOverEnvelope` |
| I10.2 | `UserID` MUST fail closed with `ErrNoPrincipal` (wrapped) on absence, bad base64, bad proto or empty id. | `TestUserID_FailsClosed` |
| I10.3 | `Envelope` and `Scope` MUST report "not present" on absent or empty values. | `TestEnvelope_FullClaims`, `TestEnvelope_FailsClosed`, `TestScope`, `TestScopeKey` |
| I10.4 | `Label` and `LabelKey` MUST report "not present" on absent or empty values. | `TestLabel_AbsentOrEmptyIsNotPresent` |
| I10.5 | Only incoming metadata MUST be read; the first value of a multi-valued key wins. | `TestUserID_FromScope`, `TestReaders_IgnoreOutgoingMetadata`, `TestReaders_FirstValueWins` |

**Deliberate limits.**
- The envelope is not signed. Its integrity rests on the gateway stripping
  client-supplied gateway-owned keys (I11) and on backend tiers being reachable
  only from trusted tiers (network policy or internal mutual TLS). Deployments
  MUST NOT expose backend tiers to untrusted callers.

---

## I11. Principal, gateway side

**Code.** `go/lib/principal/gateway.go` — `SanitizeGatewayMD`, `WithGatewayMD`; `go/lib/principal/reserved.go` — `IsGatewayOwnedKey`; `go/lib/principal/orgscope.go`

**Purpose.** Strips every client-supplied key in the gateway-owned namespaces
`x-w17-*` and `w17-label-*`, and the transaction keys `w17-tx-id` and `w17-conn-id`
(whole name), all case-insensitive, before the gateway stamps the verified
principal, then installs the result on both incoming and outgoing sides as
independent copies. `IsGatewayOwnedKey` is the per-key form for surfaces that build
metadata from headers; `IsEdgeReservedKey` adds `grpcgateway-*` and the
`Grpc-Metadata-` header form for the HTTP edges. `w17-org` is a client-supplied, untrusted organization
selector, deliberately outside the namespace.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I11.1 | A scope or label the auth response omits MUST NOT be suppliable by the client. | `TestWithGatewayMD_SanitizesBothSides` |
| I11.2 | Unauthenticated paths MUST be sanitised too. | `TestWithGatewayMD_UnauthenticatedPathAlsoSanitized` |
| I11.3 | The incoming and outgoing copies MUST NOT alias. | `TestWithGatewayMD_SidesDoNotAlias` |
| I11.4 | `IsGatewayOwnedKey` MUST classify exactly the two namespaces and the two transaction keys (whole name only), case-insensitively. | `TestIsGatewayOwnedKey`, `TestIsEdgeReservedKey` |
| I11.5 | `SanitizeGatewayMD` MUST strip the transaction keys a client sends on the rpc gateway. | `TestSanitizeGatewayMD_StripsTransactionKeys` |

---

## I12. Principal relay

**Code.** `go/lib/principal/principal.go` — `ForwardToOutgoing`

**Purpose.** Relays exactly `x-w17-user`, `x-w17-scope-*` and `w17-label-*` from
incoming to outgoing metadata for the next gRPC hop. It is installed on every
inter-tier dial and on the in-process path. When the incoming side carries any of
these keys, the verified principal wins: the outgoing copy of the whole relayed set
is replaced, so an outgoing value for a relayed key is overwritten and a relayed key
present only on the outgoing side is dropped. Every other outgoing key is kept.
Paging, tracing, locale and transaction metadata are deliberately not relayed. A
context with no incoming principal (the gateway, which stamps the outgoing side
directly, or an unauthenticated call) is returned unchanged.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I12.1 | The principal, scopes and labels MUST survive any number of hops. | `TestForwardToOutgoing_RelaysPrincipal`, `TestForwardToOutgoing_RelaysBroadcastLabels`, `TestForwardToOutgoing_SecondHopKeepsTheLabel` |
| I12.2 | Non-principal metadata MUST NOT be relayed. | `TestForwardToOutgoing_ExcludesNonPrincipal` |
| I12.3 | With an incoming principal, the relayed set MUST replace the outgoing copy (overwritten values, outgoing-only relayed keys dropped) while other outgoing keys are kept; values MUST NOT be duplicated and the call MUST be idempotent. | `TestForwardToOutgoing_VerifiedPrincipalWins` |
| I12.4 | A context without an incoming principal MUST be returned unchanged, so the stamps the gateway wrote on the outgoing side travel as written. | `TestForwardToOutgoing_NoIncomingIsNoop`, `TestForwardToOutgoing_GatewayStampsUntouched` |

**Deliberate limits.**
- Calling a downstream tier as a different principal is an explicit act: drop the incoming
  principal first (for example with `metadata.NewIncomingContext(ctx, nil)`) and stamp the
  outgoing side directly.
- The relayed envelope is unsigned; the trust boundary is the one stated in I10.

---

## I13. Permission lock and checks

**Code.** `go/lib/acllock/lock.go`; `go/lib/acllock/allocator.go` — `Allocate`; `go/lib/acllock/bits.go` — `HasPermission`, `HasAnyPermission`, `HasAllPermissions`, `HasAllPermissionsStrict`, `GrantAll`, `IDByString`; `go/lib/acllock/permission_key.go` — `PermissionKey`

**Purpose.** Model of a domain's permission allocation table, and the runtime
check helpers. `IDByString` accepts the enum value name or the cascade code.

**Configuration.**

| Input | Behaviour |
|---|---|
| `HasPermission(ids, id)` with empty `ids` | false |
| `HasAnyPermission` with empty `wanted` | false |
| `HasAllPermissions` with empty `wanted` | **true** (documented); use `HasAllPermissionsStrict`, which returns false |
| nil lock | no IDs, not found |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I13.1 | `Allocate` MUST keep existing IDs, give new permissions the smallest positive unused, unreserved ID, move removed IDs to `Reserved`, skip empty strings, and be deterministic for a given input order. | `TestAllocate_EmptyPrev`, `TestAllocate_PreservesExisting`, `TestAllocate_RemovedGoesToReserved`, `go/lib/acllock/allocator_internal_test.go` |
| I13.2 | A reserved ID MUST never be reused, across any number of generations. | `TestAllocate_GenerationsKeepEveryIDAccountedFor` |
| I13.3 | `Allocate` MUST NOT consume an ID without assigning or reserving it (for example on duplicate input entries). | `TestAllocate_DuplicateEntryConsumesNoID`, `TestAllocate_GenerationsKeepEveryIDAccountedFor` |
| I13.4 | Checks MUST deny by default and never grant on error; no helper returns an error. | `TestHasPermission`, `TestHasAllPermissions`, `go/lib/acllock/strict_test.go` |
| I13.5 | `HasAnyPermission` with empty `wanted` MUST return false. | `TestHasAnyPermission` |
| I13.6 | `GrantAll` MUST return every current, non-reserved ID. | `TestGrantAll` |
| I13.7 | `IDByString` MUST resolve declared enum names and cascade codes. | `TestIDStringLookups`, `go/lib/acllock/permission_alphabet_test.go` |

**Deliberate limits.**
- Permission IDs are per domain and `permission_ids` is a flat int32 list, so a check is meaningful only against the lock of the same domain.
- `Allocate` does not compute the checksum.

---

## I14. Message catalogs and translation

**Code.** `go/lib/i18n/i18n.go` — `Register`, `RegisterFS`, `SetDefaultLanguage`, `Reset`, `T`, `LocaleFromContext`, `negotiateCatalog`

**Purpose.** gettext catalogs registered from embedded `.po` files; `T(ctx, msgid,
params)` translates for the request's locale and substitutes `{name}` placeholders.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `RegisterCatalog(lang, po) error` | — | empty `lang` → no-op; returns syntax problems (with line numbers) and conflicting translations; a malformed `.po` is still registered best-effort |
| `Register(lang, po)` | — | as `RegisterCatalog`, but logs the problems instead of returning them |
| `RegisterFS(fs)` | — | language = file base name; returns the problems joined per file path, and `ErrNoCatalogs` for a tree with no `.po` file |
| Same language registered twice | — | the catalogs are merged; a conflicting translation keeps the first one and is reported |
| `SetDefaultLanguage` | `en` | empty ignored; a second, different value is logged and takes effect |
| Catalog tag | — | matched regardless of case and `_`/`-` spelling, for catalogs and for the default language (`pt_BR.po` serves `pt-BR`) |
| Locale | last `w17-language` value, incoming before outgoing, else default | parsed as Accept-Language: each tag, then its base language, else the default catalog |

**Lookup.** For each message `T` looks in the negotiated catalog, then in the
default-language catalog, then returns the msgid. An empty `msgstr` counts as
untranslated. Substitution is one left-to-right pass over the translated text.

**Shares.** Catalogs and the default language are process-global.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I14.1 | Translation and substitution MUST work; the default catalog MUST be the fallback; a missing locale MUST give the msgid. | `TestT_RegisteredLocale_TranslatesAndSubstitutes`, `TestT_DefaultLocaleFallback`, `TestT_RequestedLocaleMissing_FallsBackToDefault`, `TestT_NoCatalogForDefaultLanguage_ReturnsMsgid` |
| I14.2 | Negotiation MUST handle multi-value Accept-Language, region to base language, last value wins, incoming before outgoing. | `TestT_AcceptLanguageMultiValue_NegotiatesCatalog_B31`, `TestT_AcceptLanguageRegion_FallsBackToBase_B31`, `TestLocaleFromContext_LastValueWins`, `TestLocaleFromContext_OutgoingMetadata`, `TestLocaleFromContext_IncomingBeatsOutgoing` |
| I14.3 | Unknown placeholders MUST survive verbatim; an empty msgid MUST return "" so the `.po` header never leaks. | `TestSubstitute_UnknownPlaceholderPassesThrough`, `TestT_EmptyMsgid_ReturnsEmpty` |
| I14.4 | Concurrent use MUST be safe. | `go/lib/i18n/i18n_extra_test.go` |
| I14.5 | A msgid missing from the requested catalog, or present with an empty `msgstr`, MUST fall back to the default-language catalog, then to the msgid (per message, not per catalog). | `TestT_FallbackIsPerMessage`, `TestT_EmptyMsgstrFallsThrough` |
| I14.6 | Substitution MUST be deterministic and MUST NOT expand placeholder tokens inside parameter values. | `TestSubstitute_DeterministicSinglePass`, `TestSubstitute_BraceEdges` |
| I14.7 | Catalog names MUST be matched regardless of `_`/`-` spelling and case (`pt_BR.po` serves `pt-BR`). | `TestCatalogTag_UnderscoreAndCaseInsensitive` |
| I14.8 | A malformed catalog, and a catalog tree without any `.po` file, MUST be reported at registration: `ValidatePO` and `RegisterCatalog` return the problems with line numbers, `RegisterFS` joins them per file path and adds `ErrNoCatalogs`. | `TestValidatePO`, `TestRegisterFS_ReportsMalformedCatalog`, `TestRegisterFS_NoCatalogIsReported` |
| I14.9 | Registering a language twice MUST merge the catalogs; a conflicting translation MUST keep the first one and be reported. | `TestRegisterCatalog_MergesAndReportsConflicts` |

---

## I15. Number and date formatting

**Code.** `go/lib/i18n/formats.go`, `go/lib/i18n/formatnum.go`, `go/lib/i18n/formatdate.go`, `go/lib/i18n/formatlocale.go` — `FormatLocaleFromContext`

**Purpose.** A frozen per-locale table embedded from `formats.json`, resolved locale
→ base language → `en` with case and `_` normalisation, with per-kind project
overrides. `FormatLocaleFromContext` uses the last `w17-format-locale` value, incoming
before outgoing, else `LocaleFromContext`; Accept-Language lists are negotiated.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I15.1 | Numbers MUST use decimal-string arithmetic, round half-up away from zero, never print negative zero, and return non-decimal input unchanged. | `TestFormatGoldenVectors` |
| I15.2 | Dates MUST render in UTC with a closed token vocabulary; unparseable input MUST be returned unchanged. | `TestFormatGoldenVectors` |
| I15.3 | The table MUST be complete; the resolution chain and overrides MUST apply. | `go/lib/i18n/format_test.go` |
| I15.4 | Format-locale fallback and negotiation MUST apply. | `go/lib/i18n/format_test.go` |
| I15.5 | An explicit format locale MUST be honoured on both incoming and outgoing metadata. | `TestFormatLocaleFromContext_OutgoingMetadata` |

---

## I16. Catalog tooling

**Code.** `go/lib/i18n/extract.go`, `go/lib/i18n/harvest.go` — `MarshalPO`, `ScaffoldPO`, `DefaultLocalePO`, `MarshalJSON`, `MergePO`, `EntriesFromPO`, `EmitDomainCatalogs`, `HarvestEnumLabels`

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I16.1 | `MergePO` MUST keep translator edits, drop obsolete msgids, and keep untranslated entries empty. | `go/lib/i18n/merge_untranslated_test.go`, `go/lib/i18n/extract_test.go` |
| I16.2 | Extraction, scaffolding and enum-label harvesting MUST produce the catalogs the runtime reads. | `go/lib/i18n/extract_test.go`, `go/lib/i18n/extract_extra_test.go`, `go/lib/i18n/harvest_test.go`, `go/lib/i18n/harvest_extra_test.go` |

---

## I17. Shared value validators

**Code.** `go/lib/typere/typere.go` — `UUID`, `Slug`, `URL`, `MAC`, `ValidEmail`, `FitsDecimal`

**Purpose.** Validators referenced by generated handlers.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I17.1 | `ValidEmail` MUST require one `@`, a non-empty local part and a domain with an inner dot, and reject any byte `<= 0x20` or `0x7f`. | `TestValidEmail`, `TestValidEmail_RejectsControlChars` |
| I17.2 | `FitsDecimal(s, precision, scale)` MUST be value preserving, reject exponent and NaN, and accept empty input. | `TestFitsDecimal` |
| I17.3 | `FitsDecimal` MUST agree with the database for every column shape the declaration validator admits (including scale greater than precision and negative scale). | `TestFitsDecimal_AgreesWithPostgresOnEveryScale` |
| I17.4 | `UUID` (8-4-4-4-12 hex, any version, case-insensitive), `Slug`, `URL` (`^https?://.+$`) and `MAC` MUST match their documented shapes. | `TestTypeRegexes_MatchTheirDocumentedShapes` |

**Deliberate limits.**
- These are shape checks. `URL` does no host check and accepts only a lower-case scheme. `ValidEmail` blocks control characters but not markup; output escaping is the renderer's job.

---

## I18. Random tokens

**Code.** `go/lib/randstr/randstr.go` — `Token`

**Purpose.** `Token(n)` returns `n` base64url characters from `crypto/rand`
(6·n bits of entropy).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| I18.1 | Output MUST be exactly `n` characters from the base64url alphabet, with no padding. | `TestToken_LengthInvariant`, `TestToken_CharsetInvariant`, `TestToken_NoPaddingInTruncated` |
| I18.2 | Every character MUST be drawn from `crypto/rand`, never `math/rand`. | `TestToken_NonRepeating` |
| I18.3 | `n <= 0` MUST be an error. | `TestToken_RejectsNonPositive` |

**Deliberate limits.**
- The caller chooses `n`; a short `n` gives low entropy.
