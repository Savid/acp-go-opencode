#!/bin/sh
set -eu

# Official release source: https://github.com/anomalyco/opencode/releases/tag/v1.18.13
version=1.18.13
case "$(uname -m)" in
  x86_64)
    archive=opencode-linux-x64.tar.gz
    archive_sha=8d500b20fed2d26e537e221895b1a575476571b4f0089bb29fb13eeb8eb9e937
    binary_sha=bcebc898d22419417b8dbec0f5e3f00f5b67b5a38c1de37c77c699d0e31fe0dd
    ;;
  aarch64|arm64)
    archive=opencode-linux-arm64.tar.gz
    archive_sha=dd4ac8c2167a8338caf296b002c955141d52a2e9c95ee0a95f4ae9939c293ab0
    binary_sha=e09c47471c6987a89dddacc7bcbe44d0f3b3f2a30b7e1d20e47ae09517d29990
    ;;
  *)
    echo "unsupported browser-canary architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
output_dir="$repo_root/.tmp/browser-canary"
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM

url="https://github.com/anomalyco/opencode/releases/download/v${version}/${archive}"
curl --fail --location --proto '=https' --retry 3 --silent --show-error --output "$work_dir/$archive" "$url"
printf '%s  %s\n' "$archive_sha" "$work_dir/$archive" | sha256sum -c -
tar -xzf "$work_dir/$archive" -C "$work_dir" opencode
printf '%s  %s\n' "$binary_sha" "$work_dir/opencode" | sha256sum -c -

mkdir -p "$output_dir"
install -m 0755 "$work_dir/opencode" "$output_dir/native"
