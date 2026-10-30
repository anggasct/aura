#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: $0 <version> <install-dir>" >&2
	exit 2
fi

version="$1"
install_dir="$2"

case "$(uname -s)/$(uname -m)" in
Linux/x86_64) target="x86_64-unknown-linux-musl" ;;
Linux/aarch64 | Linux/arm64) target="aarch64-unknown-linux-musl" ;;
Darwin/x86_64) target="x86_64-apple-darwin" ;;
Darwin/arm64) target="aarch64-apple-darwin" ;;
*)
	echo "fetch-restate-server: unsupported platform $(uname -s)/$(uname -m)" >&2
	exit 1
	;;
esac

case "$version/$target" in
1.7.14/x86_64-unknown-linux-musl) want="5d6199c0506b979eef19efee004499b628c54ed593b01c12b9d89c2cbbdb0637" ;;
1.7.14/aarch64-unknown-linux-musl) want="5d90726189089955719a0bd445837f89c54b7ee44b1836d24059759a8a8b27d9" ;;
1.7.14/x86_64-apple-darwin) want="ff8c12f87c3cab533e0e82d4f041bf0646260f7e1317a5476cf846ac6eb95108" ;;
1.7.14/aarch64-apple-darwin) want="6a3de949bcadfa6ac239c8192929ad65aaa70c577c2422ffc63acf3c55d5c032" ;;
*)
	echo "fetch-restate-server: no pinned checksum for restate-server $version on $target" >&2
	exit 1
	;;
esac

archive="restate-server-$target.tar.xz"
url="https://github.com/restatedev/restate/releases/download/v$version/$archive"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

curl --fail --silent --show-error --location --retry 3 --output "$work/$archive" "$url"

if command -v sha256sum >/dev/null 2>&1; then
	got="$(sha256sum "$work/$archive" | cut -d' ' -f1)"
else
	got="$(shasum -a 256 "$work/$archive" | cut -d' ' -f1)"
fi
if [ "$got" != "$want" ]; then
	echo "fetch-restate-server: checksum mismatch for $archive: got $got, want $want" >&2
	exit 1
fi

tar -xJf "$work/$archive" -C "$work"
mkdir -p "$install_dir"
install -m 0644 "$work/restate-server-$target/LICENSE" "$install_dir/LICENSE"
install -m 0755 "$work/restate-server-$target/restate-server" "$install_dir/restate-server.tmp"
mv "$install_dir/restate-server.tmp" "$install_dir/restate-server"
echo "restate-server $version installed at $install_dir/restate-server"
