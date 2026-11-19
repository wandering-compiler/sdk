# ghcr.io/wandering-compiler/sdk-go — the build image of a w17 Go service.
#
# Go, the SDK's modules downloaded and the SDK's packages compiled for both
# targets a w17 build produces (linux/amd64 and linux/arm64, CGO off), so a
# generated bundle's build compiles only the project's own code and what it
# adds. The image is built for each builder platform: Go's build cache is keyed
# by the compiler, so an amd64 builder needs objects its own toolchain made,
# for both targets, and so does an arm64 one. The generated Dockerfile takes it
# `FROM --platform=$BUILDPLATFORM` and seeds its cache mounts from it.
#
# The opt-in SQLite driver (tooling/migrate/sqlitedriver) is left out, neither
# compiled nor kept (its .mod files stay, the module graph reads them): the few
# projects that link it download and compile it themselves. Module zips go too;
# a build reads the extracted trees.
ARG GO_IMAGE=golang:1.27-alpine
FROM ${GO_IMAGE}
ARG SDK_VERSION
ENV CGO_ENABLED=0
WORKDIR /opt/w17-sdk-warm
RUN test -n "$SDK_VERSION" \
 && go mod init w17.dev/sdk-warm >/dev/null 2>&1 \
 && go get "github.com/wandering-compiler/sdk/go/...@${SDK_VERSION}" \
 && pkgs=$(go list -f '{{.ImportPath}} {{join .Deps " "}}' github.com/wandering-compiler/sdk/go/... \
      | grep -v ' modernc.org/' | cut -d' ' -f1) \
 && for arch in amd64 arm64; do GOOS=linux GOARCH=$arch go build $pkgs; done \
 && rm -rf /go/pkg/mod/modernc.org \
 && find /go/pkg/mod/cache/download -name '*.zip' -delete \
 && go clean -testcache
LABEL org.opencontainers.image.source="https://github.com/wandering-compiler/sdk" \
      org.opencontainers.image.description="Go with the w17 SDK's modules and packages compiled for linux/amd64 and linux/arm64" \
      org.opencontainers.image.licenses="MIT"
WORKDIR /src
