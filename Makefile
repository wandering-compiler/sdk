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
	$(PROTOC) $$(ls proto/w17/*.proto | sed 's|^proto/||')
	$(PROTOC) $$(ls proto/w17/*/*.proto | sed 's|^proto/||')
	$(PROTOC) w17apply/dev_plan.proto
	$(PROTOC_GRPC) w17apply/fetch.proto
	$(PROTOC_GRPC) w17compiler/codegen.proto
	$(PROTOC_GRPC) w17registry/registry.proto
	$(PROTOC_GRPC) common/distx/distributed_tx.proto
	$(PROTOC) domains/console/secrets/secrets.proto
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
