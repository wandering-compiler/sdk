use std::io::Write as _;

use crate::{
    config::Config,
    manifest::ConfigKey,
    secrets::{Env, Resolver, Secret, new_resolver, parse_dotenv},
};

fn key(name: &str, typ: &str, default: &str) -> ConfigKey {
    ConfigKey {
        name: name.into(),
        env: format!("N_{}", name.to_uppercase()),
        r#type: typ.into(),
        default: default.into(),
        required: false,
        secret: false,
        description: String::new(),
    }
}

fn env(pairs: &[(&str, &str)]) -> Env {
    pairs
        .iter()
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .collect()
}

#[test]
fn values_defaults_and_types() {
    let keys = [
        key("greeting", "string", "hi"),
        key("limit", "int", "10"),
        key("verbose", "bool", "false"),
    ];
    let cfg = Config::load(&keys, &env(&[("N_LIMIT", "-42"), ("N_VERBOSE", "1")]), None).unwrap();
    assert_eq!(cfg.str("greeting").unwrap(), "hi");
    assert_eq!(cfg.int("limit").unwrap(), -42);
    assert!(cfg.bool("verbose").unwrap());
    assert!(
        cfg.int("greeting")
            .unwrap_err()
            .0
            .contains("is a string, not a int")
    );
    assert!(
        cfg.str("nope")
            .unwrap_err()
            .0
            .contains("no key \"nope\" is declared")
    );
    let zero = Config::load(
        &[key("n", "int", ""), key("b", "bool", "")],
        &Env::new(),
        None,
    )
    .unwrap();
    assert_eq!(
        (zero.int("n").unwrap(), zero.bool("b").unwrap()),
        (0, false)
    );
}

#[test]
fn a_bad_value_names_its_variable() {
    for (typ, raw, want) in [
        ("bool", "yes", "N_X: invalid bool"),
        ("int", "1.5", "N_X: invalid int"),
        ("int", "9223372036854775808", "out of range"),
        ("int", "-9223372036854775809", "out of range"),
    ] {
        let e = Config::load(&[key("x", typ, "")], &env(&[("N_X", raw)]), None).unwrap_err();
        assert!(e.0.contains(want), "{typ} {raw}: {e}");
    }
    let max = Config::load(
        &[key("x", "int", "")],
        &env(&[("N_X", "+9223372036854775807")]),
        None,
    )
    .unwrap();
    assert_eq!(max.int("x").unwrap(), i64::MAX);
}

#[test]
fn required_and_secret_keys() {
    let mut k = key("token", "string", "");
    k.required = true;
    k.secret = true;
    let e = Config::load(
        std::slice::from_ref(&k),
        &Env::new(),
        Some(&Resolver::default()),
    )
    .unwrap_err();
    assert_eq!(e.0, "config key 'token': N_TOKEN is required");
    let r = Resolver::from_sources(vec![env(&[("N_TOKEN", "s3cret")])]);
    let cfg = Config::load(&[k], &Env::new(), Some(&r)).unwrap();
    let s = cfg.secret("token").unwrap();
    assert_eq!(
        (s.to_string(), format!("{s:?}"), s.reveal()),
        ("***".into(), "***".into(), "s3cret")
    );
    assert!(!format!("{cfg:?}").contains("s3cret"));
    assert!(Secret::new(Some(String::new())).is_zero());
}

#[test]
fn dotenv_as_go_parses_it() {
    let kv = parse_dotenv("# c\n A = b=c \r\nNOEQ\n=x\nQ=\"quoted\"\r\n");
    assert_eq!(kv.get("A").map(String::as_str), Some(" b=c "));
    assert_eq!(kv.get("Q").map(String::as_str), Some("\"quoted\""));
    assert_eq!(kv.len(), 2);
}

#[test]
fn the_secrets_chain() {
    let dir = tempfile::tempdir().unwrap();
    let plain = dir.path().join(".secrets");
    std::fs::write(&plain, "K=plain\nP=only-plain\n").unwrap();
    let f = plain.to_string_lossy().to_string();
    let r = new_resolver(&env(&[("K", "env"), ("W17_SECRETS_FILE", &f)])).unwrap();
    assert_eq!(
        (r.get("K"), r.get("P"), r.get("X")),
        ("env".into(), "only-plain".into(), String::new())
    );

    // age: a file encrypted to a fresh identity decrypts with it.
    let id = age::x25519::Identity::generate();
    let ct = age::encrypt(&id.to_public(), b"K=from-age\nA=1\n").unwrap();
    let age_file = dir.path().join(".secrets.age");
    std::fs::File::create(&age_file)
        .unwrap()
        .write_all(&ct)
        .unwrap();
    let key = age::secrecy::ExposeSecret::expose_secret(&id.to_string()).to_string();
    let a = age_file.to_string_lossy().to_string();
    let r = new_resolver(&env(&[
        ("W17_SECRETS_AGE_FILE", &a),
        ("W17_SECRETS_AGE_KEY", &key),
        ("W17_SECRETS_FILE", &f),
    ]))
    .unwrap();
    assert_eq!(
        (r.get("K"), r.get("A"), r.get("P")),
        ("from-age".into(), "1".into(), "only-plain".into())
    );
    let r = new_resolver(&env(&[
        ("W17_SECRETS_MODE", "plain"),
        ("W17_SECRETS_AGE_FILE", &a),
        ("W17_SECRETS_AGE_KEY", &key),
        ("W17_SECRETS_FILE", &f),
    ]))
    .unwrap();
    assert_eq!(r.get("K"), "plain");

    let other = age::x25519::Identity::generate();
    let wrong = age::secrecy::ExposeSecret::expose_secret(&other.to_string()).to_string();
    let e = new_resolver(&env(&[
        ("W17_SECRETS_AGE_FILE", &a),
        ("W17_SECRETS_AGE_KEY", &wrong),
    ]))
    .unwrap_err();
    assert!(e.contains("wrong key?"), "{e}");
    let e = new_resolver(&env(&[("W17_SECRETS_MODE", "age")])).unwrap_err();
    assert!(e.contains("mode=age but no age key"), "{e}");
    let e = new_resolver(&env(&[("W17_SECRETS_MODE", "vault")])).unwrap_err();
    assert!(e.contains("unknown W17_SECRETS_MODE"), "{e}");
}
