//! Secret resolution, as sdk/go/service/secret resolves it.
//!
//! The chain, first hit wins:
//!
//! 1. the process environment;
//! 2. `.secrets.age` (`W17_SECRETS_AGE_FILE`), decrypted with the age
//!    identity in `W17_SECRETS_AGE_KEY` or the file `W17_SECRETS_AGE_KEY_FILE`
//!    (fallbacks `SOPS_AGE_KEY` / `SOPS_AGE_KEY_FILE`);
//! 3. `.secrets` (`W17_SECRETS_FILE`), plain dotenv.
//!
//! `W17_SECRETS_MODE`: `auto` (default: the age file when a key AND the file
//! exist, then the plain file when it exists), `plain` (env + plain file
//! only), `age` (strict: a key is required, the age file must decrypt). A
//! corrupt or undecryptable age file, or `age` with no key, fails the boot
//! rather than resolving every secret to empty.

use std::{collections::HashMap, fmt, io::Read as _, path::Path, str::FromStr as _};

/// The environment a tier reads: a snapshot of the process's, taken at boot.
pub type Env = HashMap<String, String>;

/// The process environment, now.
pub fn process_env() -> Env {
    std::env::vars_os()
        .filter_map(|(k, v)| Some((k.into_string().ok()?, v.into_string().ok()?)))
        .collect()
}

pub const REDACTED: &str = "***";

/// A secret value that does not print itself: `Display` and `Debug` render
/// `***` (Go's `secret.String`), so a config dumped to a log, Sentry or an
/// error message carries no value. [`Secret::reveal`] is the one way to read
/// it.
#[derive(Clone, Default, PartialEq, Eq)]
pub struct Secret(Option<String>);

impl Secret {
    pub fn new(value: Option<String>) -> Self {
        Secret(value.filter(|v| !v.is_empty()))
    }

    /// The value ("" when unset).
    pub fn reveal(&self) -> &str {
        self.0.as_deref().unwrap_or("")
    }

    /// Whether no value is set.
    pub fn is_zero(&self) -> bool {
        self.0.is_none()
    }
}

impl fmt::Display for Secret {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(REDACTED)
    }
}

impl fmt::Debug for Secret {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(REDACTED)
    }
}

/// Go's parseDotenv: `KEY=value` lines, `#` comments and blank lines
/// skipped, the key trimmed, the value verbatim (no quote stripping) less a
/// trailing CR.
pub fn parse_dotenv(s: &str) -> Env {
    let mut kv = Env::new();
    for line in s.split('\n') {
        let t = line.trim();
        if t.is_empty() || t.starts_with('#') {
            continue;
        }
        let Some((k, v)) = line.split_once('=') else {
            continue;
        };
        let key = k.trim();
        if key.is_empty() {
            continue;
        }
        kv.insert(
            key.to_string(),
            v.strip_suffix('\r').unwrap_or(v).to_string(),
        );
    }
    kv
}

/// The chain of sources, first hit wins.
#[derive(Clone, Debug, Default)]
pub struct Resolver {
    sources: Vec<Env>,
}

impl Resolver {
    pub fn from_sources(sources: Vec<Env>) -> Self {
        Self { sources }
    }

    /// The value of `key` and whether any source had it.
    pub fn lookup(&self, key: &str) -> Option<&str> {
        self.sources
            .iter()
            .find_map(|s| s.get(key).map(String::as_str))
    }

    /// The value of `key`, "" when no source has it.
    pub fn get(&self, key: &str) -> String {
        self.lookup(key).unwrap_or("").to_string()
    }
}

fn decrypt_age(path: &str, key_data: &str) -> Result<Env, String> {
    let mut ids: Vec<age::x25519::Identity> = Vec::new();
    for raw in key_data.lines() {
        let line = raw.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        ids.push(
            age::x25519::Identity::from_str(line)
                .map_err(|e| format!("secret: parse age identity: {e}"))?,
        );
    }
    if ids.is_empty() {
        return Err("secret: no age identities found in key".into());
    }
    let data = std::fs::read(path).map_err(|e| format!("secret: open age file {path}: {e}"))?;
    let dec = age::Decryptor::new(&data[..])
        .map_err(|e| format!("secret: decrypt {path} (wrong key?): {e}"))?;
    let mut reader = dec
        .decrypt(ids.iter().map(|i| i as &dyn age::Identity))
        .map_err(|e| format!("secret: decrypt {path} (wrong key?): {e}"))?;
    let mut plain = String::new();
    reader
        .read_to_string(&mut plain)
        .map_err(|e| format!("secret: decrypt {path} (wrong key?): {e}"))?;
    Ok(parse_dotenv(&plain))
}

fn is_file(p: &str) -> bool {
    !p.is_empty() && Path::new(p).is_file()
}

/// The resolver for `env`, per `W17_SECRETS_MODE`.
pub fn new_resolver(env: &Env) -> Result<Resolver, String> {
    let get = |k: &str| env.get(k).cloned().unwrap_or_default();
    let or = |k: &str, d: &str| {
        Some(get(k))
            .filter(|v| !v.is_empty())
            .unwrap_or_else(|| d.to_string())
    };
    let mode = Some(or("W17_SECRETS_MODE", "auto").trim().to_ascii_lowercase())
        .filter(|m| !m.is_empty())
        .unwrap_or_else(|| "auto".into());
    let secrets_file = or("W17_SECRETS_FILE", ".secrets");
    let age_file = or("W17_SECRETS_AGE_FILE", ".secrets.age");
    let age_key = Some(get("W17_SECRETS_AGE_KEY"))
        .filter(|v| !v.is_empty())
        .unwrap_or_else(|| get("SOPS_AGE_KEY"));
    let age_key_file = Some(get("W17_SECRETS_AGE_KEY_FILE"))
        .filter(|v| !v.is_empty())
        .unwrap_or_else(|| get("SOPS_AGE_KEY_FILE"));

    let (key_data, have_key) = if !age_key.trim().is_empty() {
        (age_key, true)
    } else if !age_key_file.trim().is_empty() {
        let k = std::fs::read_to_string(&age_key_file)
            .map_err(|e| format!("secret: read age key file {age_key_file}: {e}"))?;
        (k, true)
    } else {
        (String::new(), false)
    };

    let mut sources = vec![env.clone()];
    let plain = |sources: &mut Vec<Env>| -> Result<(), String> {
        if is_file(&secrets_file) {
            let s = std::fs::read_to_string(&secrets_file)
                .map_err(|e| format!("secret: read plain secrets file {secrets_file:?}: {e}"))?;
            sources.push(parse_dotenv(&s));
        }
        Ok(())
    };
    match mode.as_str() {
        "plain" => plain(&mut sources)?,
        "age" => {
            if !have_key {
                return Err("secret: mode=age but no age key (set W17_SECRETS_AGE_KEY or W17_SECRETS_AGE_KEY_FILE)".into());
            }
            sources.push(decrypt_age(&age_file, &key_data)?);
        }
        "auto" => {
            if have_key && is_file(&age_file) {
                sources.push(decrypt_age(&age_file, &key_data)?);
            }
            plain(&mut sources)?;
        }
        _ => {
            return Err(format!(
                "secret: unknown W17_SECRETS_MODE {:?} (auto|plain|age)",
                get("W17_SECRETS_MODE")
            ));
        }
    }
    Ok(Resolver::from_sources(sources))
}
