# First box

This guide goes from an empty Linux machine to an agent working in a box on your project. You'll need the `hutch` CLI on your computer ([install](install.md)) and a Git repository with an `origin` remote.

Commands run on your computer unless they say otherwise.

## Prepare the machine

Use an x86_64 Linux machine with systemd and kernel 6.1 or newer; an Ubuntu VPS with 8 GB of memory is a good start. See [requirements](install.md#the-machine) for the details, including Firecracker and gVisor.

Make `ssh <ssh-host>` work with a key, as a user with passwordless sudo. Check:

```sh
ssh <ssh-host> sudo -n true && echo ready
```

## Set it up

```sh
hutch setup <ssh-host>
```

Setup checks the machine, installs the runtimes and `hutchd`, and builds the default image, which takes a few minutes. `/var/lib/hutch` must be XFS with reflink: setup proposes a blank disk to format, or an image file, and asks before it formats anything. Read that prompt carefully.

It then offers a backup bucket. Without one, losing the machine loses its boxes; with one, save the recovery passphrase somewhere you can reach without the machine. You can add one later with `hutch machine bucket`; see [backups and recovery](backups.md).

Setup ends with `hutch doctor`. Every line should pass, or say how to fix it.

## Sign in

```sh
hutch login claude gh      # or: hutch login codex gh
hutch user sync            # review, then apply
```

Each login opens the tool's own sign-in. `gh` lets boxes clone private GitHub repositories. Sync copies your agent config, like `CLAUDE.md` and MCP servers, into every new box; read its preview before applying. See [agents, logins, and MCPs](agents.md).

Check: `hutch user status` shows each login as signed in.

## Set up your project

In your checkout:

```sh
hutch init
```

Init starts the project's template box and writes `.hutch/SETUP_PROMPT.md`. Give it to your agent, in your checkout:

```sh
claude "$(cat .hutch/SETUP_PROMPT.md)"
```

The agent writes the recipe in `.hutch/`: a Dockerfile, the services, and scripts that install dependencies and check the project works. Review it like any code you'd run, then build and snapshot it:

```sh
hutch template snapshot
```

The first time, a form asks you to confirm the box size. The build commits and pushes `.hutch/` if you haven't, after asking. It shows each step, then `Built and verified …` and `Template v1`. A failing step leaves the template box running so you can look: `hutch template shell`. See [projects and templates](templates.md).

## Start an agent

```sh
hutch new "Run the tests and fix any that fail"
```

The box starts in seconds, as a copy of the template on its own branch, and the agent starts with your prompt. Open the TUI to watch it:

```sh
hutch
```

Your projects are tabs along the top, with their boxes and terminals on the left and the selected terminal on the right. Enter types into the terminal, and `ctrl+]` gives the keys back. Space lists every action, and `?` every key. See [the TUI](tui.md).

## Review the work

When the agent is done:

```sh
hutch diff <box>
hutch pr <box>       # push its branch and open a pull request
```

Or, in the TUI, `g` opens the box's Git view. To carry on in your own checkout instead, `hutch local <box>` brings its branch and the agent's conversation back. See [review and deliver work](git.md).

Boxes sleep on their own after 10 minutes without activity, and wake when you come back to them. Remove one when its work is pushed:

```sh
hutch rm <box>
```

## Next

- [Boxes and terminals](boxes.md): more terminals, shells, sleep and wake, and resizing.
- [Ports, SSH, and editors](networking.md): reach a box's dev server, or open it in your editor.
- [Several machines](machines.md): add more machines for more boxes.
- [Troubleshooting](troubleshooting.md), when something doesn't work.
