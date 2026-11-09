//! The business tier's manifest (`w17-business.json`), formats 1 and 2.
//!
//! Codegen writes it beside `descriptors.binpb` in
//! `w17/services/<domain>-business/`; the SDK reads it at boot. The
//! platform's spec `bundles/business-manifest` is the contract; this module
//! is its reader, with the rules of the Python and TypeScript readers:
//!
//! - `format` is checked first, and a format this SDK does not know is
//!   refused naming the ones it reads;
//! - the keys the Go companion decodes (`bizcompanion.Load`) are read with
//!   its rules: `domain` and `env_prefix` are required strings, `bundle` and
//!   `descriptor_set` strings that default as Go defaults them;
//! - format 2 is format 1 plus the `companion` section; a manifest whose
//!   `companion` is null stays format 1, and a format-1 manifest carrying one
//!   is refused;
//! - a field is never repurposed, so unknown keys are ignored;
//! - `null` (or an empty list) means "this domain has none".

use std::{
    collections::HashSet,
    fmt,
    path::{Path, PathBuf},
    time::Duration,
};

use serde_json::{Map, Value};

pub const SUPPORTED_FORMATS: &[i64] = &[1, 2];
pub const LANGUAGE: &str = "rust";
/// The only file service a manifest may name (bizcompanion.FilesService).
pub const FILES_SERVICE: &str = "w17.common.files.W17Files";

/// The manifest cannot be read; the message names what to fix.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ManifestError(pub String);

impl fmt::Display for ManifestError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for ManifestError {}

fn err<T>(msg: impl Into<String>) -> Result<T, ManifestError> {
    Err(ManifestError(msg.into()))
}

/// Where the author's crate is: `path` is its project-relative directory,
/// `package` its `[package].name`, `module` the binary the image builds.
/// `function` is informative: the binary names its register function itself.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Register {
    pub path: String,
    pub package: String,
    pub module: String,
    pub function: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MethodSpec {
    pub name: String,
    pub path: String,
    pub input: String,
    pub output: String,
    pub client_streaming: bool,
    pub server_streaming: bool,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ServiceSpec {
    pub name: String,
    pub methods: Vec<MethodSpec>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ConfigKey {
    pub name: String,
    pub env: String,
    /// string | bool | int
    pub r#type: String,
    /// Normalised to the text a variable would hold.
    pub default: String,
    pub required: bool,
    pub secret: bool,
    pub description: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct StorageTier {
    pub address_env: String,
    pub default_address: String,
    pub services: Vec<String>,
    /// The coordinator's full service name, "" when the storage bundle
    /// registers none.
    pub distributed_transactions: String,
}

/// One event field and where its value comes from.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PayloadBinding {
    pub field: String,
    /// request | response | literal
    pub source: String,
    pub path: Vec<String>,
    /// For literal.
    pub value: String,
}

/// A method's `(w17.event_emit)`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Emit {
    /// The gRPC path, "/pkg.Svc/Method".
    pub method: String,
    pub event: String,
    pub envelope: String,
    pub envelope_field: String,
    pub channel: String,
    pub topic: String,
    pub fields: Vec<PayloadBinding>,
}

/// A method's `(w17.touches_entities)`: one `w17.InvalidateEvent` per
/// entity, published after the method's emit.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Invalidate {
    pub method: String,
    pub channel: String,
    pub topic: String,
    pub group: String,
    pub entities: Vec<String>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Events {
    /// "" when the project runs no NATS.
    pub nats_dsn_env: String,
    /// "" when no channel is on Redis Streams.
    pub redis_dsn_env: String,
    pub redis_channels: Vec<String>,
    pub durable_prefix_env: String,
    pub durable_prefix_default: String,
    pub envelope: String,
    pub emits: Vec<Emit>,
    pub invalidates: Vec<Invalidate>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PluginHook {
    pub plugin: String,
    pub registered_as: String,
    pub slot: String,
    /// gRPC paths.
    pub methods: Vec<String>,
}

/// One egress client the companion serves.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct EgressClient {
    /// "quotes"
    pub client: String,
    /// "clients.quotes.QuotesApi"
    pub service: String,
    /// Read by the companion, not here.
    pub dsn_env: String,
}

/// One upload connection an `UPLOADED_*` field of the domain names.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct FileConnection {
    pub name: String,
    pub root_env: String,
    pub default_root: String,
    /// The business tier writes it (`upload.writer: BUSINESS`).
    pub writable: bool,
}

/// One `writer: BUSINESS` table field: where its files go.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct FilePlacement {
    /// Full proto name, "app.example.NoteExport.file".
    pub field: String,
    pub name: String,
    pub connection: String,
    pub bucket_path: String,
    pub expected_objects: u64,
    pub max_per_bucket: u32,
    /// 0 = no cap.
    pub max_size_bytes: u64,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct FileStorage {
    pub service: String,
    pub connections: Vec<FileConnection>,
    pub placements: Vec<FilePlacement>,
}

/// The business companion: the Go process that serves this tier the
/// project's egress clients and the domain's file stores over gRPC.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Companion {
    pub address_env: String,
    pub default_address: String,
    pub egress: Vec<EgressClient>,
    pub files: Option<FileStorage>,
}

#[derive(Clone, Debug, PartialEq)]
pub struct Manifest {
    pub format: i64,
    pub domain: String,
    pub bundle: String,
    pub language: String,
    pub sdk_version: String,
    pub env_prefix: String,
    pub listen_env: String,
    pub default_listen_addr: String,
    pub reflection_env: String,
    pub shutdown_timeout: Duration,
    /// Relative to the manifest.
    pub descriptor_set: String,
    pub register: Register,
    pub services: Vec<ServiceSpec>,
    pub config: Vec<ConfigKey>,
    pub storage: Option<StorageTier>,
    pub events: Option<Events>,
    pub plugin_hooks: Vec<PluginHook>,
    pub companion: Option<Companion>,
    pub base_dir: PathBuf,
}

impl Manifest {
    /// A path of the manifest, resolved against its directory.
    pub fn path(&self, rel: &str) -> PathBuf {
        let p = Path::new(rel);
        if p.is_absolute() {
            p.to_path_buf()
        } else {
            self.base_dir.join(p)
        }
    }

    pub fn emits(&self) -> &[Emit] {
        self.events
            .as_ref()
            .map(|e| e.emits.as_slice())
            .unwrap_or(&[])
    }

    pub fn invalidates(&self) -> &[Invalidate] {
        self.events
            .as_ref()
            .map(|e| e.invalidates.as_slice())
            .unwrap_or(&[])
    }
}

type Obj = Map<String, Value>;

/// A JSON value for a message.
fn show(v: &Value) -> String {
    if let Some(f) = v.as_f64()
        && v.is_f64()
    {
        if f == 0.0 && f.is_sign_negative() {
            return "-0".into();
        }
        if f.fract() == 0.0 {
            return format!("{v} written with a fraction or an exponent");
        }
    }
    v.to_string()
}

/// A JSON scalar as text; anything else as its JSON.
fn text(v: &Value) -> String {
    match v {
        Value::String(s) => s.clone(),
        Value::Number(n) => n.to_string(),
        Value::Bool(b) => b.to_string(),
        other => other.to_string(),
    }
}

/// A lenient read of a key the Go companion does not decode: a scalar as
/// its text, absent (or null) as `dflt`.
fn str_or(d: &Obj, key: &str, dflt: &str) -> String {
    match d.get(key) {
        None | Some(Value::Null) => dflt.to_string(),
        Some(v) => text(v),
    }
}

fn req(d: &Obj, key: &str, w: &str) -> Result<String, ManifestError> {
    match d.get(key) {
        None | Some(Value::Null) => err(format!("manifest: {w}{key} is required")),
        Some(Value::String(s)) if s.is_empty() => err(format!("manifest: {w}{key} is required")),
        Some(v) => Ok(text(v)),
    }
}

fn obj<'a>(d: &'a Obj, key: &str, w: &str) -> Result<Option<&'a Obj>, ManifestError> {
    match d.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Object(o)) => Ok(Some(o)),
        Some(_) => err(format!("manifest: {w}{key} must be an object or null")),
    }
}

fn list(v: Option<&Value>) -> Vec<&Obj> {
    match v {
        Some(Value::Array(a)) => a.iter().filter_map(Value::as_object).collect(),
        _ => Vec::new(),
    }
}

fn strs(v: Option<&Value>) -> Vec<String> {
    match v {
        Some(Value::Array(a)) => a.iter().map(text).collect(),
        _ => Vec::new(),
    }
}

fn truthy(v: Option<&Value>) -> bool {
    match v {
        None | Some(Value::Null) => false,
        Some(Value::Bool(b)) => *b,
        Some(Value::Number(n)) => n.as_f64() != Some(0.0),
        Some(Value::String(s)) => !s.is_empty(),
        Some(_) => true,
    }
}

/// The manifest types a default as its key's type (25, true); config reads
/// it as the text an env variable would hold.
fn default_text(v: Option<&Value>) -> String {
    match v {
        None | Some(Value::Null) => String::new(),
        Some(v) => text(v),
    }
}

// --- the keys Go decodes ---------------------------------------------------------
//
// The Go companion (bizcompanion.Load) decodes the top level's format,
// domain, bundle, env_prefix and descriptor_set, and the whole companion
// section, into typed structs and refuses what does not fit them; these
// readers refuse the same, so the companion and the tier agree on which
// manifests boot: a string field takes a string, an integer one an integer
// in its Go type's range (uint32, uint64), a bool true or false, a list a
// list, an object an object; null is the field's zero value (a null list
// element an empty object). In the companion section a key is matched
// exactly, and one Go's encoding/json would match case-insensitively is
// refused (the Go companion refuses it too); any other unknown key is
// ignored.

const COMPANION_KEYS: &[&str] = &["address_env", "default_address", "egress", "files"];
const EGRESS_KEYS: &[&str] = &["client", "service", "dsn_env"];
const FILES_KEYS: &[&str] = &["service", "connections", "placements"];
const CONNECTION_KEYS: &[&str] = &["name", "root_env", "default_root", "writable"];
const PLACEMENT_KEYS: &[&str] = &[
    "field",
    "name",
    "connection",
    "bucket_path",
    "expected_objects",
    "max_per_bucket",
    "max_size_bytes",
];

/// Unicode simple case folding onto the ASCII lowercase field names: A-Z,
/// and U+017F (long s) and U+212A (Kelvin), the two non-ASCII runes that
/// fold to an ASCII letter.
fn fold(k: &str) -> String {
    k.chars()
        .map(|c| match c {
            'A'..='Z' => c.to_ascii_lowercase(),
            '\u{017F}' => 's',
            '\u{212A}' => 'k',
            other => other,
        })
        .collect()
}

fn exact_keys(d: &Obj, names: &[&str], w: &str) -> Result<(), ManifestError> {
    for k in d.keys() {
        if names.contains(&k.as_str()) {
            continue;
        }
        let f = fold(k);
        if names.contains(&f.as_str()) {
            return err(format!(
                "manifest: {w}: key {k:?} is not {f:?} — keys are matched exactly"
            ));
        }
    }
    Ok(())
}

/// A JSON string; absent (or null) is "".
fn cstr(d: &Obj, key: &str, w: &str) -> Result<String, ManifestError> {
    match d.get(key) {
        None | Some(Value::Null) => Ok(String::new()),
        Some(Value::String(s)) => Ok(s.clone()),
        Some(v) => err(format!(
            "manifest: {w}{key} must be a string, not {}",
            show(v)
        )),
    }
}

fn creq(d: &Obj, key: &str, w: &str) -> Result<String, ManifestError> {
    let v = cstr(d, key, w)?;
    if v.is_empty() {
        return err(format!("manifest: {w}{key} is required"));
    }
    Ok(v)
}

/// A JSON bool; absent (or null) is false. "false" is not a bool.
fn cbool(d: &Obj, key: &str, w: &str) -> Result<bool, ManifestError> {
    match d.get(key) {
        None | Some(Value::Null) => Ok(false),
        Some(Value::Bool(b)) => Ok(*b),
        Some(v) => err(format!(
            "manifest: {w}{key} must be true or false, not {}",
            show(v)
        )),
    }
}

/// A JSON integer from 0 to `max`; absent (or null) is 0. 1.0, "1" and -0
/// are not.
fn cuint(d: &Obj, key: &str, w: &str, max: u64) -> Result<u64, ManifestError> {
    match d.get(key) {
        None | Some(Value::Null) => Ok(0),
        Some(v) => match v.as_u64() {
            Some(n) if n <= max => Ok(n),
            _ => err(format!(
                "manifest: {w}{key} must be an integer from 0 to {max}, not {}",
                show(v)
            )),
        },
    }
}

/// A JSON list of objects; absent (or null) is empty, a null element is {}.
fn objects(d: &Obj, key: &str, w: &str) -> Result<Vec<Obj>, ManifestError> {
    match d.get(key) {
        None | Some(Value::Null) => Ok(Vec::new()),
        Some(Value::Array(a)) => a
            .iter()
            .map(|x| match x {
                Value::Null => Ok(Obj::new()),
                Value::Object(o) => Ok(o.clone()),
                other => err(format!(
                    "manifest: {w}{key}[] must be an object, not {}",
                    show(other)
                )),
            })
            .collect(),
        Some(v) => err(format!(
            "manifest: {w}{key} must be a list or null, not {}",
            show(v)
        )),
    }
}

fn unique<'a>(values: impl IntoIterator<Item = &'a str>, what: &str) -> Result<(), ManifestError> {
    let mut seen = HashSet::new();
    for v in values {
        if !seen.insert(v) {
            return err(format!("manifest: {what} {v} is listed twice"));
        }
    }
    Ok(())
}

// Then what bizcompanion.Companion.check refuses: the required fields, a
// name listed twice, a placement on an unknown connection, a file service
// other than FILES_SERVICE.
fn companion_of(d: &Obj) -> Result<Option<Companion>, ManifestError> {
    let Some(c) = obj(d, "companion", "")? else {
        return Ok(None);
    };
    exact_keys(c, COMPANION_KEYS, "companion")?;
    let address_env = creq(c, "address_env", "companion.")?;
    let default_address = cstr(c, "default_address", "companion.")?;
    let mut egress = Vec::new();
    for e in objects(c, "egress", "companion.")? {
        exact_keys(&e, EGRESS_KEYS, "companion.egress[]")?;
        egress.push(EgressClient {
            client: creq(&e, "client", "companion.egress[].")?,
            service: creq(&e, "service", "companion.egress[].")?,
            dsn_env: creq(&e, "dsn_env", "companion.egress[].")?,
        });
    }
    unique(
        egress.iter().map(|e| e.client.as_str()),
        "companion.egress: client",
    )?;
    unique(
        egress.iter().map(|e| e.service.as_str()),
        "companion.egress: service",
    )?;
    let files = match obj(c, "files", "companion.")? {
        None => None,
        Some(f) => {
            exact_keys(f, FILES_KEYS, "companion.files")?;
            let service = cstr(f, "service", "companion.files.")?;
            if service != FILES_SERVICE {
                return err(format!(
                    "manifest: companion.files.service is {service:?}; the companion serves {FILES_SERVICE}"
                ));
            }
            let mut connections = Vec::new();
            for x in objects(f, "connections", "companion.files.")? {
                let w = "companion.files.connections[].";
                exact_keys(&x, CONNECTION_KEYS, "companion.files.connections[]")?;
                connections.push(FileConnection {
                    name: creq(&x, "name", w)?,
                    root_env: creq(&x, "root_env", w)?,
                    default_root: cstr(&x, "default_root", w)?,
                    writable: cbool(&x, "writable", w)?,
                });
            }
            unique(
                connections.iter().map(|x| x.name.as_str()),
                "companion.files.connections:",
            )?;
            let mut placements = Vec::new();
            for x in objects(f, "placements", "companion.files.")? {
                let w = "companion.files.placements[].";
                exact_keys(&x, PLACEMENT_KEYS, "companion.files.placements[]")?;
                placements.push(FilePlacement {
                    field: creq(&x, "field", w)?,
                    name: cstr(&x, "name", w)?,
                    connection: creq(&x, "connection", w)?,
                    bucket_path: cstr(&x, "bucket_path", w)?,
                    expected_objects: cuint(&x, "expected_objects", w, u64::MAX)?,
                    max_per_bucket: cuint(&x, "max_per_bucket", w, u32::MAX as u64)? as u32,
                    max_size_bytes: cuint(&x, "max_size_bytes", w, u64::MAX)?,
                });
            }
            unique(
                placements.iter().map(|x| x.field.as_str()),
                "companion.files.placements:",
            )?;
            for x in &placements {
                if !connections.iter().any(|c| c.name == x.connection) {
                    return err(format!(
                        "manifest: companion.files.placements: {} is on connection {:?}, which is not in \
                         companion.files.connections",
                        x.field, x.connection
                    ));
                }
            }
            Some(FileStorage {
                service,
                connections,
                placements,
            })
        }
    };
    Ok(Some(Companion {
        address_env,
        default_address,
        egress,
        files,
    }))
}

fn formats() -> String {
    SUPPORTED_FORMATS
        .iter()
        .map(i64::to_string)
        .collect::<Vec<_>>()
        .join(" and ")
}

/// Read a manifest from its JSON tree; `base_dir` is the directory relative
/// paths resolve against.
pub fn from_value(v: &Value, base_dir: &Path) -> Result<Manifest, ManifestError> {
    let Some(d) = v.as_object() else {
        return err("manifest: not a JSON object");
    };
    let fmt = match d.get("format") {
        Some(Value::Number(n)) if n.is_i64() && n.as_i64().unwrap_or(0) >= 1 => {
            n.as_i64().unwrap_or(0)
        }
        other => {
            let shown = other.map(show).unwrap_or_else(|| "null".into());
            return err(format!(
                "manifest: format {shown} is not a manifest format version (this SDK reads {})",
                formats()
            ));
        }
    };
    if !SUPPORTED_FORMATS.contains(&fmt) {
        return err(format!(
            "manifest: format {fmt} is not one this SDK reads (it reads formats {}) — build against the w17-business \
             release of the project's sdk_version",
            formats()
        ));
    }
    // An absent language is Python's default, as the Python SDK reads it.
    let language = str_or(d, "language", "python");
    if language != LANGUAGE {
        return err(format!(
            "manifest: language {language:?} is not rust — this is the Rust business SDK"
        ));
    }
    let domain = creq(d, "domain", "")?;
    let prefix = creq(d, "env_prefix", "")?;
    let bundle = Some(cstr(d, "bundle", "")?)
        .filter(|b| !b.is_empty())
        .unwrap_or_else(|| format!("{domain}-business"));
    let descriptor_set = Some(cstr(d, "descriptor_set", "")?)
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| "descriptors.binpb".into());
    let empty = Obj::new();
    let listen = obj(d, "listen", "")?.unwrap_or(&empty);
    let Some(reg) = obj(d, "register", "")? else {
        return err("manifest: register is required");
    };
    let path = req(reg, "path", "register.")?;
    let package = Some(str_or(reg, "package", ""))
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| {
            Path::new(&path)
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_default()
        });
    let register = Register {
        module: Some(str_or(reg, "module", ""))
            .filter(|s| !s.is_empty())
            .unwrap_or_else(|| package.clone()),
        function: Some(str_or(reg, "function", ""))
            .filter(|s| !s.is_empty())
            .unwrap_or_else(|| "register_business".into()),
        path,
        package,
    };
    let mut services = Vec::new();
    for s in list(d.get("services")) {
        let name = req(s, "name", "services[].")?;
        let mut methods = Vec::new();
        for m in list(s.get("methods")) {
            let mname = req(m, "name", "services[].methods[].")?;
            methods.push(MethodSpec {
                path: Some(str_or(m, "path", ""))
                    .filter(|p| !p.is_empty())
                    .unwrap_or_else(|| format!("/{name}/{mname}")),
                input: str_or(m, "input", ""),
                output: str_or(m, "output", ""),
                client_streaming: truthy(m.get("client_streaming")),
                server_streaming: truthy(m.get("server_streaming")),
                name: mname,
            });
        }
        services.push(ServiceSpec { name, methods });
    }
    let mut config = Vec::new();
    for c in list(d.get("config")) {
        let name = req(c, "name", "config[].")?;
        config.push(ConfigKey {
            env: Some(str_or(c, "env", ""))
                .filter(|e| !e.is_empty())
                .unwrap_or_else(|| format!("{prefix}_{}", name.to_uppercase())),
            r#type: Some(str_or(c, "type", ""))
                .filter(|t| !t.is_empty())
                .unwrap_or_else(|| "string".into()),
            default: default_text(c.get("default")),
            required: truthy(c.get("required")),
            secret: truthy(c.get("secret")),
            description: str_or(c, "description", ""),
            name,
        });
    }
    let storage = match obj(d, "storage", "")? {
        None => None,
        Some(st) => Some(StorageTier {
            address_env: req(st, "address_env", "storage.")?,
            default_address: str_or(st, "default_address", ""),
            services: strs(st.get("services")),
            distributed_transactions: match st.get("distributed_transactions") {
                Some(Value::Object(dt)) => str_or(dt, "service", ""),
                _ => String::new(),
            },
        }),
    };
    let events = match obj(d, "events", "")? {
        None => None,
        Some(ev) => {
            let envelope = str_or(ev, "envelope", "");
            let nats = ev.get("nats").and_then(Value::as_object).unwrap_or(&empty);
            let redis = ev
                .get("redis_streams")
                .and_then(Value::as_object)
                .unwrap_or(&empty);
            let mut emits = Vec::new();
            for e in list(ev.get("emits")) {
                let w = "events.emits[].";
                let mut fields = Vec::new();
                for f in list(e.get("fields")) {
                    fields.push(PayloadBinding {
                        field: req(f, "field", "events.emits[].fields[].")?,
                        source: Some(str_or(f, "source", ""))
                            .filter(|s| !s.is_empty())
                            .unwrap_or_else(|| "literal".into()),
                        path: strs(f.get("path")),
                        value: str_or(f, "value", ""),
                    });
                }
                let emit = Emit {
                    method: req(e, "method", w)?,
                    event: req(e, "event", w)?,
                    envelope: Some(str_or(e, "envelope", ""))
                        .filter(|s| !s.is_empty())
                        .unwrap_or_else(|| envelope.clone()),
                    envelope_field: req(e, "envelope_field", w)?,
                    channel: req(e, "channel", w)?,
                    topic: req(e, "topic", w)?,
                    fields,
                };
                if emit.envelope.is_empty() {
                    return err(format!(
                        "manifest: events.envelope is required for the emit of {}",
                        emit.method
                    ));
                }
                emits.push(emit);
            }
            let mut invalidates = Vec::new();
            for i in list(ev.get("invalidates")) {
                let w = "events.invalidates[].";
                invalidates.push(Invalidate {
                    method: req(i, "method", w)?,
                    channel: req(i, "channel", w)?,
                    topic: Some(str_or(i, "topic", ""))
                        .filter(|s| !s.is_empty())
                        .unwrap_or_else(|| "w17.invalidate".into()),
                    group: str_or(i, "group", ""),
                    entities: strs(i.get("entities")),
                });
            }
            Some(Events {
                nats_dsn_env: str_or(nats, "dsn_env", ""),
                redis_dsn_env: str_or(redis, "dsn_env", ""),
                redis_channels: strs(redis.get("channels")),
                durable_prefix_env: str_or(ev, "durable_prefix_env", ""),
                durable_prefix_default: str_or(ev, "durable_prefix_default", ""),
                envelope,
                emits,
                invalidates,
            })
        }
    };
    let companion = companion_of(d)?;
    if companion.is_some() && fmt < 2 {
        return err(format!(
            "manifest: format {fmt} has no companion section; a manifest with one is format 2"
        ));
    }
    let plugin_hooks = list(d.get("plugin_hooks"))
        .into_iter()
        .map(|h| PluginHook {
            plugin: str_or(h, "plugin", ""),
            registered_as: str_or(h, "registered_as", ""),
            slot: str_or(h, "slot", ""),
            methods: strs(h.get("methods")),
        })
        .collect();
    let timeout = d
        .get("shutdown_timeout_seconds")
        .and_then(Value::as_f64)
        .filter(|t| *t > 0.0)
        .unwrap_or(10.0);
    Ok(Manifest {
        format: fmt,
        bundle,
        language,
        sdk_version: str_or(d, "sdk_version", ""),
        listen_env: Some(str_or(listen, "env", ""))
            .filter(|s| !s.is_empty())
            .unwrap_or_else(|| format!("{prefix}_LISTEN_ADDR")),
        default_listen_addr: Some(str_or(listen, "default", ""))
            .filter(|s| !s.is_empty())
            .unwrap_or_else(|| ":50051".into()),
        reflection_env: Some(str_or(d, "reflection_env", ""))
            .filter(|s| !s.is_empty())
            .unwrap_or_else(|| format!("{prefix}_GRPC_REFLECTION")),
        shutdown_timeout: Duration::from_secs_f64(timeout),
        descriptor_set,
        register,
        services,
        config,
        storage,
        events,
        plugin_hooks,
        companion,
        base_dir: base_dir.to_path_buf(),
        domain,
        env_prefix: prefix,
    })
}

/// Read the manifest at `path`.
pub fn load(path: &Path) -> Result<Manifest, ManifestError> {
    let raw = std::fs::read_to_string(path)
        .map_err(|e| ManifestError(format!("manifest: read {}: {e}", path.display())))?;
    let v: Value = serde_json::from_str(&raw)
        .map_err(|e| ManifestError(format!("manifest: {} is not JSON: {e}", path.display())))?;
    if !v.is_object() {
        return err(format!("manifest: {} is not a JSON object", path.display()));
    }
    from_value(&v, path.parent().unwrap_or(Path::new(".")))
}
