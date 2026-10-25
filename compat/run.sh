#!/usr/bin/env bash
# Runs the compat tests as root inside a throwaway container (CI, or make compat).
set -euo pipefail
apt-get update -qq >/dev/null
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq tmux openssh-client >/dev/null
id dev >/dev/null 2>&1 || useradd -m -u 1000 -s /bin/bash dev
git config --global --add safe.directory "$(pwd)"
go test -tags compat -count=1 -timeout 5m -v ./internal/compat
