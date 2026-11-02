#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -lt 1 ]; then
	echo "usage: $0 <image> [<image>...]" >&2
	exit 2
fi

status=0
for image in "$@"; do
	failed=0
	user="$(docker image inspect --format '{{.Config.User}}' "$image")"
	if [ "$user" != "65532:65532" ]; then
		echo "check-image: $image runs as '$user', want 65532:65532" >&2
		failed=1
	fi
	platform="$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image")"
	if ! docker run --rm --read-only --user 65532:65532 --network none --platform "$platform" "$image" version >/dev/null; then
		echo "check-image: $image does not run 'aura version' read-only as 65532" >&2
		failed=1
	fi
	container="$(docker create --platform "$platform" "$image")"
	listing="$(docker export "$container" | tar -t)"
	docker rm "$container" >/dev/null
	while IFS= read -r binary; do
		[ -n "$binary" ] || continue
		dir="$(dirname "$binary")"
		if ! grep -Fxq "$dir/LICENSE" <<<"$listing"; then
			echo "check-image: $image ships $binary without $dir/LICENSE" >&2
			failed=1
		fi
	done < <(grep -E '(^|/)restate-server$' <<<"$listing" || true)
	if [ "$failed" -ne 0 ]; then
		status=1
		continue
	fi
	echo "check-image: $image ($platform) ok"
done
exit "$status"
