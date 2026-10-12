# Edges and traffic

Everything between a caller and a generated service's handlers, plus the outgoing side of a
service: the REST gateway runtime, the per-client rate limiter, the MCP proxy runtime, the
w17 JSON dialect, the gRPC dial helpers, the database-error mapper and the egress JSON codec.
Packages: `go/lib/restgw`, `go/lib/ratelimit`, `go/lib/mcp`, `go/lib/protojsonx`,
`go/lib/grpcclient` (dial side; server TLS is in [server.md](server.md)), `go/lib/egress/wire`,
`go/core/grpcerr` (with `go/core/grpcerr/dialect`), `go/core/grpcx`.

Much of the gateway's behaviour is decided by the generated code that calls these helpers:
which middleware is mounted and in which order, which routes skip authentication, and whether a
stream is wrapped in the re-authentication watchdog. Where a guarantee depends on that wiring it
is stated as a contract the caller must honour.

## Request path

The middleware order the helpers are designed for, outermost first. Generated `main.go`
assembles this chain; the SDK does not.

```
http.Server
 └─ RecoverPanicMiddleware
     └─ StripReservedHeadersMiddleware        drops X-W17-Credential
         └─ ObservabilityMiddleware           otelhttp span + request id
             └─ CORSMiddleware                preflight answered here (204)
                 └─ MetadataPropagation → MetadataRename → MetadataDefaultStamp
                     └─ chi router
                         ├─ per-RPC handler: [AdmitPublic] → [WithURLCredential / WithOrgFromPath]
                         │      → AuthFunc (optionally CachedAuthFunc) → SetUserMetadata → scopes
                         │      → DecodeRequest → backend gRPC (grpcclient.Dial conn)
                         │      → WriteResponse / WriteGRPCErrorCtx
                         ├─ SSE / WS handlers: [AdmitPublicStream] → auth → WatchStreamAuth
                         ├─ /w17-events: RequireAuth(NewWSAuth(...)) → WatchStreamAuth → HandleW17Events
                         ├─ ticket issuer (POST): NewTicketIssuer
                         └─ downloads: auth → ServeDownload
```

Outgoing: `grpcclient.Dial` (gateway → backend) and `grpcx.DialOpts` + `grpcx.Pool` (business
tier → storage or other domains; adds the principal relay interceptors).

## Features

| ID | Feature | Code |
|---|---|---|
| E1 | Request decoding and wire-format negotiation | `go/lib/restgw/json.go`, `go/lib/restgw/negotiate.go` — `DecodeRequest`, `WriteResponse` |
| E2 | The w17 JSON dialect | `go/lib/restgw/json.go`, `aliases.go`, `jsoncodec.go` — `Marshaller`, `RegisterJSONMarshaler` |
| E3 | Error envelope and gRPC → HTTP mapping | `go/lib/restgw/errors.go` — `WriteGRPCErrorCtx` |
| E4 | Gateway-own refusals | `go/lib/restgw/refusal.go` — `WriteRefusal`, `WriteBadInput` |
| E5 | Authentication hook and principal threading | `go/lib/restgw/auth.go` — `MakeAuthFunc`, `RequireAuth`, `SetUserMetadata` |
| E6 | Auth result cache | `go/lib/restgw/auth.go`, `authlookupcache.go` — `CachedAuthFunc`, `AuthLookupCache` |
| E7 | URL-carried credentials and reserved-header scrub | `go/lib/restgw/url_credential.go` — `WithURLCredential`, `StripReservedHeadersMiddleware` |
| E8 | Organization from the path | `go/lib/restgw/org_from_path.go` — `WithOrgFromPath` |
| E9 | Data-scope key helpers | `go/lib/restgw/scope.go` — `ScopeKey` |
| E10 | Header → metadata propagation, rename, default stamp | `go/lib/restgw/metadata_propagation.go`, `metadata_rename.go` |
| E11 | Request id, tracing, metrics | `go/lib/restgw/request_id.go`, `otel.go` — `ObservabilityMiddleware` |
| E12 | CORS | `go/lib/restgw/cors.go` — `CORSMiddleware` |
| E13 | HTTP panic recovery | `go/lib/restgw/recover.go` — `RecoverPanicMiddleware` |
| E14 | Per-RPC SSE helpers | `go/lib/restgw/streaming.go` — `WriteSSEHeaders`, `WriteSSEEvent` |
| E15 | WebSocket helpers | `go/lib/restgw/streaming.go` — `AcceptWebSocket`, `WSReadProto` |
| E16 | Browser stream auth: tickets | `go/lib/restgw/ws_auth.go`, `ticket.go` — `NewWSAuth`, `NewTicketIssuer`, `MemoryTicketStore` |
| E17 | Stream re-authentication watchdog | `go/lib/restgw/stream_reauth.go` — `WatchStreamAuth` |
| E18 | The reserved `w17-events` channel | `go/lib/restgw/w17events.go`, `w17events_eventbus.go` — `HandleW17Events`, `EventbusEventSource` |
| E19 | Public-method rate-limit adapters | `go/lib/restgw/publiclimit.go` — `AdmitPublic`, `AdmitPublicStream` |
| E20 | Multipart uploads | `go/lib/restgw/upload.go` — `ProcessFilePart`, `RollbackUploads` |
| E21 | Downloads | `go/lib/restgw/download.go` — `ServeDownload`, `BuildDownloadURL` |
| E22 | Browser redirects | `go/lib/restgw/browserredirect.go` — `WriteBrowserRedirect` |
| E23 | Pagination links | `go/lib/restgw/pagination.go` |
| E24 | Query-string enum parsing | `go/lib/restgw/queryenum.go` — `SetEnumField` |
| E25 | Health handler | `go/lib/restgw` — `HealthHandler` |
| E26 | Per-client rate limiter | `go/lib/ratelimit/ratelimit.go` — `Limiter`, `FromEnv`, `FromEnvScope` |
| E27 | MCP server and transports | `go/lib/mcp/server.go` — `NewServer`, `ServeStreamableHTTP`, `CallUnary` |
| E28 | MCP auth translation and permission gating | `go/lib/mcp/auth.go`, `server.go` — `DefaultAuthFunc`, `SetPermResolver`, `SetToolPerm` |
| E29 | Oneof collapse and expand | `go/lib/protojsonx/oneof.go` — `ExpandOneofs` |
| E30 | Client dial | `go/lib/grpcclient/grpcclient.go` — `Dial`, `TLSDialOption` |
| E31 | Inter-tier dial options and principal relay | `go/core/grpcx/dial.go` — `DialOpts`, `InternalTLSEnabled` |
| E32 | Connection pool | `go/core/grpcx/pool.go` — `Pool`, `Connect`, `ConnectScaled` |
| E33 | Database error → gRPC status | `go/core/grpcerr/wrap.go`, `go/core/grpcerr/dialect` — `Wrap`, `RecoverPanic` |
| E34 | Egress JSON and form codec | `go/lib/egress/wire` — `plan.go`, `marshal.go`, `unmarshal.go`, `form.go`, `union.go` |

---

## E1. Request decoding and wire-format negotiation

**Code.** `go/lib/restgw/json.go` — `DecodeRequest`, `DecodeRequestRewrite`, `WriteResponse`, `WriteResponseFiltered`, `MaxRequestBodyBytes`, `addVary`; `go/lib/restgw/negotiate.go` — response format selection.

**Purpose.** Reads a request body into a proto message, choosing JSON or binary protobuf from
`Content-Type`, and writes responses in the format the `Accept` header prefers.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `MaxRequestBodyBytes` (package var) | 10 MiB | Process-global, not synchronised: set before serving |
| `Content-Type` | JSON | `application/protobuf` or `application/x-protobuf` → binary; anything else or absent → JSON |
| `Accept` | JSON | q-values, specificity and order decide; no recognised entry → JSON; `*/*` and `application/*` count as JSON |
| `WriteResponse(w, status, msg, r)` | — | `r` optional; without it the response is always JSON and carries no `Vary: Accept` |
| `WriteResponseFiltered(..., omitFields, r)` | — | Clears the named top-level fields on a clone |
| `DecodeRequestRewrite(r, msg, fn)` | — | Raw-JSON key rewrite before decode, JSON only |

**Shares.** The JSON dialect (E2), the error envelope (E3), and buffer pools with the SSE and WS
writers.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E1.1 | A body longer than `MaxRequestBodyBytes` MUST be refused without reading more than limit + 1 bytes | `TestDecodeRequest_BodyTooLarge` |
| E1.2 | Empty, whitespace-only and `null` JSON bodies MUST leave the message untouched and succeed | `TestDecodeRequest_EmptyNoop` |
| E1.3 | An empty protobuf body MUST succeed; a malformed one MUST be an error | `TestDecodeRequest_ProtobufEmptyAndBad` |
| E1.4 | Unknown JSON fields MUST be ignored | `TestDecodeRequest_IgnoresUnknownFields` |
| E1.5 | The response format MUST follow the highest-q, most specific, earliest `Accept` entry | `TestResponseFormat`, `TestNegotiation_HTTPRoundTrip` |
| E1.6 | A marshal failure MUST be a JSON 500 envelope, never a partial body, in either format | `TestWriteResponse_MarshalErrorIs500` |
| E1.7 | `WriteResponseFiltered` MUST NOT mutate the caller's message | `TestWriteResponseFiltered_ClearsNamedFields` |
| E1.8 | A JSON key rewrite hook MUST NOT be called for protobuf or empty bodies | `TestDecodeRequestRewrite` |
| E1.9 | Decode errors MUST NOT reach the client as decoder prose; callers route them through `WriteInvalidRequest` (E4) | `TestWriteInvalidRequest_KeepsDecoderProseFromTheClient` |
| E1.10 | A negotiated response (a request passed to `WriteResponse`) MUST carry `Vary: Accept`, whichever format won, so a shared cache never serves one format to a request for the other | `TestWriteResponse_NegotiatedResponseVariesOnAccept` |

**Deliberate limits.**
- The protobuf input path skips alias and oneof normalisation (binary keys on field numbers).

---

## E2. The w17 JSON dialect

**Code.** `go/lib/restgw/json.go` — `Marshaller`, `Unmarshaller`; `go/lib/restgw/aliases.go` — alias rewrite; `go/lib/protojsonx/oneof.go` — `DecodeTree`; `go/lib/restgw/jsoncodec.go`, `jsoncodec_wkt.go` — `RegisterJSONMarshaler`, `MarshalProtoAppend`.

**Purpose.** One JSON shape for every surface: proto field names, enum numbers, 64-bit
integers as strings, zero values omitted, oneofs collapsed (E29), optional per-field
`rest_alias` renames, and a zero-allocation fast path when codegen registered a marshaler.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `Marshaller` / `Unmarshaller` (package vars) | dialect options | Exported but not an operator knob: changing them makes the fast path and the reflective path disagree |
| `RegisterJSONMarshaler(fullName, fn)` | none | Called from generated `init()` |
| `(w17.field).rest_alias` | none | Descriptor option, read at runtime |

**Shares.** `protojsonx` (E29) for the oneof reshape. MCP (E27) uses the same options without
the alias rewrite. SSE and WS writers use `MarshalProtoAppend`.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E2.1 | Enums MUST be written as integers; names and numbers MUST both be accepted on input | `oneof_test.go`, `queryenum_test.go`, `TestWriteResponse_EnumsAreNumbers` |
| E2.2 | A message with no alias and no oneof MUST be byte-identical to protojson | `TestOneof_NoOneof_ByteIdenticalToProtojson`, `TestMarshalProto_NoAliasFastPath` |
| E2.3 | Aliased fields MUST be written under the alias; input MUST accept the alias or the proto name | `TestMarshalProto_AliasedFieldRenamesKey`, `TestRestoreAliasesOnRequestJSON_AcceptsBoth` |
| E2.4 | A body giving the same field under both alias and canonical name MUST be refused | `TestUnmarshalProto_AliasAndCanonicalCollision_Rejected` |
| E2.5 | A registered generated marshaler MUST be preferred over protojson | `TestMarshalProto_PrefersRegisteredMarshaler` |
| E2.6 | Invalid UTF-8 in a string field MUST be a marshal error, not mangled output | `TestMarshalProto_InvalidUTF8Errors` |
| E2.7 | Decoding MUST NOT lose numeric precision on input: a 64-bit integer sent as a JSON number keeps its value on every message, including messages reshaped for a oneof or a `rest_alias` | `TestUnmarshalProto_KeepsSixtyFourBitNumbersExact`, `TestDecodeRequest_KeepsSixtyFourBitNumbersExact`, `TestRestoreAliasesOnRequestJSON_KeepsNumbersExact` |
| E2.8 | The fast path and the reflective path MUST produce the same JSON value for the same message | `TestFastPathEncoders_MatchTheReflectivePath` (the well-known-type encoders; per-message marshalers are generated and not covered here) |

**Deliberate limits.**
- The reflective alias and oneof rewrite re-encodes through `encoding/json`, which HTML-escapes
  `<`, `>` and `&`; the fast path does not. The JSON value is the same, the bytes differ.
- The reshape decodes with `UseNumber` (`protojsonx.DecodeTree`) and only renames keys; numbers
  are carried as their original text.

---

## E3. Error envelope and gRPC → HTTP mapping

**Code.** `go/lib/restgw/errors.go` — `WriteGRPCError`, `WriteGRPCErrorCtx`, `ClientError`, `HTTPStatusFromGRPCCode`; `go/lib/restgw/json.go` — envelope writer.

**Purpose.** Every error leaves the gateway as
`{"error":{"code":"<GRPC_CODE_NAME>","message":"<sentence>","details":[...]}}` with an HTTP
status derived from the gRPC code. The backend's `status.Message()` is operator-only. The client
sees a request-level `w17.ErrorDetail` sentence if the backend attached one, otherwise the
code's generic, translated sentence.

**Configuration.** None. The `...Ctx` variants translate with the request context's locale
(`go/lib/i18n`); context-free variants answer in the source locale.

**Shares.** `grpcerr.UserMsgid` (E33), the single source of fallback sentences; SSE and WS error
writers (E14, E15); refusal writers (E4); `observx` reporting. `ClientError` returns the code,
sentence and field details the envelope carries without writing it; the MCP proxy (E27) builds its
tool errors from it.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E3.1 | Status mapping MUST be: Canceled→499; InvalidArgument, FailedPrecondition, OutOfRange→400; NotFound→404; AlreadyExists, Aborted→409; PermissionDenied→403; Unauthenticated→401; ResourceExhausted→429; Unimplemented→501; Unavailable→503; DeadlineExceeded→504; everything else→500 | `TestHTTPStatusFromGRPCCode` |
| E3.2 | `code` MUST always be the canonical UPPER_SNAKE gRPC name, never a detail code | `TestGRPCCodeName_Canonical`, `TestClientFacing_RequestLevelDetailStillWins` |
| E3.3 | `status.Message()` MUST NOT reach the client | `TestWriteGRPCError_StatusMessageNeverReachesTheClient` |
| E3.4 | A non-status error (dial failure, transport) MUST become a generic 500 "internal error" without backend addresses | `TestWriteGRPCError_DialFailureDoesNotLeakBackendAddress`, `TestWriteGRPCError_PlainErr` |
| E3.5 | Field-level `ErrorDetail`s MUST be forwarded as `details[]` | `TestWriteGRPCError_ForwardsFieldViolationDetails` |
| E3.6 | A nil error MUST still produce a valid 500 envelope | `TestWriteGRPCError_NilErr` |
| E3.7 | A client error MUST NOT be reported as a server fault: `WriteGRPCErrorCtx` classifies by code as the SSE and WS writers do, refusals going to the non-exception channel and faults to the exception channel; reports MUST carry the request's trace context | `TestWriteGRPCErrorCtx_ReportsByCodeOnTheRequestsTrace` |

**Deliberate limits.**
- Request-level and field-level detail messages written by a backend are shown verbatim; the
  backend is trusted to have written them for a person.

---

## E4. Gateway-own refusals

**Code.** `go/lib/restgw/refusal.go` — `WriteRefusal`, `WriteBadInput`, `WriteInvalidRequest`, `WriteInternal`, `PreflightSentence`, `ClientSentence`, `Vocabulary`; `go/lib/restgw/auth.go` — `WriteForbidden(Ctx)`, `WriteUnauthorized(Ctx)`; `go/lib/restgw/scope.go` — `WriteMissingScope(Ctx)`; `go/lib/restgw/upload.go` — `WriteUploadErrorCtx`, `MsgidUploadMissing`, `MsgidUploadTooLarge`, `MsgidUploadBadType`; `go/lib/restgw/publiclimit.go` — `WriteTooManyRequestsCtx`.

**Purpose.** Refusals decided by the gateway itself (missing permission, unreadable path
parameter, malformed body, no auth method for a scheme) follow the same two-audience rule as E3:
the operator string goes to observability, the client gets a code and a catalog sentence.

**Configuration.** None. The `...Ctx` writers translate with the request locale; the
context-free ones keep their signatures and answer in the source locale.

**Shares.** `grpcerr.UserMsgid`, `grpcerr.CodeMissingPermission`; `Vocabulary()` (harvests the
upload msgids); `observx.ReportRefusal`
(non-exception channel) versus `ReportError` (server faults).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E4.1 | A 403 for a missing permission MUST NOT name the permission to the client; it carries `details[0].code = MISSING_PERMISSION` | `TestWriteForbiddenCtx_NamesThePermissionOnBothChannels`, `TestWriteForbiddenCtx_NoPermissionStaysGeneric` |
| E4.2 | A 401 for an unsupported credential scheme MUST NOT name the scheme | `TestWriteUnauthorized` |
| E4.3 | `WriteBadInput` MUST name the caller-facing field in `details[].field` with code `INVALID_VALUE`, never the parser's prose | `TestWriteBadInput` |
| E4.4 | Refusals MUST reach the operator channel with no environment knob set | `TestWriteRefusal_ReachesTheOperatorWithNoKnobSet` |
| E4.5 | Gateway pre-flight validation and backend InvalidArgument MUST produce the same sentence | `preflight_sentence_test.go` (all four tests) |
| E4.6 | Every gateway-written envelope MUST carry a catalog sentence, never an operator or configuration string (a scope name, the extension allowlist, a size limit); the detail goes to the operator channel | `TestWriteMissingScopeCtx_NameReachesTheOperatorOnly`, `TestWriteMissingScope`, `TestWriteUploadErrorCtx_RefusalsAreCatalogSentences`, `TestVocabulary_CarriesTheUploadSentences`, `TestAdmitPublic_RefusalIsTheCatalogSentence`, `TestWriteBrowserRedirect_RefusalIsTheCatalogSentence`, `TestTicketIssuer_ErrorsAreTheEnvelope`, `TestServeDownload_ErrorsAreTheEnvelope` |

---

## E5. Authentication hook and principal threading

**Code.** `go/lib/restgw/auth.go` — `MakeAuthFunc`, `NoAuth`, `ClassifyAuthScheme`, `RequireAuth`, `SetUserMetadata`, `GetUserMetadata`, `WriteAuthError`.

**Purpose.** Runs the surface's auth RPC with all request headers and stores the marshalled
auth response in outgoing metadata (`x-w17-user`, base64) and on the request context.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `MakeAuthFunc(caller)` | — | Forwards the first value of every header, keyed by canonical name |
| `NoAuth()` | — | Always succeeds with nil user data |
| `ClassifyAuthScheme(r)` | `""` | `Bearer` / `Basic`, case-insensitive |
| `RequireAuth(authFn, next)` | — | Gate for routes mounted outside per-RPC handlers (SSE, WS) |

**Shares.** Auth cache (E6), URL credentials (E7), org from path (E8), WS tickets (E16), stream
re-auth (E17), `w17-events` label filtering (E18), and `go/lib/principal`, which reads
`x-w17-user` on the backend.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E5.1 | Empty user data MUST NOT write `x-w17-user` | `TestAuth_SetUserMetadata_NilNoOp` |
| E5.2 | `SetUserMetadata` MUST overwrite (not append) `x-w17-user` and preserve other outgoing metadata | `TestSetUserMetadata_PreservesExistingOutgoing` (partial: replacement of a pre-existing value has no test) |
| E5.3 | `RequireAuth` MUST map a missing or invalid ticket to 401 and other failures through `WriteAuthError` (status → mapped, non-status → 500) | `TestRequireAuth_TicketSentinel401`, `TestRequireAuth_TransportError500` |
| E5.4 | `RequireAuth` MUST stash user data on the context for streaming handlers | `TestRequireAuth_StashesPrincipal` |
| E5.5 | `GetUserMetadata` MUST treat absent, empty and non-base64 values as "no principal" | `TestAuth_GetUserMetadata_AbsentOrBad`, `TestGetUserMetadata_EmptyValueIsAbsent` |
| E5.6 | A handler MUST NOT run after a failed auth (per-RPC routes: generated handler; mounted streams: `RequireAuth`) | `TestRequireAuth_TicketSentinel401`, `TestRequireAuth_TransportError500` (mounted streams; per-RPC routes are generated and not covered here) |
| E5.7 | A client MUST NOT be able to set `x-w17-user` through the REST surface, on any route | `TestEdge_ClientCannotSetReservedMetadataOnAnySurface`, `TestMetadataMiddlewares_RefuseReservedKeysWithoutTheScrub` |

**Deliberate limits.**
- `RequireAuth`'s non-ticket errors use the context-free writer (source locale).
- Multi-valued headers reach the auth method as their first value only.

---

## E6. Auth result cache

**Code.** `go/lib/restgw/auth.go` — `CachedAuthFunc`, `hashHeaders`; `go/lib/restgw/authlookupcache.go` — `NewAuthLookupCache`.

**Purpose.** An optional TTL cache of successful auth results keyed on credential headers, so
that not every request costs an auth RPC.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `<PREFIX>_AUTH_CACHE_TTL_SECONDS` | off | Unset, 0, negative or unparseable → off |
| `<PREFIX>_AUTH_CACHE_CRED_HEADERS` | none | Extra credential headers folded into the key |
| Key headers | `Authorization`, `Cookie`, `X-Api-Key`, `X-W17-Credential` | — |
| Size ceiling | 8192 entries, swept to 4096 | Not configurable |
| `NewAuthLookupCache(ttl)` | — | Same key rule for a typed lookup; nil (off) when `ttl <= 0` |

**Shares.** Key derivation (`hashHeaders`) between both caches; E17 (revocation latency becomes
`max(reauth interval, TTL)`); E7 (credential slot in the key); E8 (`W17-Org` forces a bypass).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E6.1 | Two requests MUST share a slot only if every credential header value is identical | `TestAuth_CachedAuthFunc_DistinctHeadersDistinctSlots`, `TestAuthLookupCache_TwoCredentialsNeverShareASlot` |
| E6.2 | A request carrying any header outside the credential set and the curated non-credential list MUST bypass the cache | `TestAuth_CachedAuthFunc_UnknownHeaderBypassesCache`, `TestAuth_CachedAuthFunc_CustomCredentialHeader_NoCrossPrincipal` |
| E6.3 | A request with no credential header MUST NOT be cached | `TestAuthLookupCache_UncredentialedRequestIsNotCached` |
| E6.4 | Errors MUST NOT be cached | `TestAuth_CachedAuthFunc_ErrorsNotCached` |
| E6.5 | Every hit MUST return a private copy | `TestAuth_CachedAuthFunc_HitsDoNotShareABackingArray`, `TestAuthLookupCache_HitsDoNotAlias` |
| E6.6 | The entry count MUST stay bounded under distinct valid credentials; expired entries MUST be reaped without a same-key get | `authcache_bound_internal_test.go` (both tests) |
| E6.7 | TTL expiry MUST force a fresh auth call | `TestAuth_CachedAuthFunc_TTLExpiry`, `TestAuthLookupCache_Expires` |

**Deliberate limits.**
- Revocation is invisible for up to the TTL.
- Eviction past the ceiling is arbitrary, not LRU.

---

## E7. URL-carried credentials and reserved-header scrub

**Code.** `go/lib/restgw/url_credential.go` — `CredentialHeader`, `URLCredentialFromPath`, `URLCredentialFromQuery`, `WithURLCredential`, `HasURLCredential`, `StripReservedHeadersMiddleware`.

**Purpose.** Capability links (`/public/invoices/{token}`, `?t=…`) authenticate through the
normal auth path by copying the URL value into a reserved header slot that clients can never set.

**Configuration.** None. `CredentialHeader = "X-W17-Credential"` is fixed.

**Shares.** E6 (key header), E5, E17 (the watchdog re-probes the request carrying the slot).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E7.1 | Any inbound `X-W17-Credential`, in any case variant, MUST be removed before the next handler runs, without mutating the caller's header map | `TestStripReservedHeadersMiddleware_ForgedCredentialNeverReachesHandler`, `..._CanonicalisationVariants`, `..._DoesNotMutateCallersHeaders` |
| E7.2 | An empty URL credential MUST count as absent and answer 401, not 400 | `TestHasURLCredential_EmptyValueIsNotACredential`, `TestMissingURLCredentialIsUnauthenticated` |
| E7.3 | Injection MUST return a clone and leave the original request unchanged | `TestWithURLCredential_RoundTrip` |
| E7.4 | `StripReservedHeadersMiddleware` MUST be mounted outside every header reader (caller contract) | no test at chain level |

**Deliberate limits.**
- The scrub list contains only `X-W17-Credential`; other gateway-internal markers (such as the
  ticket promotion header, E16) are protected by unguessable values instead.

---

## E8. Organization from the path

**Code.** `go/lib/restgw/org_from_path.go` — `WithOrgFromPath`, `ErrOrgPathConflict`.

**Purpose.** Makes an `{org}` path segment the active organization by writing it into the
`W17-Org` header before authentication.

**Configuration.** The path parameter name, from generated code.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E8.1 | The path org MUST be set as the `W17-Org` header on a clone | `TestWithOrgFromPath_SetsTheHeader` |
| E8.2 | A conflicting `W17-Org` header MUST be refused (`ErrOrgPathConflict`) | `TestWithOrgFromPath_RefusesAConflictingHeader` |
| E8.3 | An empty path value MUST be an error | `TestWithOrgFromPath_EmptySegmentIsRefused` |

**Deliberate limits.**
- The returned errors contain both values; callers must not echo them. The HTTP answer is left
  to generated code.

---

## E9. Data-scope key helpers

**Code.** `go/lib/restgw/scope.go` — `ScopeKey`, `WriteMissingScope`.

**Purpose.** One owner for the `x-w17-scope-<name>` metadata key convention.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E9.1 | `ScopeKey(name)` MUST equal `x-w17-scope-` + name verbatim | `TestScopeKey` |

**Deliberate limits.**
- `name` is not validated here; the parser validates it upstream.

---

## E10. Header → metadata propagation, rename, default stamp

**Code.** `go/lib/restgw/metadata_propagation.go` — `MetadataPropagationMiddleware`, `MetadataDefaultStampMiddleware`; `go/lib/restgw/metadata_rename.go` — `MetadataRenameMiddleware`.

**Purpose.** Forwards a configured header set to the backend as gRPC metadata, renames headers
to other keys (for example `Accept-Language` → `w17-language`), and stamps defaults only where
nothing set the key.

**Configuration.** Lists generated from the surface definition; no environment. An empty list
returns `next` unchanged. `Host` travels as `x-forwarded-host`, `User-Agent` as
`x-forwarded-user-agent`.

**Shares.** i18n locale resolution (the last value of `w17-language` wins), E5,
`go/lib/principal`.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E10.1 | Absent or empty headers MUST produce no metadata | `TestMetadataPropagationMiddleware_NoHeadersPresent_NoMetadataAttached`, `TestMetadataRenameMiddleware_SkipsAbsentHeader` |
| E10.2 | Existing outgoing metadata MUST be preserved (append, never overwrite) | `TestMetadataPropagationMiddleware_PreservesIncomingMetadata` |
| E10.3 | The default stamp MUST write only keys nobody wrote | `TestMetadataDefaultStampMiddleware_SkipsAlreadySetKey` |
| E10.4 | Host and User-Agent MUST reach the backend under their forwarded names | `TestMetadataPropagationMiddleware_HostAndUserAgentTravelForwarded` |
| E10.5 | A gateway-owned key (`x-w17-*`, `w17-label-*`, `w17-tx-id`, `w17-conn-id`, `grpcgateway-*`, also in the `Grpc-Metadata-` form) MUST NOT be forwarded from a client header on any edge (REST, MCP, rpc gateway), including through a configured propagation, rename or default-stamp entry | `TestEdge_ClientCannotSetReservedMetadataOnAnySurface`, `TestMetadataMiddlewares_RefuseReservedKeysWithoutTheScrub`, `TestServeStreamableHTTP_ClientCannotSetReservedMetadata`, `TestCollectAuthHeaders_DropsReservedKeysFromEverySource`, `TestIsEdgeReservedKey`, `TestSanitizeGatewayMD_StripsTransactionKeys`, `TestHeaderName_IsGatewayOwned`, `TestConnIDHeader_IsGatewayOwned` |

**Deliberate limits.**
- Only the first header value is forwarded.

---

## E11. Request id, tracing, metrics

**Code.** `go/lib/restgw/request_id.go` — request id validation; `go/lib/restgw/otel.go` — `ObservabilityMiddleware`, `MountMetricsListener`.

**Purpose.** Attaches a request id (client-supplied if well-formed, else a UUIDv4), echoes it,
tags the OTel span, and serves Prometheus metrics on an optional side port.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `<PREFIX>_REQUEST_ID_HEADER` | `X-Request-ID` | — |
| `<PREFIX>_REQUEST_ID_DISABLED` | false | `true` (any case) disables the id; any other value leaves it on |
| `<PREFIX>_METRICS_PORT` | unset → no listener | A bind failure is logged and metrics are off |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E11.1 | A client id MUST be used only if it is 1–128 characters of `[A-Za-z0-9-_.:]`; otherwise it is replaced | `TestObservabilityMiddleware_RejectsMalformedRequestID_*` |
| E11.2 | The chosen id MUST be echoed in the response header | `TestObservabilityMiddleware_EchoesIncomingID`, `TestRequestIDMiddleware_PropagatesIncoming` |
| E11.3 | Disabled MUST mean no response header written and no context value; the request is passed on with a well-formed request-id header unchanged and a malformed, over-long or repeated one removed (the span is still created) | `TestObservabilityMiddleware_DisabledSkipsRequestID`, `TestRequestID_DisabledHandsOnOnlyAWellFormedHeader` |
| E11.4 | A metrics listener failure MUST NOT fail startup | `TestMountMetricsListener_UnsetAndBindError` |
| E11.5 | The request handed on MUST carry the chosen id as the only value of the request-id header, so header propagation (E10) forwards the validated or generated id, never the raw client value; the caller's header map MUST NOT be modified | `TestObservabilityMiddleware_BackendReceivesTheValidatedID` |

**Deliberate limits.**
- With the id disabled the gateway neither validates nor rewrites the request-id header; the
  fronting layer that injects ids owns its value, and header propagation forwards it as received.

---

## E12. CORS

**Code.** `go/lib/restgw/cors.go` — `CORSMiddleware`, `matchOrigin`.

**Purpose.** Opt-in CORS for browser frontends on another origin.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `<PREFIX>_CORS_ORIGINS` (CSV) | unset → middleware disabled | — |
| `<PREFIX>_CORS_METHODS` | `GET, POST, PUT, PATCH, DELETE, OPTIONS` | — |
| `<PREFIX>_CORS_HEADERS` | `Content-Type, Authorization, W17-Org` | — |
| `<PREFIX>_CORS_ALLOW_CREDENTIALS` | false | `1/true/yes/on` → true |
| `<PREFIX>_CORS_MAX_AGE` | 600 | `0` → header omitted; negative or invalid → 600 |

**Shares.** Preflight is answered before auth, rate limiting and routing.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E12.1 | `*` combined with credentials MUST be refused: credentials are dropped and a warning logged | `TestCORSMiddleware_Wildcard_WithCredentials_DropsCredentials` |
| E12.2 | Only exact origin matches MUST be echoed | `TestCORSMiddleware_SimpleRequest_DisallowedOrigin`, `TestMatchOrigin` |
| E12.3 | A preflight (OPTIONS + `Access-Control-Request-Method`) MUST answer 204 without calling `next`; a plain OPTIONS MUST pass through | `TestCORSMiddleware_Preflight`, `TestCORSMiddleware_PlainOptions_NotSwallowed` |
| E12.4 | The org header MUST be in the default allowed headers | `TestPreflightAdvertisesOrgScopeHeaderByDefault` |
| E12.5 | Every response the middleware handles MUST carry `Vary: Origin`: with no `Origin`, a disallowed one, a match, and preflights | `TestCORSMiddleware_EveryResponseVariesOnOrigin` |

**Deliberate limits.**
- Disallowed origins are still served; the browser enforces CORS.
- The WebSocket upgrade is governed by the library's same-origin check, not by this
  configuration.

---

## E13. HTTP panic recovery

**Code.** `go/lib/restgw/recover.go` — `RecoverPanicMiddleware`, `RecoverWSPump`.

**Purpose.** Converts a handler panic into a 500 envelope and an observability report, gives
each request its own error-reporting scope (`observx.WithRequestScope`), and protects WS pump
goroutines.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E13.1 | A panic MUST become 500 `INTERNAL` / "internal error"; the panic value MUST NOT appear in the body | `TestRecoverPanicMiddleware_CatchesPanic` |
| E13.2 | `http.ErrAbortHandler` MUST be re-panicked | `TestRecoverPanicMiddleware_RepanicsAbortHandler` |
| E13.3 | A pump panic MUST close the socket with `StatusInternalError` instead of crashing the process | `TestRecoverWSPump_NilContextFallback` (partial: nil-context arm only) |
| E13.4 | Each request MUST run with its own reporting scope, so breadcrumbs recorded by one request never reach another request's report | no test at this layer (the scope itself: `TestRequestScope_KeepsBreadcrumbsPerRequest` in `go/core/observx`) |

**Deliberate limits.**
- A panic after headers were sent (for example mid-SSE) appends a JSON envelope to the stream.

---

## E14. Per-RPC SSE helpers

**Code.** `go/lib/restgw/streaming.go` — `WriteSSEHeaders`, `WriteSSEEvent`, `WriteSSEEventWithTimeout`, `WriteSSEGRPCErrorCtx`.

**Purpose.** Server-stream RPCs over Server-Sent Events: headers, one `data:` frame per message,
error frames carrying the same envelope as unary responses, per-write deadlines.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `<PREFIX>_STREAM_WRITE_TIMEOUT_SECONDS` | 30 s | Unparseable → 30 s; `<= 0` → no deadline |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E14.1 | Headers MUST be flushed before the first event; a writer without `http.Flusher` is reported (false) | `TestWriteSSEHeaders_AndEvent`, `TestWriteSSEHeaders_NoFlusher` |
| E14.2 | Each event MUST be exactly one `data: <single-line json>\n\n` frame | `TestWriteSSEEvent_MultipleFrames` |
| E14.3 | A backend status mid-stream MUST become `event: error` with the E3 envelope including `details[]`; a non-status error MUST become a generic INTERNAL frame | `TestWriteSSEGRPCErrorCtx_ForwardsDetails`, `TestWriteSSEGRPCError_NonStatusInternal` |
| E14.4 | With a timeout, each write MUST have a deadline that is cleared afterwards; writers without deadline support fall back to none | `streaming_timeout_test.go` (all), `TestWriteSSEEventWithTimeout_DeadlineInstalled` |
| E14.5 | A half-open client MUST NOT pin a goroutine when the `WithTimeout` variants are used | covered by E14.4 tests |

**Deliberate limits.**
- Per-RPC streams have no heartbeat (only `w17-events` has one); idle streams can be cut by
  proxies.
- A client disconnect is noticed only through request-context cancellation or a failed write.

---

## E15. WebSocket helpers

**Code.** `go/lib/restgw/streaming.go` — `AcceptWebSocket`, `WSReadProto`, `WSReadProtoOrEOF`, `WSWriteProtoWithTimeout`, `WSWriteGRPCError`, `WSWatchClientClose`; `go/lib/restgw/json.go` — frame discriminator.

**Purpose.** Server-, client- and bidi-stream RPCs over WebSocket with negotiated JSON or
protobuf frames, a mandatory JSON frame discriminator, a client-stream half-close sentinel, and
close detection for write-only sockets.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| Subprotocol | JSON | `w17.pb` → protobuf, `w17.json` → JSON |
| Write timeout | from `<PREFIX>_STREAM_WRITE_TIMEOUT_SECONDS` (E14) | — |
| `MaxWebSocketMessageBytes` (package var) | 0 → same as `MaxRequestBodyBytes` (10 MiB) | Zero or negative → `MaxRequestBodyBytes`; process-global, set before serving |
| Close reason | the client sentence | Cut on a UTF-8 boundary with an ellipsis to fit the 123-byte close-frame limit |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E15.1 | Every JSON frame the server writes MUST carry `"w17":"d"` (data) or `"w17":"e"` (error); inbound JSON frames without the discriminator MUST be refused | `ws_frame_discriminator_test.go` (all) |
| E15.2 | `{"w17":"f"}` as a TEXT frame MUST be the client-stream EOF regardless of wire format | `TestWSReadProtoOrEOF_Sentinel`, `TestWSClientStreamEOFIsDiscriminated` |
| E15.3 | Protobuf mode MUST require binary frames, JSON mode text frames | `TestWSReadProto_ExpectedBinaryGotText` |
| E15.4 | A graceful client close on a write-only socket MUST cancel the stream context | `TestWSWatchClientClose_GracefulCloseReleasesTheStream` |
| E15.5 | Error frames MUST forward `details[]` and close the socket | `TestWSWriteGRPCError_ForwardsFieldViolations` |
| E15.6 | Each write MUST be bounded by the timeout when the `WithTimeout` variant is used | `TestWSWriteProtoWithTimeout_PassesTimeoutThrough` |
| E15.7 | One inbound message MUST be accepted up to `MaxWebSocketMessageBytes`, by default the REST body cap, so a frame that is a valid REST body is a valid stream message | `TestAcceptWebSocket_ReadLimitIsTheRequestBodyCap`, `TestAcceptWebSocket_ReadLimitIsConfigurable` |
| E15.8 | An error close MUST always send a close frame (1008 for a refusal): the close reason is cut to fit and stays valid UTF-8, and the full sentence arrives in the error frame | `TestWSWriteErrorWithDetails_LongSentenceStillClosesCleanly`, `TestWSCloseReason_FitsAndStaysValidUTF8` |

**Deliberate limits.**
- Cross-origin browser clients are refused by the library's same-origin check.

---

## E16. Browser stream auth: tickets

**Code.** `go/lib/restgw/ws_auth.go` — `NewWSAuth`, `WSAuthConfig`, `WSAuthTicketPrincipal`, `MaxTicketAuthorizationBytes`; `go/lib/restgw/ticket.go` — `MemoryTicketStore`, `NewMemoryTicketStore`, `WithMaxTickets`, `WithMaxTicketBytes`, `ErrTicketStoreFull`, `NewTicketIssuer`, `TicketIssuerConfig`.

**Purpose.** Browsers cannot set `Authorization` on `EventSource` or `WebSocket`; they trade it
for a one-shot ticket and pass `?ticket=`.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `WSAuthConfig.Modes` | `header` | `ticket` or both; an unknown mode, nil `Inner`, or ticket mode without a store → panic at construction |
| `TicketParam` | `ticket` | — |
| `TicketIssuerConfig.TTL` | 30 s | — |
| `MemoryTicketStore` janitor | every 60 s | A store at a limit also sweeps expired tickets itself before refusing |
| `WithMaxTickets(n)` | `DefaultMaxTickets` = 10 000 outstanding tickets | `n <= 0` keeps the default |
| `WithMaxTicketBytes(n)` | `DefaultMaxTicketBytes` = 16 MiB (ticket id, label keys and values, plus a fixed per-entry overhead) | `n <= 0` keeps the default |
| `MaxTicketAuthorizationBytes` | 8 KiB, fixed | A longer `Authorization` value is refused by `WSAuthTicketPrincipal` with `ErrWSAuthTicketHeaderTooLarge` |
| Issuer method | POST | Anything else → 405 with `Allow`, code `UNIMPLEMENTED` |
| Issuer statuses | 200 with the ticket | Principal failure → 401; oversized `Authorization` → 431 with code `INVALID_ARGUMENT`; full store → 503 with `Retry-After: 1`; any other store failure → 500. Every refusal is the JSON error envelope (E3) |

**Shares.** E5 (`RequireAuth`), E17 (the promoted header keeps re-auth working), E6 (the
promotion marker header forces a cache bypass on probes).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E16.1 | Tickets MUST be one-shot and expire; unknown, expired and spent tickets MUST be indistinguishable | `TestTicket_OneShot`, `TestTicket_TTLExpiry`, `TestTicket_UnknownInvalid` |
| E16.2 | Ticket-only mode MUST refuse a request without a ticket even if it carries `Authorization` | `TestWSAuth_TicketOnly_NoTicket_FailsFast`, `TestWSAuth_TicketOnlyStillRefusesAClientSuppliedBearer` |
| E16.3 | In header-and-ticket mode a header MUST win | `TestWSAuth_HeaderAndTicket_HeaderWins` |
| E16.4 | A redeemed ticket MUST be promoted to `Authorization` on the caller's request so later re-auth probes work, in both modes | `TestWatchStreamAuth_TicketStreamSurvivesTheTick`, `TestWatchStreamAuth_TicketOnlyModeSurvivesTheTick` |
| E16.5 | Issuer responses MUST be `no-store` with `Vary: Authorization, Cookie` | `TestTicketIssuer_HappyPOST` (`no-store`, and `Vary` present; its value has no test) |
| E16.6 | A client MUST NOT be able to present the promotion marker (a per-instance 256-bit random value) | `TestWSAuth_TicketOnly_ClientCannotPresentThePromotionMarker`, `TestWSAuth_PromotionMarkerIsPerInstance`, `TestWSAuth_TicketOnlyStillRefusesAClientSuppliedBearer` |
| E16.7 | The default store MUST NOT grow memory without bound: outstanding tickets and the bytes they hold are both capped, and a redeemed or expired ticket frees its share | `TestTicketIssuer_FloodIsBounded`, `TestTicketStore_ByteBudget`, `TestTicketStore_RedeemFreesCapacity`, `TestMemoryTicketStore_FullStoreSweepsExpired` |
| E16.8 | A full store MUST refuse a new ticket with `ErrTicketStoreFull`, answered by the issuer as 503 with `Retry-After`; it MUST NOT evict a ticket already handed out | `TestTicketIssuer_FloodIsBounded`, `TestTicketStore_RedeemFreesCapacity` |
| E16.9 | An `Authorization` value longer than `MaxTicketAuthorizationBytes` MUST be answered 431 and store nothing; a value at the limit is accepted | `TestTicketIssuer_OversizedAuthorization` |
| E16.10 | Every issuer refusal MUST be the JSON error envelope with a catalog sentence; the detail goes to observability | `TestTicketIssuer_ErrorsAreTheEnvelope` |

**Deliberate limits.**
- `MemoryTicketStore` is per process: issuer and redeemer must be the same replica.
- `WSAuthTicketPrincipal` records only `Authorization`; cookie-only sessions cannot obtain
  tickets.
- The issuer does not validate the credential (redeem does), so tickets are minted for any
  request that carries one; the store limits, not validation, bound what that costs.
- A full store delays new tickets until redeems or the TTL free room; it does not make room by
  evicting.

---

## E17. Stream re-authentication watchdog

**Code.** `go/lib/restgw/stream_reauth.go` — `WatchStreamAuth`.

**Purpose.** Re-runs the surface auth function against the stream's original request every
interval and cancels the stream context when auth definitely refuses. A definite refusal is
`UNAUTHENTICATED` or `PERMISSION_DENIED` from the auth method, or the ticket sentinels
`ErrTicketInvalid` / `ErrWSAuthNoTicket`. Any other result (a timeout, `UNAVAILABLE`, a non-status
error) is "no verdict". After each answered probe the watchdog publishes the latest verdict on
the stream context, which the `w17-events` hub reads (E18).

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `<PREFIX>_STREAM_REAUTH_SECONDS` | 5 min | Unparseable → 5 min; `<= 0` → off |
| Probe timeout | 10 s | Fixed |
| Retry after "no verdict" | 5 s, growing up to the interval | Fixed |
| Report of "no verdict" | exception channel, at most once per minute per process | Fixed |
| `authFn == nil` or `r == nil` | — | Off |

**Shares.** E6 (a cached verdict delays revocation), E7, E16, E18.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E17.1 | A definite refusal MUST cancel the returned context | `TestWatchStreamAuth_CancelsWhenAuthStartsFailing`, `TestWatchStreamAuth_DefiniteRefusalsCancel` |
| E17.2 | A probe that times out MUST NOT be treated as revocation | `TestWatchStreamAuth_ProbeTimeoutIsNotARevocation` |
| E17.3 | The probe MUST be independent of the stream's own cancellation | `TestWatchStreamAuth_ProbeDoesNotInheritStreamCancellation` |
| E17.4 | Disabled MUST return the input context and a no-op stop | `TestWatchStreamAuth_DisabledIsAPassThrough`, `TestWatchStreamAuth_NilAuthIsAPassThrough` |
| E17.5 | The watchdog goroutine MUST exit when the stream ends (stop or parent cancel) | `TestWatchStreamAuth_EndsWithTheStream` |
| E17.6 | An auth-backend outage of any kind MUST NOT disconnect every stream: only a definite refusal revokes | `TestWatchStreamAuth_FastBackendFailureIsNotARevocation`, `TestW17Events_AuthOutageKeepsTheStream` |
| E17.7 | A probe without a verdict MUST be reported (throttled to one report per minute per process) and retried with a backoff from 5 s up to the interval | `TestReportUnansweredReauth_ReportedAndThrottled`, `TestStreamReauthRetryDelay` |
| E17.8 | A revocation during an outage MUST take effect at the first answer after the backend recovers | `TestWatchStreamAuth_RevocationLandsAfterTheOutage` |

**Deliberate limits.**
- An auth function revokes only by returning `UNAUTHENTICATED` or `PERMISSION_DENIED` (or a
  ticket sentinel); a plain error is read as "no verdict". Generated auth functions return those
  codes; a hand-written one must too.

---

## E18. The reserved `w17-events` channel

**Code.** `go/lib/restgw/w17events.go` — `HandleW17Events`; `go/lib/restgw/w17events_eventbus.go` — `EventbusEventSource`.

**Purpose.** One SSE endpoint per surface that fans out bus events by topic, with per-principal
label isolation.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `?topics=A,B` | — | Empty → heartbeat only |
| Heartbeat argument | 30 s | `<= 0` → 30 s |
| Per-write deadline | 30 s | Fixed |
| Per-client queue | 64 frames | A full queue drops frames |
| Subscribe failure | 503 `UNAVAILABLE` | JSON error envelope, sent before any SSE header |
| Channels, transcode, principal-labels functions | from generated main | — |

**Shares.** `go/lib/eventbus` subscribers; E5 and E16 (`RequireAuth` and tickets);
`go/core/lifecycle` drain signal; `go/lib/principal` labels.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E18.1 | Only subscribed topics MUST be delivered | `TestHandleW17Events_FramesFilteredByTopic`, `TestEventbusEventSource_ForwardsMatchingTopic` |
| E18.2 | With isolation on (a labels deriver set), an unlabelled event MUST reach nobody and a labelled event MUST reach only principals holding every label | `TestEventbusEventSource_LabelFiltering`, `TestEventbusEventSource_UnlabelledEventNoCrossTenant`, `TestEventbusEventSource_UnlabelledEventDeniedToUnresolvedPrincipal` |
| E18.3 | There MUST be one bus subscription per channel regardless of client count | `TestEventbusEventSource_SharedHubSingleSubscriber` |
| E18.4 | A failed lazy open MUST NOT wedge the hub and MUST drain what it opened | `w17events_ensureopen_test.go` (all four) |
| E18.5 | A slow client MAY lose frames but MUST NOT block other clients | `TestEventbusEventSource_SlowClientLosesOnlyItsOwnFrames` |
| E18.6 | The handler MUST return on client disconnect, on drain, and on a write error or deadline | `w17events_drain_test.go` (both), `TestHandleW17Events_CtxCancelEndsHandler`, `TestHandleW17Events_StalledClientEndsWithinTheWriteDeadline` |
| E18.7 | Every replica MUST receive every event | `w17events_replicas_test.go` |
| E18.8 | A subscribe failure MUST NOT reveal internal detail to the client: it answers 503 `UNAVAILABLE` in the envelope, without broker channel names or broker error text | `TestHandleW17Events_SubscribeError`, `TestHandleW17Events_NoSource` |
| E18.9 | A revoked credential or removed membership MUST stop delivery on an open connection: under the re-auth watchdog (E17), a refusal ends the stream, and `EventbusEventSource` ends a client whose labels, derived again from the latest verdict, differ from those at connect | `TestW17Events_RevokedCredentialEndsTheStream`, `TestW17Events_ChangedMembershipEndsTheStream` |
| E18.10 | A verdict with the same labels MUST keep the stream, even when the auth response bytes differ | `TestW17Events_SameLabelsKeepTheStream` |
| E18.11 | An envelope the hub cannot transcode MUST be logged with channel, topic and cause and reported; the event is dropped | `TestEventbusEventSource_TranscodeErrorIsLogged` |

**Deliberate limits.**
- E18.9 holds only when the route runs `HandleW17Events` on the context `WatchStreamAuth`
  returns; generated code must mount it that way.
- Only `EventbusEventSource` acts on changed labels; a custom `EventSource` sees revocation only.
- A client ended for changed labels reconnects and is admitted on its current principal; frames
  are never filtered by a mix of old and new labels.

---

## E19. Public-method rate-limit adapters

**Code.** `go/lib/restgw/publiclimit.go` — `AdmitPublic`, `AdmitPublicStream`, `WriteTooManyRequests`, `WriteTooManyRequestsCtx`.

**Purpose.** Applies the per-client limiter (E26) to REST methods excluded from auth: a unary
method holds a token while it runs and keeps it charged only on failure (status ≥ 400, or nothing
written); streams are charged on every open.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| Limiter | from generated main | nil admits everything |
| `Retry-After` | 60 | Fixed |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E19.1 | A drained bucket MUST answer 429 `RESOURCE_EXHAUSTED` before the handler runs, with the catalog sentence in the request's locale | `TestAdmitPublic_ChargesFailuresPerClient`, `TestAdmitPublic_RefusalIsTheCatalogSentence` |
| E19.2 | Only responses with status ≥ 400, and handlers that wrote nothing, MUST stay charged; a success (including an implicit 200) MUST return its token | `TestAdmitPublic_ImplicitOKAndUnwrap`, `TestAdmitPublic_SuccessReturnsItsToken`, `TestAdmitPublic_NothingWrittenIsCharged` |
| E19.3 | Each stream open MUST be charged; the writer MUST NOT be wrapped (flush and hijack keep working) | `TestAdmitPublicStream_ChargesEveryStream` |
| E19.4 | Concurrent requests MUST NOT exceed the configured budget: each request in flight holds its own token | `TestAdmitPublic_ConcurrentFailuresStayWithinTheBurst` |

---

## E20. Multipart uploads

**Code.** `go/lib/restgw/upload.go` — `ProcessFilePart`, `FilePartConfig`, `WriteUploadError`, `WriteUploadErrorCtx`, `RollbackUploads`.

**Purpose.** Streams one multipart part to a staging file with a per-file size cap and an
extension allowlist, publishes it to a KV driver under a key of its own (the body's sha256 plus a
random 128-bit suffix), and rolls back on a later failure.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `FormName`, `Driver` | — | Required |
| `TmpDir` | driver's staging dir, else `os.TempDir()` | — |
| `BucketPath`, `ExpectedObjects`, `MaxPerBucket` | — | — |
| `MaxSizeBytes` | — | `0` = unlimited (generated code never uses 0) |
| `AllowedExtensions` | — | `*` = any; empty = reject all |

**Shares.** `go/lib/kvfs` (`BuildKey`, drivers); E21 (downloads read the same keys). The
request-wide body cap is the generated prelude's `http.MaxBytesReader`, not this package.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E20.1 | A part over `MaxSizeBytes` MUST be refused with `ErrUploadTooLarge` and its staging file removed | `TestProcessFilePart_TooLarge` |
| E20.2 | A missing part MUST give `ErrFilePartMissing`; a wrong or absent extension `ErrUploadBadExt` | `TestProcessFilePart_MissingPart`, `TestProcessFilePart_BadExtension`, `TestProcessFilePart_NoExtensionRejected` |
| E20.3 | Every upload MUST be stored under its own object key, `<sha256>-<random 128-bit suffix>`; identical bodies MUST get distinct objects | `TestProcessFilePart_IdenticalBodiesGetDistinctObjects` |
| E20.4 | The staging file MUST be removed after a copy-style publish | `TestProcessFilePart_RemovesTempFileOnCopyDriver` |
| E20.5 | Non-sentinel errors MUST be logged and answered as a generic 500 | `TestWriteUploadError_MasksInternalError` |
| E20.6 | `RollbackUploads` MUST be best-effort and idempotent | `TestRollbackUploads_BestEffortAndIdempotent` |
| E20.7 | An unbounded request body MUST NOT be spilled to disk (relies on the generated `MaxBytesReader`) | no test |
| E20.8 | Rollback MUST NOT delete an object another request references, including one with identical bytes that committed earlier or is still in flight | `TestRollbackUploads_KeepsAnotherRequestsIdenticalObject`, `TestRollbackUploads_ConcurrentIdenticalUploads` |
| E20.9 | Upload refusals MUST NOT show developer prose or configuration to the client: each sentinel answers one of the upload msgids (`MsgidUploadMissing`, `MsgidUploadTooLarge`, `MsgidUploadBadType`), and the allowlist and size limit go to the operator channel | `TestWriteUploadErrorCtx_RefusalsAreCatalogSentences`, `TestVocabulary_CarriesTheUploadSentences` |

**Deliberate limits.**
- Content is not sniffed; only the filename extension is checked.
- Uploads are not deduplicated: identical bodies are stored once per upload.

---

## E21. Downloads

**Code.** `go/lib/restgw/download.go` — `NewDownloadConfig`, `ServeDownload`, `BuildDownloadURL`.

**Purpose.** Serves stored objects by URL with longest-prefix bucket matching, ETag, ranges and a
forced non-renderable content type.

**Configuration.** `NewDownloadConfig(prefix, fields, cacheControl, asAttachment, disableETag,
disableRange)`.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E21.1 | Any `.` or `..` segment (either slash) MUST answer 404 | `TestServeDownload_PathTraversal404`, `TestServeDownload_BackslashTraversal404` |
| E21.2 | An unknown bucket and a missing object MUST both answer 404 | `TestServeDownload_UnknownBucket_404`, `TestServeDownload_NotFound` |
| E21.3 | Responses MUST always be `Content-Type: application/octet-stream` with `nosniff` | `TestServeDownload_NoContentSniffXSS` |
| E21.4 | Driver errors MUST become a generic, logged 500 | `TestServeDownload_StatError500`, `TestServeDownload_OpenErrors` |
| E21.5 | URLs from `BuildDownloadURL` MUST resolve through `ServeDownload` | `TestDownloadURLRoundTrip` |
| E21.6 | Only GET and HEAD MUST be served | `TestServeDownload_DELETE_405`, `TestServeDownload_HEAD` |
| E21.7 | Every refusal, 404 included, MUST be the JSON error envelope; the detail goes to observability | `TestServeDownload_ErrorsAreTheEnvelope` |

**Deliberate limits.**
- Authorization is per endpoint, not per object.

---

## E22. Browser redirects

**Code.** `go/lib/restgw/browserredirect.go` — `WriteBrowserRedirect`, `WriteBrowserRedirectExternal`, `IsSiteRelativeRedirect`.

**Purpose.** Redirects a browser (for example after sign-in), optionally with a token placed in
the URL fragment, never the query.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E22.1 | Site-only mode MUST accept only `/path` (not `//`, `/\`, `://`, control characters) | `TestIsSiteRelativeRedirect`, `TestWriteBrowserRedirect_RefusesATargetThatLeavesTheSite` |
| E22.2 | External mode MUST accept only absolute `http(s)` URLs | `TestWriteBrowserRedirectExternal_StillRefusesAScriptTarget` |
| E22.3 | The token MUST travel in the fragment and the response MUST be `no-store` | `TestWriteBrowserRedirect_SendsTheBrowserOnWithTheTokenInTheFragment`, `TestWriteBrowserRedirect_IsNotCacheable` |

**Deliberate limits.**
- External mode allows plain `http`.

---

## E23. Pagination links

**Code.** `go/lib/restgw/pagination.go`.

**Purpose.** Builds `Link: <…>; rel="next"|"prev"` headers from the current URL, as
origin-relative references (`</path?cursor=…>`, RFC 8288 §3.2).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E23.1 | Existing query parameters MUST be kept and `cursor` replaced | `pagination_test.go` (all), `TestNextPageURL` |
| E23.2 | Link targets MUST be origin-relative: `Host`, `X-Forwarded-Proto` and the connection's TLS state MUST NOT shape them | `TestSetNextPageLink_IgnoresCallerSuppliedOrigin`, `TestSetNextPageLink_IgnoresXForwardedProto`, `TestSetNextPageLink_TLSStaysRelative`, `TestSetNextPageLink_BasicHeader` |
| E23.3 | A link target MUST stay on the origin whatever the path: a leading `//` is written as `/.//` and a backslash is percent-encoded | `TestSetNextPageLink_PathNeverReadsAsAHost` |

**Deliberate limits.**
- Clients resolve the reference against the request URL (as browsers and RFC 8288 clients do);
  a client that needs an absolute URL builds it itself.

---

## E24. Query-string enum parsing

**Code.** `go/lib/restgw/queryenum.go` — `SetEnumField`.

**Purpose.** Accepts an enum query parameter by name or number.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E24.1 | A name or declared number MUST be accepted; anything else MUST be an error listing the accepted values | `queryenum_test.go` (all) |

**Deliberate limits.**
- The error text names the proto enum's full name; callers must route it through
  `WriteBadInput` (operator-only). The SDK does not enforce this.

---

## E25. Health handler

**Code.** `go/lib/restgw` — `HealthHandler`.

`HealthHandler()` delegates to `go/core/healthz` (see the HTTP liveness endpoint in
[server.md](server.md)).

| ID | Guarantee | Test |
|---|---|---|
| E25.1 | The gateway health route MUST answer as `core/healthz` does | `health_test.go` (both) |

---

## E26. Per-client rate limiter

**Code.** `go/lib/ratelimit/ratelimit.go` — `Limiter`, `FromEnv`, `FromEnvScope`, `Allow`, `Reserve`, `ReserveRequest`, `AllowChargeable`, `TrustPrivateRanges`.

**Purpose.** A token bucket per client address for methods that take no credential, so guessing
is bounded without a global cap.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `<PREFIX>_RATELIMIT_PER_MINUTE` | 30 | float; `<= 0` disables |
| `<PREFIX>_RATELIMIT_BURST` | 10 | int > 0 |
| `<PREFIX>_RATELIMIT_TRUSTED_PROXIES` | none: every caller is keyed by its connection's address | Comma-separated CIDRs or bare addresses; `private` adds the loopback and RFC 1918 ranges (combinable with CIDRs); `none` states the default; any other entry is logged once and skipped. List exactly the hops that terminate HTTP and append to `X-Forwarded-For` |
| `FromEnvScope(prefix, scope, transport)` | `<PREFIX>_<SCOPE>_RATELIMIT_*` | The unscoped `<PREFIX>_RATELIMIT_*` form is read with a one-time warning; `_<TRANSPORT>_PER_MINUTE` / `_BURST` override per transport; an off limiter is logged once |
| `TTL` | 10 min | Struct field only |
| `MaxKeys` | 10 000 | Struct field only; reaching it evicts the least recently seen bucket |
| Malformed `PER_MINUTE` / `BURST` | — | Ignored silently; the previous value stands |

**Shares.** E19 (REST adapters); the gRPC transport through `Allow(ctx)` (`peer` and
`x-forwarded-for` metadata); admin sign-in.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E26.1 | Unset environment MUST mean on, at the defaults | `TestFromEnvDefaultsAreOn` |
| E26.2 | Burst then deny; keys MUST be independent | `TestBurstThenDeny`, `TestKeysAreIndependent` |
| E26.3 | `X-Forwarded-For` MUST be believed only from a configured proxy and read right-to-left, taking the first untrusted hop; a hop that is not an address MUST end the walk; an IPv4-mapped peer MUST match IPv4 prefixes | `TestForwardedIgnoredFromUntrustedPeer`, `TestForwardedPicksRightmostUntrusted`, `TestRequestKey_ForwardedRules`, `TestForwardedUnparseableHopStopsTheWalk`, `TestTrustedProxyMatchesMappedPeer` |
| E26.4 | IPv6 MUST be bucketed per /64 | `TestLimiter_IPv6BucketsByPrefixNotByAddress` |
| E26.5 | A trusted peer that forwarded no address MUST log a collapse warning (at most hourly) and keep limiting | `TestLimiter_WarnsWhenEveryCallerCollapsesIntoOneBucket` |
| E26.6 | The key count MUST stay ≤ `MaxKeys`; eviction MUST remove the least recently seen bucket (an idle one whenever one exists) and never the bucket just created | `TestEvictionRespectsMaxKeys`, `TestLimiter_EvictsTheLeastRecentlySeen`, `TestLimiter_EvictsIdleKeysFirst` |
| E26.7 | `AllowChargeable` MUST take the token at admission and give it back when the call's context ends without `charge`; `charge` MUST be idempotent; a nil limiter admits | `TestAllowChargeable_ChargesOnlyFailures`, `TestAllowChargeable_ChargeIsIdempotentAndNilSafe`, `TestAllowChargeable_RefundFollowsTheContext`, `TestAllowChargeableRequest_ChargesOnlyFailures` |
| E26.8 | An unparseable peer MUST share one `unknown` bucket | `TestUnknownPeerSharesOneBucket` |
| E26.9 | A client MUST NOT be able to choose its own bucket: with the default configuration a forwarded header is ignored, whatever the peer's range | `TestForwardedFromUnconfiguredPrivatePeerIsIgnored`, `TestForwardedIgnoredFromUntrustedPeer` |
| E26.10 | Concurrency MUST NOT multiply the per-client budget: every attempt in flight holds its own token | `TestChargeable_ConcurrentFailuresStayWithinTheBudget`, `TestAdmitPublic_ConcurrentFailuresStayWithinTheBurst` |
| E26.11 | The limiter MUST NOT become a CPU sink under a stream of fresh keys: eviction at the ceiling is constant time (buckets sit on a recency list) | `TestLimiter_FreshKeysAtTheCeilingStayCheap` (with `BenchmarkLimiter_FreshKeyAtTheCeiling`, `BenchmarkLimiter_KnownKey`) |
| E26.12 | Unset `TRUSTED_PROXIES` MUST trust no proxy; `private` MUST trust the loopback and private ranges only when set, together with any listed CIDRs; a bare address MUST trust exactly that host | `TestFromEnvDefaultsAreOn`, `TestTrustedProxiesPrivateKeywordIsOptIn` |
| E26.13 | A private-range peer that sends `X-Forwarded-For` without being a configured proxy MUST be keyed by its own address and produce a warning (at most hourly) naming `_RATELIMIT_TRUSTED_PROXIES` | `TestForwardedFromUnconfiguredPrivatePeerIsIgnored` (the hourly throttle has no test) |
| E26.14 | `Reserve` MUST take the token at admission; `done(false)` MUST give it back, `done(true)` MUST keep it, and only the first call counts | `TestReserve_SettlesOnce` |

**Deliberate limits.**
- The limiter is in-process: N replicas give N times the budget.
- Behind a terminator that is not configured as a trusted proxy, every client shares the
  terminator's bucket; the warning (E26.13) names the setting.
- `AllowChargeable` cannot hear a success, so a call whose context never ends keeps its token
  charged; `Reserve` settles explicitly.

---

## E27. MCP server and transports

**Code.** `go/lib/mcp/server.go` — `NewServer`, `ServeStreamableHTTP`, `CallUnary`, `RecoverTool`; `go/lib/mcp/errors.go`; `go/lib/mcp/bodylimit.go` — `SetMaxRequestBodyBytes`.

**Purpose.** Runs generated MCP tool proxies over stdio or streamable HTTP; each tool call is a
unary gRPC call using the w17 JSON dialect.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `W17_MCP_TRANSPORT` | `stdio` | `http` / `streamable` / `streamable_http` → HTTP; anything else → error |
| `W17_MCP_HTTP_ADDR` | `:8081` | — |
| Endpoint path | `/mcp` | Health at the `core/healthz` path |
| `ReadHeaderTimeout` | 10 s | — |
| Shutdown timeout | `W17_MCP_SHUTDOWN_TIMEOUT_SECONDS` via `lifecycle.ShutdownTimeoutFromEnv` | — |
| Request body cap | `SetMaxRequestBodyBytes(n)`, else `W17_MCP_MAX_BODY_BYTES`, else 10 MiB (`DefaultMaxRequestBodyBytes`, the REST default) | `n <= 0` restores the default; a non-numeric or non-positive variable is ignored. A declared `Content-Length` over the cap → 413 with a JSON-RPC error; a body without one is cut at the cap |

**Shares.** `protojsonx` (E29), `go/core/lifecycle` drain, `go/core/healthz`, `observx`;
`restgw.ClientError` (E3) for tool errors.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E27.1 | A tool panic MUST become an error result; the process survives | `TestRecoverTool_PanicBecomesErrorResult` |
| E27.2 | Tool results MUST use proto names, integer enums and collapsed oneofs | `TestCallUnary_DecodesArgsAndMergesMetadata` (partial: integer enums have no test) |
| E27.3 | Unknown argument fields MUST be ignored | `TestUnmarshalProto_EmptyAndDecode` |
| E27.4 | The long-lived GET stream MUST end at shutdown start; POST and DELETE keep the drain budget | `serve_shutdown_test.go` (both) |
| E27.5 | The health route MUST be answered outside MCP auth | `healthz_test.go` |
| E27.6 | Tool and auth errors MUST NOT return backend or developer prose to the client: no backend status message, auth-backend text, resolver error or decode detail | `TestCallUnary_BackendErrorIsSanitised`, `TestCallUnary_AuthBackendErrorIsSanitised`, `TestCallUnary_NonStatusErrorIsInternal`, `TestCallUnary_DecodeError`, `TestCallUnary_GateResolverError` |
| E27.7 | The HTTP transport MUST NOT read unbounded request bodies: a declared length over the cap MUST be refused with 413 before the handler runs, and an undeclared body MUST stop at the cap | `TestLimitBody`, `TestMaxBodyBytes_DefaultAndOverrides`, `TestServeStreamableHTTP_RefusesOversizedBody` |
| E27.8 | Every `CallUnary` error MUST be a gRPC status carrying the code, the sentence a REST client reads for the same failure, and its field details; a non-status error MUST read as `INTERNAL` | `TestCallUnary_BackendErrorIsSanitised`, `TestCallUnary_PermissionDeniedIsAStatus`, `TestCallUnary_NonStatusErrorIsInternal` |
| E27.9 | Tool arguments MUST keep 64-bit integers exact over HTTP and stdio: the raw arguments are captured before the MCP library decodes them and handed to `CallUnary`; a refused call leaves no captured arguments behind | `TestServeStreamableHTTP_ToolArgumentsKeepSixtyFourBitNumbers`, `TestHandleMessage_ScopedContextKeepsSixtyFourBitNumbers`, `TestHandleMessage_RefusedCallLeavesNoCapture` |
| E27.10 | A backend error MUST be reported by code as E3.7: refusals to the non-exception channel, faults to the exception channel | `TestCallUnary_BackendErrorsAreReportedByCode` |
| E27.11 | Each tool call MUST run with its own reporting scope (`observx.WithRequestScope`) | no test at this layer (the scope itself: `TestRequestScope_KeepsBreadcrumbsPerRequest` in `go/core/observx`) |

**Deliberate limits.**
- Unary tools only.

---

## E28. MCP auth translation and permission gating

**Code.** `go/lib/mcp/auth.go` — `DefaultAuthFunc`, `CollectAuthHeaders`, `SetAuthEnvPrefix`; `go/lib/mcp/server.go` — `SetPermResolver`, `SetToolPerm`, `SetToolPerms`.

**Purpose.** Turns connection identity (HTTP headers, or the environment on stdio) into backend
metadata, and hides or refuses tools whose permission ids the caller's token does not cover.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `NewServer(name, version, auth)` | — | nil auth → empty metadata |
| `DefaultAuthFunc(keys...)` | — | Resolves each key from the connection's identity; gateway-owned keys dropped. Falls back to the process environment on stdio only |
| `SetAuthEnvPrefix(prefix)` | — | Environment prefix for the stdio identity; not attached to HTTP connections |
| `SetPermResolver(r)` | nil → no gating | — |
| `SetToolPerm(s)` | — | AND of all ids; `0` ignored |

**Shares.** `principal.IsGatewayOwnedKey`; the backend auth service.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E28.1 | With a resolver, `tools/list` MUST show a gated tool only if all its ids are held; a resolver error or panic MUST hide every gated tool | `TestFilterTools_GatedByPerms`, `TestFilterTools_AuthError_FailsClosed`, `TestResolvePermsSafe_PanicFailsClosed`, `TestSetToolPerms_ListHidesToolMissingAnyPerm` |
| E28.2 | `tools/call` MUST re-check permissions before decoding arguments or dialling | `TestCallUnary_Gate_DeniesWithoutPerm`, `TestCallUnary_Gate_DeniesBeforeDecode`, `TestSetToolPerms_CallRequiresEveryPerm` |
| E28.3 | `x-w17-*` and `w17-label-*` keys MUST NOT be forwarded by `DefaultAuthFunc` | `TestDefaultAuthFunc_DropsGatewayOwnedNamespaces` |
| E28.4 | HTTP headers MUST outrank the environment | `TestDefaultAuthFunc_HTTPWinsOverEnv`, `TestCollectAuthHeaders_Precedence` |
| E28.5 | Over HTTP, a caller's identity MUST come from that request only, never from the process environment; a caller that sends no credential has none | `TestConnInfo_HTTPCallerGetsNoEnvIdentity`, `TestDefaultAuthFunc_HTTPHasNoEnvFallback`, `TestServeStreamableHTTP_AnonymousCallerGetsNoEnvIdentity`, `TestCollectAuthHeaders_MergesAllSources` |
| E28.6 | Over HTTP no environment value MUST be forwarded to the auth backend; on stdio only `<PREFIX>_*` variables are | `TestConnInfo_HTTPCallerGetsNoEnvIdentity`, `TestServeStreamableHTTP_AnonymousCallerGetsNoEnvIdentity`, `TestSetAuthEnvPrefix_ScopesAndStrips` |

**Deliberate limits.**
- With no resolver every tool is open, even when permissions are recorded.
- On stdio every `<PREFIX>_*` variable is forwarded as a header; keep that prefix for identity
  values only.
- The resolver runs on every `tools/list` and every gated `tools/call`; there is no caching.

---

## E29. Oneof collapse and expand

**Code.** `go/lib/protojsonx/oneof.go` — `CollapseOneofs`, `ExpandOneofs`, `DecodeTree`.

**Purpose.** The dialect's oneof shape: one key per genuine oneof, message arms tagged with
`w17_discriminator`, scalar arms bare; expanded back before decode.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E29.1 | proto3 `optional` (synthetic oneof) MUST NOT be collapsed | `TestOneof_Proto3Optional_NotCollapsed` |
| E29.2 | Message, scalar, enum and object-WKT arms MUST round-trip | `TestCollapse_AllArmShapes`, `TestObjectWKT_RoundTrip` |
| E29.3 | Ambiguous bare values MUST be left unresolved (protojson then decides) | `TestExpand_Ambiguities` |
| E29.4 | snake_case and lowerCamel oneof keys MUST both be accepted | `TestExpand_CamelCaseOneofKey`, `TestExpand_SnakeCaseOneofKey` |
| E29.5 | Malformed JSON MUST pass through unchanged so protojson reports it | `TestExpand_NonObjectAndMalformed` |
| E29.6 | Reshaping MUST NOT change any value: numbers are decoded with `UseNumber` and written back as their original text | `TestExpand_KeepsNumbersExact`, `TestCollapse_KeepsNumbersExact`, `TestMarshalProto_ReshapedResponseKeepsValues` |
| E29.7 | `DecodeTree` MUST refuse trailing data after the JSON value | `TestDecodeTree_RejectsTrailingData` |

**Deliberate limits.**
- At most one bare arm per JSON primitive kind.
- An unknown discriminator leaves the oneof key in place, where `DiscardUnknown` drops it: the
  oneof arrives unset, with no error.

---

## E30. Client dial

**Code.** `go/lib/grpcclient/grpcclient.go` — `Dial`, `TLSDialOption`, `ClientTLSConfig`, `InternalTLSEnabled`, `DefaultServiceConfig`.

**Purpose.** One dial path for gateway → backend: the stack-wide TLS posture, OTel client spans,
retry of `UNAVAILABLE`, keepalive. Full TLS detail is in [server.md](server.md).

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `W17_INTERNAL_TLS` | plaintext h2c | `on/true/1/yes` → TLS; `terminated` → TLS against system roots; `off/false/0/no` → plaintext; any other value → error at dial construction |
| `W17_INTERNAL_TLS_CA` | system roots | Unreadable or no PEM → error; ignored in `terminated` mode, with a warning naming it |
| `W17_INTERNAL_TLS_CERT` + `_KEY` | none | Only one set → error; ignored in `terminated` mode, with a warning naming them |
| Retry | 3 attempts, 0.1 s → 1 s backoff, `UNAVAILABLE` only; throttling 100 tokens, ratio 0.1 | — |
| Keepalive | 30 s / 10 s, permit without stream | — |
| Caller `opts` | appended last | Override earlier options |
| Per-call deadline | none | — |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E30.1 | Only `UNAVAILABLE` MUST be retried, with throttling | `TestDefaultServiceConfig_RetriesOnlyUnavailable`, `TestDefaultServiceConfig_HasRetryThrottling` |
| E30.2 | Misconfigured TLS environment MUST fail at dial construction, not at the first RPC | `TestTLSDialOption_MissingCAFile_Errors`, `TestTLSDialOption_HalfMTLS_Errors` |
| E30.3 | TLS MUST verify the server; mTLS MUST present the client leaf | `handshake_test.go` (all four) |
| E30.4 | Dial MUST be non-blocking | `TestDial_NonBlocking` |
| E30.5 | Keepalive MUST pair with the server's enforcement policy | `TestDefaultClientKeepalive_PairsWithServerEnforcement` |
| E30.6 | An unrecognised `W17_INTERNAL_TLS` value MUST be refused at dial construction, not read as plaintext | `TestTLSDialOption_UnrecognisedSwitchIsRefused` |
| E30.7 | In `terminated` mode the client MUST verify against the system roots only, whatever the CA and client-pair variables say; with the switch off `ClientTLSConfig` MUST return no TLS config | `TestClientTLSConfig_TerminatedUsesSystemRootsOnly`, `TestClientTLSConfig_OffIsNil`, `TestInternalTLS_Terminated` |

**Deliberate limits.**
- No circuit breaker beyond retry throttling.

---

## E31. Inter-tier dial options and principal relay

**Code.** `go/core/grpcx/dial.go` — `DialOpts`, `InternalTLSEnabled`, principal forwarding interceptors.

**Purpose.** Dial options for business → storage and domain → domain calls: the same retry and
keepalive as E30, plus interceptors that copy the gateway's principal stamps (`x-w17-user`,
`x-w17-scope-*`, `w17-label-*`) from incoming to outgoing metadata.

**Configuration.** `DialOpts(enabled bool)`. `InternalTLSEnabled()` reads `W17_INTERNAL_TLS`
(`terminated` counts as on). It has no error channel: an unrecognised value is logged and
answers true, so the dial fails closed over TLS. CA, cert and key come from the same variables as
E30, and are ignored the same way in `terminated` mode.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E31.1 | Principal stamps MUST be relayed through `principal.ForwardToOutgoing` and other metadata MUST NOT be; when the incoming side carries a principal, the relayed set replaces any outgoing copy of it (I12) | `TestForwardPrincipalUnaryInterceptor`, `TestForwardToOutgoing_VerifiedPrincipalWins` (partial: the stream interceptor has no test) |
| E31.2 | The retry config MUST parse and retry only `UNAVAILABLE` | `TestDefaultServiceConfig_ParsesAndRetriesOnlyUnavailable` |
| E31.3 | An unrecognised `W17_INTERNAL_TLS` value MUST make `InternalTLSEnabled` answer true (dial TLS), never plaintext | `TestInternalTLSEnabled_UnrecognisedValueFailsClosed` |
| E31.4 | In `terminated` mode `DialOpts` MUST verify against the system roots only, ignoring a private CA | `TestClientTLSConfig_TerminatedIgnoresPrivateCA` |

---

## E32. Connection pool

**Code.** `go/core/grpcx/pool.go` — `Pool`, `Connect`, `ConnectScaled`, `Reconnect`, `EnableHealthWatch`, `Close`, `ScalingConfig`.

**Purpose.** Caches client connections by address, with optional load-scaled connections per
address and an optional health watch. A pooled connection is replaced only once it is shut
down; a connection in `TransientFailure` is kept and its backoff reset, because it heals itself
when the backend returns. `Reconnect` resets the backoff, moves the channel out of idle and
returns the same connection unless it was closed.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `ScalingConfig.StreamsPerConn` | 100 | — |
| `ScalingConfig.MaxConns` | 1000 | — |
| `EnableHealthWatch(interval)` | off | — |
| Dial options | captured on first use per address | Later options are ignored |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E32.1 | `Connect` MUST keep one connection per address; options of later calls are ignored | `TestPool_Connect_ReusesConnection` |
| E32.2 | `ConnectScaled` MUST pick the least-loaded connection and grow only when all are full, up to `MaxConns` | `TestPool_ConnectScaled_*` |
| E32.3 | After `Close`, every operation MUST fail and the health goroutine MUST stop | `TestPool_Connect_AfterClose`, `TestPool_Close_StopsHealthWatch`, `TestPool_EnableHealthWatch_AfterCloseIsNoop` |
| E32.4 | Concurrent use MUST be race-free | `TestPool_ConcurrentAccess`, `TestPool_Reconnect_Concurrent` |
| E32.5 | The pool MUST NOT close a connection a caller still holds: neither the health watch nor `Reconnect` closes a connection in `TransientFailure`; only a shut-down connection is replaced | `TestPool_HealthWatch_KeepsATransientFailureConnOpen`, `TestPool_Reconnect_KeepsATransientFailureConnOpen`, `TestPool_Reconnect_ReplacesAClosedConnection` |

**Deliberate limits.**
- `Release` must be called exactly once per `ConnectScaled`; nothing detects a missed release.

---

## E33. Database error → gRPC status

**Code.** `go/core/grpcerr/wrap.go` — `Wrap`, `ConstraintRegistry`, `UserMsgid`, `Vocabulary`, `RecoverPanic`, `RecoverPump`; `go/core/grpcerr/dialect` — per-driver classification.

**Purpose.** Translates driver errors (Postgres via pgx or lib/pq, MySQL, SQLite) into gRPC
statuses carrying a user-facing `w17.ErrorDetail` and an operator-facing message.

**Configuration.** `ConstraintRegistry` (generated): `ByName`, `ByColumns`, `SoleFKByTable` with
`MultiFKPresent`, `SoleNotNullByColumn`. A `Dialect` constant per service.

**Shares.** E3 (gateway demotion), `go/lib/i18n` (messages translated with the request context),
`observx` (`ReportError` for faults and codegen gaps, `ReportEvent` for mapped constraint hits).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E33.1 | Precedence MUST be: nil → nil; `sql.ErrNoRows` → NotFound; serialization or deadlock → Aborted; context canceled or deadline → Canceled or DeadlineExceeded (not reported); class-22 caller value errors → InvalidArgument; mapped constraint → InvalidArgument with a field detail; unmapped constraint → FailedPrecondition and a report; anything else → Internal and a report | `wrap_test.go`, `wrap_ctx_test.go`, `wrap_dialect_test.go` (each arm) |
| E33.2 | Every branch MUST attach a detail with a stable code and a translated sentence | `constraint_locale_test.go`, `foruserwith_test.go`, `onempty_test.go` |
| E33.3 | The raw driver error MUST NOT become the status message; Internal says only `<method>: internal error` | `TestWrap_InternalOnUnknownError`, `TestWrap_InternalErrorRoutesViaErrorx` |
| E33.4 | A bare SQLite FK failure MUST be attributed only when the whole service has exactly one FK | `TestWrap_SQLite_FK_SingleFKTableWithMultiFKElsewhere_NoAttribution` |
| E33.5 | Every fallback sentence MUST be in `Vocabulary()` | `vocabulary_completeness_test.go` |
| E33.6 | Panic recovery (`RecoverPanic`, the interceptors, `RecoverPump`) MUST return a generic Internal and report the stack | `recover_test.go`, `recover_interceptor_test.go` |
| E33.7 | `Wrap` MUST NOT retry on its own; Aborted is the caller's signal | holds by construction (`Wrap` takes an error, not an operation it could run again); no test |
| E33.8 | Constraint and table names MUST NOT reach a client on any surface | `TestConstraintFailure_NamesNoConstraintOrTableToTheClient` (REST and the MCP reading; the generated rpc surface is not covered here) |

**Deliberate limits.**
- `RecoverPump` blocks if its error channel is unbuffered and nobody reads it.

---

## E34. Egress JSON and form codec

**Code.** `go/lib/egress/wire` — `plan.go`, `marshal.go`, `unmarshal.go` (`UnmarshalOptions`), `form.go`, `union.go`.

**Purpose.** Encodes a proto message the way a third-party HTTP API expects (keys, enum strings,
unions, wrapper bodies, form encoding) and decodes its JSON back, from options the compiler put on
the client proto. It is a codec only: the HTTP client, base URL and timeouts live in generated
code.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `UnmarshalOptions.Report` | nil → issues dropped | Issue sink |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E34.1 | Unknown upstream keys MUST be ignored, `null` MUST leave a field unset, and wrong JSON types MUST be errors naming the path | `TestUnmarshal_Lenience`, `TestUnmarshal_Refusals` |
| E34.2 | Numbers MUST be decoded exactly (no float64 round trip), quoted numbers accepted, and huge exponents or lengths rejected without big-number work | `TestNumbers`, `TestNumbers_HugeExponentsAreCheap`, `TestNumbers_TinyAndZero` |
| E34.3 | Decode MUST replace, never merge | `TestUnmarshal_Replaces` |
| E34.4 | Unknown enum strings and union tags MUST be reported as issues, not errors | `TestUnmarshal_Lenience` |
| E34.5 | Trailing data after the JSON value MUST be an error | `TestUnmarshal_Refusals` |

**Deliberate limits.**
- No input size cap: the caller bounds the response body.
- Plans are cached per descriptor without eviction (the generated set is fixed).
