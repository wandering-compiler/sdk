//! The build helper of a w17 Rust business tier.
//!
//! The author's `build.rs` is one call:
//!
//! ```ignore
//! fn main() -> Result<(), Box<dyn std::error::Error>> {
//!     w17_business_build::compile()
//! }
//! ```
//!
//! [`compile`] finds the tier's manifest (`w17-business.json`), reads the
//! descriptor set it names (`descriptors.binpb`, written by `w17ctl codegen`)
//! and runs `tonic-prost-build` over it — no `protoc`, no `.proto` text, so
//! the build needs nothing the descriptor set does not carry. Server and
//! client code are generated for every service in it; a missing method is a
//! compile error, never an `Unimplemented` at run time.
//!
//! What the SDK owns maps onto the SDK's own types, so a message that
//! crosses into the SDK is one type, not two: `.w17` (the vocabulary, the
//! transaction coordinator, the companion's file service) onto
//! `::w17_business::pb::w17`, `.google.protobuf` onto
//! `::w17_business::pb::google::protobuf`, and the codec onto
//! `::w17_business::codec`. The crate needs `tonic` and `prost` of its own,
//! because the generated code names them; nothing else.
//!
//! The generated code is one include file, `w17_protos.rs` in `OUT_DIR`,
//! with a module per proto package: `w17_business::include_protos!()`
//! includes it. The build also sets `W17_DESCRIPTORS_SHA256`, which
//! `w17_business::main!` hands the runtime so a binary refuses to serve a
//! descriptor set other than the one it was compiled from.

use std::{
    env, fmt, fs,
    path::{Path, PathBuf},
};

use prost::Message as _;
use sha2::{Digest as _, Sha256};

/// The variable that names the manifest outright (the image sets it to
/// `/app/w17-business.json`).
pub const ENV_MANIFEST: &str = "W17_BUSINESS_MANIFEST";
/// The variable the build sets for `w17_business::main!`.
pub const ENV_DIGEST: &str = "W17_DESCRIPTORS_SHA256";
/// The include file the build writes into `OUT_DIR`.
pub const INCLUDE_FILE: &str = "w17_protos.rs";

/// What the build helper could not do; the message names it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Error(pub String);

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "w17-business-build: {}", self.0)
    }
}

impl std::error::Error for Error {}

fn err<T>(msg: impl Into<String>) -> Result<T, Error> {
    Err(Error(msg.into()))
}

/// The manifest of the crate being built: `$W17_BUSINESS_MANIFEST` when set,
/// else found from `CARGO_MANIFEST_DIR` (see [`find_manifest`]).
pub fn locate_manifest() -> Result<PathBuf, Error> {
    if let Ok(p) = env::var(ENV_MANIFEST)
        && !p.is_empty()
    {
        return Ok(PathBuf::from(p));
    }
    let dir = env::var("CARGO_MANIFEST_DIR")
        .map_err(|_| Error("CARGO_MANIFEST_DIR is not set: run from build.rs".into()))?;
    find_manifest(Path::new(&dir))
}

/// The manifest whose `register.path` is `crate_dir`: walks up from it to
/// the first directory holding `w17/services/`, reads every
/// `w17/services/*-business/w17-business.json` there, and takes the one whose
/// `language` is `rust` and whose `register.path` is the crate's directory
/// relative to that root. None, or more than one, is an error naming what it
/// looked for.
pub fn find_manifest(crate_dir: &Path) -> Result<PathBuf, Error> {
    let crate_dir = crate_dir
        .canonicalize()
        .unwrap_or_else(|_| crate_dir.to_path_buf());
    let mut root = crate_dir.as_path();
    loop {
        if root.join("w17").join("services").is_dir() {
            break;
        }
        root = match root.parent() {
            Some(p) => p,
            None => {
                return err(format!(
                    "no w17/services/ above {}: set {ENV_MANIFEST} to the tier's w17-business.json, or build the crate \
                     inside the project after `w17ctl codegen`",
                    crate_dir.display()
                ));
            }
        };
    }
    let rel = crate_dir
        .strip_prefix(root)
        .unwrap_or(&crate_dir)
        .to_string_lossy()
        .replace('\\', "/");
    let services = root.join("w17").join("services");
    let mut entries: Vec<PathBuf> = fs::read_dir(&services)
        .map_err(|e| Error(format!("{}: {e}", services.display())))?
        .filter_map(Result::ok)
        .map(|e| e.path())
        .filter(|p| {
            p.file_name()
                .is_some_and(|n| n.to_string_lossy().ends_with("-business"))
        })
        .map(|p| p.join("w17-business.json"))
        .filter(|p| p.is_file())
        .collect();
    entries.sort();
    let mut found = Vec::new();
    for m in entries {
        let raw = fs::read_to_string(&m).map_err(|e| Error(format!("{}: {e}", m.display())))?;
        let v: serde_json::Value = serde_json::from_str(&raw)
            .map_err(|e| Error(format!("{} is not JSON: {e}", m.display())))?;
        let lang = v
            .get("language")
            .and_then(|l| l.as_str())
            .unwrap_or("python");
        let path = v
            .get("register")
            .and_then(|r| r.get("path"))
            .and_then(|p| p.as_str())
            .unwrap_or("");
        if lang == "rust" && path.trim_end_matches('/') == rel {
            found.push(m);
        }
    }
    match found.len() {
        1 => Ok(found.remove(0)),
        0 => err(format!(
            "no manifest under {} has language \"rust\" and register.path {rel:?} — run `w17ctl codegen` after setting \
             the domain's business bundle to this crate, or set {ENV_MANIFEST}",
            services.display()
        )),
        _ => err(format!(
            "{} manifests under {} have language \"rust\" and register.path {rel:?}: {}",
            found.len(),
            services.display(),
            found
                .iter()
                .map(|p| p.display().to_string())
                .collect::<Vec<_>>()
                .join(", ")
        )),
    }
}

/// The descriptor set a manifest names, resolved against it.
pub fn descriptor_set_of(manifest: &Path) -> Result<PathBuf, Error> {
    let raw =
        fs::read_to_string(manifest).map_err(|e| Error(format!("{}: {e}", manifest.display())))?;
    let v: serde_json::Value = serde_json::from_str(&raw)
        .map_err(|e| Error(format!("{} is not JSON: {e}", manifest.display())))?;
    let rel = match v.get("descriptor_set") {
        None | Some(serde_json::Value::Null) => "descriptors.binpb",
        Some(serde_json::Value::String(s)) if s.is_empty() => "descriptors.binpb",
        Some(serde_json::Value::String(s)) => s.as_str(),
        Some(other) => {
            return err(format!(
                "{}: descriptor_set must be a string, not {other}",
                manifest.display()
            ));
        }
    };
    let p = Path::new(rel);
    Ok(if p.is_absolute() {
        p.to_path_buf()
    } else {
        manifest.parent().unwrap_or(Path::new(".")).join(p)
    })
}

/// The SHA-256 of a descriptor set, hex — what the runtime compares.
pub fn digest(bytes: &[u8]) -> String {
    hex::encode(Sha256::digest(bytes))
}

/// The builder [`compile_manifest`] uses: every mapping onto the SDK's
/// types, client and server code, no default stubs, no transport helpers.
pub fn builder() -> tonic_prost_build::Builder {
    tonic_prost_build::configure()
        .build_client(true)
        .build_server(true)
        .build_transport(false)
        .generate_default_stubs(false)
        .extern_path(".w17", "::w17_business::pb::w17")
        .codec_path("::w17_business::codec::ProstCodec")
        .include_file(INCLUDE_FILE)
}

/// The prost configuration: the well-known types are the SDK's re-export.
pub fn prost_config() -> tonic_prost_build::Config {
    let mut c = tonic_prost_build::Config::new();
    c.prost_types_path("::w17_business::pb::google::protobuf");
    c
}

/// Compile the descriptor set of the manifest at `manifest` into `out_dir`
/// (`OUT_DIR` when `None`), and return its digest. Prints the cargo
/// directives when `cargo` is true.
pub fn compile_manifest(
    manifest: &Path,
    out_dir: Option<&Path>,
    cargo: bool,
) -> Result<String, Error> {
    let set = descriptor_set_of(manifest)?;
    let raw = fs::read(&set).map_err(|e| {
        Error(format!(
            "{}: {e} (the descriptor set {} names; run `w17ctl codegen`)",
            set.display(),
            manifest.display()
        ))
    })?;
    let fds = prost_types::FileDescriptorSet::decode(raw.as_slice())
        .map_err(|e| Error(format!("{} is not a descriptor set: {e}", set.display())))?;
    let mut b = builder();
    if let Some(dir) = out_dir {
        b = b.out_dir(dir);
    }
    b.compile_fds_with_config(fds, prost_config())
        .map_err(|e| Error(format!("compile {}: {e}", set.display())))?;
    let sum = digest(&raw);
    if cargo {
        println!("cargo:rerun-if-changed={}", manifest.display());
        println!("cargo:rerun-if-changed={}", set.display());
        println!("cargo:rerun-if-env-changed={ENV_MANIFEST}");
        println!("cargo:rustc-env={ENV_DIGEST}={sum}");
    }
    Ok(sum)
}

/// Compile the tier's descriptor set from `build.rs`: see the crate doc.
pub fn compile() -> Result<(), Box<dyn std::error::Error>> {
    let manifest = locate_manifest()?;
    compile_manifest(&manifest, None, true)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::fs;

    use super::*;

    const SET: &str = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../tests/conformance/testdata/descriptors.binpb"
    );

    fn project(manifests: &[(&str, &str, &str)]) -> tempfile::TempDir {
        let root = tempfile::tempdir().unwrap();
        for (bundle, lang, path) in manifests {
            let dir = root.path().join("w17/services").join(bundle);
            fs::create_dir_all(&dir).unwrap();
            fs::copy(SET, dir.join("descriptors.binpb")).unwrap();
            let m = format!(
                r#"{{"format": 1, "domain": "d", "language": "{lang}", "register": {{"path": "{path}"}}}}"#
            );
            fs::write(dir.join("w17-business.json"), m).unwrap();
        }
        fs::create_dir_all(root.path().join("business/shop-rs/src")).unwrap();
        root
    }

    #[test]
    fn the_manifest_is_the_one_naming_the_crate() {
        let root = project(&[
            ("shop-business", "rust", "business/shop-rs"),
            ("other-business", "python", "business/shop-rs"),
        ]);
        let found = find_manifest(&root.path().join("business/shop-rs")).unwrap();
        assert!(
            found.ends_with("w17/services/shop-business/w17-business.json"),
            "{}",
            found.display()
        );
        assert_eq!(
            descriptor_set_of(&found).unwrap(),
            found.parent().unwrap().join("descriptors.binpb")
        );
    }

    #[test]
    fn none_or_several_is_refused_naming_what_it_looked_for() {
        let root = project(&[("a-business", "rust", "business/elsewhere")]);
        let e = find_manifest(&root.path().join("business/shop-rs")).unwrap_err();
        assert!(e.0.contains("register.path \"business/shop-rs\""), "{e}");
        let root = project(&[
            ("a-business", "rust", "business/shop-rs"),
            ("b-business", "rust", "business/shop-rs/"),
        ]);
        assert!(
            find_manifest(&root.path().join("business/shop-rs"))
                .unwrap_err()
                .0
                .contains("2 manifests")
        );
        let lone = tempfile::tempdir().unwrap();
        assert!(
            find_manifest(lone.path())
                .unwrap_err()
                .0
                .contains("no w17/services/")
        );
    }

    #[test]
    fn the_descriptor_set_compiles_without_protoc() {
        let root = project(&[("shop-business", "rust", "business/shop-rs")]);
        let m = root
            .path()
            .join("w17/services/shop-business/w17-business.json");
        let out = tempfile::tempdir().unwrap();
        let sum = compile_manifest(&m, Some(out.path()), false).unwrap();
        assert_eq!(sum, digest(&fs::read(SET).unwrap()));
        let include = fs::read_to_string(out.path().join(INCLUDE_FILE)).unwrap();
        assert!(
            include.contains("pub mod conformance") && include.contains("pub mod clients"),
            "{include}"
        );
        assert!(
            !include.contains("pub mod w17 "),
            "the vocabulary is the SDK's, not generated again"
        );
        let v1 = fs::read_to_string(out.path().join("conformance.v1.rs")).unwrap();
        assert!(v1.contains("::w17_business::codec::ProstCodec"));
        assert!(v1.contains("pub trait NoteService"));
        let bad = root.path().join("bad.json");
        fs::write(&bad, r#"{"descriptor_set": 5}"#).unwrap();
        assert!(compile_manifest(&bad, Some(out.path()), false).is_err());
    }
}
