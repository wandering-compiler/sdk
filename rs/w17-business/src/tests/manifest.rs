use std::path::Path;

use serde_json::{Value, json};

use crate::manifest::{from_value, load};

fn conformance() -> std::path::PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/conformance/service/w17-business.json")
}

fn min(change: Value) -> Value {
    let mut d = json!({"format": 1, "domain": "d", "env_prefix": "D", "language": "rust", "register": {"path": "biz/d"}});
    for (k, v) in change.as_object().unwrap() {
        d[k] = v.clone();
    }
    d
}

fn refused(v: Value) -> String {
    from_value(&v, Path::new(".")).expect_err("refused").0
}

#[test]
fn the_conformance_manifest_loads() {
    let m = load(&conformance()).unwrap();
    assert_eq!(m.domain, "notes");
    assert_eq!(m.register.package, "conformance-notes");
    assert_eq!(m.emits()[0].fields[2].value, "conformance");
    assert_eq!(m.emits()[0].envelope, "conformance.events.Event");
    assert_eq!(
        m.storage.as_ref().unwrap().distributed_transactions,
        "w17.common.distx.W17DistributedTransaction"
    );
    let required: Vec<&str> = m
        .config
        .iter()
        .filter(|k| k.required)
        .map(|k| k.name.as_str())
        .collect();
    assert_eq!(required, ["api_token"]);
    assert_eq!(m.shutdown_timeout.as_secs(), 10);
    let c = m.companion.as_ref().unwrap();
    assert_eq!(c.egress[0].service, "clients.echo.EchoApi");
    let f = c.files.as_ref().unwrap();
    assert_eq!(f.placements[0].max_size_bytes, 262144);
    assert!(!f.connections[1].writable);
    assert!(
        m.path(&m.descriptor_set)
            .ends_with("tests/conformance/service/../testdata/descriptors.binpb")
    );
}

#[test]
fn the_specs_example_reads_as_specified() {
    let m = load(&Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/testdata/shop.json")).unwrap();
    assert_eq!(
        (
            m.bundle.as_str(),
            m.env_prefix.as_str(),
            m.listen_env.as_str(),
            m.default_listen_addr.as_str()
        ),
        ("shop-business", "SHOP", "SHOP_LISTEN_ADDR", ":50051")
    );
    let svc = &m.services[0];
    assert_eq!(svc.name, "shop.core.CheckoutService");
    assert!(svc.methods[1].server_streaming && !svc.methods[0].server_streaming);
    let cfg: std::collections::HashMap<_, _> =
        m.config.iter().map(|k| (k.name.as_str(), k)).collect();
    assert_eq!(
        (
            cfg["max_items"].default.as_str(),
            cfg["dry_run"].default.as_str()
        ),
        ("25", "true")
    );
    assert!(cfg["api_key"].secret);
    let e = &m.emits()[0];
    assert_eq!(e.envelope, "w17lock.shop.events.Event");
    assert_eq!(e.fields[2].path, ["cart", "total"]);
    assert_eq!(m.invalidates()[0].topic, "w17.invalidate");
}

#[test]
fn format_and_language_are_checked() {
    assert!(refused(min(json!({"format": 3}))).contains("reads formats 1 and 2"));
    assert!(
        from_value(&min(json!({"format": 2})), Path::new("."))
            .unwrap()
            .companion
            .is_none()
    );
    assert!(refused(min(json!({"language": "python"}))).contains("is not rust"));
    let mut no_lang = min(json!({}));
    no_lang.as_object_mut().unwrap().remove("language");
    assert!(
        refused(no_lang).contains("is not rust"),
        "an absent language is Python's default"
    );
}

// The top level the Go companion decodes (bizcompanion.Load), refused as it
// refuses it: the same table in every SDK's unit tests (py, ts, rs).
#[test]
fn the_top_level_is_read_with_gos_rules() {
    let one_point_oh: Value = serde_json::from_str("1.0").unwrap();
    let table: Vec<(Value, &str)> = vec![
        (json!({"format": "1"}), "format"),
        (json!({"format": true}), "format"),
        (json!({"format": one_point_oh}), "format"),
        (json!({"format": 3}), "reads formats 1 and 2"),
        (json!({"domain": null}), "domain is required"),
        (json!({"domain": ""}), "domain is required"),
        (json!({"domain": 5}), "domain must be a string"),
        (json!({"env_prefix": null}), "env_prefix is required"),
        (json!({"env_prefix": ""}), "env_prefix is required"),
        (json!({"env_prefix": ["D"]}), "env_prefix must be a string"),
        (json!({"bundle": 7}), "bundle must be a string"),
        (
            json!({"descriptor_set": {}}),
            "descriptor_set must be a string",
        ),
    ];
    for (change, want) in table {
        let msg = refused(min(change.clone()));
        assert!(msg.contains(want), "{change}: {msg}");
    }
}

#[test]
fn top_level_nulls_take_gos_defaults() {
    let m = from_value(
        &min(json!({"bundle": null, "descriptor_set": null})),
        Path::new("."),
    )
    .unwrap();
    assert_eq!(
        (
            m.bundle.as_str(),
            m.descriptor_set.as_str(),
            m.env_prefix.as_str()
        ),
        ("d-business", "descriptors.binpb", "D")
    );
    assert_eq!(m.listen_env, "D_LISTEN_ADDR");
    assert_eq!(m.reflection_env, "D_GRPC_REFLECTION");
    assert_eq!(
        (
            m.register.package.as_str(),
            m.register.module.as_str(),
            m.register.function.as_str()
        ),
        ("d", "d", "register_business")
    );
}

#[test]
fn required_fields() {
    let mut no_reg = min(json!({}));
    no_reg.as_object_mut().unwrap().remove("register");
    assert!(refused(no_reg).contains("register is required"));
    let ev = json!({"events": {"envelope": "", "emits": [{"method": "/a.S/M", "event": "a.E", "envelope_field": "e", "channel": "c", "topic": "t"}]}});
    assert!(refused(min(ev)).contains("envelope is required"));
    let dir = tempfile::tempdir().unwrap();
    let p = dir.path().join("m.json");
    std::fs::write(&p, "{nope").unwrap();
    assert!(load(&p).unwrap_err().0.contains("not JSON"));
    assert!(load(&dir.path().join("missing.json")).is_err());
}

fn companion(c: Value) -> Value {
    min(json!({"format": 2, "companion": c}))
}

#[test]
fn the_companion_section_is_read_as_go_reads_it() {
    let files = |p: Value| {
        json!({"address_env": "A", "files": {"service": "w17.common.files.W17Files",
            "connections": [{"name": "c", "root_env": "R"}], "placements": [p]}})
    };
    let ok = files(json!({"field": "a.B.f", "connection": "c", "max_per_bucket": 100}));
    assert!(from_value(&companion(ok), Path::new(".")).is_ok());
    let cases: Vec<(Value, &str)> = vec![
        (json!({"address_env": ""}), "address_env is required"),
        (
            json!({"ADDRESS_ENV": "A", "address_env": "A"}),
            "keys are matched exactly",
        ),
        (
            json!({"address_env": "A", "egress": {}}),
            "must be a list or null",
        ),
        (
            json!({"address_env": "A", "egress": [{"client": "x", "service": "s", "dsn_env": "D"}, {"client": "x", "service": "t", "dsn_env": "E"}]}),
            "listed twice",
        ),
        (
            json!({"address_env": "A", "files": {"service": "other"}}),
            "the companion serves w17.common.files.W17Files",
        ),
        (
            files(json!({"field": "a.B.f", "connection": "nope"})),
            "not in companion.files.connections",
        ),
        (
            files(json!({"field": "a.B.f", "connection": "c", "max_per_bucket": -1})),
            "integer from 0 to 4294967295",
        ),
        (
            files(json!({"field": "a.B.f", "connection": "c", "max_size_bytes": "1"})),
            "integer from 0 to",
        ),
        (
            files(
                json!({"field": "a.B.f", "connection": "c", "expected_objects": serde_json::from_str::<Value>("-0").unwrap()}),
            ),
            "-0",
        ),
    ];
    for (c, want) in cases {
        let msg = refused(companion(c.clone()));
        assert!(msg.contains(want), "{c}: {msg}");
    }
    let one = refused(min(json!({"companion": {"address_env": "A"}})));
    assert!(one.contains("format 1 has no companion section"), "{one}");
    assert!(
        from_value(&min(json!({"companion": null})), Path::new("."))
            .unwrap()
            .companion
            .is_none()
    );
}
