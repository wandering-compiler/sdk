use std::{sync::Arc, time::Duration};

use bytes::Bytes;
use futures::StreamExt as _;
use http::HeaderValue;

use crate::{
    channel::encode_timeout,
    files::{self, Body, CHUNK_SIZE, Failure, PutStream, resolve_field},
    manifest::FilePlacement,
    pb::w17::common::files::put_request::Part,
    runtime::{parse_addr, parse_duration, probe_target},
    server::{frame_message, parse_timeout},
    txscope::{Outcome, Scope},
};

#[test]
fn grpc_timeouts_both_ways() {
    assert_eq!(
        parse_timeout(&HeaderValue::from_static("300m")),
        Some(Duration::from_millis(300))
    );
    assert_eq!(
        parse_timeout(&HeaderValue::from_static("2S")),
        Some(Duration::from_secs(2))
    );
    assert_eq!(
        parse_timeout(&HeaderValue::from_static("1H")),
        Some(Duration::from_secs(3600))
    );
    for bad in ["", "m", "12x", "123456789m"] {
        assert_eq!(
            parse_timeout(&HeaderValue::from_str(bad).unwrap()),
            None,
            "{bad}"
        );
    }
    assert_eq!(encode_timeout(Duration::from_millis(1500)), "1500m");
    assert_eq!(
        encode_timeout(Duration::from_micros(10)),
        "1m",
        "never zero"
    );
    assert_eq!(encode_timeout(Duration::from_secs(200_000)), "200000S");
}

#[test]
fn grpc_frames() {
    assert_eq!(
        frame_message(Bytes::from_static(&[0, 0, 0, 0, 2, 7, 8])).unwrap(),
        Bytes::from_static(&[7, 8])
    );
    assert!(
        frame_message(Bytes::from_static(&[1, 0, 0, 0, 0])).is_err(),
        "compressed"
    );
    assert!(
        frame_message(Bytes::from_static(&[0, 0, 0, 0, 3, 1])).is_err(),
        "truncated"
    );
}

#[test]
fn addresses_and_durations() {
    assert_eq!(parse_addr(":50051").unwrap(), ("localhost".into(), 50051));
    assert_eq!(parse_addr("[::1]:9").unwrap(), ("::1".into(), 9));
    assert!(parse_addr("host").is_err());
    assert_eq!(probe_target(":50061").unwrap(), ("127.0.0.1".into(), 50061));
    assert_eq!(probe_target("0.0.0.0:1").unwrap().0, "127.0.0.1");
    assert!(probe_target(":0").is_err());
    assert_eq!(parse_duration("500ms").unwrap(), Duration::from_millis(500));
    assert_eq!(parse_duration("1.5s").unwrap(), Duration::from_millis(1500));
    assert_eq!(parse_duration("1m").unwrap(), Duration::from_secs(60));
    assert!(parse_duration("3").is_err() && parse_duration("-1s").is_err());
}

fn placement(field: &str) -> FilePlacement {
    FilePlacement {
        field: field.into(),
        name: String::new(),
        connection: "c".into(),
        bucket_path: "/b".into(),
        expected_objects: 0,
        max_per_bucket: 0,
        max_size_bytes: 0,
    }
}

#[test]
fn file_fields_resolve_by_full_or_short_name() {
    let ps = [
        placement("app.a.Note.file"),
        placement("app.b.Note.file"),
        placement("app.a.Doc.pdf"),
    ];
    assert_eq!(
        resolve_field("app.a.Note.file", &ps).unwrap().field,
        "app.a.Note.file"
    );
    assert_eq!(
        resolve_field("Doc.pdf", &ps).unwrap().field,
        "app.a.Doc.pdf"
    );
    assert!(
        resolve_field("Note.file", &ps)
            .unwrap_err()
            .to_string()
            .contains("ambiguous")
    );
    assert!(
        resolve_field("pdf", &ps)
            .unwrap_err()
            .to_string()
            .contains("not a field")
    );
}

async fn parts(body: Body) -> (Vec<Part>, Arc<Failure>) {
    let failed = Arc::new(Failure::default());
    let s = PutStream::new("f".into(), body, failed.clone());
    // A failed body never ends the stream: take what it sends before that.
    let parts = tokio::time::timeout(
        Duration::from_millis(200),
        s.map(|r| r.part.unwrap()).collect::<Vec<_>>(),
    )
    .await
    .unwrap_or_default();
    (parts, failed)
}

#[tokio::test]
async fn a_put_is_header_64k_chunks_and_the_end() {
    let (p, _) = parts(Body::from(vec![1u8; 200 << 10])).await;
    assert!(matches!(&p[0], Part::Header(h) if h.field == "f"));
    let sizes: Vec<usize> = p
        .iter()
        .filter_map(|x| {
            if let Part::Chunk(c) = x {
                Some(c.len())
            } else {
                None
            }
        })
        .collect();
    assert_eq!(sizes, [CHUNK_SIZE, CHUNK_SIZE, CHUNK_SIZE, 8 << 10]);
    assert!(matches!(p.last(), Some(Part::End(e)) if e.size == 200 << 10));

    let stream = futures::stream::iter(
        (0..5).map(|_| Ok::<_, std::io::Error>(Bytes::from(vec![0u8; 50_000]))),
    );
    let (p, _) = parts(Body::stream(stream)).await;
    assert_eq!(
        p.len(),
        1 + 4 + 1,
        "re-chunked to 64 KiB whatever the parts"
    );
}

#[tokio::test]
async fn a_failed_body_never_sends_the_end() {
    let stream = futures::stream::iter(vec![
        Ok(Bytes::from(vec![0u8; 10])),
        Err(std::io::Error::other("boom")),
    ]);
    let (p, failed) = parts(Body::stream(stream)).await;
    assert!(p.iter().all(|x| !matches!(x, Part::End(_))), "{p:?}");
    let err = files::Error::Body(failed.take());
    assert_eq!(err.to_string(), "boom");
}

#[tokio::test]
async fn parked_work_runs_after_commit_and_never_after_rollback() {
    let committed = Scope::default();
    let ran = Arc::new(std::sync::Mutex::new(Vec::new()));
    for i in 0..2 {
        let r = ran.clone();
        let (out, back) = committed.park(Box::pin(async move {
            r.lock().unwrap().push(i);
            if i == 1 {
                Err("hook failed".into())
            } else {
                Ok(())
            }
        }));
        assert_eq!((out, back.is_none()), (Outcome::Deferred, true));
    }
    assert_eq!(committed.run_committed().await, ["hook failed"]);
    assert_eq!(*ran.lock().unwrap(), [0, 1]);
    let (out, back) = committed.park(Box::pin(async { Ok(()) }));
    assert_eq!((out, back.is_some()), (Outcome::AlreadyCommitted, true));

    let rolled = Scope::default();
    rolled.park(Box::pin(async { panic!("must not run") }));
    rolled.mark_rollback_only();
    assert!(rolled.rollback_only());
    rolled.discarded();
    assert!(!rolled.active());
    assert_eq!(
        rolled.park(Box::pin(async { Ok(()) })).0,
        Outcome::Discarded
    );
}
