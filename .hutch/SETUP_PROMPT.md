# Set up hutch for Hutch

You are setting up this repository so Hutch can run coding agents in it, each in its
own Linux VM (a "box"). Every box is forked from a template that is built from a
recipe in `.hutch/`. Your job is to draft that recipe so I can review and commit it.

## What a box starts from

- Ubuntu 24.04, x86_64, systemd. User `dev` (uid 1000) with passwordless sudo.
- Installed: git, curl, tmux, gh, Node 24 with npm, uv (and Python through it),
  openssh-server. Nothing else: no compilers, databases, Docker, or browsers.
- The repository is cloned at `/workspace`, owned by `dev`.
- Full outbound internet. No inbound access except through Hutch.

A template box for this project is running now: **hutch-template**. Use it to try
every command before you put it in the recipe:

```sh
hutch exec hutch-template -- bash -lc 'cd /workspace && <command>'              # as dev
hutch exec hutch-template --user root -- bash -lc 'cd /workspace && <command>'  # as root
```

It is thrown away when the template is built; only what is in the recipe survives.

## Write these files

1. **`.hutch/setup.sh`**: runs as root in a fresh VM, in `/workspace`, with `bash -eo pipefail`.
   Install everything the project needs to build, run, and test: system packages,
   language runtimes, databases and services, project dependencies (run the project's
   own install as `dev`, e.g. `sudo -u dev npm ci`), and anything generated.
   Pin versions. Make it idempotent. Enable services the app needs with systemd so they
   start in every box. Don't leave background processes or one-off systemd units
   running at the end: everything pending when the template is snapshotted runs again
   in every box.
2. **`.hutch/verify.sh`**: runs as `dev` after setup.sh and must exit 0, or no template is
   made. Prove the app actually works: build it, run a fast test subset, or start the
   server and curl a health endpoint. Stop whatever it starts before it exits.
3. **`.hutch/on-create.sh`** (optional): runs as `dev` in every new box after its branch is
   checked out, which may be newer than the template. Keep it fast and incremental, like
   `npm install --prefer-offline` or pending migrations.
4. **`.hutch/uploads.txt`** (optional): one local path per line, relative to the repo, for
   files the environment needs but that must never be committed (`.env.local`,
   `seed.sql`). Don't put secrets anywhere else.
5. **`.hutch/hutch.toml`**: the project settings, starting with `version = 1`. Propose each
   one with a one-line comment giving your reason from what you found in the repo:

   ```toml
   version = 1
   size = { cpu = 4, memory = "8G" }   # Next.js build + Postgres + Redis in compose
   disk.size = "50G"                   # node_modules and a 5 GB seed database
   idle_timeout = "10m"
   # network.allow = ["10.0.5.20:5432"]  # only if the app must reach a private host
   ```

   Every box in the project gets this size, so keep it modest. Run `hutch sizes --json`
   to see the options and how many boxes of each fit on my workers. Ask me when unsure.

## Then

Tell me what you wrote and anything you couldn't make work. I will review the files,
commit and push them, and run `hutch template build`.
