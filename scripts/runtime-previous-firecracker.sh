#!/usr/bin/env bash
# Only the snapshot matrix needs the older runtime; production setup is unchanged.
set -euo pipefail
[[ ${GITHUB_ACTIONS:-} == true && ${RUNNER_ENVIRONMENT:-} == github-hosted ]] || exit 1
runtime_dir=/var/lib/hutch/firecracker/v1.16.0
runtime_tmp=$(mktemp -d)
trap 'rm -r -- "$runtime_tmp"' EXIT
curl --proto '=https' --tlsv1.2 -fsSL --retry 3 \
  https://github.com/firecracker-microvm/firecracker/releases/download/v1.16.0/firecracker-v1.16.0-x86_64.tgz \
  -o "$runtime_tmp/firecracker.tgz"
echo "bd04e26952d4e158085778c6230a0b383d2619c319182e27eaa9d61a212e92d6  $runtime_tmp/firecracker.tgz" | sha256sum -c --quiet
tar -xzf "$runtime_tmp/firecracker.tgz" -C "$runtime_tmp"
sudo mkdir -p "$runtime_dir"
for binary in firecracker jailer; do
  sudo install -m 755 "$runtime_tmp/release-v1.16.0-x86_64/$binary-v1.16.0-x86_64" "$runtime_dir/$binary"
done
