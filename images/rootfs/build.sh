#!/usr/bin/env bash
# usage: images/rootfs/build.sh <out-dir> <hutch-guest-binary>
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
out="$(mkdir -p "$1" && cd "$1" && pwd)"
guest="$(cd "$(dirname "$2")" && pwd)/$(basename "$2")"
builder=ubuntu@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac

docker run --rm --privileged -v "$here:/build:ro" -v "$guest:/hutch-guest:ro" -v "$out:/out" "$builder" bash -euo pipefail -c "
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq && apt-get install -y -qq debootstrap e2fsprogs curl ca-certificates xz-utils >/dev/null
  debootstrap --variant=minbase --include=$(paste -sd, "$here/packages.txt") noble /rootfs http://archive.ubuntu.com/ubuntu >/dev/null
  /build/configure.sh /rootfs
  rm -f /out/rootfs-x86_64.ext4
  mkfs.ext4 -q -L rootfs -d /rootfs /out/rootfs-x86_64.ext4 4G
  chown $(id -u):$(id -g) /out/rootfs-x86_64.ext4
"
