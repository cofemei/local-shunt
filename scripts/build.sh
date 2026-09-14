#!/bin/sh
# Build the local-shunt binaries that bin/local-shunt runs, for every supported platform.
# Usage: scripts/build.sh [os/arch ...]   (default: all platforms)
set -eu

cd "$(dirname "$0")/.."
platforms=${*:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64}

for platform in $platforms; do
    os=${platform%/*}
    arch=${platform#*/}
    out=dist/$os-$arch/local-shunt
    [ "$os" = windows ] && out=$out.exe
    echo "building $out"
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=false -ldflags "-s -w" -o "$out" ./cmd/local-shunt
done
