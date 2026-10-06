#!/usr/bin/env bash
# Builds and packages bambu-util for released platforms. ci runs it on every
# change and release runs it for each version, so a target that stops building
# fails a pull request rather than a release.
#
# Usage: build-dist.sh VERSION OUTDIR PLATFORM...
#        build-dist.sh checksums OUTDIR
#
# PLATFORM is os/arch as the release assets name it: linux/amd64, linux/arm64,
# windows/amd64 and windows/arm64 build on Linux (cross-compiled with
# cargo-zigbuild); darwin/amd64 and darwin/arm64 need macOS, for Apple's SDK.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

version=${1:?usage: build-dist.sh VERSION OUTDIR PLATFORM...}
out=${2:?usage: build-dist.sh VERSION OUTDIR PLATFORM...}
shift 2
mkdir -p "$out"

if [ "$version" = checksums ]; then
  (cd "$out" && sha256sum -- *.tar.gz *.zip > checksums.txt)
  exit 0
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for platform in "$@"; do
  os=${platform%/*}
  arch=${platform#*/}
  case "$platform" in
    linux/amd64)   target=x86_64-unknown-linux-musl ;;
    linux/arm64)   target=aarch64-unknown-linux-musl ;;
    windows/amd64) target=x86_64-pc-windows-gnu ;;
    windows/arm64) target=aarch64-pc-windows-gnullvm ;;
    darwin/amd64)  target=x86_64-apple-darwin ;;
    darwin/arm64)  target=aarch64-apple-darwin ;;
    *) echo "unknown platform $platform" >&2; exit 1 ;;
  esac
  echo "building $platform ($target)"
  rustup target add "$target"
  if [ "$os" = darwin ]; then
    cargo build --release --locked --target "$target"
  else
    cargo zigbuild --release --locked --target "$target"
  fi

  bin=bambu-util
  [ "$os" = windows ] && bin=bambu-util.exe
  cp "target/$target/release/$bin" README.md LICENSE "$work/"
  name="bambu-util_${version}_${os}_${arch}"
  if [ "$os" = windows ]; then
    (cd "$work" && zip -q - "$bin" README.md LICENSE) > "$out/$name.zip"
  else
    tar -czf "$out/$name.tar.gz" -C "$work" "$bin" README.md LICENSE
  fi
  rm "$work/$bin"
done
