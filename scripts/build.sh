#!/bin/sh
# Build the local-shunt binaries for the Claude Code and Codex plugin packages.
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
    codex_out=codex-marketplace/plugins/local-shunt/$out
    mkdir -p "${codex_out%/*}"
    cp "$out" "$codex_out"
done

cp bin/local-shunt bin/bulk-read bin/code-write codex-marketplace/plugins/local-shunt/bin/
