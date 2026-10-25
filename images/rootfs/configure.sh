#!/usr/bin/env bash
# Runs in the build container against the debootstrapped tree at $1.
set -euo pipefail
r="$1"
ch() { chroot "$r" "$@"; }

fetch() { # url sha256 dest
  curl -fsSL -o "$3" "$1"
  echo "$2  $3" | sha256sum -c --quiet
}

cat > "$r/etc/apt/sources.list" <<'EOF'
deb http://archive.ubuntu.com/ubuntu noble main universe
deb http://archive.ubuntu.com/ubuntu noble-updates main universe
deb http://security.ubuntu.com/ubuntu noble-security main universe
EOF
ch apt-get update -qq
DEBIAN_FRONTEND=noninteractive ch apt-get upgrade -y -qq >/dev/null

echo hutch > "$r/etc/hostname"
printf '127.0.0.1 localhost\n127.0.1.1 hutch\n' > "$r/etc/hosts"
printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > "$r/etc/resolv.conf"
printf 'LABEL=rootfs / ext4 defaults,noatime 0 1\n' > "$r/etc/fstab"
: > "$r/etc/machine-id"

ch useradd -m -u 1000 -s /bin/bash dev
echo 'dev ALL=(ALL) NOPASSWD:ALL' > "$r/etc/sudoers.d/dev"
chmod 440 "$r/etc/sudoers.d/dev"
install -d -o 1000 -g 1000 "$r/workspace"

fetch https://nodejs.org/dist/v24.22.1/node-v24.22.1-linux-x64.tar.xz \
  6c172a9ab6718f7b801fb5a7bd486af2ee5e32758086a04b84fcce2aa77095e6 /tmp/node.tar.xz
tar -xJf /tmp/node.tar.xz -C "$r/usr/local" --strip-components=1 --exclude='*/CHANGELOG.md' --exclude='*/README.md' --exclude='*/LICENSE'

fetch https://github.com/cli/cli/releases/download/v2.102.0/gh_2.102.0_linux_amd64.tar.gz \
  bb766f710eef8ede859c18578c72c327597cd4c8a85b06001b1f3843c6019386 /tmp/gh.tar.gz
tar -xzf /tmp/gh.tar.gz -C /tmp
install -m755 /tmp/gh_2.102.0_linux_amd64/bin/gh "$r/usr/local/bin/gh"

fetch https://github.com/astral-sh/uv/releases/download/0.13.0/uv-x86_64-unknown-linux-musl.tar.gz \
  b0828fc4cdc47d65951e94b5f0a15a5364420e8dcd88723f3b21e7a2eb30bbc3 /tmp/uv.tar.gz
tar -xzf /tmp/uv.tar.gz -C /tmp
install -m755 /tmp/uv-x86_64-unknown-linux-musl/uv /tmp/uv-x86_64-unknown-linux-musl/uvx "$r/usr/local/bin/"

# Threads are tmux windows numbered from 1; a window stays after its agent exits.
install -m644 /build/tmux.conf "$r/etc/tmux.conf"

install -m755 /hutch-guest "$r/usr/local/bin/hutch-guest"
install -m644 /build/hutch-guest.service "$r/etc/systemd/system/hutch-guest.service"
ch systemctl enable hutch-guest.service >/dev/null 2>&1

# Host keys are created on first boot; hutch-guest replaces them in every clone.
# RSA is left out: generating it takes about a second, on every fork.
rm -f "$r"/etc/ssh/ssh_host_*
printf 'HostKey /etc/ssh/ssh_host_ed25519_key\nHostKey /etc/ssh/ssh_host_ecdsa_key\n' > "$r/etc/ssh/sshd_config.d/hostkeys.conf"
mkdir -p "$r/etc/systemd/system/ssh.service.d"
cat > "$r/etc/systemd/system/ssh.service.d/hostkeys.conf" <<'EOF'
[Service]
ExecStartPre=
ExecStartPre=/bin/sh -c 'for t in ed25519 ecdsa; do [ -f /etc/ssh/ssh_host_$${t}_key ] || ssh-keygen -q -t $$t -N "" -f /etc/ssh/ssh_host_$${t}_key; done'
ExecStartPre=/usr/sbin/sshd -t
EOF

# Root on the serial console, reachable only through `hutch console --interactive`.
mkdir -p "$r/etc/systemd/system/serial-getty@ttyS0.service.d"
cat > "$r/etc/systemd/system/serial-getty@ttyS0.service.d/autologin.conf" <<'EOF'
[Service]
ExecStart=
ExecStart=-/sbin/agetty --autologin root --noclear --keep-baud 115200,38400,9600 %I $TERM
EOF

# Calendar timers catch up right after every wake (the clock jumps forward), so they
# would run in every restored box: no apt runs, scrubs, or news fetches behind agents.
ch systemctl mask apt-daily.timer apt-daily-upgrade.timer motd-news.timer e2scrub_all.timer dpkg-db-backup.timer fstrim.timer >/dev/null 2>&1

# The network comes from the kernel ip= argument.
ch systemctl mask systemd-networkd-wait-online.service systemd-resolved.service >/dev/null 2>&1

ch apt-get clean
rm -rf "$r"/var/lib/apt/lists/* "$r"/var/log/*.log "$r"/tmp/*
