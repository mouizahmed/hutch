# Set up {{.Slug}} for Hutch

You are setting up this repository so Hutch can run coding agents in it, each in its
own isolated Linux box. Every box is forked from a template built from `.hutch/`: an
image from `.hutch/Dockerfile`, then the repository, then the scripts below. Your job
is to draft those files so I can review and commit them.

## What a box starts from

- Hutch's default image, `{{.DefaultRef}}`: Ubuntu 24.04 ({{.Arch}}) with git, curl,
  tmux, gh, Node {{.Node}} with npm, uv (and Python through it), build-essential, and
  openssh-server. No databases, Docker, or browsers.
- User `dev` (uid 1000) with passwordless sudo. Hutch adds it on top of any image, and
  takes over an image's own uid-1000 user (`ubuntu` in the default image).
- No systemd: Hutch's init runs the box. Long-running services go in `.hutch/services`.
- The repository is cloned at `/workspace`, owned by `dev`, after the image is built.
- Full outbound internet. No inbound access except through Hutch.

A template box for this project is running now: **{{.TemplateBox}}**, started from the
default image. Use it to try every command before you put it in a file:

```sh
hutch exec {{.TemplateBox}} -- bash -lc 'cd /workspace && <command>'              # as dev
hutch exec {{.TemplateBox}} --user root -- bash -lc 'cd /workspace && <command>'  # as root
```

It is thrown away when the template is built; only what is in `.hutch/` survives.

## Write these files

1. **`.hutch/Dockerfile`**: the environment. Start with `FROM {{.DefaultRef}}` (or another
   image the project already uses) and install what the project needs to build, run,
   and test: system packages, language runtimes, databases. It is built on my machine
   with `.hutch/` as its build context, so it can `COPY` files from `.hutch/` but not
   from the rest of the repository: install the project's own dependencies in
   `on-create.sh` instead. Pin versions. Files a service writes should be owned by
   `1000:1000` (the dev user).
2. **`.hutch/services`** (optional): one command per line for services every box runs,
   started as `dev` by Hutch's init and restarted if they exit, for example
   `postgres -D /var/lib/postgresql/data` or `redis-server`. They are running in the
   template snapshot, so every box starts with them up.
3. **`.hutch/on-create.sh`** (optional): runs as `dev` in `/workspace` after checkout, in
   the template and again in every new box, whose branch may be newer than the
   template. Install the project's dependencies and generated files here, incrementally
   (`npm install --prefer-offline`, pending migrations): the template does the slow
   first run, so later runs are fast.
4. **`.hutch/verify.sh`**: runs as `dev` after on-create.sh and must exit 0, or no template
   is made. Prove the app actually works: build it, run a fast test subset, or start the
   server and curl a health endpoint. Stop whatever it starts before it exits.
5. **`.hutch/uploads.txt`** (optional): one local path per line, relative to the repo, for
   files the environment needs but that must never be committed (`.env.local`,
   `seed.sql`). Don't put secrets anywhere else.
6. **`.hutch/hutch.toml`**: the project settings, starting with `version = 1`. Propose each
   one with a one-line comment giving your reason from what you found in the repo:

   ```toml
   version = 1
   size = { cpu = 4, memory = "8G" }   # Next.js build + Postgres + Redis
   disk.size = "50G"                   # node_modules and a 5 GB seed database
   idle_timeout = "10m"
   # network.allow = ["10.0.5.20:5432"]  # only if the app must reach a private host
   ```

   Every box in the project gets this size, so keep it modest. Run `hutch sizes --json`
   to see the options and how many boxes of each fit on my machine. Ask me when unsure.

If the project has a `compose.yaml` for its services, prefer `.hutch/services` with the
same services installed in the Dockerfile; Docker inside a box works, but is slower.

## Then

Tell me what you wrote and anything you couldn't make work. I will review the files,
commit and push them, and run `hutch template build`.
