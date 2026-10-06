#!/usr/bin/env bash
# Builds and packages bambu-util for every released platform, with a
# checksums.txt alongside. ci runs it on every change and release runs it for
# each version, so a target that stops building fails a pull request rather
# than a release.
#
# Usage: build-dist.sh VERSION [OUTDIR]   (OUTDIR defaults to dist)
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

version=${1:?usage: build-dist.sh VERSION [OUTDIR]}
out=${2:-dist}

platforms="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

mkdir -p "$out"
for platform in $platforms; do
  goos=${platform%/*}
  goarch=${platform#*/}
  echo "building $platform"
  bin=bambu-util
  [ "$goos" = windows ] && bin=bambu-util.exe
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
    go build -trimpath -ldflags="-s -w" -o "$work/$bin" ./cmd/bambu-util
  cp README.md LICENSE "$work/"
  name="bambu-util_${version}_${goos}_${goarch}"
  if [ "$goos" = windows ]; then
    (cd "$work" && zip -q - "$bin" README.md LICENSE) > "$out/$name.zip"
  else
    tar -czf "$out/$name.tar.gz" -C "$work" "$bin" README.md LICENSE
  fi
  rm "$work/$bin"
done
(cd "$out" && sha256sum -- *.tar.gz *.zip > checksums.txt)
