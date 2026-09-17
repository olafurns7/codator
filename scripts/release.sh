#!/bin/sh
# Build the release assets used by CI and GitHub Releases.
set -eu

root=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
dist=${DIST_DIR:-$root/dist}
mkdir -p "$dist"
cd "$root"

build() {
	CGO_ENABLED=0 GOOS=$1 GOARCH=$2 go build -trimpath -ldflags='-s -w' -o "$dist/codator-$1-$2" .
}

for platform in linux darwin; do
	build "$platform" amd64
	build "$platform" arm64
done
cp LICENSE "$dist/LICENSE"
cp scripts/cdx scripts/cdl "$dist/"
(
	cd "$dist"
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum codator-linux-amd64 codator-linux-arm64 codator-darwin-amd64 codator-darwin-arm64 cdx cdl LICENSE > SHA256SUMS
	else
		shasum -a 256 codator-linux-amd64 codator-linux-arm64 codator-darwin-amd64 codator-darwin-arm64 cdx cdl LICENSE > SHA256SUMS
	fi
)
