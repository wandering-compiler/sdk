# protoc and its two Go plugins, pinned to what the committed go/pb headers
# declare: protoc v5.29.3 (protobuf release 29.3), protoc-gen-go v1.36.11,
# protoc-gen-go-grpc v1.6.2. A different version rewrites every header, so the
# pins move together and only on purpose.
FROM golang:1.26-alpine AS plugins
ENV CGO_ENABLED=0
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11 \
 && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2

FROM debian:bookworm-slim
ARG PROTOC_VERSION=29.3
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl unzip; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      amd64) protoc_arch=x86_64 ;; \
      arm64) protoc_arch=aarch_64 ;; \
      *) echo "no pinned protoc build for dpkg arch '$arch'" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/protoc.zip \
      "https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-linux-${protoc_arch}.zip"; \
    unzip -q /tmp/protoc.zip -d /usr/local bin/protoc 'include/*'; \
    rm /tmp/protoc.zip; \
    apt-get purge -y --auto-remove curl unzip; \
    rm -rf /var/lib/apt/lists/*; \
    protoc --version
COPY --from=plugins /go/bin/protoc-gen-go      /usr/local/bin/protoc-gen-go
COPY --from=plugins /go/bin/protoc-gen-go-grpc /usr/local/bin/protoc-gen-go-grpc
ENTRYPOINT ["protoc"]
