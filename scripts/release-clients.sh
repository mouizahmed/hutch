#!/usr/bin/env bash
# Build the CLI archives. Run on Linux; no cross compiler or cgo is needed.
# usage: MACHINE_SHA256=amd64=<hex> scripts/release-clients.sh v0.1.0 <out-dir>
# The machine bundles' checksums are built in: setup trusts a bundle through the
# CLI the user installed, not a checksum file downloaded beside it.
set -euo pipefail
version=$1
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$ ]] || { echo 'invalid version' >&2; exit 1; }
[[ ${MACHINE_SHA256:-} =~ ^[a-z0-9]+=[0-9a-f]{64}(,[a-z0-9]+=[0-9a-f]{64})*$ ]] || { echo 'MACHINE_SHA256 must be arch=<sha256>[,…]' >&2; exit 1; }
out=$(mkdir -p "$2" && cd "$2" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
for os in linux darwin windows; do
  for arch in amd64 arm64; do
    name=hutch
    if [ "$os" = windows ]; then name=hutch.exe; fi
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
      -ldflags "-s -w -X github.com/mouizahmed/hutch/internal/version.Version=$version -X github.com/mouizahmed/hutch/internal/release.MachineSHA256=$MACHINE_SHA256" \
      -o "$tmp/$name" ./cmd/hutch
    cp LICENSE "$tmp/LICENSE"
    tar -czf "$out/hutch_${version}_${os}_${arch}.tar.gz" -C "$tmp" "$name" LICENSE
    rm "$tmp/$name"
  done
done
