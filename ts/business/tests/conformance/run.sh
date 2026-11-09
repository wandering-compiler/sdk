#!/bin/sh
# Runs the conformance suite against the TypeScript business SDK, all in docker
# and with no published port: the Go harness (storage + distx coordinator +
# NATS + sdk/go's subscriber + the business companion and its fake upstream)
# and the TypeScript tier talk over a network this script creates and removes.
#
#   ts/business/tests/conformance/run.sh <typescript-image> <name-prefix>
#
# GO_IMAGE (default golang:1.26) runs the harness with `go test`, the
# checkout mounted read-only; GO_DOCKER_FLAGS adds flags to that container
# (cache mounts). `make conformance-ts` builds the image and calls this. Every
# container and the network are named <name-prefix>-*, and removed on exit
# whatever happens.
set -eu

IMAGE=$1
PREFIX=$2
NET="$PREFIX-net"
HARNESS="$PREFIX-harness"
BIZ="$PREFIX-biz"
GO_IMAGE=${GO_IMAGE:-golang:1.26}
GO_DOCKER_FLAGS=${GO_DOCKER_FLAGS:-}
ROOT=$(cd "$(dirname "$0")/../../../.." && pwd)

cleanup() {
	docker rm -f "$HARNESS" "$BIZ" >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM
cleanup
docker network create "$NET" >/dev/null

# 1. A required config key that is missing stops the boot and names the key.
#    (The suite's manifest declares api_token required and secret.)
set +e
out=$(docker run --rm --network "$NET" -e NOTES_STORAGE_BACKEND_ADDR=nowhere:1 "$IMAGE" 2>&1)
rc=$?
set -e
if [ "$rc" -eq 0 ] || ! printf '%s' "$out" | grep -q "'api_token'.*NOTES_API_TOKEN is required"; then
	echo "FAIL missing required config key: exit $rc, output:"
	printf '%s\n' "$out"
	exit 1
fi
echo "ok   missing required config key stops the boot naming NOTES_API_TOKEN"

# 2. The harness first: storage, NATS, the Go subscriber and the companion are
#    up once it prints its ready line; only then the tier, which dials them.
# shellcheck disable=SC2086 # GO_DOCKER_FLAGS is a list of flags
docker run -d --name "$HARNESS" --network "$NET" $GO_DOCKER_FLAGS \
	-v "$ROOT:$ROOT:ro" -w "$ROOT/ts/business/tests/conformance" \
	-e CONFORMANCE_BUSINESS_ADDR="$BIZ:50061" \
	-e W17_CLIENT_ECHO_DSN="http://127.0.0.1:50072?auth=bearer&token=conformance" \
	"$GO_IMAGE" go test -count=1 -v . >/dev/null
i=0
until docker logs "$HARNESS" 2>&1 | grep -q "conformance: harness ready"; do
	if [ "$(docker inspect -f '{{.State.Running}}' "$HARNESS")" != "true" ]; then
		docker logs "$HARNESS" 2>&1 | tail -n 100
		echo "FAIL conformance (typescript): the harness exited before it was ready"
		exit 1
	fi
	i=$((i + 1))
	if [ "$i" -gt 600 ]; then
		echo "FAIL conformance (typescript): the harness was not ready after 10 minutes"
		exit 1
	fi
	sleep 1
done
docker run -d --name "$BIZ" --network "$NET" \
	-e NOTES_STORAGE_BACKEND_ADDR="$HARNESS:50051" \
	-e NOTES_EVENTBUS_NATS_DSN="nats://$HARNESS:4222" \
	-e NOTES_COMPANION_ADDR="$HARNESS:50071" \
	-e NOTES_API_TOKEN=conformance \
	"$IMAGE" >/dev/null

rc=$(docker wait "$HARNESS")
docker logs "$HARNESS" 2>&1
if [ "$rc" -ne 0 ]; then
	echo "--- $BIZ log"
	docker logs "$BIZ" 2>&1 | tail -n 200
	echo "FAIL conformance (typescript): harness exit $rc"
	exit 1
fi
echo "ok   conformance (typescript)"
