#!/usr/bin/env bash
# Provision only a fresh GitHub-hosted VM, through the real SSH setup path.
set -euo pipefail
[[ ${GITHUB_ACTIONS:-} == true && ${RUNNER_ENVIRONMENT:-} == github-hosted ]] || {
  echo 'runtime-machine.sh requires a disposable GitHub-hosted runner' >&2; exit 1;
}
[[ ! -e /var/lib/hutch/state/hutch.db ]] || { echo 'refusing existing Hutch state' >&2; exit 1; }
[[ $(findmnt -n -o FSTYPE /) != btrfs ]] || { echo 'refusing loop-mounted XFS on btrfs' >&2; exit 1; }
case ${HUTCH_TEST_BACKEND:?} in
  firecracker)
    [[ -c /dev/kvm ]] || { echo 'Firecracker runtime check requires /dev/kvm on this runner' >&2; exit 1; }
    ;;
  gvisor)
    # This VM is dedicated to this job and has no running boxes yet.
    # The backend follows /dev/kvm, so exercise the real no-KVM setup path.
    sudo rm -f /dev/kvm
    ;;
  *) exit 1 ;;
esac

# The runner's unused Android SDK occupies the space setup needs for XFS.
sudo rm -rf /usr/local/lib/android
sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq openssh-server iproute2 iptables xfsprogs zstd e2fsprogs >/dev/null
sudo systemctl start ssh
ci_ssh_dir="$RUNNER_TEMP/hutch-ssh"
mkdir -p "$ci_ssh_dir" "$HOME/.ssh"
chmod 700 "$ci_ssh_dir" "$HOME/.ssh"
ssh-keygen -q -t ed25519 -N '' -f "$ci_ssh_dir/id_ed25519"
cat "$ci_ssh_dir/id_ed25519.pub" >> "$HOME/.ssh/authorized_keys"
chmod 600 "$HOME/.ssh/authorized_keys"
cat >> "$HOME/.ssh/config" <<SSH

Host hutch-ci
  HostName 127.0.0.1
  User $(id -un)
  IdentityFile $ci_ssh_dir/id_ed25519
  IdentitiesOnly yes
  StrictHostKeyChecking accept-new
  BatchMode yes
SSH
chmod 600 "$HOME/.ssh/config"
chmod +x dist/runtime/hutch dist/runtime/hutchd dist/runtime/hutch-guest dist/runtime/tmux-x86_64
ci_daemon="$PWD/dist/runtime/hutchd"
if [[ ${HUTCH_TEST_PREVIOUS:-} == 1 ]]; then
  chmod +x dist/runtime/hutchd-previous
  ci_daemon="$PWD/dist/runtime/hutchd-previous"
fi
dist/runtime/hutch setup hutch-ci --yes --no-bucket --disk none --image-size 32 \
  --hutchd "$ci_daemon" --images "$PWD/dist/runtime"
