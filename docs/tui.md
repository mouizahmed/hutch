# The TUI

`hutch` with no command opens the TUI: project tabs on top, the open project's boxes and their terminals on the left, and the selected terminal or a view on the right. Everything the CLI does is reachable from it; what asks questions (logins, editing config, `hutch local` and `hutch cloud`, adding or upgrading machines) gets the whole terminal and comes back when it ends.

## Finding things

- **Space** (or `:`) opens the action palette: every action that applies to the selected project, box, terminal, or view, found by typing any words of its name. One that doesn't apply says why instead of running.
- **?** lists every key and action.
- **/** searches box and terminal names, agents, branches, and machines as you type; **Tab** widens it to every project, and **Enter** on a result opens its project. **f** filters by state (working, waiting, exited, unread, awake, asleep, unreachable) or machine; **F** clears both. Searching and filtering never wake or start anything.

The footer always says where your keys go: the list, a view, or the terminal you're typing in. **Enter** types into the selected terminal (waking a sleeping box first) and **ctrl+]** gives the keys back.

## Views

Each opens in the workspace pane; **esc** goes back to the same selection.

| Key | View | What it does |
|---|---|---|
| `g` | Git | The box's checkout as the agent sees it: staged, unstaged, untracked, and conflicted files; each file's diff, with line numbers; staging and unstaging whole files (`a`/`u`) or single hunks (`A`/`U`, refused if the file changed since it was shown); a commit of what's staged (`c`); fetch, fast-forward-only pull, and push (`f`, `p`, `P`), each asked first; and the task diff against the base branch (Tab). Looking changes nothing and contacts no remote. |
| `o` | Ports | What the box listens on, and forwarding a port to this computer's loopback (Enter), choosing another local port when it's taken. Forwards stay while you look elsewhere and close when you stop them or quit; a forward to a sleeping box wakes it on its first connection. |
| `i` | Details | The box's machine, branch, allocation, snapshot state, terminals, and (awake) what it listens on; its serial console; a copy of the details. |
| `m` | Machines | Each machine's role, state, version, room, and boxes; adding, removing, removing as lost, making another the lead, and upgrading every machine. |
| `t` | Tasks | Every command the TUI ran for you, with its whole output to scroll, search, and copy; cancel one that's running, and retry one that ended (asked first: nothing is replayed on its own). Finished tasks are kept, bounded by `tui.task_history`, `tui.task_lines`, and `tui.task_expiry`. |
| `,` | Settings | Your logins (a captured login isn't proof the provider still takes it), the effective config with where each value comes from, and editing, syncing, logging in, and grants. Secret values aren't shown. |

The palette also has new boxes on a chosen machine (or Auto), new terminals, sleep and wake, remove, move to another machine, resize, rename a terminal (`r`), send a message to one, pull requests, `hutch local` and `hutch cloud`, templates, doctor, sizes, the bucket, and garbage collection.

## Two panes

**v** shows the selected terminal in a second pane, beside the workspace pane or below it on a narrow screen; the workspace pane keeps following the list, so an agent's terminal can stay in view while you review its diff. **w** switches which pane your keys go to (a click does too), and **v** again closes it. On a screen too small for two, the focused pane fills the space and the other stays connected. A terminal off screen doesn't count as seen and doesn't keep its box awake.
