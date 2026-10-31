#!/usr/bin/env bash
# Run only as a brand-new OS account on the disposable release rehearsal VM.
# The public installer is unchanged. A strict curl shim substitutes downloaded,
# signed rehearsal artifacts for release URLs that do not exist before launch.
set -euo pipefail
[[ ${GITHUB_ACTIONS:-} == true && ${RUNNER_ENVIRONMENT:-} == github-hosted ]] || exit 1
[[ $(id -un) == hutch-first && ! -d "$HOME/.config/hutch" ]] || exit 1
first_box_started=$SECONDS
first_box_source=${HUTCH_TEST_SOURCE:?}
first_box_assets=${HUTCH_TEST_ASSETS:?}
first_box_version=${HUTCH_VERSION:?}
printf 'Fresh OS account: %s; no agent or cloud credentials configured.\n' "$(id -un)"
printf 'Rehearsal: unpublished release endpoints use the downloaded workflow artifacts.\n'
mkdir -p tools dist/runtime
first_box_curl=$(command -v curl)
export HUTCH_TEST_REAL_CURL="$first_box_curl"
cat > tools/curl <<'SH'
#!/usr/bin/env bash
set -euo pipefail
original=("$@")
destination= url=
while (($#)); do
  case $1 in
    -o) destination=$2; shift 2 ;;
    https://*) url=$1; shift ;;
    *) shift ;;
  esac
done
base=https://github.com/mouizahmed/hutch/releases
case $url in
  "$base/download/$HUTCH_VERSION/checksums.txt"|"$base/download/$HUTCH_VERSION/hutch_${HUTCH_VERSION}_linux_amd64.tar.gz")
    [[ -n $destination ]] || exit 1
    cp "$HUTCH_TEST_ASSETS/${url##*/}" "$destination"
    ;;
  "$base"/*) echo 'unexpected rehearsal release request' >&2; exit 1 ;;
  *) exec "$HUTCH_TEST_REAL_CURL" "${original[@]}" ;;
esac
SH
chmod +x tools/curl
export PATH="$PWD/tools:$HOME/.local/bin:$PATH"
printf '\n$ curl -fsSL <GitHub installer at candidate commit> | sh\n'
curl -fsSL "https://raw.githubusercontent.com/mouizahmed/hutch/${HUTCH_TEST_COMMIT:?}/install.sh" -o installer.sh
cmp installer.sh "$first_box_assets/install.sh"
sh installer.sh
hutch --version
tar -xzf "$first_box_assets/hutch-machine_${first_box_version}_linux_amd64.tar.gz" -C dist/runtime
ln -s "$HOME/.local/bin/hutch" dist/runtime/hutch
printf '\n$ hutch setup hutch-ci (explicit no-backup, blank-machine storage plan)\n'
# This prepares SSH and invokes the installed CLI with --yes --no-bucket
# --disk none --image-size 32 and the exact rehearsal machine bundle.
bash "$first_box_source/scripts/runtime-machine.sh"
printf '\n$ hutch doctor\n'
hutch doctor
printf '\n$ hutch new --image base --cpus 1 --mem 1024 --cmd bash --name first-box\n'
hutch new --image base --cpus 1 --mem 1024 --cmd bash --name first-box
printf '\n$ hutch exec first-box -- sh -c "write and read a file"\n'
hutch exec first-box -- sh -c 'printf hello-from-a-fresh-install > /workspace/first-box; cat /workspace/first-box; printf "\n"'
first_box_elapsed=$((SECONDS-first_box_started))
printf '\nFirst working box: %s seconds.\n' "$first_box_elapsed"
[[ $first_box_elapsed -lt 900 ]]
printf '\n$ hutch ls\n'
hutch ls
printf '\n$ hutch sleep first-box\n'
hutch sleep first-box
printf '\n$ hutch wake first-box\n'
hutch wake first-box
[[ $(hutch exec first-box -- cat /workspace/first-box) == hello-from-a-fresh-install ]]
printf '\n$ hutch rm first-box\n'
hutch rm first-box
printf '{"first_box_seconds":%s,"fresh_os_account":true,"rehearsal_transport":true,"completed":true}\n' \
  "$first_box_elapsed" > first-box-result.json
