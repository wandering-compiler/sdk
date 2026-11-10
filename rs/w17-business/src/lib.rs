//! The w17 business-tier SDK for Rust.
//!
//! A domain's business tier written in Rust is one binary crate. Its
//! `build.rs` compiles the generated `descriptors.binpb` with
//! [`w17-business-build`](https://github.com/wandering-compiler/sdk/tree/main/rs/w17-business/build),
//! the author implements the tonic traits that generates for the domain's
//! business services, and this crate serves them with the middleware the Go,
//! Python and TypeScript tiers apply — the context, header forwarding,
//! deadlines, distributed transactions, events after commit, the companion's
//! egress clients and file stores, health, reflection, telemetry and one
//! shutdown budget.
//!
//! ```ignore
//! mod pb { w17_business::include_protos!(); }
//!
//! w17_business::main!(register_business);
//!
//! fn register_business(cfg: &Config, registry: &mut Registry, clients: &Clients) -> Result<(), BootError> {
//!     registry.register(CheckoutServiceServer::new(Checkout {
//!         orders: OrderMutationClient::new(clients.storage()),
//!         tx: clients.distributed_transaction()?,
//!     }))
//! }
//! ```
//!
//! See the crate's README for the whole author guide.

pub mod channel;
pub mod config;
pub mod ctx;
pub mod distx;
pub mod egress;
pub mod errors;
pub mod events;
pub mod files;
pub mod manifest;
pub mod metadata;
pub mod observability;
pub mod paging;
pub mod pb;
pub mod principal;
pub mod runtime;
pub mod secrets;
pub mod server;
pub mod tlsconfig;
pub mod txscope;

#[cfg(test)]
mod tests;

use std::{fmt, future::Future};

pub use channel::{CompanionChannel, StorageChannel};
pub use config::{Config, ConfigError};
pub use ctx::{Ctx, CtxError, ctx};
pub use errors::{error_detail, for_user, for_user_dev};
pub use metadata::Metadata;
pub use runtime::{Clients, RegisterFn, run};
pub use secrets::Secret;
pub use server::{Registry, SelfChannel};
pub use tonic::{Code, Request, Response, Status};
pub use txscope::Outcome;

/// The codec the generated code names (`tonic-prost-build`'s `codec_path`),
/// so an author's crate needs no `tonic-prost` of its own.
pub mod codec {
    pub use tonic_prost::ProstCodec;
}

/// The tier cannot start; the message names what to fix. A register
/// function returns it, and the binary exits 1 printing it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BootError(pub String);

impl fmt::Display for BootError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for BootError {}

impl From<BootError> for Status {
    fn from(e: BootError) -> Self {
        Status::internal(e.0)
    }
}

/// What an [`on_commit`] future may end with: nothing, or a result whose
/// error is reported.
pub trait HookOutput {
    /// `Err` with the failure's text.
    fn into_result(self) -> Result<(), String>;
}

impl HookOutput for () {
    fn into_result(self) -> Result<(), String> {
        Ok(())
    }
}

impl<T, E: fmt::Display> HookOutput for Result<T, E> {
    fn into_result(self) -> Result<(), String> {
        self.map(|_| ()).map_err(|e| e.to_string())
    }
}

/// Run `work` once the transaction `ctx` belongs to commits; never if it
/// rolls back; at once outside a transaction or after its commit.
///
/// Outside a transaction (or after its commit) the work runs now, before
/// this returns, and its failure (an `Err`, or a panic) is returned — it is
/// the caller's own code on the caller's own path, as in Go, Python and
/// TypeScript. It runs to completion even if the caller is dropped meanwhile.
///
/// Inside a transaction the work is parked and the result is
/// `Ok(Outcome::Deferred)`; it runs in order after the root commit, before
/// the commit returns, and a failure then is reported, never returned into
/// the commit (the transaction has landed). Inside a nested [`distx::run`]
/// that JOINED an outer transaction, it waits for the OUTER commit. The hook
/// the event middleware itself uses, offered for any side effect that must
/// not outrun the write it describes (an e-mail, an external call). Go's
/// twin is `distx.OnCommit`.
///
/// Parked work outlives the request: build what it needs before
/// (`ctx.without_cancel()` for a call it makes).
pub async fn on_commit<F>(ctx: &Ctx, work: F) -> Result<Outcome, String>
where
    F: Future + Send + 'static,
    F::Output: HookOutput,
{
    let hook: txscope::Hook = Box::pin(async move { work.await.into_result() });
    match txscope::after_commit(ctx, hook) {
        (Outcome::Discarded, _) => {
            observability::report_error(
                "distx: on_commit work dropped: it was registered after its transaction rolled back",
            );
            Ok(Outcome::Discarded)
        }
        (out, Some(hook)) => {
            txscope::shielded(txscope::run_hook(hook)).await??;
            Ok(out)
        }
        (out, None) => Ok(out),
    }
}

/// The binary's `main`: `w17_business::main!(register_business);` expands to
/// `fn main()` that hands `register_business` to [`run`] with the SHA-256 of
/// the descriptor set the crate was compiled from (set by
/// `w17_business_build::compile()`), so a binary built against an older
/// proto refuses to serve a newer manifest.
#[macro_export]
macro_rules! main {
    ($register:path) => {
        fn main() -> ::std::process::ExitCode {
            $crate::run(::core::env!("W17_DESCRIPTORS_SHA256"), $register)
        }
    };
}

/// The Rust code `w17_business_build::compile()` generated from the
/// descriptor set, as nested modules by proto package:
/// `mod pb { w17_business::include_protos!(); }`, then
/// `pb::shop::core::OrderMutationClient`. Every message of the descriptor
/// set is generated, so the ones the crate does not use are not warned
/// about.
#[macro_export]
macro_rules! include_protos {
    () => {
        #[allow(dead_code, missing_docs, clippy::all, clippy::pedantic)]
        mod __w17_protos {
            include!(concat!(env!("OUT_DIR"), "/w17_protos.rs"));
        }
        #[allow(unused_imports)]
        pub use __w17_protos::*;
    };
}
