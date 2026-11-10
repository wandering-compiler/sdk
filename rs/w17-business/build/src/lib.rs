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
//! crosses into the SDK is one type, not two: every message and enum of the
//! SDK's descriptor set (the vocabulary, the transaction coordinator, the
//! companion's file service) onto `::w17_business::pb::w17`, type by type —
//! a plugin's package under `w17` (`w17.contrib.auth`) is generated into the
//! crate like any other — `.google.protobuf` onto
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

/// The SDK's own descriptor set: what `w17_business::pb` was compiled from.
const SDK_DESCRIPTORS: &[u8] = include_bytes!("../../src/sdk_descriptors.binpb");

/// Where the SDK's generated types live.
const SDK_PB: &str = "::w17_business::pb";

/// prost-build's identifier rules (its `ident.rs`), so a path named here is
/// the one prost generated in the SDK.
fn sanitize(ident: String) -> String {
    match ident.as_str() {
        "as" | "break" | "const" | "continue" | "else" | "enum" | "false" | "fn" | "for" | "if"
        | "impl" | "in" | "let" | "loop" | "match" | "mod" | "move" | "mut" | "pub" | "ref"
        | "return" | "static" | "struct" | "trait" | "true" | "type" | "unsafe" | "use"
        | "where" | "while" | "dyn" | "abstract" | "become" | "box" | "do" | "final" | "macro"
        | "override" | "priv" | "typeof" | "unsized" | "virtual" | "yield" | "async" | "await"
        | "try" | "gen" => format!("r#{ident}"),
        "_" | "super" | "self" | "Self" | "extern" | "crate" => format!("{ident}_"),
        s if s.starts_with(|c: char| c.is_numeric()) => format!("_{ident}"),
        _ => ident,
    }
}

/// Every top-level message and enum of the SDK's descriptor set, as the
/// `(proto path, Rust path)` pairs [`builder`] maps onto the SDK's types.
///
/// Type by type, never by package: a package mapping also takes every package
/// under it, and `.w17` would take the plugins' (`w17.contrib.auth`), which
/// the SDK does not compile — the crate would name `AuthResp` at a path that
/// does not exist. A nested type follows its parent's mapping (prost resolves
/// it under the parent's module).
pub fn sdk_extern_paths() -> Result<Vec<(String, String)>, Error> {
    use heck::{ToSnakeCase as _, ToUpperCamelCase as _};
    let fds = prost_types::FileDescriptorSet::decode(SDK_DESCRIPTORS)
        .map_err(|e| Error(format!("the SDK's descriptor set: {e}")))?;
    let mut out = Vec::new();
    for f in &fds.file {
        let pkg = f.package();
        let module: String = pkg
            .split('.')
            .filter(|s| !s.is_empty())
            .map(|s| format!("::{}", sanitize(s.to_snake_case())))
            .collect();
        let names = f
            .message_type
            .iter()
            .map(|m| m.name())
            .chain(f.enum_type.iter().map(|e| e.name()));
        for name in names {
            let proto = if pkg.is_empty() {
                format!(".{name}")
            } else {
                format!(".{pkg}.{name}")
            };
            let rust = format!("{SDK_PB}{module}::{}", sanitize(name.to_upper_camel_case()));
            out.push((proto, rust));
        }
    }
    Ok(out)
}

/// The builder [`compile_manifest`] uses: every mapping onto the SDK's
/// types, client and server code, no default stubs, no transport helpers.
pub fn builder() -> Result<tonic_prost_build::Builder, Error> {
    let mut b = tonic_prost_build::configure()
        .build_client(true)
        .build_server(true)
        .build_transport(false)
        .generate_default_stubs(false)
        .codec_path("::w17_business::codec::ProstCodec")
        .include_file(INCLUDE_FILE);
    for (proto, rust) in sdk_extern_paths()? {
        b = b.extern_path(proto, rust);
    }
    Ok(b)
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
    let mut b = builder()?;
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

    fn message(name: &str, fields: &[(&str, &str)]) -> prost_types::DescriptorProto {
        use prost_types::field_descriptor_proto::{Label, Type};
        prost_types::DescriptorProto {
            name: Some(name.into()),
            field: fields
                .iter()
                .enumerate()
                .map(|(i, (f, ty))| prost_types::FieldDescriptorProto {
                    name: Some((*f).into()),
                    number: Some(i as i32 + 1),
                    label: Some(Label::Optional as i32),
                    r#type: Some(if ty.is_empty() {
                        Type::String
                    } else {
                        Type::Message
                    } as i32),
                    type_name: (!ty.is_empty()).then(|| (*ty).into()),
                    ..Default::default()
                })
                .collect(),
            ..Default::default()
        }
    }

    #[test]
    fn a_plugin_package_under_w17_is_the_crates_own() {
        // A domain with the auth plugin: its descriptor set carries
        // w17.contrib.auth, which the SDK does not compile, beside the SDK's
        // own w17.common.distx.
        let mut fds = prost_types::FileDescriptorSet::decode(SDK_DESCRIPTORS).unwrap();
        let distx = fds
            .file
            .iter()
            .find(|f| f.package() == "w17.common.distx")
            .unwrap()
            .name()
            .to_string();
        fds.file.push(prost_types::FileDescriptorProto {
            name: Some("w17/contrib/auth/types.proto".into()),
            package: Some("w17.contrib.auth".into()),
            message_type: vec![message("AuthResp", &[("user_id", "")])],
            syntax: Some("proto3".into()),
            ..Default::default()
        });
        fds.file.push(prost_types::FileDescriptorProto {
            name: Some("shop/v1/svc.proto".into()),
            package: Some("shop.v1".into()),
            dependency: vec!["w17/contrib/auth/types.proto".into(), distx],
            message_type: vec![message(
                "Who",
                &[
                    ("user", ".w17.contrib.auth.AuthResp"),
                    ("begin", ".w17.common.distx.BeginRequest"),
                ],
            )],
            syntax: Some("proto3".into()),
            ..Default::default()
        });
        let out = tempfile::tempdir().unwrap();
        builder()
            .unwrap()
            .out_dir(out.path())
            .compile_fds_with_config(fds, prost_config())
            .unwrap();
        let auth = fs::read_to_string(out.path().join("w17.contrib.auth.rs")).unwrap_or_default();
        assert!(
            auth.contains("pub struct AuthResp"),
            "the plugin's types are generated into the crate: {auth}"
        );
        let shop = fs::read_to_string(out.path().join("shop.v1.rs")).unwrap();
        assert!(
            shop.contains("::w17_business::pb::w17::common::distx::BeginRequest"),
            "{shop}"
        );
        assert!(
            !shop.contains("::w17_business::pb::w17::contrib::auth"),
            "{shop}"
        );
        assert!(
            !out.path().join("w17.common.distx.rs").exists() || {
                let d = fs::read_to_string(out.path().join("w17.common.distx.rs")).unwrap();
                !d.contains("pub struct BeginRequest")
            }
        );
    }

    #[test]
    fn every_sdk_type_maps_where_the_sdk_generated_it() {
        let paths = sdk_extern_paths().unwrap();
        let get = |p: &str| {
            paths
                .iter()
                .find(|(proto, _)| proto == p)
                .map(|(_, r)| r.as_str())
        };
        assert_eq!(
            get(".w17.common.distx.BeginRequest"),
            Some("::w17_business::pb::w17::common::distx::BeginRequest")
        );
        assert_eq!(
            get(".w17.common.files.PutEnd"),
            Some("::w17_business::pb::w17::common::files::PutEnd")
        );
        assert!(
            paths
                .iter()
                .all(|(p, _)| !p.starts_with(".w17.contrib.auth"))
        );
    }
}
