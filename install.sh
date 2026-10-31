#!/bin/sh
# curl -fsSL https://raw.githubusercontent.com/mouizahmed/hutch/main/install.sh | sh
set -eu
export LC_ALL=C

fail() { printf 'hutch: %s\n' "$*" >&2; exit 1; }
for tool in curl tar mktemp; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done
if command -v sha256sum >/dev/null 2>&1; then
  hash() { sha256sum "$1" | cut -d ' ' -f 1; }
elif command -v shasum >/dev/null 2>&1; then
  hash() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
else
  fail 'sha256sum or shasum is required'
fi

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail 'install.sh supports Linux and macOS; on Windows use the release archive or WSL' ;;
esac
machine=$(uname -m)
# A shell under Rosetta reports x86_64 on an Apple Silicon Mac.
if [ "$os" = darwin ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null)" = 1 ]; then machine=arm64; fi
case $machine in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) fail 'Hutch supports x86_64 and arm64' ;;
esac

base=https://github.com/mouizahmed/hutch/releases
fetch() { curl --proto '=https' --tlsv1.2 -fsSL --retry 3 -o "$2" "$1"; }
tmp=$(mktemp -d)
stage=
trap 'rm -rf "$tmp"; if [ -n "$stage" ]; then rm -f "$stage"; fi' EXIT
trap 'exit 1' HUP INT TERM
version=${HUTCH_VERSION:-}
if [ -z "$version" ]; then
  fetch "$base/latest/download/version.txt" "$tmp/version.txt"
  version=$(cat "$tmp/version.txt")
fi
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$' || fail "invalid version: $version"
asset=hutch_${version}_${os}_${arch}.tar.gz
printf 'Downloading Hutch %s for %s/%s...\n' "$version" "$os" "$arch"
fetch "$base/download/$version/checksums.txt" "$tmp/checksums.txt"
expected=$(awk -v asset="$asset" '$2 == asset { print $1 }' "$tmp/checksums.txt")
[ ${#expected} -eq 64 ] || fail "missing or invalid checksum for $asset"
case "$expected" in *[!0-9a-f]*) fail "invalid checksum for $asset" ;; esac
fetch "$base/download/$version/$asset" "$tmp/bundle.tar.gz"
[ "$(hash "$tmp/bundle.tar.gz")" = "$expected" ] || fail 'checksum mismatch; nothing installed'
tar -xOzf "$tmp/bundle.tar.gz" hutch > "$tmp/hutch"
[ -s "$tmp/hutch" ] || fail 'release does not contain a hutch binary'

dir=${HUTCH_INSTALL_DIR:-"${HOME:?HOME is not set}/.local/bin"}
[ ! -d "$dir/hutch" ] || fail "$dir/hutch is a directory"
mkdir -p "$dir"
stage=$(mktemp "$dir/.hutch.XXXXXX")
cat "$tmp/hutch" > "$stage"
chmod 755 "$stage"
mv -f "$stage" "$dir/hutch"
stage=
printf 'Installed %s/hutch (%s).\n' "$dir" "$version"
case ":${PATH:-}:" in
  *":$dir:"*) ;;
  *) printf 'Add this directory to your PATH: %s\n' "$dir" ;;
esac
printf 'Next: hutch setup <ssh-host>\n'
