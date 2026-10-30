#!/bin/sh
# Exercise the real installer against local release fixtures; no network or sudo.
set -eu
root=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/tools" "$tmp/assets" "$tmp/payload" "$tmp/bin with spaces"
export HUTCH_TEST_ASSETS="$tmp/assets"
printf '#!/bin/sh\nprintf "fixture hutch\\n"\n' > "$tmp/payload/hutch"
tar -czf "$tmp/assets/hutch_v0.1.0_linux_amd64.tar.gz" -C "$tmp/payload" hutch
printf 'v0.1.0\n' > "$tmp/assets/version.txt"
(cd "$tmp/assets" && sha256sum hutch_*.tar.gz) > "$tmp/assets/checksums.txt"
cat > "$tmp/tools/curl" <<'SH'
#!/bin/sh
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) dest=$2; shift 2 ;;
    https://*) url=$1; shift ;;
    --proto|--tlsv1.2|--retry) if [ "$1" = --tlsv1.2 ]; then shift; else shift 2; fi ;;
    *) shift ;;
  esac
done
cp "$HUTCH_TEST_ASSETS/${url##*/}" "$dest"
SH
cat > "$tmp/tools/uname" <<'SH'
#!/bin/sh
case "$1" in -s) echo "${HUTCH_TEST_OS:-Linux}";; -m) echo "${HUTCH_TEST_ARCH:-x86_64}";; esac
SH
chmod +x "$tmp/tools/"*
export PATH="$tmp/tools:$PATH" HUTCH_INSTALL_DIR="$tmp/bin with spaces"
sh "$root/install.sh"
[ "$("$HUTCH_INSTALL_DIR/hutch")" = 'fixture hutch' ]
# Failed verification must leave an existing install intact.
printf 'changed' >> "$tmp/assets/hutch_v0.1.0_linux_amd64.tar.gz"
if sh "$root/install.sh" > "$tmp/failure.log" 2>&1; then exit 1; fi
grep -q 'checksum mismatch' "$tmp/failure.log"
[ "$("$HUTCH_INSTALL_DIR/hutch")" = 'fixture hutch' ]
# Pinning and unsupported platforms fail before installing anything.
if HUTCH_VERSION='../evil' sh "$root/install.sh" > "$tmp/failure.log" 2>&1; then exit 1; fi
grep -q 'invalid version' "$tmp/failure.log"
if HUTCH_TEST_OS=Unknown sh "$root/install.sh" > "$tmp/failure.log" 2>&1; then exit 1; fi
grep -q 'supports Linux and macOS' "$tmp/failure.log"
printf 'Installer checks passed.\n'
