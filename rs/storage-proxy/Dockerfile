# Multi-stage build for the storage gRPC proxy.
# The image is the shippable artifact (one published image serves every
# project — the proxy is contract-blind and never recompiles per schema).

# ---- builder ----------------------------------------------------------
FROM rust:1-bookworm AS builder
WORKDIR /build

# Cache dependencies separately from source for fast rebuilds.
# Cargo.lock too, and --locked below: the published image is built from exactly
# the dependency versions this repository's CI audited, never re-resolved.
COPY Cargo.toml Cargo.lock ./
# A throwaway lib + main so `cargo build` can resolve+compile deps before
# the real sources are copied in.
RUN mkdir -p src \
    && echo 'fn main() {}' > src/main.rs \
    && echo '' > src/lib.rs \
    && cargo build --release --locked || true

# Real sources.
COPY src ./src
COPY tests ./tests
# Touch entrypoints so cargo rebuilds them (timestamps changed).
RUN touch src/main.rs src/lib.rs \
    && cargo build --release --locked \
    && strip target/release/storage-proxy

# ---- runtime ----------------------------------------------------------
# debian:bookworm-slim matches the builder's glibc; the binary is dynamic
# against glibc + libgcc only. TLS is rustls + the `ring` provider
# (pure-Rust, statically linked — no OpenSSL .so at runtime). The proxy is
# a re-encrypting TLS hop by default; `ca-certificates` is installed so the
# system-roots path works when a backend uses a publicly-trusted cert and
# no CA is pinned (the common dev/prod paths use a pinned CA or skip-verify
# and don't need it). Distroless cc would also work; slim keeps debugging easy.
FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN useradd --system --uid 10001 --no-create-home w17proxy
COPY --from=builder /build/target/release/storage-proxy /usr/local/bin/storage-proxy

USER 10001
ENV W17_PROXY_LISTEN=0.0.0.0:50051
EXPOSE 50051
ENTRYPOINT ["/usr/local/bin/storage-proxy"]
