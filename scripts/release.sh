#!/bin/sh
# Build the release assets used by CI and GitHub Releases.
set -eu

root=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
dist=${DIST_DIR:-$root/dist}
mkdir -p "$dist"
cd "$root"

build() {
	CGO_ENABLED=0 GOOS=linux GOARCH=$1 go build -trimpath -ldflags='-s -w' -o "$dist/codator-linux-$1" .
}

build amd64
build arm64
(
	cd "$dist"
	sha256sum codator-linux-amd64 codator-linux-arm64 > SHA256SUMS
)
