//! Errors that say one thing to an operator and another to a person.
//!
//! The Rust twin of `grpcerr.ForUser` (sdk/go/core/grpcerr): the gRPC status
//! carries the OPERATOR's copy, prefixed with the method (`Svc.Method:
//! <dev>`) when it leaves the handler, and a `w17.ErrorDetail{code, message,
//! field}` carries the caller's sentence, which the gateway renders. Encoded
//! as Go encodes it: a `google.rpc.Status` in `grpc-status-details-bin` with
//! the detail packed as `type.googleapis.com/w17.ErrorDetail`.
//!
//! How the runtime answers what a handler returns:
//!
//! - a `Status` goes out as it is, details included — [`for_user`]'s (with
//!   the method prefix), or one a storage or companion call returned that
//!   the handler let through, as a Go handler returns `err` unchanged;
//! - the SDK's own errors ([`crate::distx::Error`],
//!   [`crate::principal::NoPrincipal`], [`crate::config::ConfigError`])
//!   convert into `UNKNOWN` with their message — a Go *returned* error;
//! - a panic is a Go panic: reported, and answered `INTERNAL`
//!   `"<method>: internal error"`; its text never reaches the caller.

use bytes::Bytes;
use prost::Message as _;
use tonic::{Code, Status, metadata::MetadataMap};

use crate::pb::{google::protobuf::Any, w17::ErrorDetail};

const TYPE_PREFIX: &str = "type.googleapis.com/";

/// The status metadata key that marks a [`for_user`] status for the server
/// middleware, which prefixes its message with the method and drops the
/// key. Never sent on the wire.
pub(crate) const FOR_USER_MARKER: &str = "x-w17-sdk-for-user";

/// `google.rpc.Status` bytes.
pub(crate) fn encode_rpc_status(code: Code, message: &str, details: Vec<Any>) -> Bytes {
    tonic_types::pb::Status {
        code: code as i32,
        message: message.to_string(),
        details,
    }
    .encode_to_vec()
    .into()
}

/// The packed details a status carries, decoded from
/// `grpc-status-details-bin`; empty when there are none or they do not
/// decode.
pub fn status_details(st: &Status) -> Vec<Any> {
    if st.details().is_empty() {
        return Vec::new();
    }
    tonic_types::pb::Status::decode(st.details())
        .map(|s| s.details)
        .unwrap_or_default()
}

/// The status a handler returns to refuse with a sentence for a person.
///
/// ```ignore
/// return Err(for_user(Code::FailedPrecondition, "SUBSCRIPTION_INACTIVE",
///                     "This subscription is not active.", ""));
/// ```
///
/// `detail_code` is what a client dispatches on, `message` the sentence it
/// shows, `field` the request field it is about ("" for none). The operator's
/// copy (the status message) is `message`; [`for_user_dev`] gives it apart.
pub fn for_user(code: Code, detail_code: &str, message: &str, field: &str) -> Status {
    for_user_dev(code, detail_code, message, field, message)
}

/// [`for_user`] with the operator's copy for logs and Sentry, `dev`, apart
/// from the sentence for the person.
pub fn for_user_dev(
    code: Code,
    detail_code: &str,
    message: &str,
    field: &str,
    dev: &str,
) -> Status {
    let detail = ErrorDetail {
        code: detail_code.to_string(),
        message: message.to_string(),
        field: field.to_string(),
    };
    let any = Any {
        type_url: format!("{TYPE_PREFIX}w17.ErrorDetail"),
        value: detail.encode_to_vec(),
    };
    let mut md = MetadataMap::new();
    md.insert(
        FOR_USER_MARKER,
        tonic::metadata::MetadataValue::from_static("1"),
    );
    Status::with_details_and_metadata(code, dev, encode_rpc_status(code, dev, vec![any]), md)
}

/// The `w17.ErrorDetail` a status carries, if any.
pub fn error_detail(st: &Status) -> Option<ErrorDetail> {
    status_details(st)
        .into_iter()
        .find(|d| d.type_url.ends_with("/w17.ErrorDetail"))
        .and_then(|d| ErrorDetail::decode(d.value.as_slice()).ok())
}

/// A `google.rpc.ErrorInfo`: why a call failed, machine-readable. The
/// companion's errors (egress, file storage) carry one.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct ErrorInfo {
    pub domain: String,
    pub reason: String,
    pub metadata: std::collections::BTreeMap<String, String>,
}

/// The `google.rpc.ErrorInfo` a status carries, if any.
pub fn error_info(st: &Status) -> Option<ErrorInfo> {
    status_details(st)
        .into_iter()
        .find(|d| d.type_url.rsplit('/').next() == Some("google.rpc.ErrorInfo"))
        .and_then(|d| tonic_types::pb::ErrorInfo::decode(d.value.as_slice()).ok())
        .map(|i| ErrorInfo {
            domain: i.domain,
            reason: i.reason,
            metadata: i.metadata.into_iter().collect(),
        })
}

/// "/pkg.Svc/Method" → "Svc.Method", Go's `method` prefix.
pub(crate) fn short_method(path: &str) -> String {
    let p = path.trim_start_matches('/');
    let (svc, method) = p.split_once('/').unwrap_or((p, ""));
    let svc = svc.rsplit('.').next().unwrap_or(svc);
    if method.is_empty() {
        svc.to_string()
    } else {
        format!("{svc}.{method}")
    }
}

/// A [`for_user`] status as it leaves `path`'s handler: the message prefixed
/// with the method (also inside its details), the marker dropped. `None`
/// for any other status.
pub(crate) fn finish_for_user(st: &Status, path: &str) -> Option<Status> {
    st.metadata().get(FOR_USER_MARKER)?;
    let prefix = format!("{}: ", short_method(path));
    let msg = if st.message().starts_with(&prefix) {
        st.message().to_string()
    } else {
        format!("{prefix}{}", st.message())
    };
    let mut md = st.metadata().clone();
    md.remove(FOR_USER_MARKER);
    let details = encode_rpc_status(st.code(), &msg, status_details(st));
    Some(Status::with_details_and_metadata(
        st.code(),
        msg,
        details,
        md,
    ))
}
