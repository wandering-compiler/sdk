# The conformance suite's Rust business tier, built the way the platform's
# image builds an author's crate: the crate's build.rs compiles the suite's
# descriptor set (W17_BUSINESS_MANIFEST names the manifest), `cargo build
# --locked`, and the binary runs from a slim image as `serve --manifest`.
# Build context: the repository root (`make conformance-rs`).
FROM rust:1-bookworm AS build
COPY rs/w17-business /src
WORKDIR /src
ENV W17_BUSINESS_MANIFEST=/src/tests/conformance/service/w17-business.json \
    CARGO_PROFILE_DEV_DEBUG=0 \
    CARGO_INCREMENTAL=0
RUN --mount=type=cache,id=w17-sdk-rs-conformance-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=w17-sdk-rs-conformance-target,target=/src/target \
    cargo build --locked -p conformance-notes \
 && cp target/debug/conformance-notes /business

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /business /app/business
COPY rs/w17-business/tests/conformance/testdata /app/testdata
COPY rs/w17-business/tests/conformance/service/w17-business.json /app/service/w17-business.json
# A descriptor set the binary was NOT compiled from: run.sh boots on it and
# expects the stale-binary refusal.
COPY rs/w17-business/tests/conformance/service/stale.json /app/service/stale.json
COPY rs/w17-business/src/sdk_descriptors.binpb /app/stale/descriptors.binpb
USER 65532
ENTRYPOINT ["/app/business", "serve", "--manifest", "/app/service/w17-business.json"]
