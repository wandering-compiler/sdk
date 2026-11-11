# The SDK's generated Go code comes from the protos under proto/ (the repository
# root: the TypeScript runtime and the compiler read them too, not only Go), with
# protoc and its plugins pinned in tools/protoc (no host toolchain).
#
#   make pb        regenerate go/pb from proto
#   make check-pb  fail when the committed go/pb is not what make pb writes
#
#   make sync-admin-dpop   copy ts/dpop/src into ts/admin-runtime/src/dpop
#   make check-admin-dpop  fail when that copy has drifted (CI, ts job)
#
#   make check-refs  fail on a path into the w17 platform's private repository
#
# go/pb/consoleapi/rpc is the exception: the console's own API definition
# generates it, so it has no source here — the console's maintainers refresh it
# with a pull request when that API changes. make pb leaves it alone.

PROTOC_IMAGE ?= w17-sdk-protoc:29.3
MOD := github.com/wandering-compiler/sdk/go/pb
PROTOC = docker run --rm -u "$$(id -u):$$(id -g)" -v "$(CURDIR):/src" -w /src $(PROTOC_IMAGE) \
	--proto_path=proto --go_out=go/pb --go_opt=module=$(MOD)
PROTOC_GRPC = $(PROTOC) --go-grpc_out=go/pb --go-grpc_opt=module=$(MOD)

.PHONY: protoc-image pb check-pb
protoc-image:
	@docker image inspect $(PROTOC_IMAGE) >/dev/null 2>&1 || docker build -q -t $(PROTOC_IMAGE) tools/protoc >/dev/null

pb: protoc-image
	$(PROTOC_GRPC) $$(ls proto/w17/*.proto | sed 's|^proto/||')
	for d in $$(ls -d proto/w17/*/ | sed 's|^proto/||'); do $(PROTOC_GRPC) $$(ls proto/$$d*.proto | sed 's|^proto/||') || exit 1; done
	$(PROTOC) w17apply/dev_plan.proto
	$(PROTOC_GRPC) w17apply/fetch.proto
	$(PROTOC_GRPC) w17compiler/codegen.proto
	$(PROTOC_GRPC) w17registry/registry.proto
	$(PROTOC_GRPC) common/distx/distributed_tx.proto
	$(PROTOC_GRPC) common/files/files.proto
	$(PROTOC) domains/console/secrets/secrets.proto
	$(PROTOC) domains/console/support/support.proto
	$(PROTOC_GRPC) \
		domains/console/initiatives/initiatives.proto \
		domains/console/review/review.proto \
		domains/console/checkpoints/checkpoints.proto \
		domains/console/auth/business/auth_service.proto

check-pb: pb
	@if [ -n "$$(git status --porcelain -- go/pb)" ]; then \
		git status --short -- go/pb; \
		echo "check-pb: go/pb differs from what make pb writes — run make pb and commit"; exit 1; \
	fi
	@echo "check-pb ok: go/pb matches proto"

.PHONY: sync-admin-dpop check-admin-dpop
sync-admin-dpop:
	@sh tools/admin-dpop.sh sync

check-admin-dpop:
	@sh tools/admin-dpop.sh check

# Paths that only mean something inside the w17 platform's private repository
# (its docs, its source tree) point a public reader at nothing. Say the reason
# in place instead. (`git grep` exits 1 for no match and 2+ for a real failure,
# which must not read as "clean".)
.PHONY: check-refs
check-refs:
	@hits="$$(git grep -nE 'docs/(todos|decisions|specs|runbooks|audit|archive)/|\bsrcgo/(domains|tests)/' -- ':!Makefile' ':!**/go.sum' ':!**/package-lock.json')"; rc=$$?; \
	[ $$rc -le 1 ] || { echo "!! git grep failed ($$rc)"; exit 1; }; \
	if [ -n "$$hits" ]; then echo "!! these cite the platform's private repository:"; echo "$$hits"; exit 1; fi

# The Python business SDK (py/) and its conformance suite
# (py/tests/conformance/), all in Docker with no published port.
#
#   make test-py             the SDK's unit tests (pytest)
#   make py-descriptors      regenerate py/src/w17_business/sdk_descriptors.binpb
#   make check-py-descriptors  fail when the committed set is not what that writes
#   make conformance-py      the Go harness against the Python tier
#   make check-conformance-descriptors  same check for py/tests/conformance/testdata
#
# GO_DOCKER_FLAGS lets a caller mount its Go caches into the build container
# (e.g. `-v $$HOME/go:/go -v $$HOME/.cache:/root/.cache`).
PY_TEST_IMAGE ?= w17-sdk-py-test:3.13
PY_CONF_IMAGE ?= w17-sdk-py-conformance:dev
CONF_PREFIX ?= w17-sdk-conformance
GO_IMAGE ?= golang:1.26
GO_DOCKER_FLAGS ?=
PROTOC_DESC = docker run --rm -u "$$(id -u):$$(id -g)" -v "$(CURDIR):/src" -w /src $(PROTOC_IMAGE) --include_imports

.PHONY: test-py py-descriptors check-py-descriptors conformance-descriptors check-conformance-descriptors conformance-py
test-py:
	docker build -q -t $(PY_TEST_IMAGE) -f py/Dockerfile.test . >/dev/null
	docker run --rm -u "$$(id -u):$$(id -g)" -e HOME=/tmp -v "$(CURDIR):/src:ro" -w /src/py \
		-e PYTHONPATH=/src/py/src -e PYTHONDONTWRITEBYTECODE=1 $(PY_TEST_IMAGE) \
		python -m pytest -q -p no:cacheprovider tests

# The w17 messages the Python runtime speaks itself (ErrorDetail, Paging,
# InvalidateEvent, the transaction coordinator, the companion's W17Files), as
# one descriptor set.
py-descriptors: protoc-image
	$(PROTOC_DESC) --proto_path=proto --descriptor_set_out=py/src/w17_business/sdk_descriptors.binpb \
		w17/error.proto w17/paging.proto w17/client.proto common/distx/distributed_tx.proto \
		common/files/files.proto

check-py-descriptors: py-descriptors
	@if [ -n "$$(git status --porcelain -- py/src/w17_business/sdk_descriptors.binpb)" ]; then \
		echo "check-py-descriptors: run make py-descriptors and commit"; exit 1; fi

conformance-descriptors: protoc-image
	$(PROTOC_DESC) --proto_path=proto --proto_path=py/tests/conformance/proto \
		--descriptor_set_out=py/tests/conformance/testdata/descriptors.binpb \
		conformance/v1/conformance.proto conformance/events/lock.proto conformance/clients/echo/echo.proto

check-conformance-descriptors: conformance-descriptors
	@if [ -n "$$(git status --porcelain -- py/tests/conformance/testdata/descriptors.binpb)" ]; then \
		echo "check-conformance-descriptors: run make conformance-descriptors and commit"; exit 1; fi

conformance-py:
	docker build -q -t $(PY_CONF_IMAGE) -f py/tests/conformance/service/Dockerfile . >/dev/null
	GO_IMAGE="$(GO_IMAGE)" GO_DOCKER_FLAGS="$(GO_DOCKER_FLAGS)" sh py/tests/conformance/run.sh $(PY_CONF_IMAGE) $(CONF_PREFIX)

# The TypeScript business SDK (ts/business/, `@w17/business`) and its own copy
# of the conformance suite (ts/business/tests/conformance/), all in Docker
# (node:24, golang) with no published port.
#
#   make test-ts-business      typecheck, lint and the unit tests (vitest)
#   make ts-descriptors        regenerate ts/business/src/sdk_descriptors.binpb
#   make check-ts-descriptors  fail when the committed set is not what that writes
#   make conformance-ts        the Go harness against the TypeScript tier
#   make conformance-ts-descriptors / check-conformance-ts-descriptors
#                              the same for ts/business/tests/conformance/testdata
NODE_IMAGE ?= node:24
TS_CONF_IMAGE ?= w17-sdk-ts-conformance:dev
TS_CONF_PREFIX ?= w17-sdk-ts-conformance
NODE_RUN = docker run --rm -u "$$(id -u):$$(id -g)" -e HOME=/tmp -e npm_config_update_notifier=false \
	-v "$(CURDIR)/ts/business:/w" -w /w $(NODE_IMAGE)

.PHONY: test-ts-business ts-descriptors check-ts-descriptors conformance-ts-descriptors check-conformance-ts-descriptors conformance-ts
test-ts-business:
	$(NODE_RUN) npm ci --no-audit --no-fund
	$(NODE_RUN) npm run typecheck
	$(NODE_RUN) npx tsc --noEmit -p tests/conformance/service/tsconfig.json
	$(NODE_RUN) npm run lint
	$(NODE_RUN) npm test

# The w17 messages the TypeScript runtime speaks itself (ErrorDetail, Paging,
# InvalidateEvent, the transaction coordinator, the companion's W17Files), as
# one descriptor set — the same inputs as py-descriptors.
ts-descriptors: protoc-image
	$(PROTOC_DESC) --proto_path=proto --descriptor_set_out=ts/business/src/sdk_descriptors.binpb \
		w17/error.proto w17/paging.proto w17/client.proto common/distx/distributed_tx.proto \
		common/files/files.proto

check-ts-descriptors: ts-descriptors
	@if [ -n "$$(git status --porcelain -- ts/business/src/sdk_descriptors.binpb)" ]; then \
		echo "check-ts-descriptors: run make ts-descriptors and commit"; exit 1; fi

conformance-ts-descriptors: protoc-image
	$(PROTOC_DESC) --proto_path=proto --proto_path=ts/business/tests/conformance/proto \
		--descriptor_set_out=ts/business/tests/conformance/testdata/descriptors.binpb \
		conformance/v1/conformance.proto conformance/events/lock.proto conformance/clients/echo/echo.proto

check-conformance-ts-descriptors: conformance-ts-descriptors
	@if [ -n "$$(git status --porcelain -- ts/business/tests/conformance/testdata/descriptors.binpb)" ]; then \
		echo "check-conformance-ts-descriptors: run make conformance-ts-descriptors and commit"; exit 1; fi

conformance-ts:
	docker build -q -t $(TS_CONF_IMAGE) -f ts/business/tests/conformance/service/Dockerfile . >/dev/null
	GO_IMAGE="$(GO_IMAGE)" GO_DOCKER_FLAGS="$(GO_DOCKER_FLAGS)" sh ts/business/tests/conformance/run.sh $(TS_CONF_IMAGE) $(TS_CONF_PREFIX)

# The Rust business SDK (rs/w17-business/: the runtime crate `w17-business`,
# the build helper `w17-business-build` in build/, and the conformance
# service) and its own copy of the conformance suite
# (rs/w17-business/tests/conformance/), all in Docker with no published port.
#
#   make test-rs-business      rustfmt, clippy, the tests and cargo audit, for the workspace
#   make rs-descriptors        regenerate rs/w17-business/src/sdk_descriptors.binpb
#   make check-rs-descriptors  fail when the committed set is not what that writes
#   make conformance-rs        the Go harness against the Rust tier
#   make conformance-rs-descriptors / check-conformance-rs-descriptors
#                              the same for rs/w17-business/tests/conformance/testdata
#
# RS_DOCKER_FLAGS lets a caller mount a cargo home into the test container
# (e.g. `-v $$HOME/.cargo-docker:/tmp/cargo`).
RS_TEST_IMAGE ?= w17-sdk-rs-test:dev
RS_CONF_IMAGE ?= w17-sdk-rs-conformance:dev
RS_CONF_PREFIX ?= w17-sdk-rs-conformance
RS_DOCKER_FLAGS ?=
RS_RUN = docker run --rm -u "$$(id -u):$$(id -g)" -e HOME=/tmp -e CARGO_HOME=/tmp/cargo $(RS_DOCKER_FLAGS) \
	-v "$(CURDIR)/rs/w17-business:/w" -w /w $(RS_TEST_IMAGE)

.PHONY: test-rs-business rs-descriptors check-rs-descriptors conformance-rs-descriptors check-conformance-rs-descriptors conformance-rs
test-rs-business:
	docker build -q -t $(RS_TEST_IMAGE) -f rs/w17-business/Dockerfile.test rs/w17-business >/dev/null
	$(RS_RUN) cargo fmt --all --check
	$(RS_RUN) cargo clippy --locked --workspace --all-targets -- -D warnings
	$(RS_RUN) cargo test --locked --workspace
	$(RS_RUN) cargo audit

# The w17 vocabulary the Rust runtime speaks and maps an author's crate onto:
# every package under w17 (the whole vocabulary, not only the messages the
# runtime reads, because an author's descriptor set may name any of them),
# the transaction coordinator and the companion's W17Files.
rs-descriptors: protoc-image
	$(PROTOC_DESC) --proto_path=proto --descriptor_set_out=rs/w17-business/src/sdk_descriptors.binpb \
		$$(cd proto && ls w17/*.proto w17/*/*.proto) common/distx/distributed_tx.proto common/files/files.proto

check-rs-descriptors: rs-descriptors
	@if [ -n "$$(git status --porcelain -- rs/w17-business/src/sdk_descriptors.binpb)" ]; then \
		echo "check-rs-descriptors: run make rs-descriptors and commit"; exit 1; fi

conformance-rs-descriptors: protoc-image
	$(PROTOC_DESC) --proto_path=proto --proto_path=rs/w17-business/tests/conformance/proto \
		--descriptor_set_out=rs/w17-business/tests/conformance/testdata/descriptors.binpb \
		conformance/v1/conformance.proto conformance/events/lock.proto conformance/clients/echo/echo.proto

check-conformance-rs-descriptors: conformance-rs-descriptors
	@if [ -n "$$(git status --porcelain -- rs/w17-business/tests/conformance/testdata/descriptors.binpb)" ]; then \
		echo "check-conformance-rs-descriptors: run make conformance-rs-descriptors and commit"; exit 1; fi

conformance-rs:
	docker build -q -t $(RS_CONF_IMAGE) -f rs/w17-business/tests/conformance/service/Dockerfile . >/dev/null
	GO_IMAGE="$(GO_IMAGE)" GO_DOCKER_FLAGS="$(GO_DOCKER_FLAGS)" sh rs/w17-business/tests/conformance/run.sh $(RS_CONF_IMAGE) $(RS_CONF_PREFIX)
