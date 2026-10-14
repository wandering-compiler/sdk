//! storage-proxy library surface.
//!
//! A contract-blind L7 gRPC proxy that pins distributed-transaction RPCs
//! to the replica that opened the tx, routing by `:path` + `w17-conn-id`
//! metadata only — it NEVER decodes protobuf message bodies. The binary
//! (`src/main.rs`) is a thin wrapper over [`server::run`]; the routing
//! logic lives in [`router`] so it is unit- and integration-testable.

pub mod affinity;
pub mod config;
pub mod forward;
pub mod lb;
pub mod otel;
pub mod router;
pub mod server;
pub mod tls;
