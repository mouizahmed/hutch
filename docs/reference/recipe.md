# Recipe reference

A project's recipe is its `.hutch/` directory. This page gives each file's exact behavior; for writing one, see [projects and templates](../templates.md).

## Files

| File | Required | Runs as | Where | When |
| --- | --- | --- | --- | --- |
| `Dockerfile` | no | the image build | on the machine, context `.hutch/` | Every build |
| `services` | no | `dev` | box init | From the build on, in every box |
| `on-create.sh` | no | `dev` | `/workspace` | Every build, and every new box |
| `verify.sh` | no, but recommended | `dev` | `/workspace` | Every build |
| `uploads.txt` | no | read on your computer | | Every build and snapshot |
| `hutch.toml` | no | read on your computer | | Every command in the project |
| `fingerprint.sh` | no | `dev` | `/workspace` | `hutch doctor --template` only |
| `SETUP_PROMPT.md` | no | | | Written by `hutch init`, for your agent; never run |

`.hutch/` is read from the commit being built, not from your working tree, except `hutch.toml` and `uploads.txt`. It's sent to the machine as the image's build context, and is limited to 8 MB.

## Build order

`hutch template build` runs, in a fresh VM, stopping at the first step that fails:

1. **Image.** The published image named by `image` in `hutch.toml`, if set; else `.hutch/Dockerfile`, built with `.hutch/` as its context; else Hutch's default image. The machine caches layers, up to `machine.build_cache`.
2. **Boot** the template box from the image, with a `dev` user (uid 1000) and passwordless sudo added. An image's own uid-1000 user is taken over.
3. **Checkout** of the commit, at `/workspace`, owned by `dev`.
4. **Uploads.** The files in `uploads.txt`, into the checkout, mode `0600`, excluded from Git.
5. **Agents.** Each agent adapter's `install` command, as `dev`, skipped when the image already has the agent's command; then your `~/.config/hutch/user-setup.sh`, if you have one.
6. **Services.** `services` is installed and its commands start.
7. **`on-create.sh`**, with `bash -eo pipefail`.
8. **`verify.sh`**, with `bash -eo pipefail`. It must exit 0.
9. Processes `dev` left running outside the services are stopped.

The build reports its peak memory and CPU use. It must finish within `template.build_timeout` (1 hour). A failed build leaves the template box running for `hutch template shell`.

`hutch template snapshot` builds first unless the template box already holds a verified build of the same commit and uploads, waits up to `template.quiet_timeout` (2 minutes) for the services to be up, and saves the box as the next template version.

## Dockerfile

Built on the machine, with BuildKit. Start from `ghcr.io/mouizahmed/hutch-default`, or any image. The build context is `.hutch/` only: `COPY` can reach files there but not the rest of the repository, so install the project's own dependencies in `on-create.sh`.

The default image is Ubuntu 24.04 with Git, curl, tmux, the GitHub CLI, Node 24 with npm, uv, build-essential, and an SSH server. There's no systemd in a box: Hutch's own init runs it.

Files a service writes should be owned by `1000:1000`.

## services

One shell command per line, run by `/bin/sh -c` as `dev`. Blank lines and lines starting with `#` are skipped. Each service is restarted if it exits, and logs to `/var/log/hutch/service-<n>.log`.

```text
postgres -D /var/lib/postgresql/data
redis-server --save ""
```

Services are running in the template snapshot, so every box starts with them up. After a box's cold boot, they start again.

## on-create.sh

Runs after checkout: in the build, where a failure fails the build, and again in every new box, on the box's own branch, which may be newer than the template. In a new box, a failure is reported and the box is created anyway.

Keep it incremental, so the template does the slow first run and boxes are quick: `npm install --prefer-offline`, pending migrations.

## verify.sh

Runs after `on-create.sh` in the build only, and must exit 0, or no template is made. Prove the project works: build it, run a fast subset of the tests, or start the server and request a health endpoint. Stop whatever it starts before it exits; anything it leaves running is stopped.

Without one, a build says nothing checked the project builds or runs, and `hutch doctor --template` warns.

## uploads.txt

One path per line, relative to the repository root. Blank lines and lines starting with `#` are skipped. Paths must be inside the repository.

```text
.env.local
fixtures/seed.sql
```

At each build and snapshot, the files are read from your checkout and uploaded when they've changed, encrypted on the machine. If a listed file is missing here but the machine has a copy, the machine's copy is used, so another computer can build. Without `uploads.txt`, stored uploads are removed.

The files are real in every box made from the template, and in its snapshots.

## hutch.toml

Project settings, starting with `version = 1`. It's read on your computer, so it can stay uncommitted. A project sets `size`, `disk.size`, `idle_timeout`, `idle.cpu_threshold`, `base`, `default_agent`, `image`, and `template` settings, and can request `autonomy`, `credentials`, `network.allow`, and `network.egress`, which you [grant](../configuration.md#grants). See the [settings reference](settings.md) for which settings a project can hold.

```toml
version = 1
size = { cpu = 4, memory = "8G" }
disk.size = "50G"
idle_timeout = "10m"
# network.allow = ["10.0.5.20:5432"]
```

## fingerprint.sh

Used only by `hutch doctor --template`, which builds the recipe twice from scratch and compares: installed packages, the versions of `go`, `node`, `npm`, `python3`, `rustc`, and `cargo`, and the project's files that Git doesn't ignore. `fingerprint.sh`'s output is compared too, so print anything else that should be the same in every build, like a hash of generated files Git ignores. It runs with `bash -eo pipefail`, and its output is limited to 2 MB.

## setup.sh

Recipes from before Hutch v0.2 used `setup.sh`, run as root in the VM. Builds refuse it now. `hutch init` converts it to a Dockerfile that runs it during the image build; move the steps that need the repository into `on-create.sh`, and services it enabled with systemd into `services`.
