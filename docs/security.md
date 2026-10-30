# Security model

Hutch is pre-release and has not completed independent security review. Use a machine and development accounts whose access you intend to give the workloads you run. Report vulnerabilities through the [security policy](../SECURITY.md).

## Boundaries

The Linux machine and `hutchd` are trusted. Access to its SSH account, the API socket's group, root, or its master key permits controlling boxes and their credentials. The SSH API is an administrative interface, not a multi-user authorization boundary.

Firecracker boxes use KVM and a jailer. Machines without KVM use gVisor, which has a different isolation boundary and does not support every VM workload. Root inside a box controls that box; it should not provide access to the machine or another box. Terminals within one box intentionally share its files and processes.

Outbound public internet access is enabled. Private networks are blocked apart from explicitly granted `network.allow` targets. The keys proxy refuses machine addresses, loopback, metadata/link-local addresses, and the box slot networks even when named as allowed targets. Domain-restricted egress is not implemented; selecting `network.egress = "allowlist"` fails instead of silently using open egress. Open egress allows exfiltration of data the box can read.

The machine exposes SSH. Forwarding a box service uses a listener on your own computer and that SSH connection. No public token-protected port service is included in this phase.

## Credentials

For supported current templates, agent and GitHub logins are replaced with placeholders. A host-side HTTPS proxy exchanges them for real credentials only toward adapter-declared hosts and according to that box's login grants. A box can still use the granted account through that proxy while it runs. This limits credential extraction; it does not limit what the provider lets the account do.

The machine's proxy CA is trusted inside boxes. Traffic for credential hosts and opted-in identity hosts is decrypted on the machine; other HTTPS traffic is tunnelled. A client that ignores the proxy reaches an upstream service with placeholders that do not authenticate. Certificate-pinned clients may reject interception.

Important exceptions:

- Native login flows initially expose real credentials in their login box until capture replaces them. The login box is part of the trusted authentication flow.
- Old guest protocols and templates may retain real credentials. Rebuild templates and replace affected boxes; upgrading the daemon alone does not scrub old snapshots.
- MCP key substitution depends on declared secret fields, recognized token values, known destination hosts, and HTTPS header use. Unsupported credentials remain real in boxes.
- Project secret uploads, such as development `.env` files, remain real in boxes and their snapshots.

Forks cannot reduce the credential grants of their source snapshot: raw credentials may still be inherited in its files or process memory. Build and publish a template with the intended grants first. This is deliberately conservative even for a snapshot believed to hold only placeholders; the current snapshot metadata does not establish that all real credentials are absent.

Credential grants and a host restriction are not authorization for arbitrary downstream operations. Review the permissions of each account. The optional signed box identity also names the caller; the receiving service must authorize it and validate all token claims.

## Storage and updates

Local disks, memory snapshots, and the trusted machine can contain source code and secrets. Bucket chunks and manifests are encrypted under keys derived from the machine's master key. Save the recovery passphrase independently; losing the machine without a bucket loses its boxes.

The installer and setup verify downloaded release checksums over HTTPS. Release workflows generate signed build attestations, which can be verified separately; the shell installer does not perform Sigstore verification. See [release preparation](releases.md).

CI runs Go vulnerability checks and reviews changed dependencies. These do not establish isolation correctness, replace runtime testing, or amount to a complete security audit. Current release gates include both backends, credential boundaries, recovery, and prolonged idle notification checks.
