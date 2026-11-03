# The TUI

`hutch` with no command opens the TUI: project tabs on top, the open project's boxes and their terminals on the left, and the selected terminal or a view on the right. Everything the CLI does is reachable from it; what asks questions (logins, editing config, `hutch local` and `hutch cloud`, adding or upgrading machines) gets the whole terminal and comes back when it ends.

## Finding things

- **Space** opens the action palette, in the list or any view: every action that applies to the selected project, box, terminal, or view, found by typing any words of its name. One that doesn't apply says why instead of running.
- **?** lists the keys.
- **/** narrows the open project's list to boxes and terminals whose names, agents, branches, or machines hold what you type; **Esc** clears it. Searching never wakes or starts anything.

Commands the TUI runs for you (a new box, a move, a push) show as rows at the top of the list, with their output in the pane. One that ends well leaves the list and says what it did; one that fails stays, with its error, until **d** dismisses it. When a new box or terminal fails or is canceled, the next **n** or **N** opens with the prompt you wrote; Esc there discards it. **d** on a running one cancels it, asked first: it stops here, but the machine may already have done some of it. Quitting cancels what's still running, and stops it for good after `tui.cancel_grace`.

The footer always says where your keys go: the list, a view, or the terminal you're typing in. **Enter** types into the selected terminal (waking a sleeping box first) and **ctrl+]** gives the keys back.

## Views

Each opens in the workspace pane in place of the terminal; **esc** goes back to the same selection. A terminal under a view doesn't count as seen and doesn't keep its box awake.

| Key | View | What it does |
|---|---|---|
| `g` | Git | The box's checkout as the agent sees it: staged, unstaged, untracked, and conflicted files; each file's diff, with line numbers; staging and unstaging whole files (`a`/`u`) or single hunks (`A`/`U`, refused if the file changed since it was shown); a commit of what's staged (`c`, refused if that changed since it was shown); fetch (`f`); fast-forward-only pull and push (`p`, `P`), each asked first (pull needs an upstream; push sets one); and the task diff against the base branch (Tab). Looking changes nothing and contacts no remote. |
| `o` | Ports | What the box listens on, and forwarding a port to this computer's loopback (Enter), choosing another local port, or any free one, when it's taken. Forwards stay while you look elsewhere and close when you stop them or quit; a forward to a sleeping box wakes it on its first connection. |
| `m` | Machines | Each machine's role, state, version, room, and boxes; adding, removing, removing as lost, making another the lead, and upgrading every machine. |

The palette also has new boxes on a chosen machine (or Auto), new terminals, sleep and wake, remove, move to another machine, resize, rename a terminal (`r`), pull requests, `hutch local` and `hutch cloud`, templates, the serial console, logging in, `user sync`, editing and checking config, grants, doctor, sizes, the bucket, and garbage collection. What prints something to read (doctor, sizes, grants, template versions) gets the whole terminal, like what asks questions.
