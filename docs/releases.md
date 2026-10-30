# Release preparation

Completing Phase 9 means the installation, checks, and documentation are ready for validation. It does not trigger a v0.1 tag, publication, or announcement. Release timing is a separate maintainer decision.

## Artifacts

The release workflow builds static CLI binaries for Linux, macOS, and Windows, each for amd64 and arm64. Linux machine bundles include `hutchd`, `hutch-guest`, the guest kernel, static tmux, and terminfo. Setup downloads the machine bundle matching the CLI version and machine architecture; the default userspace image is still built on the machine during setup.

Run a build rehearsal with the release workflow's manual dispatch and an explicit version. It uploads workflow artifacts without creating a release. Local build commands are:

```sh
scripts/release-clients.sh v0.1.0-rc.1 dist/release
scripts/release-machine.sh v0.1.0-rc.1 amd64 dist/release
```

Machine builds require Docker; build the arm64 layer on an arm64 machine or with an explicitly configured emulator. The arm64 guest kernel can be cross-compiled from x86_64.

When a maintainer later pushes a version tag, the workflow checks the code, builds all artifacts, produces `checksums.txt`, signs build provenance with GitHub artifact attestations, and creates a **draft** release. Review and validate that draft before publishing. The installer uses the latest published release; drafts do not become its install target.

To check provenance using GitHub CLI after downloading an archive:

```sh
gh attestation verify hutch_v0.1.0_linux_amd64.tar.gz --repo mouizahmed/hutch \
  --signer-workflow mouizahmed/hutch/.github/workflows/release.yml
```

This is separate from the automatic SHA-256 checks in `install.sh` and setup. GitHub documents [artifact attestation verification](https://cli.github.com/manual/gh_attestation_verify).

## Before publishing

Record the candidate commit, artifact hashes, tested platforms and agent versions, test results, and known limitations. Validate a clean install from downloaded artifacts, private repository and template setup, native agent/MCP authentication, box sleep/wake and recovery, work preservation, credential isolation, and live daemon upgrades. Check accurate, deduplicated notifications over an extended idle period; legitimate events should notify at any hour.

Both Firecracker and gVisor need actual runtime and isolation tests. macOS and Windows builds need real client checks. A compiled arm64 kernel is not evidence that it boots correctly on arm64 KVM. Release rehearsal, signing, and public install URLs must also be exercised in GitHub before considering the pipeline proven.

No release or deployment is performed by the local build scripts.
