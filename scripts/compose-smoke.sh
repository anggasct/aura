#!/usr/bin/env bash
set -euo pipefail

if [ -z "${AURA_IMAGE:-}" ]; then
	echo "usage: AURA_IMAGE=<image> $0" >&2
	exit 2
fi

deploy_dir="$(cd "$(dirname "$0")/../deploy" && pwd)"
project="aura-smoke-$$"
env_file="$(mktemp)"
printf 'ANTHROPIC_API_KEY=compose-smoke-placeholder\n' >"$env_file"
export AURA_IMAGE AURA_ENV_FILE="$env_file" AURA_VERSION="${AURA_VERSION:-compose-smoke}"

compose() {
	docker compose -p "$project" --project-directory "$deploy_dir" -f "$deploy_dir/compose.yaml" -f "$deploy_dir/smoke/compose.smoke.yaml" "$@"
}

aura() {
	compose exec -T aura /usr/local/bin/aura "$@"
}

fail() {
	echo "compose-smoke: $*" >&2
	compose ps -a >&2 || true
	compose logs --no-color --tail 80 >&2 || true
	exit 1
}

teardown() {
	compose down -v --remove-orphans >/dev/null 2>&1 || true
	rm -f "$env_file"
}
trap teardown EXIT

wait_for_run() {
	local run="$1" want="$2" deadline=$((SECONDS + 90)) status=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		status="$(aura workflow inspect "$run" 2>/dev/null | sed -n 's/^status: //p')"
		if [ "$status" = "$want" ]; then
			return 0
		fi
		sleep 2
	done
	fail "run $run is '$status', want '$want'"
}

compose up -d --wait --wait-timeout 180 || fail "the stack did not become healthy"

if [ -n "$(compose ps --format '{{range .Publishers}}{{if .PublishedPort}}{{.PublishedPort}} {{end}}{{end}}' restate)" ]; then
	fail "restate publishes a port"
fi
status_output="$(aura status 2>&1 || true)"
grep -q '^info durable ok' <<<"$status_output" || fail "aura status does not report the durable runtime healthy: $status_output"
durable_output="$(aura durable status)" || fail "aura durable status failed: $durable_output"
grep -q '^deployment: registered' <<<"$durable_output" || fail "the handler deployment is not registered: $durable_output"

run="$(aura workflow start smoke-wait | sed -n 's/^run: //p')"
[ -n "$run" ] || fail "workflow start returned no run id"
wait_for_run "$run" suspended

compose restart aura >/dev/null || fail "aura did not restart"
compose up -d --wait --wait-timeout 120 aura || fail "aura did not become healthy after the restart"

aura workflow signal "$run" --step gate --payload '{"approved":true}' >/dev/null || fail "signal failed"
wait_for_run "$run" succeeded

echo "compose-smoke: ok ($run survived an aura restart)"
