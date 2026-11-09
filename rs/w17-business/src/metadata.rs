//! gRPC metadata: lower-case keys, each with one or more string values.
//!
//! Immutable in use, like a Go `metadata.MD` handed between contexts after a
//! `Copy()`: a helper that changes metadata returns a new value, so two
//! contexts never share a list one of them could rewrite. Keys are
//! lower-cased on the way in, as HTTP/2 and grpc-go normalise them.

use std::collections::BTreeMap;

use http::HeaderMap;

/// Request headers that are transport, not metadata: never part of a
/// context's incoming metadata.
const TRANSPORT_HEADERS: &[&str] = &[
    "te",
    "content-type",
    "grpc-timeout",
    "grpc-encoding",
    "grpc-accept-encoding",
    "accept-encoding",
];

/// gRPC metadata, key → values.
#[derive(Clone, Default, PartialEq, Eq)]
pub struct Metadata(BTreeMap<String, Vec<String>>);

impl Metadata {
    /// Empty metadata.
    pub fn new() -> Self {
        Self::default()
    }

    /// Metadata from (key, value) pairs; a key given twice keeps both values.
    pub fn from_pairs<K: AsRef<str>, V: Into<String>>(
        pairs: impl IntoIterator<Item = (K, V)>,
    ) -> Self {
        let mut d: BTreeMap<String, Vec<String>> = BTreeMap::new();
        for (k, v) in pairs {
            d.entry(k.as_ref().to_ascii_lowercase())
                .or_default()
                .push(v.into());
        }
        Self(d)
    }

    /// The metadata a request's headers carry: every header but the
    /// transport ones (`te`, `content-type`, `grpc-timeout`, the encodings).
    /// A value that is not visible ASCII is carried as latin-1 text, as the
    /// other SDKs carry it.
    pub fn from_headers(headers: &HeaderMap) -> Self {
        let mut d: BTreeMap<String, Vec<String>> = BTreeMap::new();
        for (k, v) in headers {
            let key = k.as_str();
            if TRANSPORT_HEADERS.contains(&key) {
                continue;
            }
            let text = match v.to_str() {
                Ok(s) => s.to_string(),
                Err(_) => v.as_bytes().iter().map(|&b| b as char).collect(),
            };
            d.entry(key.to_string()).or_default().push(text);
        }
        Self(d)
    }

    /// Every value of `key`, empty when absent (Go's `md.Get`).
    pub fn get_all(&self, key: &str) -> &[String] {
        self.0
            .get(&key.to_ascii_lowercase())
            .map(Vec::as_slice)
            .unwrap_or(&[])
    }

    /// The first value of `key`.
    pub fn first(&self, key: &str) -> Option<&str> {
        self.get_all(key).first().map(String::as_str)
    }

    /// Whether `key` has a value.
    pub fn has(&self, key: &str) -> bool {
        !self.get_all(key).is_empty()
    }

    /// The number of keys.
    pub fn len(&self) -> usize {
        self.0.len()
    }

    /// Whether there are no keys.
    pub fn is_empty(&self) -> bool {
        self.0.is_empty()
    }

    /// The keys, sorted.
    pub fn keys(&self) -> impl Iterator<Item = &str> {
        self.0.keys().map(String::as_str)
    }

    /// (key, values), sorted by key.
    pub fn iter(&self) -> impl Iterator<Item = (&str, &[String])> {
        self.0.iter().map(|(k, v)| (k.as_str(), v.as_slice()))
    }

    /// Flattened (key, value) pairs.
    pub fn pairs(&self) -> impl Iterator<Item = (&str, &str)> {
        self.0
            .iter()
            .flat_map(|(k, vs)| vs.iter().map(move |v| (k.as_str(), v.as_str())))
    }

    /// A copy with `key` replaced by `values` (Go's `md.Set`); no values
    /// deletes it.
    pub fn with_set<V: Into<String>>(
        &self,
        key: &str,
        values: impl IntoIterator<Item = V>,
    ) -> Self {
        let mut d = self.0.clone();
        let vals: Vec<String> = values.into_iter().map(Into::into).collect();
        if vals.is_empty() {
            d.remove(&key.to_ascii_lowercase());
        } else {
            d.insert(key.to_ascii_lowercase(), vals);
        }
        Self(d)
    }

    /// A copy without the keys `pred` answers true for.
    pub fn without(&self, pred: impl Fn(&str) -> bool) -> Self {
        Self(
            self.0
                .iter()
                .filter(|(k, _)| !pred(k))
                .map(|(k, v)| (k.clone(), v.clone()))
                .collect(),
        )
    }

    /// A copy with `other`'s keys REPLACING this one's.
    pub fn merged(&self, other: &Metadata) -> Self {
        let mut d = self.0.clone();
        for (k, v) in &other.0 {
            d.insert(k.clone(), v.clone());
        }
        Self(d)
    }
}

impl std::fmt::Debug for Metadata {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // Values are not printed: a credential rides in metadata.
        f.debug_tuple("Metadata")
            .field(&self.0.keys().collect::<Vec<_>>())
            .finish()
    }
}

impl<K: AsRef<str>, V: Into<String>> FromIterator<(K, V)> for Metadata {
    fn from_iter<T: IntoIterator<Item = (K, V)>>(iter: T) -> Self {
        Self::from_pairs(iter)
    }
}
