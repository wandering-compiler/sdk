# Edges and traffic

Everything between a caller and a generated service's handlers, plus the outgoing side of a
service: the REST gateway runtime, the per-client rate limiter, the MCP proxy runtime, the
w17 JSON dialect, the gRPC dial helpers, the database-error mapper, the egress JSON codec and
DPoP proof of possession.
Packages: `go/lib/restgw` (with `go/lib/restgw/dpopgw`), `go/lib/ratelimit`, `go/lib/mcp`, `go/lib/protojsonx`,
`go/lib/grpcclient` (dial side; server TLS is in [server.md](server.md)), `go/lib/egress/wire`,
`go/core/grpcerr` (with `go/core/grpcerr/dialect`), `go/core/grpcx`, `go/lib/dpop` (with
`go/lib/dpop/dpopclient` and `go/lib/dpop/dpopstore`); the TypeScript DPoP client is
`ts/dpop` (`@w17/dpop`).

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
                         │      → [dpopgw.Gate.Middleware] → AuthFunc (optionally CachedAuthFunc,
                         │        or dpopgw.CachedAuthFunc behind the gate) → SetUserMetadata
                         │        [+ SetDeviceMetadata] → scopes
                         │      → DecodeRequest → backend gRPC (grpcclient.Dial conn)
                         │      → WriteResponse / WriteGRPCErrorCtx
                         ├─ SSE / WS handlers: [AdmitPublicStream] → auth → WatchStreamAuth
                         │      (DPoP: dpopgw.NewWSAuth → dpopgw.WatchStreamBinding)
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
| E35 | DPoP proofs and verifier | `go/lib/dpop` — `Verifier`, `NewProof`, `Thumbprint`, `HMACNonces`, `MemoryJTIStore` |
| E36 | DPoP client helpers | `go/lib/dpop/dpopclient` — `Transport`, `Credentials`, `Refusal` |
| E37 | DPoP at the REST edge | `go/lib/restgw/dpopgw` — `Gate`, `CachedAuthFunc`, `NewWSAuth`, `WatchStreamBinding`; `go/lib/restgw` — `CachedAuthFuncWithDecision`, `WithDPoPCORS` |
| E38 | Shared DPoP replay stores | `go/lib/dpop/dpopstore` — `RedisJTIStore`, `SQLJTIStore` |
| E39 | DPoP TypeScript client | `ts/dpop` — `createDpopFetch`, `createProof`, `jwkThumbprint`, `IndexedDBKeyStore`, `issueStreamTicket` |

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
| E4.5 | Gateway pre-flight validation and backend InvalidArgument MUST produce the same sentence | `TestPreflightSentence_IsTheCatalogueSentenceForInvalidArgument`, `TestPreflightSentence_AgreesWithTheDemotionUnderALocale`, `TestPreflightSentence_IsNotTheOperatorsPhrasing`, `TestClientFacing_AgreesWithPreflightWhenNothingWasAttached`, `TestClientFacing_RequestLevelDetailStillWins` |
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
| E5.2 | `SetUserMetadata` MUST overwrite (not append) `x-w17-user` and preserve other outgoing metadata | `TestSetUserMetadata_PreservesExistingOutgoing`, `TestSetUserMetadata_ReplacesAPreexistingValue` |
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
| E6.6 | The entry count MUST stay bounded under distinct valid credentials; expired entries MUST be reaped without a same-key get | `TestAuthCache_BoundedUnderDistinctValidCredentials`, `TestAuthCache_ReapsExpiredWithoutASameKeyGet` |
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
| E7.5 | A URL credential MUST NOT reach the trace or a panic report: once injected, the span's `url.path` and the reported path name it `{credential}` | `TestURLCredential_NeverReachesTheTrace`, `TestURLCredential_NeverReachesAPanicReport` |
| E7.6 | A next-page link MUST keep the capability path and query, and a browser redirect MUST NOT copy them into `Location` | `TestURLCredential_NextPageLinkKeepsIt`, `TestURLCredential_NeverReachesARedirect` |

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

**Code.** `go/lib/restgw/cors.go` — `CORSMiddleware`, `matchOrigin`, `CORSConfig.ExposedHeaders`, `WithDPoPCORS`.

**Purpose.** Opt-in CORS for browser frontends on another origin.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `<PREFIX>_CORS_ORIGINS` (CSV) | unset → middleware disabled | — |
| `<PREFIX>_CORS_METHODS` | `GET, POST, PUT, PATCH, DELETE, OPTIONS` | — |
| `<PREFIX>_CORS_HEADERS` | `Content-Type, Authorization, W17-Org` | — |
| `<PREFIX>_CORS_ALLOW_CREDENTIALS` | false | `1/true/yes/on` → true |
| `<PREFIX>_CORS_MAX_AGE` | 600 | `0` → header omitted; negative or invalid → 600 |
| `CORSConfig.ExposedHeaders` | empty → no `Access-Control-Expose-Headers` | Set in code (no env var); `WithDPoPCORS` (E37) adds the DPoP response headers |

**Shares.** Preflight is answered before auth, rate limiting and routing.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E12.1 | `*` combined with credentials MUST be refused: credentials are dropped and a warning logged | `TestCORSMiddleware_Wildcard_WithCredentials_DropsCredentials` |
| E12.2 | Only exact origin matches MUST be echoed | `TestCORSMiddleware_SimpleRequest_DisallowedOrigin`, `TestMatchOrigin` |
| E12.3 | A preflight (OPTIONS + `Access-Control-Request-Method`) MUST answer 204 without calling `next`; a plain OPTIONS MUST pass through | `TestCORSMiddleware_Preflight`, `TestCORSMiddleware_PlainOptions_NotSwallowed` |
| E12.4 | The org header MUST be in the default allowed headers | `TestPreflightAdvertisesOrgScopeHeaderByDefault` |
| E12.5 | Every response the middleware handles MUST carry `Vary: Origin`: with no `Origin`, a disallowed one, a match, and preflights | `TestCORSMiddleware_EveryResponseVariesOnOrigin` |
| E12.6 | The default configuration MUST NOT allow `DPoP` nor send `Access-Control-Expose-Headers` | `TestWithDPoPCORS` |
| E12.7 | `ExposedHeaders` MUST be sent on non-preflight responses to an allowed origin only | `TestWithDPoPCORS` |

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
| E13.3 | A pump panic MUST close the socket with `StatusInternalError` instead of crashing the process | `TestRecoverWSPump_ClosesWithInternalError`, `TestRecoverWSPump_NilContextFallback` |
| E13.4 | Each request MUST run with its own reporting scope, so breadcrumbs recorded by one request never reach another request's report | `TestRecoverPanicMiddleware_GivesEachRequestItsOwnScope`, `TestRequestScope_KeepsBreadcrumbsPerRequest` |

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
| E16.5 | Issuer responses MUST be `no-store` with `Vary: Authorization, Cookie` | `TestTicketIssuer_HappyPOST` |
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

**Code.** `go/lib/ratelimit/ratelimit.go` — `Limiter`, `FromEnv`, `FromEnvScope`, `Allow`, `Reserve`, `ReserveRequest`, `AllowChargeable`, `OpenStream`, `TrustPrivateRanges`; `go/lib/restgw/publiclimit.go` — `AdmitPublic`, `AdmitPublicStream`.

**Purpose.** A token bucket per client address for methods that take no credential, so guessing
is bounded without a global cap.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `<PREFIX>_RATELIMIT_PER_MINUTE` | 30 | float; `<= 0` disables |
| `<PREFIX>_RATELIMIT_BURST` | 10 | int > 0 |
| `<PREFIX>_RATELIMIT_TRUSTED_PROXIES` | none: every caller is keyed by its connection's address | Comma-separated CIDRs or bare addresses; `private` adds the loopback and RFC 1918 ranges (combinable with CIDRs); `none` states the default; any other entry is logged once and skipped. List exactly the hops that terminate HTTP and append to `X-Forwarded-For` |
| `<PREFIX>_RATELIMIT_MAX_STREAMS` | 0: no cap | Opt-in cap on the public streams one client holds open at once; a value that is not a whole number `>= 0` is logged once and caps nothing. Too many streams is an overload, not a breach, so nothing is capped unasked |
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
| E26.11 | The limiter MUST NOT become a CPU sink under a stream of fresh keys: eviction at the ceiling is constant time (buckets sit on a recency list) | `TestLimiter_FreshKeysAtTheCeilingStayCheap`, `BenchmarkLimiter_FreshKeyAtTheCeiling`, `BenchmarkLimiter_KnownKey` |
| E26.12 | Unset `TRUSTED_PROXIES` MUST trust no proxy; `private` MUST trust the loopback and private ranges only when set, together with any listed CIDRs; a bare address MUST trust exactly that host | `TestFromEnvDefaultsAreOn`, `TestTrustedProxiesPrivateKeywordIsOptIn` |
| E26.13 | A private-range peer that sends `X-Forwarded-For` without being a configured proxy MUST be keyed by its own address and produce a warning (at most hourly) naming `_RATELIMIT_TRUSTED_PROXIES` | `TestForwardedFromUnconfiguredPrivatePeerIsIgnored`, `TestUntrustedForwarderWarningIsHourly` |
| E26.14 | `Reserve` MUST take the token at admission; `done(false)` MUST give it back, `done(true)` MUST keep it, and only the first call counts | `TestReserve_SettlesOnce` |
| E26.15 | Without `MAX_STREAMS` no stream is capped. With it, a client already holding that many open public streams (SSE and WebSocket together) MUST be refused with 429 without spending a token, other clients MUST be unaffected, and a stream MUST stop counting once its handler returned; a release frees exactly one slot | `TestFromEnv_MaxStreamsIsOffUnlessSet`, `TestOpenStream_NoCapAdmitsEveryStream`, `TestOpenStream_CapsOpenStreamsPerClient`, `TestAdmitPublicStream_CapsOpenStreamsAndFreesAClosedOne`, `TestAdmitPublicStream_NoCapByDefault` |

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
| E27.2 | Tool results MUST use proto names, integer enums and collapsed oneofs | `TestCallUnary_DecodesArgsAndMergesMetadata`, `TestCallUnary_ResultUsesProtoNamesAndIntegerEnums` |
| E27.3 | Unknown argument fields MUST be ignored | `TestUnmarshalProto_EmptyAndDecode` |
| E27.4 | The long-lived GET stream MUST end at shutdown start; POST and DELETE keep the drain budget | `serve_shutdown_test.go` (both) |
| E27.5 | The health route MUST be answered outside MCP auth | `healthz_test.go` |
| E27.6 | Tool and auth errors MUST NOT return backend or developer prose to the client: no backend status message, auth-backend text, resolver error or decode detail | `TestCallUnary_BackendErrorIsSanitised`, `TestCallUnary_AuthBackendErrorIsSanitised`, `TestCallUnary_NonStatusErrorIsInternal`, `TestCallUnary_DecodeError`, `TestCallUnary_GateResolverError` |
| E27.7 | The HTTP transport MUST NOT read unbounded request bodies: a declared length over the cap MUST be refused with 413 before the handler runs, and an undeclared body MUST stop at the cap | `TestLimitBody`, `TestMaxBodyBytes_DefaultAndOverrides`, `TestServeStreamableHTTP_RefusesOversizedBody` |
| E27.8 | Every `CallUnary` error MUST be a gRPC status carrying the code, the sentence a REST client reads for the same failure, and its field details; a non-status error MUST read as `INTERNAL` | `TestCallUnary_BackendErrorIsSanitised`, `TestCallUnary_PermissionDeniedIsAStatus`, `TestCallUnary_NonStatusErrorIsInternal` |
| E27.9 | Tool arguments MUST keep 64-bit integers exact over HTTP and stdio: the raw arguments are captured before the MCP library decodes them and handed to `CallUnary`; a refused call leaves no captured arguments behind | `TestServeStreamableHTTP_ToolArgumentsKeepSixtyFourBitNumbers`, `TestHandleMessage_ScopedContextKeepsSixtyFourBitNumbers`, `TestHandleMessage_RefusedCallLeavesNoCapture` |
| E27.10 | A backend error MUST be reported by code as E3.7: refusals to the non-exception channel, faults to the exception channel | `TestCallUnary_BackendErrorsAreReportedByCode` |
| E27.11 | Each tool call MUST run with its own reporting scope (`observx.WithRequestScope`) | `TestAddTool_EachCallRunsInItsOwnReportingScope`, `TestRequestScope_KeepsBreadcrumbsPerRequest` |

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
| E31.1 | Principal stamps MUST be relayed through `principal.ForwardToOutgoing` and other metadata MUST NOT be; when the incoming side carries a principal, the relayed set replaces any outgoing copy of it (I12) | `TestForwardPrincipalUnaryInterceptor`, `TestForwardToOutgoing_VerifiedPrincipalWins`, `TestDialOpts_StreamRelaysThePrincipalOnly` |
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
| E33.1 | Precedence MUST be: nil → nil; `sql.ErrNoRows` → NotFound; serialization or deadlock → Aborted; context canceled or deadline → Canceled or DeadlineExceeded (not reported); a database the call never reached → Unavailable with `DATABASE_UNAVAILABLE` (reported at most once a minute); class-22 caller value errors → InvalidArgument; mapped constraint → InvalidArgument with a field detail; unmapped constraint → FailedPrecondition and a report; anything else → Internal and a report | `wrap_test.go`, `wrap_ctx_test.go`, `wrap_dialect_test.go` (each arm) |
| E33.2 | Every branch MUST attach a detail with a stable code and a translated sentence | `constraint_locale_test.go`, `foruserwith_test.go`, `onempty_test.go` |
| E33.3 | The raw driver error MUST NOT become the status message; Internal says only `<method>: internal error` | `TestWrap_InternalOnUnknownError`, `TestWrap_InternalErrorRoutesViaErrorx` |
| E33.4 | A bare SQLite FK failure MUST be attributed only when the whole service has exactly one FK | `TestWrap_SQLite_FK_SingleFKTableWithMultiFKElsewhere_NoAttribution` |
| E33.5 | Every fallback sentence MUST be in `Vocabulary()` | `vocabulary_completeness_test.go` |
| E33.6 | Panic recovery (`RecoverPanic`, the interceptors, `RecoverPump`) MUST return a generic Internal and report the stack | `recover_test.go`, `recover_interceptor_test.go` |
| E33.7 | `Wrap` MUST NOT retry on its own; Aborted is the caller's signal | holds by construction (`Wrap` takes an error, not an operation it could run again); no test |
| E33.8 | Constraint and table names MUST NOT reach a client on any surface | `TestConstraintFailure_NamesNoConstraintOrTableToTheClient` (REST and the MCP reading; the generated rpc surface is not covered here) |
| E33.9 | A database the call never reached (the dial failed or the connection was refused) MUST answer Unavailable, the code w17 clients retry, and an outage MUST be reported at most once a minute rather than once per call; a connection that broke mid-call, or a server that answered (an authentication failure), MUST stay Internal and be reported | `TestWrap_AnUnreachableDatabaseIsUnavailableAndReportedOnce`, `TestWrap_AnOngoingOutageIsReportedAgainAfterTheWindow`, `TestWrap_ABrokenConnectionOrAnAnsweringServerIsNotUnavailable` |

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

---

## E35. DPoP proofs and verifier

**Code.** `go/lib/dpop` — `key.go` (`Signer`, `Key`, `GenerateKey`, `NewKey`, `ParseKeyPEM`, `LoadKeyFile`, `Key.SaveFile`, `Thumbprint`, `AccessTokenHash`), `proof.go` (`NewProof`, `SignClaims`, `NormalizeHTU`), `verify.go` (`Verifier`, `Verify`, `VerifyResult`, `Result`, `Config`, `MintFullPolicy`, `Request`, `NonceError`, the `Err*` refusal causes), `nonce.go` (`NonceSource`, `HMACNonces`), `jti.go` (`JTIStore`, `MemoryJTIStore`), `challenge.go` (`SetChallenge`, `HTTPStatus`, `GRPCCode`, `ChallengeMetadata`, `WWWAuthenticate`, `IsUseNonceChallenge`, `AccessToken`). JWS and JWK handling is `github.com/go-jose/go-jose/v4`.

**Purpose.** Binds a credential to a device key per RFC 9449. The device id is the RFC 7638
thumbprint of the device's public key; every request carries a proof signed with that key, and
the verifier checks it and returns the thumbprint for the caller to compare with the credential's
binding. The REST gateway wires it in opt-in (E37); elsewhere a caller builds a `Request` from
the HTTP request or gRPC metadata and answers refusals itself.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `Config.Origin` | required | The public `scheme://host[:port]` clients address; htu is compared against it plus the request path, never against `Host`. Missing, relative, or with a path → error at construction |
| `Config.Nonces` | nil (nonces off) | nil → `iat` must lie within `IATWindow`; set → `iat` is not checked and every proof needs a valid nonce |
| `Config.JTIs` | `NewMemoryJTIStore(0)` | Records bound proofs. The memory store is per process: correct only with one replica |
| `Config.MintJTIs` | `NewMemoryJTIStore(DefaultMaxMintJTIs)` = 50 000 | Records unbound (mint) proofs, apart from `JTIs`. `JTIs` set without `MintJTIs`, or both the same store → error at construction |
| `Config.MintFull` | `MintFullRefuse` (recommended) | `MintFullAcceptUnrecorded`: an unbound proof the full mint store cannot record (`ErrJTIStoreFull`) is accepted unrecorded and `Result.MintUnrecorded` is set; never for bound proofs or a failing store. Any other value → error at construction |
| `Config.ReuseWindow` | 30 s | Negative → reuse off; above 300 s → error at construction |
| `Config.IATWindow` | 60 s | Negative → error; ignored with nonces |
| `Config.SignatureCacheEntries` | 10 000 | A full cache stops caching; it never refuses |
| `Config.Now` | `time.Now` | — |
| `NewHMACNonces(key, ...)` | bucket 5 min | Key shorter than 32 bytes → error; the key is copied; `WithNonceBucket(d)` below 1 s → 1 s, `d <= 0` keeps the default |
| `NewMemoryJTIStore(n, ...)` | 200 000 entries | `n <= 0` keeps the default; expired entries leave in expiry order (a min-heap, never a scan); a full store refuses with `ErrJTIStoreFull` |
| `WithMaxJTIsPerKey(n)` | 10 000 entries per key thumbprint | `n <= 0` keeps the default; a key at its share is refused with `ErrJTIStoreFull`, other keys are not affected |
| `MaxProofBytes` / `MaxJTIBytes` | 8 KiB / 256 bytes | Longer → `ErrMalformed` |
| `GenerateKey(kt)` | `KeyP256` | `KeyEd25519`; anything else → `ErrUnsupportedKey` |
| `Request.AccessToken` / `Request.Binding` | — | Both empty on a mint endpoint; a token with no binding is refused (fail closed) |
| `Request.Path` | required | Must start with `/`; anything else → `ErrHTUMismatch` |

**Shares.** E37 (the REST wiring), E38 (shared stores). Every replica configured with the same nonce key
shares the nonce space by construction; a `JTIStore` shared between replicas is the caller's.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E35.1 | The thumbprint MUST be RFC 7638's (SHA-256, base64url) and `ath` RFC 9449's, matching the RFCs' example values | `TestThumbprint_RFC7638Vector`, `TestThumbprint_RFC9449KeyMatchesJKT`, `TestAccessTokenHash_RFC9449Vector` |
| E35.2 | The example proofs printed in RFC 9449 MUST verify and yield the RFC's `jkt` | `TestVerify_RFC9449Examples` |
| E35.3 | Only ES256 and EdDSA MUST be accepted; `none`, HS256, RS256 and ES384 MUST be refused with `ErrBadAlg` | `TestVerify_RefusalCauses` |
| E35.4 | `typ` MUST be `dpop+jwt`; the `jwk` MUST be present, hold no private member, and fit `alg`; otherwise `ErrBadType` / `ErrBadKey` | `TestVerify_RefusalCauses` |
| E35.5 | A proof whose signature does not verify with its embedded key MUST be refused with `ErrBadSignature` | `TestVerify_RefusalCauses` |
| E35.6 | Each refusal MUST match exactly one cause: missing, malformed (two headers, oversized, missing or mistyped claims, overlong jti), alg, typ, key, signature, nonce, stale iat, replay, htm, htu, ath, binding | `TestVerify_RefusalCauses` |
| E35.7 | Checks MUST run in the order header, signature, nonce, htm/htu, ath, binding, and the jti last; a proof refused by any earlier check MUST NOT spend its jti | `TestVerify_CheckOrder` |
| E35.8 | With nonces on, a missing, forged, expired or other-key nonce MUST be a `*NonceError` (`ErrUseNonce`) carrying a fresh nonce that then works | `TestVerify_RefusalCauses`, `TestVerify_NonceChallengeCarriesFreshNonce` |
| E35.9 | A nonce MUST be valid in its own and the next bucket only, never before its bucket, bound to its thumbprint, and unforgeable without the key | `TestHMACNonces_RotationAcrossBuckets`, `TestHMACNonces_KeyRequirements` |
| E35.10 | Replicas sharing the nonce key MUST accept each other's nonces with no shared state; a replica with another key MUST NOT | `TestHMACNonces_SharedKeyAcrossReplicas` |
| E35.11 | With nonces on, `iat` MUST NOT be checked (client clock skew is irrelevant); with nonces off, `iat` MUST lie within ±`IATWindow` | `TestVerify_ClockSkewIrrelevantWithNonces`, `TestVerify_IATWindowWithoutNonces` |
| E35.12 | A jti MUST NOT be accepted twice for different proofs, and MUST be remembered for as long as its proof could be accepted (the nonce lifetime, or twice the iat window) | `TestVerify_ReplayRefused`, `TestMemoryJTIStore_RecordAndExpiry` |
| E35.13 | The same proof MUST be accepted again while its reuse window lasts and refused after it; with reuse off every repeat MUST be a replay | `TestVerify_ReuseWindow`, `TestVerify_ReplayRefused` |
| E35.14 | A reused proof MUST still pass the nonce and target checks: only the signature result is cached | `TestVerify_ReuseStillChecksNonceAndTarget` |
| E35.15 | The signature cache MUST be keyed by the hash of the whole proof: altered claims under a cached proof's signature MUST be verified afresh and refused | `TestVerify_SignatureCacheKeyedByProofHash` |
| E35.16 | Concurrent presentations of one proof with reuse off MUST yield exactly one acceptance | `TestVerify_ConcurrentSameProofOneWinnerWithoutReuse` |
| E35.17 | htu MUST equal the configured origin plus the request path after normalisation (scheme and host case, default port, no query or fragment); a gRPC call MUST be POST to origin + full method name | `TestVerify_HTUNormalisation`, `TestNormalizeHTU`, `TestVerify_GRPCTarget`, `TestVerify_RefusalCauses` |
| E35.18 | `ath` MUST match the presented token, MUST be present when a token is, and MUST be absent when none is; a token presented with no binding to check MUST be refused | `TestVerify_RefusalCauses` |
| E35.19 | A full or failing jti store MUST fail closed with `ErrUnavailable` (wrapping the cause, e.g. `ErrJTIStoreFull`), never accept unrecorded; expired entries MUST free room, in expiry order, without scanning the store | `TestVerify_FullJTIStoreFailsClosed`, `TestMemoryJTIStore_ExpiresInOrder` |
| E35.20 | Invalid configuration (no origin, origin with a path, reuse window above 300 s, negative iat window, `JTIs` without `MintJTIs`, one store for both, an unknown `MintFull` policy) MUST be refused at construction | `TestNewVerifier_Config`, `TestVerify_MintFullRefuseIsTheDefault` |
| E35.21 | A refusal MUST map to the RFC 9449 challenge: `use_dpop_nonce` plus `DPoP-Nonce` for a nonce refusal, `invalid_token` for ath and binding, `invalid_dpop_proof` otherwise, `no-store`; gRPC carries the same values in the trailer | `TestChallenge_Headers`, `TestIsUseNonceChallenge`, `TestAccessToken` |
| E35.22 | Keys MUST be P-256 or Ed25519 (others refused); a saved key MUST be mode 0600, replacing any existing file atomically; any `crypto.Signer` (KMS, HSM) MUST be usable as the signer | `TestKey_GenerateBothTypes`, `TestKey_UnsupportedKeysRefused`, `TestKey_SaveLoadRoundTrip`, `TestSigner_ExternalCryptoSigner` |
| E35.23 | Proofs refused for any reason (wrong htu, ath or binding, signed with the sender's own key and fresh jtis) MUST take no room in the jti store, so they cannot lock out legitimate proofs | `TestVerify_RefusedProofsDoNotFillJTIStore` |
| E35.24 | One key MUST NOT hold more than its share of the jti store; other keys MUST keep working, and the share MUST free as its entries expire | `TestMemoryJTIStore_PerKeyCap` |
| E35.25 | The protected header, the jwk and the claims MUST be decoded strictly: exact member names, no duplicate member, nothing after the object, string members not null; private jwk members are refused in any case | `TestVerify_StrictDecoding` |
| E35.26 | A request path that does not start with `/` MUST be refused (`ErrHTUMismatch`), even where origin + path would normalise to the proof's htu | `TestVerify_StrictDecoding` |
| E35.27 | `Verify` and `NormalizeHTU` MUST NOT panic on any input; a normalised htu MUST normalise to itself, and an htu with an empty host MUST be refused | `FuzzVerify`, `FuzzNormalizeHTU` (seeds and corpus run in `go test`), `TestNormalizeHTU` |
| E35.28 | Unbound (mint) proofs MUST be recorded apart from bound ones: a flood of unbound proofs from throwaway keys can fill only the mint store, bound proofs MUST keep passing, and minting MUST recover as entries expire | `TestVerify_MintFloodCannotStarveBoundProofs` |
| E35.29 | `ErrUnavailable` MUST map to 503 with `Retry-After` and no challenge (gRPC `Unavailable`); every proof refusal to 401 (`Unauthenticated`); anything else to 500 (`Internal`) | `TestHTTPStatusAndGRPCCode`, `TestVerify_FullJTIStoreFailsClosed` |
| E35.30 | For arbitrary header and claims JSON signed with a valid key, an accepted proof MUST carry that key's thumbprint, match the request's method and token, and MUST NOT be accepted a second time with reuse off | `FuzzVerifySigned` (valid-proof seeds run in `go test`) |
| E35.31 | With `MintFullAcceptUnrecorded`, an unbound proof the full mint store cannot record MUST be accepted without being recorded and reported in `Result.MintUnrecorded`; a recorded unbound proof MUST still be a replay | `TestVerify_MintFullAcceptUnrecorded` |
| E35.32 | A bound proof meeting a full store, and any proof meeting a failing store, MUST fail closed whatever `MintFull` says; by default (`MintFullRefuse`) a full mint store MUST refuse and nothing MUST be reported unrecorded | `TestVerify_MintFullAcceptUnrecorded`, `TestVerify_MintFullRefuseIsTheDefault` |

**Deliberate limits.**
- `MemoryJTIStore` is per process. With more than one replica a proof accepted by one can be
  replayed against another until a shared `JTIStore` (Redis, a database: E38) is configured.
- A mint endpoint (no access token, no binding) accepts a proof from any key, and keys cost
  nothing to make: there the per-key share does not bound a flood. Such a flood can only fill
  the mint store (stalling minting, never authenticated requests), but mint endpoints still
  need rate limiting in front of the verifier. `MintFullAcceptUnrecorded` keeps minting up
  through such a flood at the price of best-effort replay protection for unbound proofs: a
  proof accepted unrecorded can be presented again while the store stays full. That replay
  yields only a credential bound to a key the replayer does not hold, but it re-runs the mint
  handler and everything it does: a one-time or MFA code is burned, lockout counters move,
  Device rows are written, events are emitted. `MintFullRefuse` is the default and the
  recommended setting; use `MintFullAcceptUnrecorded` only behind per-address limits on the
  mint endpoints, with an alert on `Result.MintUnrecorded`.
- The verifier does not answer the request; the caller sets the challenge with `SetChallenge` or
  `ChallengeMetadata` and writes its own status and body.
- `Request.Path` is compared as given; percent-encoding case is not normalised.

---

## E36. DPoP client helpers

**Code.** `go/lib/dpop/dpopclient` — `transport.go` (`Transport`, `TokenFunc`, `WithMint`, `MintContext`, `IsMint`), `grpc.go` (`Credentials`, `UnaryClientInterceptor`, `StreamClientInterceptor`, `DialOptions`), `refusal.go` (`RefusedError`, `Refusal`, `ErrDeviceRevoked`, `ErrSessionExpired`).

**Purpose.** Attaches `Authorization: DPoP <token>` and a fresh proof to every outgoing HTTP
request or gRPC call that carries a token, a proof alone to a mint call (a sign-in or token
request the caller marks), follows the server's nonces, and retries once when the server asks
for a fresh nonce. Anything else goes out with no DPoP headers.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `Transport.Base` | `http.DefaultTransport` | — |
| `Transport.Origins` | required | Origins that may receive the token and proofs. Empty or all invalid → every request fails with `ErrNoOrigins`; `NewTransport` refuses them up front. A request to any other origin fails with `ErrOriginNotAllowed` |
| `Transport.Signer`, `Credentials.Signer` | required | nil → every request fails |
| `Transport.Token`, `Credentials.Token` | nil (no token, no `ath`) | An error fails the request. `""` or nil: the request carries DPoP only when it is a mint call |
| `dpopclient.WithMint(req)`, `dpopclient.MintContext(ctx)` | unmarked | Marks an HTTP request (a shallow copy) or every call made with `ctx` as a mint call: without a token it still carries an unbound proof |
| `Credentials.Origin` | required | The origin the server is configured with; htu = origin + full method |
| `Credentials.AllowInsecure` | false | false → gRPC refuses to send the credentials over a connection without transport security |

**Shares.** E35 (proof builder, challenge parsing, header and metadata names).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E36.1 | An HTTP `use_dpop_nonce` refusal with a new nonce MUST be retried once with that nonce and the same body; later requests MUST carry the nonce from the start | `TestTransport_NonceRetryOnce` |
| E36.2 | A server that keeps asking for a nonce MUST get exactly one retry; the second refusal is returned | `TestTransport_RetriesExactlyOnce`, `TestCredentials_UnaryRetriesExactlyOnce` |
| E36.3 | A request whose body cannot be replayed MUST NOT be retried, but its nonce MUST still be learned | `TestTransport_UnreplayableBodyNotRetried` |
| E36.4 | A nonce sent on a success MUST be used by the next proof | `TestTransport_LearnsNonceFromSuccess` |
| E36.5 | Without a token a mint call MUST send no `Authorization` and a proof with no `ath` | `TestTransport_NoTokenNoATH`, `TestCredentials_ProofOnlyWithTokenOrMint` |
| E36.6 | The caller's request MUST NOT be modified | `TestTransport_DoesNotMutateRequest` |
| E36.7 | A gRPC unary `Unauthenticated` refusal with `use_dpop_nonce` and `dpop-nonce` in the trailer MUST be retried once; the proof MUST be POST to origin + full method name | `TestCredentials_UnaryNonceRetryOnce` |
| E36.8 | A gRPC stream MUST NOT be retried, but the nonce from its refusal MUST be learned for the next call | `TestCredentials_StreamLearnsNonce` |
| E36.9 | `DEVICE_REVOKED` and `SESSION_EXPIRED` refusals MUST come back as `*RefusedError` matching `ErrDeviceRevoked` / `ErrSessionExpired`, keeping the gRPC status; any other refusal MUST stay the plain status | `TestCredentials_RefusalTyped` |
| E36.10 | `Refusal` MUST recognise those reasons in a 401 or 403 error envelope and leave the body readable from the start | `TestRefusal_HTTPEnvelope` |
| E36.11 | The gRPC credentials MUST require transport security unless `AllowInsecure` is set | `TestCredentials_RequireTransportSecurity` |
| E36.12 | The token and proofs MUST go only to the configured origins, with no implicit default: a request elsewhere, a cross-host redirect included, MUST fail with `ErrOriginNotAllowed` before anything is sent or the token fetched, and a Transport without valid origins MUST send nothing | `TestTransport_OtherOriginsGetNoCredentials` |
| E36.13 | A request or call with no token that is not marked as a mint call MUST carry no DPoP header or metadata (no proof, no `Authorization`); a token or the mint marker MUST make it carry a proof; `WithMint` MUST NOT mark the caller's request | `TestTransport_ProofOnlyWithTokenOrMint`, `TestCredentials_ProofOnlyWithTokenOrMint` |

**Deliberate limits.**
- Only 401 with `WWW-Authenticate: DPoP error="use_dpop_nonce"` triggers the HTTP retry; a 400
  JSON `use_dpop_nonce` body is not read.
- gRPC streams are not retried: the refusal surfaces on the first receive, after the caller may
  have sent.
- The HTTP nonce is remembered per origin; the gRPC nonce per `Credentials`.
- Refusal reasons are read from the `w17.ErrorDetail` code only.
- Which calls are mint calls is the caller's to say (`WithMint`, `MintContext`): a tokenless
  sign-in call left unmarked reaches the server with no proof and is refused there.

---

## E37. DPoP at the REST edge

**Code.** `go/lib/restgw/dpopgw` — `dpop_gate.go` (`Gate`, `NewGate`, `WithBinding`, `WithRequired`, `BindingFunc`, `Verified` (`DeviceID`, `MintUnrecorded`), `FromContext`, `Error`, `SetDeviceMetadata`), `dpop_cache.go` (`CachedAuthFunc`), `dpop_ticket.go` (`TicketPrincipal`, `NewWSAuth`, `WSAuthConfig`, `WSAuth` (`Thumbprint`, `Verified`), `TicketThumbprintLabel`, `ErrTicketUnverified`), `dpop_reauth.go` (`WatchStreamBinding`, `BindingCheck`); `go/lib/restgw` — `authcache_decision.go` (`CachedAuthFuncWithDecision`, `AuthCacheDecision`, the hook `dpopgw.CachedAuthFunc` is built on), `cors.go` (`WithDPoPCORS`). `lib/restgw` and every other pre-existing package import nothing of `lib/dpop` or go-jose: a consumer that does not use DPoP gets no new dependency.

**Purpose.** Opt-in wiring of E35 into the REST gateway. The gate verifies
`Authorization: DPoP <token>` + `DPoP: <proof>`, answers refusals with the RFC 9449 challenge,
and records the verified thumbprint. Only a proof bound to a token makes that thumbprint a
device id (`dpopgw.Verified.DeviceID`), which `dpopgw.SetDeviceMetadata` stamps as `x-w17-device` (I10);
an unbound proof proves only possession of a key any client can generate. Streams are verified at set-up: a header upgrade by its own proof, a
browser stream by a ticket bound to the thumbprint of the signed request that obtained it,
whose one-shot redemption stands in for the proof. The stream watchdog asks a binding-only
question instead of replaying a proof. A gateway that builds none of this behaves as before.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `dpopgw.NewGate(v, ...)` | — | nil verifier → error |
| `dpopgw.WithBinding(fn)` | none | Without it every `Authorization: DPoP` request is refused (`invalid_token`). `fn` returning `""` → not bound (401); UNAUTHENTICATED / PERMISSION_DENIED or a dpop refusal → 401 `invalid_token`; any other error → 503 |
| `dpopgw.WithRequired()` | off | Off: a request with neither a proof nor a DPoP token passes unverified. On: refused with the bare `DPoP` challenge |
| Proof next to another `Authorization` scheme, or a DPoP token among several `Authorization` headers | — | Refused as malformed (`invalid_dpop_proof`) |
| `dpopgw.CachedAuthFunc(inner, ttl, extra)` | — | `ttl <= 0` → inner unchanged; `extra` as in E6 |
| `dpopgw.NewWSAuth` | — | nil `Gate` or the E16 configuration errors → panic |
| `dpopgw.WatchStreamBinding(ctx, check, r, thumbprint, interval)` | — | `interval <= 0` or nil `check` → ctx unchanged; no DPoP token on `r` or empty thumbprint → the stream ends at once |
| `WithDPoPCORS(cfg)` | — | Adds `DPoP` to the allowed headers (from the defaults when none are set) and `DPoP-Nonce`, `WWW-Authenticate` to `ExposedHeaders`; no duplicates; `cfg` with no origins stays CORS-off |

**Shares.** E35 (verifier, challenge, status mapping), E6 (the cache's key and eviction code),
E16 (ticket store, issuer, promotion marker), E17 (the watchdog loop), E12 (CORS), I10–I12
(`x-w17-device`).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E37.1 | A bound request whose proof verifies and whose key is the token's binding MUST reach `next` with `dpopgw.FromContext` holding the thumbprint and the token | `TestDPoPGate_VerifiesABoundRequest` |
| E37.2 | Every refusal MUST be answered by the gate — `WWW-Authenticate` per E35.21, the error envelope, 401 or 503 (`Retry-After`, no challenge) — and `next` MUST NOT run: a token without a proof, another key than the binding, an unbound token, no binding lookup, a refused or unanswered lookup, a proof next to a bearer token, two `Authorization` headers, a proof for another path, a replay, a required gate without DPoP material | `TestDPoPGate_RefusalsCarryTheChallenge` |
| E37.3 | With nonces on, a proof without a nonce MUST be answered `use_dpop_nonce` with `DPoP-Nonce`; the retry with it MUST pass, and a verified response MUST carry a fresh nonce | `TestDPoPGate_NonceChallengeAndRotation` |
| E37.4 | A gate that is not required MUST pass a request without DPoP material unchanged and unverified; a proof without `Authorization` MUST verify as unbound | `TestDPoPGate_OptionalAndMint` |
| E37.5 | `dpopgw.SetDeviceMetadata` MUST stamp the device id on a copy of the outgoing metadata and remove the key when there is none | `TestSetDeviceMetadata` |
| E37.6 | A `dpopgw.Error` MUST keep its cause for `errors.Is` and carry the status `dpop.GRPCCode` gives, so `WriteAuthError` answers 401 / 503 | `TestDPoPError_Status` |
| E37.7 | The DPoP auth cache MUST key a verified request on the verified thumbprint: one key's fresh proofs hit its entry, another key with the same credential headers MUST NOT read it | `TestCachedDPoPAuthFunc_KeysOnTheVerifiedThumbprint` |
| E37.8 | A request carrying DPoP material that was not verified on this request MUST NOT be answered from the cache nor fill it, on any presentation | `TestCachedDPoPAuthFunc_NeverAnswersAnUnverifiedRequest` |
| E37.9 | A request without DPoP material MUST be cached as E6 caches it; `ttl <= 0` MUST disable the cache | `TestCachedDPoPAuthFunc_BearerAndOff` |
| E37.10 | A bound ticket MUST be issued only for a request the gate verified as bound, and record its thumbprint; redemption MUST promote the token, report the thumbprint, survive re-auth, and stay one-shot | `TestDPoPTicket_BoundFlow`, `TestDPoPTicket_IssuerNeedsAVerifiedBoundRequest`, `TestDPoPTicketPrincipal_RefusesAnUnboundVerification` |
| E37.11 | Redemption MUST re-check the binding: a token rebound, unbound or revoked, or a bound ticket holding a non-DPoP credential, MUST be `ErrTicketInvalid` and run nothing; a lookup that cannot answer MUST be a 503-class error, not a refusal | `TestDPoPTicket_RedemptionChecksTheBinding` |
| E37.12 | An unbound ticket MUST be accepted with no device only when the gate is not required | `TestDPoPTicket_UnboundTicket` |
| E37.13 | A header upgrade MUST be verified by the gate; a bad proof MUST be a 401-class `dpopgw.Error`; a client MUST NOT make `Thumbprint` report a device through its own headers, and one wrapper MUST NOT trust another's promotion | `TestDPoPWSAuth_HeaderModeAndForgery` |
| E37.14 | The binding watchdog MUST ask the binding check with the stream's token and thumbprint, never the auth function, and end the stream on a refusal (including a dpop binding refusal); an outage MUST keep it open and re-ask | `TestWatchStreamBinding_AsksTheBindingQuestion`, `TestWatchStreamBinding_RefusedVersusUnanswered` |
| E37.15 | A watched stream with no binding to check MUST end at once; a disabled watchdog MUST return ctx unchanged | `TestWatchStreamBinding_NoBindingEndsTheStream` |
| E37.16 | `WithDPoPCORS` MUST allow `DPoP` and expose `DPoP-Nonce` and `WWW-Authenticate` to allowed origins only, keep the default and operator headers without duplicates, leave its argument and the defaults unchanged | `TestWithDPoPCORS` |
| E37.17 | A stream `Inner` refuses MUST NOT be marked: no device is reported and the spent ticket is not trusted on a second pass | `TestDPoPWSAuth_RefusedByInnerIsNotMarked` |
| E37.18 | An unbound proof (a cookie or credential-less request signed by any key) MUST NOT yield a device id: `DeviceID` is empty, `dpopgw.SetDeviceMetadata` stamps nothing and removes a stale copy, `principal.DeviceID` reads false | `TestDPoPGate_UnboundProofIsNoDevice` |
| E37.19 | A WS header upgrade MUST mark the stream with a device only for a token-bound proof; `Verified` MUST report the device and token of a bound stream only | `TestDPoPWSAuth_UnboundHeaderProofIsNoDevice` |
| E37.20 | An unbound ticket whose credential uses the DPoP scheme MUST be refused (`ErrTicketInvalid`) even when the gate is optional | `TestDPoPTicket_UnboundTicketWithDPoPTokenRefused` |
| E37.21 | Only `lib/dpop`, its subpackages and `lib/restgw/dpopgw` MAY import `lib/dpop` or go-jose; every other package MUST NOT, so the DPoP dependency stays opt-in | `TestDPoPImportsStayInsideTheOptInPackages` |
| E37.22 | `CachedAuthFuncWithDecision` MUST bypass without reading or filling, keep namespaces apart from each other and from un-namespaced entries, neither key on nor be defeated by `IgnoreHeader`, and behave as `CachedAuthFuncWithKeyHeaders` for the zero decision or a nil hook | `TestCachedAuthFuncWithDecision` |
| E37.23 | A mint request the verifier accepted unrecorded (`dpop.MintFullAcceptUnrecorded`, full mint store) MUST reach `next` with `Verified.MintUnrecorded` set and no device; a recorded one, or any one under the default policy, MUST NOT be reported unrecorded | `TestDPoPGate_MintUnrecordedIsReported` |

**Deliberate limits.**
- The gate does not decide what a token is worth; `dpopgw.BindingFunc` and the auth method do.
  The gate only refuses a proof that does not fit the binding the lookup reports.
- An `AuthFunc` cannot set response headers: a refusal raised inside `dpopgw.NewWSAuth` reaches the
  client without the challenge. Mount `dpopgw.Gate.Middleware` in front for the challenge.
- `dpopgw.WatchStreamBinding` keeps the principal the stream was set up with; a binding check says yes
  or no, it does not re-derive the principal.
- The generated gateway decides where the gate is mounted; nothing here mounts itself.

---

## E38. Shared DPoP replay stores

**Code.** `go/lib/dpop/dpopstore` — `redis.go` (`RedisJTIStore`, `NewRedisJTIStore`, `WithRedisPrefix`, `WithRedisMaxPerKey`, `WithRedisMaxEntries`), `sql.go` (`SQLJTIStore`, `NewSQLJTIStore`, `Dialect`, `Postgres`, `MySQL`, `SQLite`, `WithSQLTable`, `WithSQLMaxPerKey`, `WithSQLMaxEntries`, `WithSQLPruneEvery`, `SQLRecountEvery`, `Schema`, `EnsureSchema`, `Prune`).

**Purpose.** `dpop.JTIStore` implementations shared by every replica, so a proof accepted by one
replica is a replay on all of them (E35's memory store is per process).

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `NewRedisJTIStore(rdb, ...)` | — | nil client → error. Any `redis.Scripter` (client, cluster client) |
| `WithRedisPrefix` | `w17:dpop:jti:` | Use another prefix for the mint store |
| `WithRedisMaxPerKey` / `WithSQLMaxPerKey` | `dpop.DefaultMaxJTIsPerKey` = 10 000 | `n <= 0` keeps the default |
| `WithRedisMaxEntries` / `WithSQLMaxEntries` | `dpop.DefaultMaxJTIs` = 200 000 per store | The total cap of one store (one prefix or table); `n <= 0` keeps the default. A full store refuses with `ErrJTIStoreFull`. Size it for peak proofs per second times the jti ttl; give the mint store a smaller cap of its own |
| `NewSQLJTIStore(db, dialect, ...)` | — | nil db or unknown dialect → error |
| `WithSQLTable` | `w17_dpop_jti` | Must match `[a-z_][a-z0-9_]{0,62}` or construction fails |
| `WithSQLPruneEvery` | 256 records | `n <= 0` keeps the default |
| Schema | not created | `EnsureSchema` creates it (idempotent); `Schema()` gives the DDL for migrations |

**Shares.** E35 (`JTIStore` contract, `ErrJTIStoreFull`, `ErrUnavailable`). The Redis store
uses the repository's `go-redis` client; the SQL store `database/sql` with the caller's driver.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E38.1 | `Record` MUST insert if absent and otherwise return the stored record unchanged | `TestRedisJTIStore_Contract`, `TestSQLJTIStore_Contract_SQLite`, `TestRedisJTIStore_Contract_Live`, `TestSQLJTIStore_Contract_PostgresLive`, `TestSQLJTIStore_Contract_MySQLLive` |
| E38.2 | An expired entry MUST free its key; Redis keys MUST carry the ttl and vanish after it; expired SQL rows MUST be swept | same contract tests, `TestRedisJTIStore_KeysCarryTheTTL`, `TestSQLJTIStore_PrunesExpiredRows` |
| E38.3 | One thumbprint MUST NOT exceed its share (`ErrJTIStoreFull`); other keys MUST keep working and the share MUST free with expiry | same contract tests (`per-key share`) |
| E38.4 | Concurrent `Record`s of one key MUST have exactly one winner, and every loser MUST see the winner's record, on MySQL also with `CLIENT_FOUND_ROWS` | same contract tests (`concurrent writers, one winner`), `TestSQLJTIStore_LostRaceReadsTheWinner` |
| E38.5 | Two verifiers sharing the stores MUST refuse a proof the other accepted (`ErrReplay`) | same contract tests (`replicas sharing the store refuse a replay`) |
| E38.6 | A failing backend MUST return an error and never report a record as created; it MUST NOT read as a full store | `TestRedisJTIStore_FailingBackendFailsClosed`, `TestSQLJTIStore_FailuresAndConfig` |
| E38.7 | Invalid configuration (nil client or db, unknown dialect, unsafe table name) MUST be refused at construction | `TestRedisJTIStore_FailingBackendFailsClosed`, `TestSQLJTIStore_FailuresAndConfig` |
| E38.8 | A store MUST NOT exceed its total cap (`ErrJTIStoreFull`), MUST still return the stored record for a key it holds when full — on Redis also when the key's total-set member was lost, so a replay is never read as full (and never accepted unrecorded) — and MUST free room as entries expire (on SQL within `SQLRecountEvery`, counting its own inserts in between) | same contract tests (`total cap`), `TestSQLJTIStore_TotalCapBetweenRecounts`, `TestRedisJTIStore_FullStoreStillAnswersForARecordedJTI` |
| E38.9 | A record refused by the per-key share, or a repeat of a recorded jti, MUST NOT take a place under the total cap | same contract tests (`a refused record takes no place under the total cap`), `TestRedisJTIStore_RepeatReleasesItsReservation` |
| E38.10 | A flood of unbound proofs from throwaway keys MUST fill only the mint store: bound proofs MUST keep passing through the same backend, `MintFullAcceptUnrecorded` MUST keep minting; on Redis the flood MUST leave at most the mint cap's keys (refused proofs none), so a `noeviction` Redis keeps the memory the bound store needs | same contract tests (`a mint flood fills only the mint store`), `TestRedisJTIStore_FloodStaysWithinTheCap` |

**Deliberate limits.**
- The Redis store needs a Redis whose `maxmemory-policy` is `noeviction`, since an evicted jti
  reopens a replay window early; the total caps of the stores on it bound what each can take
  of its memory, so size `maxmemory` for both caps. The cap is reserved in a Lua script of its
  own (the total set lives in its own cluster hash slot) before the record script runs; an
  unused reservation (refused, failed, or a repeat of a recorded jti) is released, and one
  left by a crash expires with the entry's ttl. Because the reservation cannot see the jti
  key (another slot), a full answer is checked against the jti key before it is returned.
  Redis keys expire on the server's clock; the per-key count, the total and the SQL store
  judge expiry by `rec.FirstSeen`, the verifier's clock.
- The SQL total cap is judged against a count each replica keeps, recounted every
  `SQLRecountEvery` (1 s) or every prune interval of records: other replicas' inserts since
  the last count are not seen, so the table can overshoot the cap by what they insert in that
  interval, and a full table reopens up to 1 s after rows expire. Counting the table on every
  record would cost a scan per proof.
- The SQL per-key share is counted before the insert: concurrent `Record`s of one thumbprint
  can overshoot it by the number in flight.
- Live tests run against the servers named by `W17_REDIS_ADDR`, `W17_PG_DSN` and
  `W17_MYSQL_DSN`; CI requires them (`W17_REQUIRE_LIVE_REDIS`, `W17_REQUIRE_LIVE_MIGRATOR`).

---

## E39. DPoP TypeScript client

**Code.** `ts/dpop/src` (`@w17/dpop`) — `jwk.ts` (`jwkThumbprint`, `canonicalJwk`, `publicJwk`), `key.ts` (`generateKeyPair`, `dpopKey`, `loadOrCreateKey`, `KeyStore`, `MemoryKeyStore`, `IndexedDBKeyStore`), `proof.ts` (`createProof`, `signClaims`, `normalizeHTU`, `accessTokenHash`), `fetch.ts` (`createDpopFetch`, `DpopRequestInit`, `DpopOriginError`, `DpopRefusedError`, `isDeviceRevoked`, `isSessionExpired`, `isUseNonceChallenge`), `ticket.ts` (`issueStreamTicket`, `withTicket`, `DpopTicketError`), `base64url.ts`; `node.ts` (`@w17/dpop/node`: `FileKeyStore`). No runtime dependency: signing and hashing are WebCrypto (`globalThis.crypto.subtle`, browsers and Node >= 20); the package assembles the JWS and JWK JSON only. Tests are vitest under `ts/dpop/test`, named below as `file › test`; the cross-language tests are Go tests in `go/lib/dpop/ts_fixture_test.go`.

**Purpose.** The client side of E35 for browsers and Node: an ES256 device key held by
WebCrypto, its RFC 7638 thumbprint (the device id), RFC 9449 proofs, and a fetch wrapper that
attaches `Authorization: DPoP <token>` and a proof to a request with a token (a proof alone to
a mint call, nothing to any other request), follows `DPoP-Nonce`, retries once on
`use_dpop_nonce`, and turns `DEVICE_REVOKED` / `SESSION_EXPIRED` refusals into typed errors.
The generated TypeScript transport and the admin runtime are meant to embed this source.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `createDpopFetch({ origins })` | required | Each must be a bare `http(s)://host[:port]` (a trailing `/` allowed); empty, a path, a query, user info or another scheme → throws at construction |
| `keyStore` | required | `MemoryKeyStore`, `IndexedDBKeyStore`, `FileKeyStore` or the caller's own `KeyStore` |
| `getToken` | none (no `Authorization`, no `ath`) | An empty string counts as no token; a throw fails the request. Without a token a request carries DPoP only when it is a mint call |
| `mintPaths` | none | Paths whose requests are mint calls, compared with the request URL's path exactly (query ignored). Not starting with `/`, or holding `?` or `#` → throws at construction |
| `{ mint: true }` on a call | false | Marks that call as a mint call: without a token it still carries an unbound proof |
| `fetch` | `globalThis.fetch` | — |
| `IndexedDBKeyStore({ dbName, storeName, key, indexedDB })` | `w17-dpop`, `keys`, `device`, `globalThis.indexedDB` | No IndexedDB → every call rejects |
| `FileKeyStore(path, { exportPrivateKey: true })` | — | Anything but `exportPrivateKey: true`, or an empty path → throws: the store writes the private key to disk and the caller must say so. For servers, CLIs and tests |
| `issueStreamTicket(fetch, baseUrl, { path })` | `/api/ws/ticket` | Appended to `baseUrl` without its trailing `/` |

**Shares.** E35 (the wire it produces: header, claims, `NormalizeHTU`, the challenge), E36
(the same retry and refusal rules as `dpopclient`), E37 (the ticket the gateway issues to a
bound request), E3 (the error envelope a refusal is read from).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| E39.1 | The thumbprint MUST be RFC 7638: the required members only, sorted, no whitespace, SHA-256, base64url; it MUST reproduce the RFC 7638 RSA example and the RFC 9449 EC key's jkt | `jwk.test.ts › matches the RFC 7638 RSA example`, `› matches the RFC 9449 EC example's jkt`, `› hashes only the required members…`, `› is independent of member order and extra members` |
| E39.2 | A generated device key MUST be ECDSA P-256 with a non-extractable private key unless the store asks for an extractable one; a key that is not ECDSA P-256 MUST be refused | `jwk.test.ts › generates a non-extractable private key by default`, `› refuses a key that is not ECDSA P-256`, `keystore.test.ts › generates an extractable key only for a store that asks` |
| E39.3 | A proof header MUST be exactly `typ: dpop+jwt`, `alg: ES256` and a `jwk` with only `kty`, `crv`, `x`, `y` | `proof.test.ts › has the RFC 9449 header…`, `jwk.test.ts › exports exactly the four public members` |
| E39.4 | The claims MUST be `jti` (fresh, 128 random bits), `htm`, `htu`, `iat` (seconds), plus `ath` only with a non-empty token (the RFC 9449 example value) and `nonce` only when one is known | `proof.test.ts › carries jti, htm, htu and iat…`, `› sets ath to the token hash…`, `› draws a fresh 128-bit jti per proof`, `› computes the RFC's ath` |
| E39.5 | The signature MUST be the raw 64-byte r‖s that WebCrypto verifies against the embedded key, and MUST NOT verify once a claim or the key changes; the test's verifier MUST accept the RFC 9449 example proofs and refuse one with a flipped bit | `proof.test.ts › is signed with a raw 64-byte r‖s…`, `› does not verify once a claim is changed`, `› does not verify under another key`, `› verifies the RFC's example proofs`, `› refuses an RFC proof with one bit of its signature flipped` |
| E39.6 | `normalizeHTU` MUST agree with the Go `NormalizeHTU` table: lower-case scheme and host, no default port, `/` for an empty path, no query or fragment, user info and a missing authority refused | `proof.test.ts › normalises like the Go verifier`, `› refuses what the Go verifier refuses` |
| E39.7 | The Go verifier MUST accept proofs this package made — direct, and through the fetch wrapper's nonce retry to the ticket issuer and on a tokenless mint call — and return the thumbprint the TypeScript side computed; it MUST refuse each of them with another nonce, method, path, token, binding or signature | `TestTSProofsVerify`, `TestTSProofsRefusedWhenTheirBindingsChange`, `TestTSThumbprintsMatch` (fixture `go/lib/dpop/testdata/ts_proofs.json`, regenerated with `npm run fixture` in `ts/dpop`) |
| E39.8 | A request to an allowed origin with a token MUST carry `Authorization: DPoP <token>` and a proof for its method and URL (no query or fragment) with the token's `ath`; a mint call without a token, neither `Authorization` nor `ath` | `fetch.test.ts › attaches Authorization: DPoP and a proof…`, `› sends no Authorization and no ath on a mint call without a token` |
| E39.9 | A request to any other origin (another host, a suffix host, another scheme or port) MUST throw `DpopOriginError` before anything is sent or the token is read; the origin checked MUST be the one the built `Request` resolved (a URL-like object that is not a `URL`, a relative URL under a document base), never the input; a configuration without a valid origin MUST throw | `fetch.test.ts › throws for any other origin before sending or reading the token`, `› checks a URL-like object that is not a URL by what the Request resolves`, `› checks a relative URL as the Request resolves it, not against location`, `› matches origins after normalisation`, `› refuses a configuration without a valid origin` |
| E39.10 | A 401 with `use_dpop_nonce` and a new `DPoP-Nonce` MUST be retried exactly once, with a new proof carrying that nonce and the same body, a streamed body included; a second refusal MUST be returned and its nonce learned | `fetch.test.ts › retries a use_dpop_nonce refusal once…`, `› re-sends a streamed body on the retry`, `› retries exactly once against a server that keeps asking` |
| E39.11 | A 401 without the challenge, without a nonce, or naming the nonce the proof already carried MUST NOT be retried; the challenge MUST be recognised only as the DPoP scheme | `fetch.test.ts › does not retry without a challenge…`, `› finds the DPoP challenge alone or among others`, `› ignores other errors, other schemes and look-alikes` |
| E39.12 | A nonce sent on any response MUST be used by the next proof to that origin, and only that origin | `fetch.test.ts › learns a nonce from a success and keeps nonces per origin` |
| E39.13 | The caller's `Request` MUST NOT be modified; the sent request MUST refuse redirects; a `no-cors` request MUST be refused | `fetch.test.ts › does not modify the caller's Request, and refuses redirects`, `› refuses a no-cors request…` |
| E39.14 | Concurrent first requests MUST share one generated key; a failed key load MUST NOT be remembered | `fetch.test.ts › generates one key for concurrent first requests and keeps it`, `› retries the key load after a failing store…` |
| E39.15 | A 401 or 403 whose JSON envelope names `DEVICE_REVOKED` or `SESSION_EXPIRED` in `error.details[].code` MUST throw `DpopRefusedError` with the reason, status and a still-readable response; anything else MUST be returned as is; `discardKey` MUST clear the store and the nonces so the next request signs with a new key | `fetch.test.ts › throws DEVICE_REVOKED as a typed error that can discard the key`, `› throws SESSION_EXPIRED, from a 403 too`, `› returns every other refusal as a plain response`, `› forgets nonces with the key`, `› discards unconditionally without a thumbprint` |
| E39.16 | `IndexedDBKeyStore` MUST persist the CryptoKeyPair itself, so a reload gets the same non-extractable key, still able to sign; `FileKeyStore` MUST write PKCS #8 PEM (the format of Go `dpop.Key.SaveFile`) with mode 0600, replacing an existing file | `keystore.test.ts › persists a non-extractable key across store instances…`, `› keeps keys under different record keys apart`, `› writes a PKCS #8 PEM with mode 0600…`, `› replaces an existing file…`, `› must be told it writes the private key out` |
| E39.17 | `issueStreamTicket` MUST make a signed, token-bound POST to the issuer path (following a nonce challenge) and return the ticket; a refusal or a body without a ticket MUST throw `DpopTicketError`, a revoked device `DpopRefusedError` | `ticket.test.ts › POSTs a signed, token-bound request…`, `› takes a configured path…`, `› throws DpopTicketError…`, `› lets a revoked device surface as DpopRefusedError` |
| E39.18 | A refusal MUST carry the thumbprint of the key that signed the refused request, and its `discardKey` MUST discard only that key: a key made since then MUST survive a stale refusal; a request made while a discard is in progress MUST wait for it and MUST NOT sign with or reload the key being discarded | `fetch.test.ts › tags a refusal with the key it was raised for`, `› does not let a stale refusal discard a newer key`, `› makes requests wait for a discard instead of reloading the revoked key` |
| E39.19 | A refusal body MUST be read only up to `MAX_REFUSAL_BODY` (64 KiB) before parsing; a longer one MUST be returned as a plain response with its body intact | `fetch.test.ts › does not parse a refusal body over the cap, and leaves it readable`, `› still recognises a refusal just under the cap` |
| E39.20 | A request with no token that is not a mint call (`{ mint: true }` or a path in `mintPaths`, matched exactly) MUST be sent with no `DPoP` and no `Authorization` header, still refusing redirects, without loading or making a key, and its refusal MUST come back as a plain response; a mint call or a token MUST make it carry a proof; a mint path that is not a path MUST throw | `fetch.test.ts › sends a request without a token and not a mint call with no DPoP headers…`, `› signs a mint call marked on the call, and a token request`, `› signs a request to a configured mint path, by its exact path`, `› refuses a mint path that is not a path`, `› returns a refusal of an unsigned request as a plain response` |

**Deliberate limits.**
- ES256 only. The Go verifier also accepts EdDSA, but Ed25519 in WebCrypto is not available in
  every browser.
- Redirects are refused, not followed: a proof is bound to the URL it was made for, so the
  redirect target would refuse it, and following would hand it the token.
- Only a 401 with `WWW-Authenticate: DPoP error="use_dpop_nonce"` triggers the retry, as in E36.
- Nonces live in memory per `createDpopFetch` instance, per origin.
- Refusal reasons are read only from `error.details[].code` of a JSON 401 or 403 of at most 64 KiB.
- The key is never discarded on its own: after `DEVICE_REVOKED` the caller decides when to
  call `discardKey`.
- `IndexedDBKeyStore` is tested against `fake-indexeddb` in Node, not in a browser.
- `FileKeyStore` writes the private key unencrypted (mode 0600); a browser has no use for it.
- Which calls are mint calls is the caller's to say; a tokenless sign-in call that is neither
  marked nor under `mintPaths` reaches the server with no proof and is refused there.
