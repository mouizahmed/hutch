# Contributing

Hutch is pre-release. Describe the concrete problem or workflow when opening an issue. For larger changes, discuss the intended behavior first. Report vulnerabilities privately through [SECURITY.md](SECURITY.md).

## Build and check

Install the Go version in `go.mod`, Git, and a C-free Go build environment. The CLI itself does not need Docker or KVM:

```sh
make build
bin/hutch --help
make check
sh scripts/test-install.sh
```

Machine binaries and images:

```sh
make linux images
```

Image and kernel builds need Docker. Runtime tests need a disposable Linux machine with XFS/reflink and the chosen backend. Do not run destructive storage or isolation experiments against a machine holding your work. `make compat` runs the guest protocol compatibility suite in a privileged container.

Keep changes focused and explain their effect on CLI/TUI behavior, box state, failure recovery, and compatibility where relevant. Include the checks actually run and any untested runtime assumptions. Prefer shared operations for CLI and TUI workflows.

Commit subjects use short, concrete, lower-case descriptions, for example `forwarded ports wake sleeping boxes, one wake per box`. Do not add automated co-author attribution.

`private/` is a separate private documentation repository and must never be committed into this public repository. Public usage and security documentation belongs under `docs/` or the root public Markdown files.
