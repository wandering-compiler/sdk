use prost::Message as _;
use prost_reflect::{DynamicMessage, ReflectMessage as _, Value};

use super::context::conformance_pool;
use crate::{
    ctx::Ctx,
    events::{
        Dispatcher, EnvelopeBuilder, MemoryPublisher, Router, invalidate_events, is_credential_key,
        labels_from_context, metadata_from_context, nats_connect_options, reserved_metadata_key,
        validate_channel, validate_topic,
    },
    manifest::{Emit, Invalidate, PayloadBinding},
    metadata::Metadata,
    pb::w17::InvalidateEvent,
};

#[test]
fn credential_keys() {
    for k in [
        "authorization",
        "Cookie",
        "grpcgateway-authorization",
        "x-api-key",
        "x-auth-token",
        "session_id",
        "my.secret",
    ] {
        assert!(is_credential_key(k), "{k}");
    }
    for k in [
        "x-w17-user",
        "w17-label-session",
        "x-custom",
        "w17-language",
        "apiary",
    ] {
        assert!(!is_credential_key(k), "{k}");
    }
    assert!(reserved_metadata_key("W17-Tx-Id") && reserved_metadata_key("w17-conn-id"));
}

#[test]
fn channel_and_topic_names() {
    assert!(validate_channel("notes").is_ok());
    for bad in ["", "w17", "W17_x", "a.b", "a b", &"x".repeat(201)] {
        assert!(validate_channel(bad).is_err(), "{bad}");
    }
    assert!(validate_topic("note.created").is_ok());
    for bad in ["", "a..b", "a.*", "a.>", "a b"] {
        assert!(validate_topic(bad).is_err(), "{bad}");
    }
}

#[test]
fn context_data_as_go_flattens_it() {
    let c = Ctx::background().with_incoming(Metadata::from_pairs([
        ("x-w17-user", "first"),
        ("x-w17-user", "second"),
        ("x-custom", "a"),
        ("x-custom", "b"),
        ("authorization", "Bearer x"),
        ("w17-tx-id", "t"),
        ("w17-label-tenant", "t7"),
        ("w17-label-", "empty-name"),
    ]));
    let md = metadata_from_context(&c).unwrap();
    assert_eq!(
        (md["x-w17-user"].as_str(), md["x-custom"].as_str()),
        ("first", "b")
    );
    assert!(!md.contains_key("authorization") && !md.contains_key("w17-tx-id"));
    let labels = labels_from_context(&c).unwrap();
    assert_eq!(labels.len(), 1);
    assert_eq!(labels["tenant"], "t7");
    assert!(metadata_from_context(&Ctx::background()).is_none());
    let out_only = Ctx::background().with_outgoing(Metadata::from_pairs([("x-a", "1")]));
    assert_eq!(
        metadata_from_context(&out_only).unwrap()["x-a"],
        "1",
        "outgoing when there is no incoming"
    );
}

fn emit() -> Emit {
    let b = |field: &str, source: &str, path: &[&str], value: &str| PayloadBinding {
        field: field.into(),
        source: source.into(),
        path: path.iter().map(|s| s.to_string()).collect(),
        value: value.into(),
    };
    Emit {
        method: "/conformance.v1.NoteService/CreateNote".into(),
        event: "conformance.v1.NoteCreated".into(),
        envelope: "conformance.events.Event".into(),
        envelope_field: "v1_note_created".into(),
        channel: "notes".into(),
        topic: "note.created".into(),
        fields: vec![
            b("id", "response", &["id"], ""),
            b("text", "request", &["text"], ""),
            b("by", "literal", &[], "rust"),
        ],
    }
}

fn msg(name: &str, fields: &[(&str, Value)]) -> Vec<u8> {
    let mut m = DynamicMessage::new(conformance_pool().get_message_by_name(name).unwrap());
    for (k, v) in fields {
        m.set_field_by_name(k, v.clone());
    }
    m.encode_to_vec()
}

#[test]
fn the_envelope_is_built_from_request_and_response() {
    let pool = conformance_pool();
    let input = pool
        .get_message_by_name("conformance.v1.CreateNoteRequest")
        .unwrap();
    let output = pool.get_message_by_name("conformance.v1.Note").unwrap();
    let b = EnvelopeBuilder::new(&pool, &emit(), input, output).unwrap();
    let c = Ctx::background().with_incoming(Metadata::from_pairs([
        ("x-custom", "c"),
        ("w17-label-tenant", "t7"),
        ("authorization", "x"),
    ]));
    let req = msg(
        "conformance.v1.CreateNoteRequest",
        &[("text", Value::String("hello".into()))],
    );
    let resp = msg("conformance.v1.Note", &[("id", Value::I64(42))]);
    let raw = b.build(&c, &req, &resp).unwrap();
    let env = DynamicMessage::decode(
        pool.get_message_by_name("conformance.events.Event")
            .unwrap(),
        raw.as_slice(),
    )
    .unwrap();
    let ev = env.get_field_by_name("v1_note_created").unwrap();
    let ev = ev.as_message().unwrap();
    assert_eq!(ev.get_field_by_name("id").unwrap().as_i64(), Some(42));
    assert_eq!(
        ev.get_field_by_name("text").unwrap().as_str(),
        Some("hello")
    );
    assert_eq!(ev.get_field_by_name("by").unwrap().as_str(), Some("rust"));
    let md = env.get_field_by_name("metadata").unwrap();
    let md = md.as_map().unwrap();
    assert_eq!(md.len(), 2, "x-custom and the label; never the credential");
    assert_eq!(ev.descriptor().full_name(), "conformance.v1.NoteCreated");

    let mut bad = emit();
    bad.envelope_field = "nope".into();
    let (i, o) = (
        pool.get_message_by_name("conformance.v1.CreateNoteRequest")
            .unwrap(),
        pool.get_message_by_name("conformance.v1.Note").unwrap(),
    );
    assert!(EnvelopeBuilder::new(&pool, &bad, i.clone(), o.clone()).is_err());
    let mut lit = emit();
    lit.fields = vec![PayloadBinding {
        field: "id".into(),
        source: "literal".into(),
        path: vec![],
        value: "x".into(),
    }];
    assert!(
        EnvelopeBuilder::new(&pool, &lit, i, o)
            .unwrap()
            .build(&c, &req, &resp)
            .unwrap_err()
            .contains("string field only")
    );
}

#[test]
fn invalidates_carry_the_labels() {
    let inv = Invalidate {
        method: "/a.S/M".into(),
        channel: "notes".into(),
        topic: "w17.invalidate".into(),
        group: "conformance.v1".into(),
        entities: vec!["Note".into(), "Tag".into()],
    };
    let c = Ctx::background().with_incoming(Metadata::from_pairs([("w17-label-tenant", "t7")]));
    let evs = invalidate_events(&inv, &c);
    assert_eq!(evs.len(), 2);
    let e = InvalidateEvent::decode(evs[1].as_slice()).unwrap();
    assert_eq!(
        (e.group.as_str(), e.message.as_str()),
        ("conformance.v1", "Tag")
    );
    assert_eq!(e.labels["tenant"], "t7");
}

#[test]
fn nats_dsns_as_nats_go_reads_them() {
    let o = nats_connect_options("nats://u:p%40ss@h1:4223,h2").unwrap();
    assert_eq!(o.servers, ["h1:4223", "h2:4222"]);
    assert_eq!(
        (o.user.as_deref(), o.pass.as_deref(), o.token, o.tls),
        (Some("u"), Some("p@ss"), None, false)
    );
    let t = nats_connect_options("tls://tok@h").unwrap();
    assert_eq!((t.token.as_deref(), t.tls), (Some("tok"), true));
    assert!(nats_connect_options("http://h").is_err());
    assert!(nats_connect_options("").is_err());
}

#[tokio::test]
async fn the_router_sends_each_channel_to_its_bus() {
    let router = Router {
        nats: Some(Box::new(MemoryPublisher::default())),
        redis: None,
        redis_channels: ["fast".to_string()].into_iter().collect(),
    };
    router.dispatch("notes", "a.b", vec![1]).await.unwrap();
    router.dispatch("fast", "a.b", vec![2]).await.unwrap(); // its bus is off: dropped, not misrouted
    assert!(router.dispatch("w17", "a", vec![]).await.is_err());
}
