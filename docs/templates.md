# Projects and templates

Every box in a project starts from the project's template: a snapshot of a VM where your environment is built, dependencies are installed, and services are running. This page covers setting up a project, the recipe in `.hutch/`, and building, updating, and checking templates. For each recipe file's exact behavior, see the [recipe reference](reference/recipe.md).

## Set up a project

A project is a Git repository with an `origin` remote that the machine can clone. For a private GitHub repository, `hutch login gh` gives boxes access. In your checkout:

```sh
hutch init
```

Init registers the project, starts a template box named `<project>-template` from Hutch's default image, and writes `.hutch/SETUP_PROMPT.md`. Give that prompt to Claude Code or Codex in your checkout: it explains the recipe and has the agent try each command in the template box before writing it down.

```sh
claude "$(cat .hutch/SETUP_PROMPT.md)"
```

When the agent is done, review what it wrote in `.hutch/` as you would any code: the recipe runs with your project's access.

## The recipe

| File | What it does |
| --- | --- |
| `Dockerfile` | The environment: system packages, runtimes, databases. Built on the machine with `.hutch/` as its context. Start from `ghcr.io/mouizahmed/hutch-default`. |
| `services` | One command per line for services every box runs, started as `dev` and restarted if they exit. |
| `on-create.sh` | Runs as `dev` in `/workspace` after checkout, in the template and again in each new box: install the project's dependencies here. |
| `verify.sh` | Runs as `dev` after `on-create.sh` and must exit 0, or no template is made: build the project, run fast tests. |
| `uploads.txt` | Files from your checkout that boxes need but Git must never have, like `.env.local`. |
| `hutch.toml` | Project settings: box size, disk, idle timeout, and requests like private network targets. |

None of them is required: without a Dockerfile, the default image is used. `image = "node:22"` in `hutch.toml` uses a published image instead, and takes the place of the Dockerfile.

The default image is Ubuntu 24.04 with Git, curl, tmux, the GitHub CLI, Node 24, uv, build-essential, and an SSH server. Every box adds a `dev` user (uid 1000) with passwordless sudo and the agents' CLIs. There's no systemd: long-running services go in `services`.

`hutch.toml` is the one recipe file read on your computer rather than in the VM. A typical one:

```toml
version = 1
size = { cpu = 4, memory = "8G" }   # Next.js build + Postgres
disk.size = "50G"                   # node_modules and a seed database
idle_timeout = "10m"
```

Every box in the project gets this size, so keep it modest. `hutch sizes` shows the options and how many of each fit on your machines. See [configuration](configuration.md) for the other settings.

## Build and snapshot

```sh
hutch template build       # run the recipe in a fresh VM and verify it
hutch template snapshot    # freeze the build as the next template version
```

A build clones the commit at your checkout's `HEAD`, so the recipe must be committed and pushed. If it isn't, the build says what's missing (uncommitted `.hutch/` changes, unpushed commits) and offers to commit and push before it starts; `--commit` does that without asking. `hutch.toml` can stay uncommitted.

The first build of a project, and the first after its settings change, opens the project form to confirm the box size, disk, and idle timeout. `--cpu`, `--memory`, `--disk`, and `--idle` set them directly, and `--yes` accepts them as they are.

A build runs, in order: the image, the checkout, your files from `uploads.txt`, the agents' CLIs, `services`, `on-create.sh`, and `verify.sh`. It then reports the peak memory and CPU it used, which helps with sizing. A build that fails leaves the template box running so you can look: `hutch template shell`.

`snapshot` builds first when needed, waits for the services to be up, and saves the next version. Building and snapshotting the same commit again reuses the build. New boxes start from the newest version; boxes that exist already keep their own environment.

```sh
hutch template versions
```

Hutch keeps the newest `retention.template_versions` versions (2 by default), and any version a box was forked from.

## Change the environment

Edit `.hutch/` in your checkout, commit, and run `hutch template snapshot` again. To have an agent make the change, start one on the template box:

```sh
hutch terminal template "Add Redis 7 and start it as a service"
```

It works in the template box, tries commands there, and commits its recipe changes inside the box. The next build brings that work into your checkout on a branch named `hutch/template`, shows the commits, and merges them into your branch after you confirm. A merge that conflicts is undone and the template box keeps its work, so you can merge `hutch/template` yourself. Anything changed in the template box outside `.hutch/` is thrown away at the build.

`on-create.sh` runs again in every new box, whose branch may be newer than the template. Keep it incremental, like `npm install --prefer-offline` or pending migrations, so new boxes stay fast while the template does the slow first run. If it fails in a new box, the box is created anyway with a warning.

## Files that aren't in Git

List them in `.hutch/uploads.txt`, one path per line, relative to the repository:

```text
.env.local
fixtures/seed.sql
```

At each build, Hutch uploads them from your checkout to the machine, encrypted, and writes them into the template box's checkout. Git is told to ignore them there, so an agent's `git add -A` can't commit them. They're in every box made from that template, and in its snapshots: treat them as visible to every agent in the project. Removing `uploads.txt` removes them from the next build.

## Check a recipe

```sh
hutch doctor --template
```

This builds the committed, pushed recipe twice from scratch in temporary boxes, without the Dockerfile layer cache, and compares the two: package and tool versions, and the project's files that Git doesn't ignore. It also starts each MCP server, yours from `hutch user sync` and the repository's, and asks it for its tools, without running any. Two builds that differ usually mean an unpinned dependency. For your own checks, such as hashing generated files Git ignores, add `.hutch/fingerprint.sh`. The temporary boxes are removed afterwards, and the working template is left as it was.

## The TUI

A project's template has its own row at the end of its list in [the TUI](tui.md). It shows where the template stands and the one key that moves it on: set up (`i`), open the setup agent (Enter), build (`b`), and snapshot (`S`).

## Remove a project

```sh
hutch project rm
```

This removes the project's template versions and uploaded files from the machine. It's refused while the project has boxes: remove them first. It doesn't touch your repository.
