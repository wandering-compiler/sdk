//! rustls config construction for both proxy TLS legs.
//!
//! The proxy is a re-encrypting L7 hop: it terminates client TLS to read
//! the `:path` + `w17-conn-id` routing headers, then re-dials the chosen
//! backend replica over its own TLS connection. This module turns the
//! env-driven [`crate::config::ServerTls`] / [`crate::config::BackendTls`]
//! into rustls `ServerConfig` / `ClientConfig`, mirroring the Go
//! `grpcserver.TLSServerOption` / `grpcclient.TLSDialOption` posture:
//!
//!   * server leg — server-auth TLS by default; mTLS (verify client cert)
//!     switches on only when a client-CA is configured;
//!   * client leg — verify the backend against a pinned CA when configured,
//!     else the system roots. Verification is ALWAYS real: there is no
//!     skip-verify (the stack-wide switch turns TLS on only with SAN-correct
//!     dev/prod leaves, so the hostname check always passes).
//!
//! Pure-rustls with the `ring` crypto provider — no OpenSSL, consistent
//! with the slim runtime image. ALPN advertises `h2` on both legs so the
//! gRPC (HTTP/2) handshake negotiates cleanly.

use std::fs::File;
use std::io::BufReader;
use std::sync::Arc;

use rustls::crypto::CryptoProvider;
use rustls::pki_types::{CertificateDer, PrivateKeyDer};
use rustls::server::WebPkiClientVerifier;
use rustls::{ClientConfig, RootCertStore, ServerConfig};

use crate::config::{BackendTls, ServerTls};

/// ALPN protocol list advertised on both legs — gRPC rides HTTP/2 only.
const ALPN_H2: &[&[u8]] = &[b"h2"];

fn provider() -> Arc<CryptoProvider> {
    Arc::new(rustls::crypto::ring::default_provider())
}

/// Build the server-leg rustls config (clients → proxy). Server-auth TLS;
/// mTLS client verification when `client_ca_path` is set.
pub fn build_server_config(t: &ServerTls) -> Result<ServerConfig, String> {
    let certs = load_certs(&t.cert_path)?;
    let key = load_key(&t.key_path)?;

    let builder = ServerConfig::builder_with_provider(provider())
        .with_safe_default_protocol_versions()
        .map_err(|e| format!("server tls: protocol versions: {e}"))?;

    let builder = match &t.client_ca_path {
        Some(ca) => {
            let roots = load_root_store(ca)?;
            let verifier = WebPkiClientVerifier::builder_with_provider(Arc::new(roots), provider())
                .build()
                .map_err(|e| format!("server mTLS client verifier ({ca}): {e}"))?;
            builder.with_client_cert_verifier(verifier)
        }
        None => builder.with_no_client_auth(),
    };

    let mut config = builder
        .with_single_cert(certs, key)
        .map_err(|e| format!("server tls: cert/key: {e}"))?;
    config.alpn_protocols = ALPN_H2.iter().map(|p| p.to_vec()).collect();
    Ok(config)
}

/// Build the client-leg rustls config (proxy → backends). Verifies the
/// backend against a pinned CA when set, else the system roots — always
/// real verification. Presents a client cert (mTLS) when `client_identity`
/// is set.
pub fn build_client_config(t: &BackendTls) -> Result<ClientConfig, String> {
    let builder = ClientConfig::builder_with_provider(provider())
        .with_safe_default_protocol_versions()
        .map_err(|e| format!("backend tls: protocol versions: {e}"))?;

    // Verification posture — pinned CA or system roots; always verified.
    let roots = match &t.ca_path {
        Some(ca) => load_root_store(ca)?,
        None => native_root_store()?,
    };
    let verified = builder.with_root_certificates(roots);

    // Client identity (mTLS) — opt-in. NOTE: ALPN is intentionally left
    // unset here — the hyper-rustls connector sets `h2` itself in
    // `.enable_http2()` and panics if it is pre-defined on the config.
    let config = match &t.client_identity {
        Some((cert, key)) => {
            let chain = load_certs(cert)?;
            let key = load_key(key)?;
            verified
                .with_client_auth_cert(chain, key)
                .map_err(|e| format!("backend mTLS client cert/key: {e}"))?
        }
        None => verified.with_no_client_auth(),
    };
    Ok(config)
}

/// A never-exercised client config, used only to satisfy the uniform
/// `HttpsConnector` type when the proxy dials plaintext (`http://`)
/// backends. Empty roots + no client auth; no warning printed.
pub fn placeholder_client_config() -> Result<ClientConfig, String> {
    // ALPN left unset — hyper-rustls's `.enable_http2()` sets `h2` and
    // panics if it is pre-defined here.
    let config = ClientConfig::builder_with_provider(provider())
        .with_safe_default_protocol_versions()
        .map_err(|e| format!("placeholder tls: protocol versions: {e}"))?
        .with_root_certificates(RootCertStore::empty())
        .with_no_client_auth();
    Ok(config)
}

fn load_certs(path: &str) -> Result<Vec<CertificateDer<'static>>, String> {
    let mut rd = BufReader::new(File::open(path).map_err(|e| format!("open cert {path:?}: {e}"))?);
    let certs: Result<Vec<_>, _> = rustls_pemfile::certs(&mut rd).collect();
    let certs = certs.map_err(|e| format!("parse cert {path:?}: {e}"))?;
    if certs.is_empty() {
        return Err(format!("no PEM certificates found in {path:?}"));
    }
    Ok(certs)
}

fn load_key(path: &str) -> Result<PrivateKeyDer<'static>, String> {
    let mut rd = BufReader::new(File::open(path).map_err(|e| format!("open key {path:?}: {e}"))?);
    rustls_pemfile::private_key(&mut rd)
        .map_err(|e| format!("parse key {path:?}: {e}"))?
        .ok_or_else(|| format!("no private key found in {path:?}"))
}

fn load_root_store(path: &str) -> Result<RootCertStore, String> {
    let mut store = RootCertStore::empty();
    let mut rd = BufReader::new(File::open(path).map_err(|e| format!("open CA {path:?}: {e}"))?);
    let certs: Result<Vec<_>, _> = rustls_pemfile::certs(&mut rd).collect();
    let certs = certs.map_err(|e| format!("parse CA {path:?}: {e}"))?;
    if certs.is_empty() {
        return Err(format!("no PEM certificates found in CA {path:?}"));
    }
    for c in certs {
        store
            .add(c)
            .map_err(|e| format!("add CA cert from {path:?}: {e}"))?;
    }
    Ok(store)
}

fn native_root_store() -> Result<RootCertStore, String> {
    let mut store = RootCertStore::empty();
    let loaded = rustls_native_certs::load_native_certs();
    for e in loaded.errors {
        tracing::warn!(error = %e, "loading a native root cert");
    }
    if loaded.certs.is_empty() {
        return Err(
            "backend TLS is on but no CA was configured and no system root certificates \
             were found — set W17_INTERNAL_TLS_CA to the backend's CA (dev PKI: \
             w17/certs/dev.local/ca.crt)"
                .to_string(),
        );
    }
    let (added, _ignored) = store.add_parsable_certificates(loaded.certs);
    if added == 0 {
        return Err("no usable system root certificates for backend TLS".to_string());
    }
    Ok(store)
}
