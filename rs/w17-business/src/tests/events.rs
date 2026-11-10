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

// --- payload shapes beyond scalars --------------------------------------------------

mod shapes {
    use prost_types::{
        DescriptorProto, EnumDescriptorProto, EnumValueDescriptorProto, FieldDescriptorProto,
        FileDescriptorProto, FileDescriptorSet, MessageOptions, OneofDescriptorProto,
        field_descriptor_proto::{Label, Type},
    };

    pub(super) fn f(name: &str, n: i32, ty: Type, type_name: &str) -> FieldDescriptorProto {
        FieldDescriptorProto {
            name: Some(name.into()),
            json_name: Some(name.into()),
            number: Some(n),
            label: Some(Label::Optional as i32),
            r#type: Some(ty as i32),
            type_name: (!type_name.is_empty()).then(|| format!(".s.{type_name}")),
            ..Default::default()
        }
    }

    fn repeated(mut x: FieldDescriptorProto) -> FieldDescriptorProto {
        x.label = Some(Label::Repeated as i32);
        x
    }

    fn in_oneof(mut x: FieldDescriptorProto, i: i32) -> FieldDescriptorProto {
        x.oneof_index = Some(i);
        x
    }

    fn optional(x: FieldDescriptorProto, i: i32) -> FieldDescriptorProto {
        let mut x = in_oneof(x, i);
        x.proto3_optional = Some(true);
        x
    }

    fn message(
        name: &str,
        field: Vec<FieldDescriptorProto>,
        oneofs: &[&str],
        nested: Vec<DescriptorProto>,
    ) -> DescriptorProto {
        DescriptorProto {
            name: Some(name.into()),
            field,
            nested_type: nested,
            oneof_decl: oneofs
                .iter()
                .map(|o| OneofDescriptorProto {
                    name: Some((*o).into()),
                    ..Default::default()
                })
                .collect(),
            ..Default::default()
        }
    }

    fn map_entry(name: &str, value: Type) -> DescriptorProto {
        DescriptorProto {
            name: Some(name.into()),
            field: vec![f("key", 1, Type::String, ""), f("value", 2, value, "")],
            options: Some(MessageOptions {
                map_entry: Some(true),
                ..Default::default()
            }),
            ..Default::default()
        }
    }

    /// Request, response, event and envelope with every shape a binding can
    /// read: nested messages, a oneof, repeated, a map, proto3 `optional`,
    /// an enum.
    pub(super) fn pool() -> prost_reflect::DescriptorPool {
        use prost::Message as _;
        let both = |name: &str, extra: Vec<FieldDescriptorProto>, oneofs: &[&str]| {
            let mut fields = vec![
                f("inner", 1, Type::Message, "Inner"),
                repeated(f("tags", 2, Type::String, "")),
                repeated(f(
                    "counts",
                    3,
                    Type::Message,
                    &format!("{name}.CountsEntry"),
                )),
                f("color", 4, Type::Enum, "Color"),
            ];
            fields.extend(extra);
            message(
                name,
                fields,
                oneofs,
                vec![map_entry("CountsEntry", Type::Int64)],
            )
        };
        let file = FileDescriptorProto {
            name: Some("s.proto".into()),
            package: Some("s".into()),
            syntax: Some("proto3".into()),
            enum_type: vec![EnumDescriptorProto {
                name: Some("Color".into()),
                value: ["COLOR_UNSPECIFIED", "COLOR_RED"]
                    .iter()
                    .enumerate()
                    .map(|(i, v)| EnumValueDescriptorProto {
                        name: Some((*v).into()),
                        number: Some(i as i32),
                        ..Default::default()
                    })
                    .collect(),
                ..Default::default()
            }],
            message_type: vec![
                message("Deep", vec![f("n", 1, Type::Int64, "")], &[], vec![]),
                message(
                    "Inner",
                    vec![
                        f("name", 1, Type::String, ""),
                        f("deep", 2, Type::Message, "Deep"),
                    ],
                    &[],
                    vec![],
                ),
                both(
                    "Req",
                    vec![
                        in_oneof(f("a", 5, Type::String, ""), 0),
                        in_oneof(f("b", 6, Type::Message, "Inner"), 0),
                        optional(f("opt", 7, Type::Int32, ""), 1),
                    ],
                    &["pick", "_opt"],
                ),
                message(
                    "Resp",
                    vec![f("inner", 1, Type::Message, "Inner")],
                    &[],
                    vec![],
                ),
                both(
                    "Ev",
                    vec![
                        f("name", 5, Type::String, ""),
                        f("deep_n", 6, Type::Int64, ""),
                        f("a", 7, Type::String, ""),
                        f("b", 8, Type::Message, "Inner"),
                        optional(f("opt", 9, Type::Int32, ""), 0),
                        f("resp_inner", 10, Type::Message, "Inner"),
                        in_oneof(f("one_name", 11, Type::String, ""), 1),
                    ],
                    &["_opt", "which"],
                ),
                message(
                    "Env",
                    vec![
                        repeated(f("metadata", 50001, Type::Message, "Env.MetadataEntry")),
                        repeated(f("labels", 50002, Type::Message, "Env.LabelsEntry")),
                        in_oneof(f("ev", 1, Type::Message, "Ev"), 0),
                    ],
                    &["payload"],
                    vec![
                        map_entry("MetadataEntry", Type::String),
                        map_entry("LabelsEntry", Type::String),
                    ],
                ),
            ],
            ..Default::default()
        };
        let raw = FileDescriptorSet { file: vec![file] }.encode_to_vec();
        prost_reflect::DescriptorPool::decode(raw.as_slice()).unwrap()
    }
}

#[test]
fn a_binding_reads_nested_oneof_repeated_map_optional_and_enum_fields() {
    use prost_reflect::MapKey;
    let pool = shapes::pool();
    let b = |field: &str, source: &str, path: &[&str]| PayloadBinding {
        field: field.into(),
        source: source.into(),
        path: path.iter().map(|s| s.to_string()).collect(),
        value: String::new(),
    };
    let emit = Emit {
        method: "/s.Svc/Do".into(),
        event: "s.Ev".into(),
        envelope: "s.Env".into(),
        envelope_field: "ev".into(),
        channel: "c".into(),
        topic: "t".into(),
        fields: vec![
            b("inner", "request", &["inner"]),
            b("tags", "request", &["tags"]),
            b("counts", "request", &["counts"]),
            b("color", "request", &["color"]),
            b("name", "request", &["inner", "name"]),
            b("deep_n", "request", &["inner", "deep", "n"]),
            b("a", "request", &["a"]),
            b("b", "request", &["b"]),
            b("opt", "request", &["opt"]),
            b("resp_inner", "response", &["inner"]),
            b("one_name", "request", &["inner", "name"]),
        ],
    };
    let input = pool.get_message_by_name("s.Req").unwrap();
    let output = pool.get_message_by_name("s.Resp").unwrap();
    let builder = EnvelopeBuilder::new(&pool, &emit, input.clone(), output.clone()).unwrap();
    let ev_of = |req: &DynamicMessage, resp: &DynamicMessage| {
        let raw = builder
            .build(
                &Ctx::background(),
                &req.encode_to_vec(),
                &resp.encode_to_vec(),
            )
            .unwrap();
        let env =
            DynamicMessage::decode(pool.get_message_by_name("s.Env").unwrap(), raw.as_slice())
                .unwrap();
        env.get_field_by_name("ev")
            .unwrap()
            .as_message()
            .unwrap()
            .clone()
    };

    // Everything set: copied as it is.
    let inner_desc = pool.get_message_by_name("s.Inner").unwrap();
    let mut deep = DynamicMessage::new(pool.get_message_by_name("s.Deep").unwrap());
    deep.set_field_by_name("n", Value::I64(9));
    let mut inner = DynamicMessage::new(inner_desc.clone());
    inner.set_field_by_name("name", Value::String("nm".into()));
    inner.set_field_by_name("deep", Value::Message(deep));
    let mut req = DynamicMessage::new(input.clone());
    req.set_field_by_name("inner", Value::Message(inner.clone()));
    req.set_field_by_name(
        "tags",
        Value::List(vec![Value::String("x".into()), Value::String("y".into())]),
    );
    req.set_field_by_name(
        "counts",
        Value::Map([(MapKey::String("k".into()), Value::I64(3))].into()),
    );
    req.set_field_by_name("color", Value::EnumNumber(1));
    req.set_field_by_name("a", Value::String("pick-a".into()));
    req.set_field_by_name("opt", Value::I32(0));
    let mut resp = DynamicMessage::new(output.clone());
    resp.set_field_by_name("inner", Value::Message(inner.clone()));
    let ev = ev_of(&req, &resp);
    let get = |m: &DynamicMessage, k: &str| m.get_field_by_name(k).unwrap().into_owned();
    assert_eq!(get(&ev, "inner"), Value::Message(inner.clone()));
    assert_eq!(
        get(&ev, "tags"),
        Value::List(vec![Value::String("x".into()), Value::String("y".into())])
    );
    assert_eq!(
        get(&ev, "counts"),
        Value::Map([(MapKey::String("k".into()), Value::I64(3))].into())
    );
    assert_eq!(get(&ev, "color"), Value::EnumNumber(1));
    assert_eq!(get(&ev, "name"), Value::String("nm".into()));
    assert_eq!(get(&ev, "deep_n"), Value::I64(9));
    assert_eq!(get(&ev, "a"), Value::String("pick-a".into()));
    assert!(
        !ev.has_field_by_name("b"),
        "the oneof's other case is unset"
    );
    assert!(
        ev.has_field_by_name("opt"),
        "a proto3 optional target has presence for a zero value"
    );
    assert_eq!(get(&ev, "opt"), Value::I32(0));
    assert_eq!(get(&ev, "resp_inner"), Value::Message(inner));
    assert!(ev.has_field_by_name("one_name"), "a oneof target is set");

    // Nothing set: Go's chained getters read zero values through unset
    // messages, and a message target stays unset.
    let ev = ev_of(&DynamicMessage::new(input), &DynamicMessage::new(output));
    assert!(!ev.has_field_by_name("inner"));
    assert!(!ev.has_field_by_name("b"));
    assert!(!ev.has_field_by_name("resp_inner"));
    assert_eq!(get(&ev, "deep_n"), Value::I64(0));
    assert_eq!(get(&ev, "name"), Value::String(String::new()));
    assert_eq!(get(&ev, "tags"), Value::List(vec![]));
    assert!(ev.has_field_by_name("opt"));
}
