#!/usr/bin/env bash
# usage: images/kernel/build.sh <out-dir> [x86_64|aarch64]
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
out="$(mkdir -p "$1" && cd "$1" && pwd)"
arch="${2:-x86_64}"
case "$arch" in
  x86_64) karch=x86 target=vmlinux cross= ;;
  aarch64) karch=arm64 target=Image cross=aarch64-linux-gnu- ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac
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
  apt-get update -qq && apt-get install -y -qq build-essential flex bison bc libelf-dev libssl-dev xz-utils gcc-aarch64-linux-gnu >/dev/null
  cd /tmp && tar xf /src.tar.xz && cd linux-$v
  cp /k/$arch.config .config
  if [ '$arch' = aarch64 ]; then
    for opt in CGROUPS MEMCG CGROUP_PIDS CGROUP_SCHED CFS_BANDWIDTH CPUSETS CGROUP_DEVICE CGROUP_FREEZER NAMESPACES UTS_NS IPC_NS USER_NS PID_NS NET_NS SECCOMP SECCOMP_FILTER VETH BRIDGE BRIDGE_NETFILTER NETFILTER NETFILTER_ADVANCED NF_CONNTRACK NF_NAT NF_TABLES NF_TABLES_INET NF_TABLES_IPV4 NF_TABLES_IPV6 NFT_CT NFT_NAT NFT_MASQ NETFILTER_XTABLES NETFILTER_XT_MATCH_ADDRTYPE NETFILTER_XT_MATCH_CONNTRACK NETFILTER_XT_TARGET_MASQUERADE IP_NF_IPTABLES IP_NF_FILTER IP_NF_NAT IP_NF_MANGLE IP_NF_RAW IP6_NF_IPTABLES IP6_NF_FILTER IP6_NF_NAT IP_VS OVERLAY_FS CHECKPOINT_RESTORE; do
      scripts/config --enable \$opt
    done
    scripts/config --set-val ARM64_VA_BITS 48 --enable ARM64_VA_BITS_48 --disable ARM64_VA_BITS_39
  fi
  make ARCH=$karch CROSS_COMPILE=$cross olddefconfig >/dev/null
  make ARCH=$karch CROSS_COMPILE=$cross -j\$(nproc) $target >/dev/null
  if [ '$arch' = aarch64 ]; then cp arch/arm64/boot/Image /out/vmlinux-$arch; else cp vmlinux /out/vmlinux-$arch; fi
  chown $(id -u):$(id -g) /out/vmlinux-$arch
"
