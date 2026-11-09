/**
 * w17 business SDK for TypeScript (Node 24).
 *
 * Serves a domain's business tier from what codegen wrote — `descriptors.binpb`
 * and `w17-business.json` — with sdk/go's semantics: header forwarding,
 * distributed transactions, events after commit, `w17.ErrorDetail` errors.
 *
 * The project provides one function, in `<register.path>/w17.ts`:
 *
 *     export function registerBusiness(cfg, registry, clients) {
 *       registry.register("notes.v1.NoteService", new NoteService(cfg, registry, clients));
 *     }
 *
 * and the handler methods, named exactly as the RPCs, each taking the context
 * first: `async CreateNote(ctx, req)`. Pass `ctx` on to every call.
 */

export * as distx from "./distx.js";
export * as egress from "./egress.js";
export * as events from "./events.js";
export * as files from "./files.js";
export * as paging from "./paging.js";
export * as principal from "./principal.js";
export * as txscope from "./txscope.js";
export * as observability from "./observability.js";
export * as tlsconfig from "./tlsconfig.js";

export { Clients, App, BootError } from "./runtime.js";
export { CompanionConn, Conn, ServiceClient, TxCoordinator, type CallOptions } from "./client.js";
export { Config, ConfigError } from "./config.js";
export { Context, Metadata, background, type ContextInit, type MetadataInit } from "./context.js";
export { Pool, getDefaultPool, type AnyMessage, type MessageType, type MethodInfo } from "./descriptors.js";
export { onCommit } from "./distx.js";
export { Status, StatusError, UserError, errorDetail, forUser, ErrorDetail, errorInfo, type ErrorInfo } from "./errors.js";
export { FileStore, type FileInfo } from "./files.js";
export { Secret } from "./secrets.js";
export { Registry, RegistrationError, Served, type ShutdownHook } from "./server.js";
export { load as loadManifest, type Manifest, ManifestError } from "./manifest.js";
