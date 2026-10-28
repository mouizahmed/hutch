#!/usr/bin/env bash
# Builds what Hutch adds to images that lack it: a static tmux, and terminfo for the
# terminals people attach from.
# usage: images/layer/build.sh <out-dir> [arch]   writes tmux-<arch>, terminfo.tar
set -euo pipefail
out="$(mkdir -p "$1" && cd "$1" && pwd)"
arch="${2:-x86_64}"
platform=$(case $arch in x86_64) echo linux/amd64;; aarch64) echo linux/arm64;; esac)
builder=alpine@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f
terms="tmux tmux-256color screen screen-256color xterm xterm-256color xterm-ghostty xterm-kitty wezterm alacritty foot linux vt100 dumb"

docker run --rm --platform "$platform" -v "$out:/out" "$builder" sh -euc "
  apk add -q build-base curl bison libevent-dev libevent-static ncurses-dev ncurses-static ncurses-terminfo
  curl -fsSL -o /tmp/tmux.tgz https://github.com/tmux/tmux/releases/download/3.8/tmux-3.8.tar.gz
  echo 'e79c699c7e949dccd0a4a125e17b8d1261e311b16979dbc8eb34542f3966d82e  /tmp/tmux.tgz' | sha256sum -c -s
  tar -C /tmp -xzf /tmp/tmux.tgz && cd /tmp/tmux-3.8
  ./configure --enable-static --prefix=/usr/local >/dev/null && make -s -j\$(nproc) >/dev/null
  strip tmux && install -m755 tmux /out/tmux-$arch
  cd /usr/share/terminfo && tar -cf /out/terminfo.tar \$(for t in $terms; do f=\$(echo \$t | cut -c1)/\$t; [ -e \$f ] && echo \$f; done)
  chown $(id -u):$(id -g) /out/tmux-$arch /out/terminfo.tar
"
