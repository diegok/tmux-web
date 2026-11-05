# tmux-web — agents driving tmux with the verbs the browser already has

Date: 2026-11-05
Status: design, not agreed. Revision 4.

**Revision 4 answers one question from the owner**, and it is the best thing
that has happened to this design: *"hay forma de abrir un nuevo pane o window
sin usar run?… si voy a levantar el server de tmux-web yo abriria un pane o
window y ahi escribi `tmux-web -h ...` y luego enter para ejecutar."* There was
not, and that was a hole. `run` made the command the pane's **process**; what he
does is open a pane with its **shell** and type into it. Adding `open` and
building `run` on top of it removes the only new tmux seam this design had, and
deletes `remain-on-exit` entirely (decisions 1 and 6).

Revision 3 folded in the owner's answers to the four questions a builder check
raised; one of them reversed a decision revision 2 made badly (the missing
`close`).

**Revision 2 answers a builder check.** Four things revision 1 got wrong are
corrected rather than softened, and each one was argued from this repo's own
comments:

- the transport was named backwards. Revision 1 said "over the admin socket"
  and then offered `report` as proof the path works — but `report` *"talks to
  tmux and never to the daemon"* (`cli.go:35`). The proof proved the other
  transport, and the other transport is the better one (constraint 7).
- `run` was called a thin shell over `NewWindow`. `NewWindow` says, in capitals,
  that it must never grow a command parameter, and `ResumeAgent`'s three-entry
  lookup table *is* the security story of the route in front of it
  (`manage.go:84`, `:118`). `run` is new code and revision 1 did not admit it
  (constraint 6, decision 8).
- `state` was going to treat a report as authority *"exactly as the poller
  treats it"*. The poller does no such thing: `reports.go` makes a resting
  report survive multi-poll evidence, and records that *"a blocked report costs
  a fork per poll — the case the app exists for is exactly the one where the
  report is wrong"* (decision 3).
- `type` was described as if typing were a solved transport here. It is the
  opposite: this repo has refused `send-keys` twice, and in copy mode
  `send-keys` does not type at all (decision 1, open question 2).

The owner's framing, which is the whole brief: *"la idea es que un agente pueda
interactuar con tmux-web de la misma forma que yo puedo interactuar."*

## Purpose

Several agents, each with its own context, one of them coordinating. The
coordinator needs to see what the others are doing and ask them for things. And
when something has to run for a long time — a server, a build, a `sudo` — it
should live in a **pane the owner can see**, not in a background task hidden
inside one agent's session.

This design adds no capability. An agent inside this tmux already has the whole
`tmux` CLI and no sandbox to break: the daemon that serves this app was restarted
on 2026-11-05 by an agent doing exactly that, `send-keys` into window 0. What is
missing is not power. It is three things the daemon already has and `tmux` does
not:

1. **Knowing what a pane is doing.** `Snapshot` already carries
   working/idle/blocked per pane, and `blocked.go` already carries the question
   an agent is waiting on.
2. **One verb set with the guards already written.** `refuseAppSession`
   (`manage.go:336`) already refuses to kill the app's own sessions. Every agent
   that reaches for raw `tmux` re-derives pane targeting and has no guards at
   all.
3. **A default that does not surprise the owner.**

## What the owner decided

Recorded because each one removes a subsystem, and a later reader will otherwise
try to put it back.

- **The screen is the answer; a file is the attachment.** *"creo que vale con la
  pantalla. Si necesitas texto fiable siempre puedes pedirle al agente que te lo
  escriba en /tmp/xxx.md no?"* So: **no mailbox, no reply protocol, no change to
  the agents' hooks.** Short answers are read off the screen, long ones are
  asked for as a file. This is also what makes the feature work with anything in
  a pane — a `psql`, a `top`, a shell — and not only the three agents whose
  hooks we install.
- **Polling, not subscribing.** *"creo que podria valer con que cada X minutos
  uses la tool para saber si el agente está trabajando o esperando... y si
  espera leer su pantalla."* So: **no subscription, no blocking wait, no
  scheduler in the daemon.** The calling agent decides its own cadence. This
  also unifies two cases that looked separate — coordinating another agent, and
  sharing a shell with the owner — into one loop: *ask the state; if it is
  waiting, read the screen.*
- **This is not a security boundary.** *"no me preocupa la seguridad de esto
  porque el usuario que pide esto ya tiene acceso a la maquina y el agente que
  lo hace tambien."* The owner also observed, correctly, that any scope limit is
  bypassable by going to `tmux` directly. Agreed, and recorded: the limits below
  exist against **accidents and surprise**, never as containment. Nothing in
  this design should be argued for on security grounds later.
- **The CLI, not MCP.** Approach 1 below.

## Goals

- An agent can ask what is running where, and what each pane is doing.
- An agent can read any pane's screen or scrollback.
- An agent can type into another pane, including control keys.
- An agent can start a command in a **new pane or window the owner can see**,
  and say which pane it landed in.
- Everything works for the three hooked agents **and** for anything else in a
  pane.
- It keeps working **when the owner is away**, which is stronger than "when no
  browser is connected" and is the claim that matters: no tab open, and —
  because of constraint 7 — no daemon running either.

## Non-goals

- No message bus, no agent-to-agent protocol, no reply correlation.
- No subscription, no long-poll, no "wait until X changes".
- No change to what the browser UI shows. In particular no new state dot: the
  repo already prohibits rendering a state nothing computed, and that
  prohibition has a test (`snapshot.go:128-137`, Task 11).
- No remote reach. Everything here runs as the owner, on the owner's machine,
  against the tmux server the calling pane is already in.
- No scheduling, retrying or supervising of what an agent starts.
- No new HTTP route and no new admin-socket route. This feature adds nothing the
  browser can reach.

## Anti-goals

Things that would make this a failure even if every test passed:

- **The owner is surprised.** Something moved in a session he was not looking
  at, and nobody asked for it. Note that two tmux defaults reach this on their
  own: `new-window` and `split-window` without `-d` make the new thing current.
- **Two agents talk to each other unattended and loop.**
- **A state appears in the browser for a pane nothing computed.** That is a
  tested prohibition and this feature must not be the thing that breaks it.
- **Two of these verbs disagree about the same pane in the same second.** A
  reader of two outputs will notice, and will be right to stop trusting both.
- **It only works while a tab is open.** That would defeat the owner's own use
  case.

## Constraints the repo already imposes

Found by reading, each one load-bearing. Items 6 and 7 are corrections to
revision 1.

1. **One list gates three things.** `agent.go`: `Agents = ["claude",
   "opencode", "pi"]`, matched exactly, and that list decides whether a pane is
   captured, whether it gets a state, and whether it gets a logo — *"so that a
   pane can never show a badge for a state nothing computed"*. A shell gets no
   state today, by design.
2. **"Blocked" does not generalise, and that is measured.** `blocked.go` keeps
   one grammar per agent and says why: over the three captured screens no two
   share a structural signal (corners/rules/gutter/box/numbered: claude
   0/100/0/0/3, opencode 0/0/18/0/0, pi 4/319/0/60/4), and *"a rule broad enough
   to cover all three shapes is broad enough to fire on prose"*. Nothing in this
   design may loosen those grammars to reach a shell.
3. **The churn classifier does generalise.** `state.go`: `settleAfter` — two
   consecutive identical captures mean idle. That rule needs no grammar and no
   knowledge of what is running. It also **cannot see a blocked dialog**: a held
   dialog is byte-identical poll after poll, which is the same shape as
   stillness.
4. **With no browser connected the classifier is emptied and the captures are
   gated off.** `poller.go:562-610`: only the hooks' reports are read, because
   *"with no client connected a report is the only authority there is"*. So the
   poller's cached state is **not** a dependable answer for the owner's
   away-from-the-tab case.
5. **`Capture` does not go through the poller.** It forks tmux on demand
   (`client.go:492`), so reading a screen works with no browser at all.
6. **The reading verbs exist in Go; the running verb is forbidden.**
   `manage.go` has `NewSession`, `NewWindow`, `SplitPane`, `ResumeAgent`, the
   `Rename*`, `SetLabel`, `ToggleZoom` and the `Kill*` — so `panes`, `state` and
   `peek` are front ends on existing code. But `NewWindow` carries *"IT
   DELIBERATELY APPENDS NO SHELL-COMMAND, AND MUST NOT GROW A PARAMETER FOR
   ONE"*, and `ResumeAgent` takes a **lookup key** rather than a command
   precisely so the two cannot be confused: *"the only argvs this daemon can
   ever hand `new-window` are the three fixed ones in `resume.go`"*. `run` is
   therefore new code, and where it may live is decision 8.
7. **`report` proves the no-daemon path, not the admin socket.** `cli.go:35`:
   *"`report` is the exception, and deliberately so. It talks to tmux and never
   to the daemon… an agent reporting into a machine with no tmux-web running
   costs nothing. Its authorization is the same one tmux itself uses — whoever
   can drive the tmux socket can already set the option by hand."* It dials the
   socket from `$TMUX` (`report.go:44`). That is the shape these verbs want, and
   revision 1 had it backwards.
8. **This repo has refused `send-keys` for typing, twice, with measurements.**
   The v3 implementation plan: *"There is no `SendKeys` in this daemon and there
   never was"*, because the browser types through a real PTY and so *"nothing
   the user types is ever an argv element"* — no quoting, no encoding, no
   chunking. `send-keys -H` was measured anyway and recorded: it round-trips
   bytes exactly, `0d` is Enter (a raw `0a` is `C-j`), and the limit is on the
   **command line** — 16 331 bytes succeeded, 16 355 failed with `command too
   long`, so roughly 5.4 KiB per call. And in copy mode it is worse than
   limited: *"`send-keys` does not type into a pane in copy mode, it dispatches
   copy-mode commands"*, and on 3.7b against a pane in copy mode with a client
   attached the command **did not return at all**.
9. **A pane in copy mode truncates a reply rather than swallowing it.**
   `reply_integration_test.go:17-33`: `q` is cancel in both copy-mode key
   tables, so the pane leaves copy mode and *"every keystroke after it goes to
   the shell — as a command, ending at the Enter the reply's own line ends
   with"*. Prose contains `q` early and often.
10. **A paste is bracketed and gets no Return.** `ReplyBox.tsx:18-22` wraps text
    containing a newline in `ESC[200~`…`ESC[201~` and deliberately sends no
    Return, because the measured alternative is one submitted turn per newline
    — *"twelve submitted turns is not an acceptable failure"*.
11. **Every open tab adds a grouped `_web-*` session**, so `list-panes -a`
    reports the same pane once per group member. `Dedupe` exists only for this
    and prefers the non-app row. The app's sessions carry
    `destroy-unattached on`.

## Approaches

**1. Subcommands on `tmux-web` that fork tmux directly, the way `report`
does.** The shape the repo already uses for the one command agents call today.
Works from any agent, present or future, in any language, because shell is the
one thing all of them have. It needs **no daemon running**, it follows `$TMUX`
so it acts on the server the calling pane is in, and its authorization is tmux's
own — constraint 7's argument, verbatim, applied to more verbs. Costs: the agent
must be *taught* the verbs, and the guards live in a binary rather than behind a
route.

**2. An MCP server (`tmux-web mcp`).** Typed arguments, discoverable tool
descriptions, so the agent needs no skill to learn the verbs. Costs: three
different agent configurations to install and keep working, another process per
agent, support that varies per agent, and it only ever serves things that speak
MCP.

**3. A skill that teaches raw `tmux`.** Zero code, and it throws away every one
of the three things in Purpose — the state, the grammars, the guards. This is
the status quo, and the owner ruled it out explicitly: he wants *"más
conocimiento de agentes que usar tmux a pelo"*.

**Chosen: 1**, with the core factored so that 2 can later be a thin front end
over the same functions — the same code, two faces. The deciding argument is not
elegance: it is that this is the one path already proven from inside all three
agents, and it is the only one that survives the owner being away with nothing
running.

## Decisions

### 1. The verb set

| Verb | What it does |
| --- | --- |
| `tmux-web panes [--all]` | one line per pane: pane id, `session:window.pane`, command, agent name, the free state, label, title |
| `tmux-web state <pane>` | one word, plus the question when there is one |
| `tmux-web peek <pane> [--lines N]` | the current screen, or N lines of scrollback |
| `tmux-web type <pane> <text> [--enter]` | types text as a person would |
| `tmux-web type <pane> --key <key>` | one tmux key name, e.g. `C-c` |
| `tmux-web open [--window NAME \| --split] [--label TEXT]` | a **new window or split with its shell**, empty, and prints the pane id |
| `tmux-web run <command> [--window NAME \| --split] [--label TEXT]` | `open`, then `type --enter`. A convenience, not a second mechanism |
| `tmux-web close <pane>` | closes a pane the way the owner closes one |

**`open` and `type` are the pair; `run` is sugar over them.** This is the
correction revision 4 exists for. `new-window <command>` makes the command the
pane's process: no shell, nothing to type next, and the pane dies with it.
`new-window` with no command is *"an empty shell to all four of its callers"*
(`manage.go:84`) — the `+` button in the browser — and typing into that shell is
what the owner does and what an agent restarting this daemon did by hand on
2026-11-05.

Building `run` as `open` then `type --enter` has three consequences, all good:

1. **No new tmux seam.** `NewWindow` and `SplitPane` are used exactly as their
   comments intend, so constraint 6's prohibition is never approached, let alone
   reversed. Revision 3 had `run` building its own `new-window` call in the CLI
   and flagged it for review; there is now nothing to review.
2. **`remain-on-exit` is unnecessary.** MEASURED on 3.7b on a throwaway socket,
   with a command that exits 7: the pane is alive (`dead=0`), the shell is back
   (`pane_current_command` returns to `zsh`), **the output is still on screen**,
   and the pane accepts another command. The shell is what survives, so there is
   no corpse to hold and nothing to clear.
3. **A pane can be used twice.** C-c and run it again, or run something else —
   which is what the owner means by *"yo abro una pane/window lo uso"* and which
   `new-window <command>` could never do.

`pane_current_command` still reports the running program rather than the shell,
so `panes` shows `make` or `tmux-web` while it runs. Observed on the owner's live
server: the daemon's pane reported `cmd=tmux-web` with `pane_pid` pointing at the
zsh that was typed into.

The owner's framing for the whole verb stands: *"podrias ofrecerme correrlo en un
nuevo pane aqui al lado o una nueva window en esta session. yo veria los logs
cuando quiero, veria lo que escribes."*

**How a verb knows who is calling.** tmux sets `$TMUX_PANE` in every pane, so
the calling agent's own pane id is in its environment, and `$TMUX` gives the
socket. Nothing has to be guessed and nothing has to be passed — the same two
variables `report` already relies on.

**Targets are pane ids (`%N`) only**, validated with `ValidatePaneID`.
`internal/tmux/target.go` is unambiguous about this, and tmux prefix-matches
session and window *names* without reporting the ambiguity. `panes` prints
`session:window.pane` for a human to read and the `%N` for a verb to take.

**`type` is `send-keys`, and the owner has accepted what that costs.** A
one-shot CLI has no attached PTY, so the transport this repo twice refused
(constraint 8) is the only one available short of attaching a throwaway client,
which was offered and declined as too much build for the fidelity. The costs are
recorded rather than hidden: ~5.4 KiB per call, `0d` for Enter, and **it does
not type into a pane in copy mode at all** — which is why decision 6 refuses
such a pane instead of trying. This is the one place where the chosen approach
is materially worse than the browser's path, and it is a knowing trade.

**`run` takes one shell-command string**, the owner's decision: tmux hands it to
`sh -c`, so `tmux-web run 'make test 2>&1 | tee /tmp/out'` works and is what an
agent actually wants to write. An argv after `--` was the alternative and was
declined. Not a security question — the owner settled that above — but it does
mean a badly built line gets a whole shell, so the skill must say that the
string is shell, not argv.

### 2. State for a pane that is not a known agent

A shell gets no agent state today and must not start getting one. But the churn
rule (constraint 3) needs no grammar, and for a shell "stopped changing" means
exactly "waiting for someone" — which, under the owner's decision, is the point
at which the caller reads the screen anyway.

So `state` answers from two different vocabularies and never mixes them:

- a known agent: `working`, `idle` or `blocked`, with the question when blocked;
- anything else: `changing` or `quiet`;
- and `unknown` when it could not be determined.

`changing`/`quiet` are deliberately *not* the UI's words. A reader of the output
must not be able to mistake a churn verdict for the agent state the sidebar
shows, and the browser keeps showing nothing for these panes.

### 3. What `state` believes, and why it corroborates

Revision 1 said a report is the authority *"exactly as the poller treats it"*.
That was wrong, and the correction is the most important one in this revision.
`reports.go` makes a resting report survive evidence: a reported `idle` is
dropped if the classifier says working for `NIdle` (= 4) consecutive polls; a
reported `blocked` is dropped on the first capture that moves, or after
`NBlocked` (= 3) settled polls with no registered form; a rejection is
remembered so the same standing value cannot come back; and a `working` report
expires at `workingTTL` (60 s). A one-shot CLI call has **none of that
history**, and `reports.go:252` names the exact hazard: *"a blocked report costs
a fork per poll — the case the app exists for is exactly the one where the
report is wrong."*

The owner's away case is precisely the case where nothing has been
corroborating. So `state` does this, per authority:

- **`working` report, unexpired** — trusted, no fork. A report younger than
  `workingTTL` is a claim about right now.
- **`blocked` report** — corroborated with one capture and `IsBlocked(agent,
  screen)`. Uncorroborated it is reported as `idle` with a note that a stale
  blocked report was dropped. This is the one direction the owner's loop cannot
  absorb: a false `blocked` sends the coordinator to read a screen that has
  nothing to answer, and it would do so for as long as the stale option stands.
- **`idle` report, or no report** — two captures a settle apart, compared;
  identical is `idle`/`quiet`, different is `working`/`changing`. For a known
  agent the first capture also runs `IsBlocked`, because churn alone can never
  see a dialog (constraint 3).

**The question text comes from two places and they are different texts.** A
screen verdict gives `ExtractQuestion(agent, screen)`; a report verdict gives
`Report.Activity`. `state` says which one it is holding rather than presenting
them as one field — revision 1 conflated them.

### 4. `panes` prints only the state it gets for free

`panes` must not be a multi-second command, and it must not disagree with
`state` (anti-goal). Computing decision 3 for every pane would be two or three
forks per agent pane plus a settle wait, on a server with a dozen panes.

So `panes` prints the **report-derived** state where there is one — which costs
no fork, is what the poller itself leans on with no client connected, and is
honest about its own freshness — and a dash for every other pane, including
agent panes whose report is stale. A dash means "ask `state`". One listing, one
cost, no contradiction.

### 5. Scope: the caller's own session by default

`panes` lists the caller's session. `state`, `peek`, `type` and `run` accept a
pane outside it only when it is named explicitly. This is not containment
(constraint: it is bypassable and the owner said so first); it is so that
nothing moves in a session nobody was looking at.

**Resolved the way `Dedupe` resolves it** (constraint 11): the caller's session
is the non-app member of the group. An agent's own pane is reported once per open
browser tab, and the `_web-*` answer is the wrong one — those sessions carry
`destroy-unattached on`, so a window created in one dies when the tab closes.

### 6. The guards, which are about accidents

- **`type` refuses the caller's own pane.** An agent typing into itself is a
  loop, and there is no case where it is what was meant.
- **`type` refuses a pane in copy mode.** `#{pane_mode}` is already on every
  snapshot row (`snapshot.go` `PaneMode`). Refusing rather than cancelling
  first, as the browser does, because a cancel changes what the owner is looking
  at, and because constraints 8 and 9 make the alternative either silent
  truncation or a call that does not return.
- **`type` refuses multi-line text with `--enter`.** Constraint 10 measured what
  that is: one submitted turn per newline. Multi-line text without `--enter` is
  bracketed, exactly as `ReplyBox` brackets it, and the agent is told it must
  submit it in the pane.
- **`type` and `run` refuse the app's own `_web-*` sessions.** For `type` it is
  meaningless; for `run` it is worse, because the window would be destroyed with
  the tab. `refuseAppSession` is the existing precedent.
- **No `remain-on-exit`, and this is revision 4's deletion.** The owner chose
  "always on" in revision 3, and he was answering a problem that only existed
  because `run` made the command the pane's process. With `open` + `type` the
  shell survives the command and keeps its output (decision 1, measured), so
  there is nothing to hold open. His stated reason — *"Yo abro una pane/window
  lo uso y lo cierro cuando quiero"* — is this design rather than an option on
  it.

  Recorded so nobody re-derives it: `remain-on-exit` takes `on`, `off` and
  `failed` on 3.7b, and `set -p` scopes it to one pane — a split's neighbour
  does not inherit it and neither does the window. Measured on a throwaway
  socket, and not needed.
- **`run` passes `-d`.** Both `new-window` and `split-window` make the new thing
  current without it, so the default reaches anti-goal 1 on its own: an agent
  starting a server would yank the owner's terminal off whatever he was reading.
  The pane id is printed instead, and the owner goes there when he wants to.
- **`close` exists, and refusing it was revision 2's mistake.** Revision 2 left
  the verb out because killing panes is the most destructive thing in the set.
  The owner reversed it with the brief's own principle: *"La idea es que el
  agente pueda hacer lo que yo hago. Yo abro una pane/window lo uso y lo cierro
  cuando quiero."* He is right, and the omission was worse than destructive —
  with `remain-on-exit on` below, a design with no `close` leaves dead panes
  that nothing but his own hands can clear.

  Its guards, which are mine: `close` refuses the **caller's own pane**, because
  an agent closing it kills itself mid-turn and no agent means that; it refuses
  the app's `_web-*` sessions, as everything else here does; and it takes the
  same explicit-naming rule outside the caller's session. It is not restricted
  to panes the agent created — the owner's principle is that the agent does what
  he does, and he closes any pane he likes.

### 7. Reading a screen is taking instructions, and that is accepted

`peek` hands an agent text that another program wrote. That text can contain
instructions, and the reading agent cannot tell the difference by looking. The
owner has accepted the risk on the stated ground that everyone involved already
has the machine.

What this design does about it is therefore not a control but a rule, and it
belongs in the skill rather than in code: **a `peek` is data, never
instructions.** An agent acting on something it read in another pane is doing
what a person would call reading the room wrong.

### 8. Where the code goes

- `internal/tmux` keeps the reading verbs. Nothing there needs to learn about
  agents calling it.
- The on-demand state (decision 3) is new, and belongs beside the classifier and
  the report rules it borrows from rather than in the CLI.
- **Nothing hands a command to `new-window`, so constraint 6 is left alone.**
  Revision 3 had `run` building its own `new-window` call in the CLI and asked a
  reader to check it was not routing around the prohibition. Revision 4 removes
  the question: `open` uses `NewWindow`/`SplitPane` for the empty shell they
  were written for, and the command arrives as keystrokes. The only argvs this
  daemon ever hands `new-window` are still the three in `resume.go`.
- `cmd/tmux-web/cli.go` gains the subcommands, next to `report` and
  `install-integration`.
- **The installer grows a second owned file, and `tmuxWebSchema` goes to 2.**
  `agentSpec` holds exactly one owned file today (`install.go:99`) under a
  managed header. The owner confirms he uses skills in **all three** agents, so
  the slot is built to hold N owned files and the skill is filled in for claude
  only: *"puedes empezar por claude asi lo probamos y cuando funcione bien
  investigamos para los otros dos, pero seguro que se puede."* The shape is
  therefore designed for three and delivered for one — not a claude-only
  mechanism that would have to be rebuilt.

## Sequencing

In the owner's order of use, each step shippable on its own:

1. **`panes`, `state`, `peek`** — read-only. Immediately useful, nothing moves,
   and no part of it depends on the open questions.
2. **`open`, `type`, `close`, then `run`** — the visible-process case and the
   shared-shell case, in dependency order: `run` is the last of the four because
   it is the first two in one call. This is the step that answers both the
   `sudo` example and the daemon restart that prompted the conversation.
3. **`type <text>` into another agent, plus the claude skill** — the
   coordinator, and the etiquette that keeps two agents from looping.

## Open questions

The owner's four are answered and have moved into the decisions above:
`send-keys` with its limits (decision 1), `remain-on-exit on` plus a `close`
verb (decision 6), a skill for claude on an installer slot built for three
(decision 8), and one shell string for `run` (decision 1). What is left is
measurement, and belongs to the builder.

1. The settle interval `state` uses for its two captures. `state.go`'s existing
   constant is the starting point, and whatever is chosen should say in a
   comment why.
2. Whether `--split` splits the caller's pane or the window's active pane. The
   caller's own is the less surprising of the two, and `open` and `run` must
   answer it the same way.
3. The key-name vocabulary `--key` accepts. tmux's own names, passed through, is
   the cheapest answer and the one a reader can look up.
4. The exact output format of `panes`. One line per pane, stable field order,
   parseable without a parser; `devices` is the precedent.
