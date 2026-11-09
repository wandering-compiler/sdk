//! Internal TLS, read from the same variables as sdk/go/lib/grpcclient and
//! lib/grpcserver.
//!
//! `W17_INTERNAL_TLS`:
//!
//! - `on` | `true` | `1` | `yes` — servers listen TLS, clients dial TLS;
//! - `terminated` — TLS ends at the platform: servers listen plain, clients
//!   still dial TLS (system roots, no client pair);
//! - `off` | `false` | `0` | `no` | unset — plaintext;
//! - anything else refuses the boot (a typo must not silently downgrade).
//!
//! `W17_INTERNAL_TLS_CERT` / `_KEY` are this service's leaf pair (required
//! for a TLS server, presented as the client pair when dialing);
//! `W17_INTERNAL_TLS_CA` is the roots a client verifies against (that CA
//! alone — never the system roots beside it); `W17_INTERNAL_TLS_CLIENT_CA`,
//! when set on a server, requires and verifies a client certificate.
//!
//! rustls with the `ring` provider, as `rs/storage-proxy`.

use std::sync::Arc;

use rustls::{
    RootCertStore, ServerConfig,
    pki_types::{CertificateDer, PrivateKeyDer, pem::PemObject as _},
    server::WebPkiClientVerifier,
};
use tonic::transport::{Certificate, ClientTlsConfig, Identity};

use crate::secrets::Env;

pub const ENV_INTERNAL_TLS: &str = "W17_INTERNAL_TLS";
pub const ENV_INTERNAL_TLS_CA: &str = "W17_INTERNAL_TLS_CA";
pub const ENV_INTERNAL_TLS_CERT: &str = "W17_INTERNAL_TLS_CERT";
pub const ENV_INTERNAL_TLS_KEY: &str = "W17_INTERNAL_TLS_KEY";
pub const ENV_INTERNAL_TLS_CLIENT_CA: &str = "W17_INTERNAL_TLS_CLIENT_CA";

fn norm(v: &str) -> String {
    v.trim().to_ascii_lowercase()
}

pub fn enabled(v: &str) -> bool {
    ["on", "true", "1", "yes"].contains(&norm(v).as_str())
}

pub fn terminated(v: &str) -> bool {
    norm(v) == "terminated"
}

fn off(v: &str) -> bool {
    ["", "off", "false", "0", "no"].contains(&norm(v).as_str())
}

/// Refuses a value that is not recognised.
pub fn check(v: &str) -> Result<(), String> {
    if enabled(v) || terminated(v) || off(v) {
        return Ok(());
    }
    Err(format!(
        "{ENV_INTERNAL_TLS}={v:?} is not a recognised value; accepted: on|true|1|yes (TLS), terminated (TLS ends at \
         the platform; servers listen plain), off|false|0|no or unset (plaintext)"
    ))
}

pub fn dial_tls(v: &str) -> bool {
    enabled(v) || terminated(v)
}

fn get<'a>(env: &'a Env, k: &str) -> &'a str {
    env.get(k).map(String::as_str).unwrap_or("")
}

fn read(path: &str, variable: &str) -> Result<Vec<u8>, String> {
    std::fs::read(path).map_err(|e| format!("reading {variable} at {path:?}: {e}"))
}

/// The certificates of a PEM bundle; one that holds none is refused naming
/// the variable (Go's AppendCertsFromPEM finding nothing).
fn certs(pem: &[u8], path: &str, variable: &str) -> Result<Vec<CertificateDer<'static>>, String> {
    let certs: Vec<CertificateDer<'static>> = CertificateDer::pem_slice_iter(pem)
        .filter_map(Result::ok)
        .collect();
    if certs.is_empty() {
        return Err(format!("{variable} at {path:?}: no PEM certs found"));
    }
    Ok(certs)
}

fn pair(env: &Env) -> Result<(Vec<u8>, Vec<u8>), String> {
    let cert = get(env, ENV_INTERNAL_TLS_CERT);
    let key = get(env, ENV_INTERNAL_TLS_KEY);
    let where_ = format!("({ENV_INTERNAL_TLS_CERT}={cert:?} / {ENV_INTERNAL_TLS_KEY}={key:?})");
    let c = std::fs::read(cert).map_err(|e| format!("loading the cert/key pair {where_}: {e}"))?;
    let k = std::fs::read(key).map_err(|e| format!("loading the cert/key pair {where_}: {e}"))?;
    let chain = certs(&c, cert, ENV_INTERNAL_TLS_CERT)
        .map_err(|e| format!("loading the cert/key pair {where_}: {e}"))?;
    let der = PrivateKeyDer::from_pem_slice(&k)
        .map_err(|e| format!("loading the cert/key pair {where_}: no private key: {e}"))?;
    // The pair must match, as Go's tls.X509KeyPair checks it.
    let provider = rustls::crypto::ring::default_provider();
    let signing = provider
        .key_provider
        .load_private_key(der)
        .map_err(|e| format!("loading the cert/key pair {where_}: {e}"))?;
    rustls::sign::CertifiedKey::new(chain, signing)
        .keys_match()
        .map_err(|e| format!("loading the cert/key pair {where_}: {e}"))?;
    Ok((c, k))
}

/// What a client dials with: `None` for plaintext.
///
/// An unrecognised value dials TLS (Go's grpcx.InternalTLSEnabled logs and
/// dials TLS: refusing a call is safer than sending it in the clear). Under
/// `terminated` the platform's certificate is verified against the system
/// roots and the stack's CA and pair are not used. Refused — the boot stops
/// — for a CA that cannot be read or holds no certificate, a cert/key pair
/// half set, or a pair that does not load.
pub fn client_tls(env: &Env) -> Result<Option<ClientTlsConfig>, String> {
    let v = get(env, ENV_INTERNAL_TLS);
    if check(v).is_ok() && !dial_tls(v) {
        return Ok(None);
    }
    if terminated(v) {
        return Ok(Some(ClientTlsConfig::new().with_native_roots()));
    }
    let mut cfg = ClientTlsConfig::new();
    let ca = get(env, ENV_INTERNAL_TLS_CA);
    if ca.is_empty() {
        cfg = cfg.with_native_roots();
    } else {
        let pem = read(ca, ENV_INTERNAL_TLS_CA)?;
        certs(&pem, ca, ENV_INTERNAL_TLS_CA)?;
        cfg = cfg.ca_certificate(Certificate::from_pem(pem));
    }
    let cert = get(env, ENV_INTERNAL_TLS_CERT);
    let key = get(env, ENV_INTERNAL_TLS_KEY);
    if cert.is_empty() != key.is_empty() {
        return Err(format!(
            "{ENV_INTERNAL_TLS_CERT} / {ENV_INTERNAL_TLS_KEY} are not both set — set both (this service's client pair \
             for mutual TLS) or neither"
        ));
    }
    if !cert.is_empty() {
        let (c, k) = pair(env)?;
        cfg = cfg.identity(Identity::from_pem(c, k));
    }
    Ok(Some(cfg))
}

/// The TLS a server listens with: `None` for plaintext.
///
/// Refused for a value that is not recognised, TLS on without a loadable
/// leaf pair, or a `W17_INTERNAL_TLS_CLIENT_CA` that cannot be read. Client
/// certificates are verified against that CA alone, never the system's.
pub fn server_tls(env: &Env) -> Result<Option<Arc<ServerConfig>>, String> {
    let v = get(env, ENV_INTERNAL_TLS);
    check(v)?;
    if !enabled(v) {
        return Ok(None);
    }
    let cert = get(env, ENV_INTERNAL_TLS_CERT);
    let key = get(env, ENV_INTERNAL_TLS_KEY);
    if cert.is_empty() || key.is_empty() {
        return Err(format!(
            "{ENV_INTERNAL_TLS}=on but {ENV_INTERNAL_TLS_CERT} / {ENV_INTERNAL_TLS_KEY} are not both set — point them \
             at this service's leaf cert+key (dev PKI: w17/certs/dev.local/)"
        ));
    }
    let (c, k) = pair(env)?;
    let chain = certs(&c, cert, ENV_INTERNAL_TLS_CERT)?;
    let der: PrivateKeyDer<'static> =
        PrivateKeyDer::from_pem_slice(&k).map_err(|e| format!("no private key: {e}"))?;
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let builder = ServerConfig::builder_with_provider(provider.clone())
        .with_safe_default_protocol_versions()
        .map_err(|e| e.to_string())?;
    let client_ca = get(env, ENV_INTERNAL_TLS_CLIENT_CA);
    let mut cfg = if client_ca.is_empty() {
        builder
            .with_no_client_auth()
            .with_single_cert(chain, der)
            .map_err(|e| e.to_string())?
    } else {
        let pem = read(client_ca, ENV_INTERNAL_TLS_CLIENT_CA)?;
        let mut roots = RootCertStore::empty();
        for c in certs(&pem, client_ca, ENV_INTERNAL_TLS_CLIENT_CA)? {
            roots
                .add(c)
                .map_err(|e| format!("{ENV_INTERNAL_TLS_CLIENT_CA}: {e}"))?;
        }
        let verifier = WebPkiClientVerifier::builder_with_provider(Arc::new(roots), provider)
            .build()
            .map_err(|e| format!("{ENV_INTERNAL_TLS_CLIENT_CA}: {e}"))?;
        builder
            .with_client_cert_verifier(verifier)
            .with_single_cert(chain, der)
            .map_err(|e| e.to_string())?
    };
    cfg.alpn_protocols = vec![b"h2".to_vec()];
    Ok(Some(Arc::new(cfg)))
}

/// The first dNSName of a PEM certificate's subjectAltName. The health probe
/// verifies the listener against it, as Go's does: the leaf names this
/// service, not 127.0.0.1.
pub fn leaf_server_name(cert_path: &str) -> Option<String> {
    let pem = std::fs::read(cert_path).ok()?;
    let der = CertificateDer::pem_slice_iter(&pem).next()?.ok()?;
    let (_, cert) = x509_parser::parse_x509_certificate(der.as_ref()).ok()?;
    let san = cert.subject_alternative_name().ok()??;
    san.value.general_names.iter().find_map(|n| match n {
        x509_parser::extensions::GeneralName::DNSName(d) => Some(d.to_string()),
        _ => None,
    })
}

/// The OTLP exporter's TLS: the mesh's CA and client pair (system roots
/// without a CA); system roots alone under `terminated`.
pub fn otlp_tls(env: &Env) -> Result<ClientTlsConfig, String> {
    if terminated(get(env, ENV_INTERNAL_TLS)) {
        return Ok(ClientTlsConfig::new().with_native_roots());
    }
    let mut cfg = ClientTlsConfig::new();
    let ca = get(env, ENV_INTERNAL_TLS_CA);
    match std::fs::read(ca) {
        Ok(pem) if !ca.is_empty() => cfg = cfg.ca_certificate(Certificate::from_pem(pem)),
        _ => cfg = cfg.with_native_roots(),
    }
    let (cert, key) = (
        get(env, ENV_INTERNAL_TLS_CERT),
        get(env, ENV_INTERNAL_TLS_KEY),
    );
    if let (Ok(c), Ok(k)) = (std::fs::read(cert), std::fs::read(key)) {
        cfg = cfg.identity(Identity::from_pem(c, k));
    }
    Ok(cfg)
}
