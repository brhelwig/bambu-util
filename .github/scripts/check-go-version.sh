#!/usr/bin/env bash
# Fails when the Dockerfile's golang image and go.mod's toolchain name
# different Go minor versions, so the image and the release binaries are built
# with the same compiler. Dependabot bumps each separately.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

mod=$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2 }' go.mod)
[ -n "$mod" ] || mod=$(awk '$1 == "go" { print $2 }' go.mod)
image=$(sed -nE 's/^FROM .*golang:([0-9]+\.[0-9]+(\.[0-9]+)?).*/\1/p' Dockerfile | head -1)

if [ -z "$image" ]; then
  echo "no golang image found in Dockerfile" >&2
  exit 1
fi

minor() { echo "$1" | cut -d. -f1,2; }
if [ "$(minor "$mod")" != "$(minor "$image")" ]; then
  echo "go.mod builds with Go $mod but the Dockerfile uses golang:$image; update both together" >&2
  exit 1
fi
echo "Go $(minor "$mod") in both go.mod and the Dockerfile"
