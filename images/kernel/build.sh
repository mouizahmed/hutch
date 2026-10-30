#!/usr/bin/env bash
# usage: images/kernel/build.sh <out-dir>
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
out="$(mkdir -p "$1" && cd "$1" && pwd)"
v=6.1.155
sha=c29387aeee085fbcbd91236224b9df805063bac43615e75cea2c6b29604a5c73
builder=ubuntu@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac

cache="${XDG_CACHE_HOME:-$HOME/.cache}/hutch"
mkdir -p "$cache"
src="$cache/linux-$v.tar.xz"
[ -f "$src" ] || curl -fsSL -o "$src" "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$v.tar.xz"
echo "$sha  $src" | sha256sum -c --quiet

docker run --rm -v "$src:/src.tar.xz:ro" -v "$here:/k:ro" -v "$out:/out" "$builder" bash -euo pipefail -c "
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq && apt-get install -y -qq build-essential flex bison bc libelf-dev libssl-dev xz-utils >/dev/null
  cd /tmp && tar xf /src.tar.xz && cd linux-$v
  cp /k/x86_64.config .config
  make olddefconfig >/dev/null
  make -j\$(nproc) vmlinux >/dev/null
  cp vmlinux /out/vmlinux-x86_64
  chown $(id -u):$(id -g) /out/vmlinux-x86_64
"
