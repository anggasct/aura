#!/usr/bin/env bash
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
image="aura-exec-check:$$"
cleanup() {
	docker image rm -f "$image" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

mkdir -p "$work/linux/amd64"
(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
	-ldflags "-s -w -X github.com/anggasct/aura/internal/capability.buildProfile=exec-linux -X github.com/anggasct/aura/internal/capability.buildCapabilities=exec-linux" \
	-o "$work/linux/amd64/aura" ./cmd/aura)
cp "$repo/Dockerfile" "$work/Dockerfile"
docker buildx build --quiet --load --platform linux/amd64 -t "$image" "$work" >/dev/null

run() {
	timeout 60 docker run --rm --read-only --tmpfs /tmp --user 65532:65532 --cap-drop ALL \
		--security-opt no-new-privileges:true --network none \
		-e ANTHROPIC_API_KEY=exec-check-placeholder \
		-v "$repo/deploy/smoke/exec-profile.yaml:/etc/aura/aura.yaml:ro" -e AURA_CONFIG=/etc/aura/aura.yaml \
		"$image" "$@"
}

fail() {
	echo "check-exec-container: $*" >&2
	exit 1
}

status_exit=0
status_output="$(run status --offline 2>&1)" || status_exit=$?
missing="$(sed -n 's/^missing: //p' <<<"$status_output")"
echo "check-exec-container: missing containment primitives in the hardened container: ${missing:-none}"
[ "$status_exit" -eq 2 ] || fail "aura status exited $status_exit, want 2: $status_output"
grep -qw cgroup_v2 <<<"$missing" || fail "cgroup_v2 is no longer missing; the exec-profile container decision must be revisited: $status_output"
grep -q 'capability/exec-linux capability_unavailable' <<<"$status_output" || fail "exec-linux is not reported unavailable: $status_output"

server_exit=0
server_output="$(run server 2>&1)" || server_exit=$?
[ "$server_exit" -ne 0 ] && [ "$server_exit" -ne 124 ] || fail "aura server did not refuse to start (exit $server_exit)"
grep -q 'dependency_missing: exec-linux: required dependency process-containment is missing' <<<"$server_output" ||
	fail "aura server did not fail closed on the missing sandbox: $server_output"

echo "check-exec-container: ok (exec-linux fails closed in the hardened container)"
