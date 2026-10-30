#!/usr/bin/env bash
# usage: scripts/release-machine.sh v0.1.0 amd64|arm64 <out-dir>
# Builds the kernel and static layer with Docker, then packs the machine bundle.
set -euo pipefail
version=$1
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$ ]] || { echo 'invalid version' >&2; exit 1; }
arch=$2
case $arch in amd64) uname_arch=x86_64;; arm64) uname_arch=aarch64;; *) exit 1;; esac
out=$(mkdir -p "$3" && cd "$3" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
  -ldflags "-s -w -X github.com/mouizahmed/hutch/internal/version.Version=$version" \
  -o "$tmp/" ./cmd/hutchd ./cmd/hutch-guest
images/kernel/build.sh "$tmp" "$uname_arch"
images/layer/build.sh "$tmp" "$uname_arch"
tar -czf "$out/hutch-machine_${version}_linux_${arch}.tar.gz" -C "$tmp" \
  hutchd hutch-guest "vmlinux-$uname_arch" "tmux-$uname_arch" terminfo.tar
