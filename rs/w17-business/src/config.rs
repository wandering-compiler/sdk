//! The tier's configuration: every key the manifest declares, read at boot.
//!
//! Go generates a typed `EnvConfig` struct; the SDK gives the same values
//! behind typed getters. The rules are Go's `envStr` / `envBool` / `envInt`:
//!
//! - an unset (or empty) variable takes the declared default;
//! - a value that does not parse as the declared type stops the boot, naming
//!   the variable — a typo in a production env must not run with a default;
//! - `bool` accepts `true`, `false`, `1`, `0` and nothing else;
//! - `int` is a base-10 64-bit integer (Go's `strconv.Atoi`);
//! - a `secret` key is resolved through the secrets chain (env,
//!   `.secrets.age`, `.secrets`) and comes back as a [`Secret`] that never
//!   prints its value.
//!
//! Beyond Go: a key declared `required` with no value and no default stops
//! the boot too, naming the key and its variable.

use std::{collections::BTreeMap, fmt};

use tonic::Status;

use crate::{
    manifest::ConfigKey,
    secrets::{Env, Resolver, Secret},
};

/// A configuration value that cannot be read; stops the boot. As a status
/// (reading an undeclared key in a handler) it is `UNKNOWN` with its
/// message, as Go returns it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ConfigError(pub String);

impl fmt::Display for ConfigError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for ConfigError {}

impl From<ConfigError> for Status {
    fn from(e: ConfigError) -> Self {
        Status::unknown(e.0)
    }
}

#[derive(Clone, Debug, PartialEq)]
enum Val {
    Str(String),
    Bool(bool),
    Int(i64),
    Secret(Secret),
}

fn parse(k: &ConfigKey, raw: &str) -> Result<Val, ConfigError> {
    match k.r#type.as_str() {
        "" | "string" => Ok(Val::Str(raw.to_string())),
        "bool" => match raw {
            "true" | "1" => Ok(Val::Bool(true)),
            "false" | "0" => Ok(Val::Bool(false)),
            _ => Err(ConfigError(format!(
                "env {}: invalid bool {raw:?} (want true, false, 1 or 0)",
                k.env
            ))),
        },
        "int" => {
            let digits = raw.strip_prefix(['+', '-']).unwrap_or(raw);
            if digits.is_empty() || !digits.bytes().all(|b| b.is_ascii_digit()) {
                return Err(ConfigError(format!("env {}: invalid int {raw:?}", k.env)));
            }
            raw.strip_prefix('+')
                .unwrap_or(raw)
                .parse::<i64>()
                .map(Val::Int)
                .map_err(|_| {
                    ConfigError(format!(
                        "env {}: invalid int {raw:?}: value out of range (64-bit)",
                        k.env
                    ))
                })
        }
        other => Err(ConfigError(format!(
            "config key {:?}: unknown type {other:?} (string, bool, int)",
            k.name
        ))),
    }
}

/// The declared configuration, by key name: `cfg.str("greeting")?`,
/// `cfg.bool("verbose")?`, `cfg.int("limit")?`,
/// `cfg.secret("api_token")?.reveal()`. Reading an undeclared key is an
/// error — a typo must not read as "unset".
#[derive(Clone, Default)]
pub struct Config {
    keys: BTreeMap<String, ConfigKey>,
    values: BTreeMap<String, Val>,
}

impl Config {
    /// Read every key from `env`, secrets through `resolver`.
    pub fn load(
        keys: &[ConfigKey],
        env: &Env,
        resolver: Option<&Resolver>,
    ) -> Result<Config, ConfigError> {
        let mut values = BTreeMap::new();
        for k in keys {
            if k.secret {
                let raw = resolver.map(|r| r.get(&k.env)).unwrap_or_default();
                if raw.is_empty() && k.required {
                    return Err(ConfigError(format!(
                        "config key '{}': {} is required",
                        k.name, k.env
                    )));
                }
                values.insert(k.name.clone(), Val::Secret(Secret::new(Some(raw))));
                continue;
            }
            let mut raw = env.get(&k.env).cloned().unwrap_or_default();
            if raw.is_empty() {
                raw = k.default.clone();
            }
            if raw.is_empty() {
                if k.required {
                    return Err(ConfigError(format!(
                        "config key '{}': {} is required",
                        k.name, k.env
                    )));
                }
                let zero = match k.r#type.as_str() {
                    "bool" => Val::Bool(false),
                    "int" => Val::Int(0),
                    _ => Val::Str(String::new()),
                };
                values.insert(k.name.clone(), zero);
                continue;
            }
            values.insert(k.name.clone(), parse(k, &raw)?);
        }
        Ok(Config {
            keys: keys.iter().map(|k| (k.name.clone(), k.clone())).collect(),
            values,
        })
    }

    fn get(&self, name: &str, kind: &str) -> Result<&Val, ConfigError> {
        let Some(k) = self.keys.get(name) else {
            let declared: Vec<&str> = self.keys.keys().map(String::as_str).collect();
            return Err(ConfigError(format!(
                "config: no key {name:?} is declared (declared: {})",
                declared.join(", ")
            )));
        };
        let want = if k.secret {
            "secret"
        } else if k.r#type.is_empty() {
            "string"
        } else {
            k.r#type.as_str()
        };
        if want != kind {
            return Err(ConfigError(format!(
                "config: key {name:?} is a {want}, not a {kind}"
            )));
        }
        Ok(&self.values[name])
    }

    pub fn str(&self, name: &str) -> Result<String, ConfigError> {
        match self.get(name, "string")? {
            Val::Str(s) => Ok(s.clone()),
            _ => unreachable!("a string key holds a string"),
        }
    }

    pub fn bool(&self, name: &str) -> Result<bool, ConfigError> {
        match self.get(name, "bool")? {
            Val::Bool(b) => Ok(*b),
            _ => unreachable!("a bool key holds a bool"),
        }
    }

    pub fn int(&self, name: &str) -> Result<i64, ConfigError> {
        match self.get(name, "int")? {
            Val::Int(n) => Ok(*n),
            _ => unreachable!("an int key holds an int"),
        }
    }

    pub fn secret(&self, name: &str) -> Result<Secret, ConfigError> {
        match self.get(name, "secret")? {
            Val::Secret(s) => Ok(s.clone()),
            _ => unreachable!("a secret key holds a secret"),
        }
    }

    /// Whether `name` is declared.
    pub fn has(&self, name: &str) -> bool {
        self.keys.contains_key(name)
    }

    /// The declared keys' names, sorted.
    pub fn keys(&self) -> impl Iterator<Item = &str> {
        self.keys.keys().map(String::as_str)
    }

    /// The variables of the secret keys (scrubbed from Sentry events).
    pub fn secret_env_names(&self) -> Vec<String> {
        self.keys
            .values()
            .filter(|k| k.secret)
            .map(|k| k.env.clone())
            .collect()
    }
}

impl fmt::Debug for Config {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let mut m = f.debug_map();
        for (k, v) in &self.values {
            match v {
                Val::Str(s) => m.entry(k, s),
                Val::Bool(b) => m.entry(k, b),
                Val::Int(n) => m.entry(k, n),
                Val::Secret(s) => m.entry(k, s),
            };
        }
        m.finish()
    }
}
