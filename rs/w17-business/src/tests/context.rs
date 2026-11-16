use std::time::Duration;

use base64::Engine as _;
use http::{HeaderMap, HeaderValue};
use prost_reflect::{DescriptorPool, DynamicMessage, Value};

use crate::{
    ctx::{Ctx, CtxError, ctx},
    errors::{error_detail, error_info, finish_for_user, for_user, for_user_dev, short_method},
    metadata::Metadata,
    paging, principal,
};

/// The principal readers' AuthResp type is process-wide: the tests that set
/// it take this lock.
pub(crate) static AUTH_RESP_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

pub(crate) fn conformance_pool() -> DescriptorPool {
    let raw = std::fs::read(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/tests/conformance/testdata/descriptors.binpb"
    ))
    .unwrap();
    DescriptorPool::decode(raw.as_slice()).unwrap()
}

fn with_incoming(pairs: &[(&str, &str)]) -> Ctx {
    Ctx::background().with_incoming(Metadata::from_pairs(pairs.iter().copied()))
}

fn envelope(user: &str) -> String {
    let pool = conformance_pool();
    let desc = pool.get_message_by_name("conformance.v1.AuthResp").unwrap();
    let mut m = DynamicMessage::new(desc);
    m.set_field_by_name("user_id", Value::String(user.into()));
    base64::engine::general_purpose::STANDARD.encode(prost::Message::encode_to_vec(&m))
}

#[test]
fn metadata_is_lower_case_and_immutable() {
    let md = Metadata::from_pairs([("X-A", "1"), ("x-a", "2"), ("b", "3")]);
    assert_eq!(md.get_all("x-A"), ["1", "2"]);
    let set = md.with_set("b", ["4"]);
    assert_eq!((md.first("b"), set.first("b")), (Some("3"), Some("4")));
    assert!(!md.with_set("b", Vec::<String>::new()).has("b"));
    assert_eq!(
        md.merged(&Metadata::from_pairs([("x-a", "9")]))
            .get_all("x-a"),
        ["9"]
    );
    let mut h = HeaderMap::new();
    h.insert("te", HeaderValue::from_static("trailers"));
    h.insert("grpc-timeout", HeaderValue::from_static("1S"));
    h.insert("x-custom", HeaderValue::from_static("c"));
    let from = Metadata::from_headers(&h);
    assert_eq!(from.keys().collect::<Vec<_>>(), ["x-custom"]);
    assert!(!format!("{md:?}").contains('3'), "values are not printed");
}

#[tokio::test]
async fn deadlines_cancellation_and_without_cancel() {
    let root = Ctx::background();
    assert_eq!(root.err(), None);
    let child = root.with_timeout(Duration::from_millis(20));
    assert_eq!(child.done().await, CtxError::DeadlineExceeded);
    assert_eq!(child.err(), Some(CtxError::DeadlineExceeded));
    assert_eq!(
        child.err().unwrap().status().code(),
        tonic::Code::DeadlineExceeded
    );
    let c = root.with_cancel();
    let grandchild = c.with_timeout(Duration::from_secs(60));
    c.cancel();
    assert_eq!(grandchild.err(), Some(CtxError::Canceled));
    assert_eq!(
        root.err(),
        None,
        "a child's cancel does not reach its parent"
    );
    let detached = grandchild.without_cancel();
    assert_eq!((detached.err(), detached.deadline()), (None, None));
    let outer = root.with_timeout(Duration::from_millis(50));
    assert!(
        outer.with_timeout(Duration::from_secs(60)).deadline() <= outer.deadline(),
        "the earlier deadline wins"
    );
}

/// Go keeps the first cause: a cancel before the deadline stays a cancel
/// after it passes — through Ctx::cancel and through the server ending a
/// call — and a deadline that had passed when the context was cancelled is
/// the reason.
#[tokio::test]
async fn a_cancel_before_the_deadline_stays_a_cancel_after_it_passes() {
    let ctx = Ctx::background().with_timeout(Duration::from_millis(10));
    ctx.cancel();
    tokio::time::sleep(Duration::from_millis(25)).await;
    assert_eq!(ctx.err(), Some(CtxError::Canceled));

    let parent = Ctx::background().with_timeout(Duration::from_millis(5));
    let child = parent.with_cancel();
    tokio::time::sleep(Duration::from_millis(20)).await;
    parent.cancel();
    assert_eq!(child.err(), Some(CtxError::DeadlineExceeded));

    // A served call: the server cancels its context when the call ends or
    // the caller goes away (here before its 10 ms deadline).
    let token = tokio_util::sync::CancellationToken::new();
    let clock = crate::ctx::CancelClock::default();
    let served = Ctx::served(
        Metadata::new(),
        Some(std::time::Instant::now() + Duration::from_millis(10)),
        token.clone(),
        clock.clone(),
        "/a.S/M",
        None,
        false,
    );
    drop(crate::ctx::CancelOnDrop::new(token, clock));
    tokio::time::sleep(Duration::from_millis(25)).await;
    assert_eq!(served.err(), Some(CtxError::Canceled));
}

#[test]
fn a_request_carries_its_context() {
    let c = with_incoming(&[("x-a", "1")]);
    let req = c.request(());
    assert_eq!(ctx(&req).unwrap().incoming().first("x-a"), Some("1"));
    let e = ctx(&tonic::Request::new(())).unwrap_err();
    assert_eq!(e.code(), tonic::Code::Internal);
}

#[test]
fn principal_readers_fail_closed() {
    let _g = AUTH_RESP_LOCK.lock().unwrap_or_else(|e| e.into_inner());
    principal::set_auth_resp_type(
        conformance_pool().get_message_by_name("conformance.v1.AuthResp"),
    );
    let c = with_incoming(&[
        ("x-w17-user", &envelope("u-1")),
        ("x-w17-scope-org_id", "org-7"),
        ("w17-label-tenant", "t"),
        ("x-w17-device", "d"),
        ("x-w17-data-scope", "org"),
    ]);
    assert_eq!(principal::user_id(&c).unwrap(), "u-1");
    assert_eq!(principal::scope(&c, "org_id").as_deref(), Some("org-7"));
    assert_eq!(principal::label(&c, "tenant").as_deref(), Some("t"));
    assert_eq!(principal::device_id(&c).as_deref(), Some("d"));
    assert_eq!(principal::data_scope(&c), "org");
    assert_eq!(
        principal::user_id(&with_incoming(&[("x-w17-scope-user_id", "u-2")])).unwrap(),
        "u-2"
    );
    for md in [
        vec![],
        vec![("x-w17-user", "%%%")],
        vec![("x-w17-user", "AAAA")],
    ] {
        assert!(principal::user_id(&with_incoming(&md)).is_err(), "{md:?}");
    }
    assert_eq!(
        principal::data_scope(&with_incoming(&[("x-w17-data-scope", "weird")])),
        "owner"
    );
    assert_eq!(principal::data_scope(&with_incoming(&[])), "");
    assert_eq!(
        principal::device_id(&with_incoming(&[
            ("x-w17-device", "a"),
            ("x-w17-device", "b")
        ])),
        None
    );
    let st: tonic::Status = principal::user_id(&with_incoming(&[])).unwrap_err().into();
    assert_eq!(st.code(), tonic::Code::Unknown);
}

#[test]
fn the_relay_replaces_the_outgoing_principal() {
    let c = with_incoming(&[
        ("x-w17-user", "U"),
        ("x-w17-scope-org_id", "o"),
        ("authorization", "Bearer x"),
        ("w17-language", "cs"),
    ])
    .with_outgoing(Metadata::from_pairs([
        ("x-w17-user", "stale"),
        ("x-w17-scope-only_out", "leak"),
        ("w17-tx-id", "t"),
    ]));
    let out = principal::forward_to_outgoing(&c);
    let o = out.outgoing();
    assert_eq!(o.get_all("x-w17-user"), ["U"]);
    assert_eq!(o.get_all("x-w17-scope-org_id"), ["o"]);
    assert!(!o.has("x-w17-scope-only_out") && !o.has("authorization") && !o.has("w17-language"));
    assert_eq!(o.get_all("w17-tx-id"), ["t"]);
    let none = Ctx::background().with_outgoing(Metadata::from_pairs([("x-w17-user", "keep")]));
    assert_eq!(
        principal::forward_to_outgoing(&none)
            .outgoing()
            .get_all("x-w17-user"),
        ["keep"]
    );
    assert!(
        principal::is_gateway_owned_key("W17-Conn-Id")
            && !principal::is_gateway_owned_key("w17-language")
    );
}

#[test]
fn act_as_stamps_the_outgoing_side_only() {
    let _g = AUTH_RESP_LOCK.lock().unwrap_or_else(|e| e.into_inner());
    let pool = conformance_pool();
    principal::set_auth_resp_type(pool.get_message_by_name("conformance.v1.AuthResp"));
    let mut user =
        DynamicMessage::new(pool.get_message_by_name("conformance.v1.AuthResp").unwrap());
    user.set_field_by_name("user_id", Value::String("svc".into()));
    let c = with_incoming(&[("x-w17-user", "U"), ("x-w17-scope-org_id", "o")]);
    let a = principal::act_as(&c, &user, &[("org_id", "o2")]).unwrap();
    assert!(!a.incoming().has("x-w17-user"));
    assert_eq!(a.outgoing().get_all("x-w17-scope-org_id"), ["o2"]);
    assert_eq!(
        principal::forward_to_outgoing(&a)
            .outgoing()
            .get_all("x-w17-scope-org_id"),
        ["o2"]
    );
    let empty = DynamicMessage::new(pool.get_message_by_name("conformance.v1.AuthResp").unwrap());
    assert!(principal::act_as(&c, &empty, &[]).is_err());
    assert!(principal::act_as(&c, &user, &[(" bad", "x")]).is_err());
}

/// A descriptor set with two realms' `AuthResp` (auth + admin_auth, the
/// reference / matrix-admin shape), the user id at different numbers.
fn two_realms_pool() -> DescriptorPool {
    use prost_types::{
        DescriptorProto, FieldDescriptorProto, FileDescriptorProto, FileDescriptorSet,
        field_descriptor_proto::{Label, Type},
    };
    let file = |name: &str, pkg: &str, uid: i32| FileDescriptorProto {
        name: Some(name.into()),
        package: Some(pkg.into()),
        syntax: Some("proto3".into()),
        message_type: vec![DescriptorProto {
            name: Some("AuthResp".into()),
            field: [("email", 1), ("user_id", uid), ("token", 9)]
                .into_iter()
                .filter(|(f, _)| uid != 1 || *f != "email")
                .map(|(f, n)| FieldDescriptorProto {
                    name: Some(f.into()),
                    number: Some(n),
                    label: Some(Label::Optional as i32),
                    r#type: Some(Type::String as i32),
                    json_name: Some(f.into()),
                    ..Default::default()
                })
                .collect(),
            ..Default::default()
        }],
        ..Default::default()
    };
    DescriptorPool::from_file_descriptor_set(FileDescriptorSet {
        file: vec![
            file("app/auth.proto", "app.auth", 1),
            file("app/admin_auth.proto", "app.admin_auth", 2),
        ],
    })
    .unwrap()
}

#[test]
fn act_as_and_user_id_with_two_realms() {
    let _g = AUTH_RESP_LOCK.lock().unwrap_or_else(|e| e.into_inner());
    let pool = two_realms_pool();
    crate::runtime::install_auth_resp(&pool);
    for realm in ["app.auth.AuthResp", "app.admin_auth.AuthResp"] {
        let mut user = DynamicMessage::new(pool.get_message_by_name(realm).unwrap());
        user.set_field_by_name("user_id", Value::String("svc".into()));
        let a = principal::act_as(&Ctx::background(), &user, &[])
            .unwrap_or_else(|e| panic!("{realm}: {e}"));
        // The envelope it stamped reads back as the same user, with no
        // user_id scope to shortcut it.
        let back = Ctx::background().with_incoming(a.outgoing().clone());
        assert_eq!(principal::user_id(&back).as_deref(), Ok("svc"), "{realm}");
        let empty = DynamicMessage::new(pool.get_message_by_name(realm).unwrap());
        assert!(
            principal::act_as(&Ctx::background(), &empty, &[]).is_err(),
            "{realm}"
        );
    }
    // An envelope the two types read as two different users is refused, not
    // guessed: field 1 is auth's user_id and admin_auth's email, field 2 the
    // reverse.
    let mut both =
        DynamicMessage::new(pool.get_message_by_name("app.admin_auth.AuthResp").unwrap());
    both.set_field_by_name("email", Value::String("alice".into()));
    both.set_field_by_name("user_id", Value::String("bob".into()));
    let raw =
        base64::engine::general_purpose::STANDARD.encode(prost::Message::encode_to_vec(&both));
    assert!(principal::user_id(&with_incoming(&[("x-w17-user", &raw)])).is_err());
    assert!(principal::act_as(&Ctx::background(), &both, &[]).is_err());
    principal::set_auth_resp_type(None);
}

#[test]
fn paging_is_forwarded_as_one_set() {
    let c = with_incoming(&[("x-w17-paging-limit", "2")]).with_outgoing(Metadata::from_pairs([
        ("x-w17-paging-boundaries", "old"),
        ("x-other", "k"),
    ]));
    let f = paging::forward(&c);
    assert!(paging::forwarded(&f) && !paging::forwarded(&c));
    assert_eq!(f.outgoing().get_all("x-w17-paging-limit"), ["2"]);
    assert!(
        !f.outgoing().has("x-w17-paging-boundaries"),
        "a stale boundary does not go out"
    );
    assert!(f.outgoing().has("x-other"));
    let none = Ctx::background();
    assert!(!paging::forwarded(&paging::forward(&none)));
}

#[test]
fn for_user_carries_the_detail_and_takes_the_method_prefix() {
    let st = for_user_dev(
        tonic::Code::FailedPrecondition,
        "NOTE_LOCKED",
        "Locked.",
        "text",
        "note 7 is locked",
    );
    let d = error_detail(&st).unwrap();
    assert_eq!(
        (d.code.as_str(), d.message.as_str(), d.field.as_str()),
        ("NOTE_LOCKED", "Locked.", "text")
    );
    let out = finish_for_user(&st, "/conformance.v1.NoteService/Refuse").unwrap();
    assert_eq!(out.message(), "NoteService.Refuse: note 7 is locked");
    assert!(out.metadata().get(crate::errors::FOR_USER_MARKER).is_none());
    assert_eq!(error_detail(&out).unwrap().code, "NOTE_LOCKED");
    assert!(
        finish_for_user(&tonic::Status::internal("x"), "/a.S/M").is_none(),
        "only a for_user status"
    );
    assert_eq!(for_user(tonic::Code::NotFound, "C", "m", "").message(), "m");
    assert_eq!(short_method("/pkg.sub.Svc/Method"), "Svc.Method");
    assert!(error_info(&st).is_none());
}

#[test]
fn error_info_is_read_from_the_details() {
    let info = tonic_types::pb::ErrorInfo {
        reason: "UPSTREAM_HTTP_404".into(),
        domain: "echo".into(),
        metadata: [("http_status".to_string(), "404".to_string())]
            .into_iter()
            .collect(),
    };
    let any = prost_types::Any {
        type_url: "type.googleapis.com/google.rpc.ErrorInfo".into(),
        value: prost::Message::encode_to_vec(&info),
    };
    let st = tonic::Status::with_details(
        tonic::Code::NotFound,
        "echo: upstream answered HTTP 404",
        crate::errors::encode_rpc_status(tonic::Code::NotFound, "x", vec![any]),
    );
    let got = crate::egress::error_info(&st).unwrap();
    assert_eq!(
        (got.domain.as_str(), got.reason.as_str()),
        ("echo", "UPSTREAM_HTTP_404")
    );
    assert_eq!(got.metadata["http_status"], "404");
}
