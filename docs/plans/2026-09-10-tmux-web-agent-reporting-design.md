# tmux-web v3 — agent-side reporting

Date: 2026-09-10
Status: design, not agreed. Two questions need measurement before a plan; see the
last section.
Follows: `2026-09-10-tmux-web-v2-design.md` (v2), which it partly supersedes.
Depends on: the hardening of `internal/tmux/snapshot.go` against hostile
`@wterm_label` values, which landed while this was being written. It is a
prerequisite, it answers one of the questions this document opened with, and it
constrains the transport more than expected — see "The hazard" and "Why the
report is not a fourteenth field".

## Purpose

v2 answers "which one needs me?" from pixels: it hashes each agent pane's screen
to decide working from idle, and positive-matches an approval box to decide
blocked. That works, and it is the only thing that can work for an agent that
tells us nothing.

It cannot answer "what is it doing", and v2 thought it could. This document
replaces the part of v2 that was wrong, and adds a second, exact source of state:
small integrations, shipped with tmux-web, that each supported agent runs, which
publish what the agent is doing onto its own tmux pane.

## The premise that collapsed

v2, Decision 1: **"The pane title is the sidebar label."** The section under it
shows two live rows —

```
%6   title=[✳ Categorización productos southafrica]
%9   title=[✳ Product-join tasks southafrica]
```

— and calls that "a far better sidebar row than `claude`". It is. But v2 read
those titles as *what the agent is working on*, and they are not. They are what
it was **first asked**, frozen at the first turn and never revised. Measured in
the last few hours, in isolated tmux servers, against the real agents:

- **opencode** derives its session title from the first message and never
  revises it. A session started with a haiku request, then asked an unrelated
  question about prime numbers, still reported the haiku title. It also
  truncates to exactly 37 characters plus `…`.
- **Claude Code**'s title is the statusline's `session_name`, generated on the
  first turn and frozen after it. Held byte-identical across three turns with
  unrelated prompts (`✳ Terminal multiplexers haiku` throughout), and it stays
  stale while the agent is blocked. No statusline field carries current activity.
- **pi**'s title is `π - <cwd basename>`. It never mentions the task at all.

So v2's revision 1 was wrong that the title is a state *signal*, revision 2
corrected that — and revision 2 was still wrong that it is a good *label*. It is
a good **session name**: stable, human, and useful for telling two panes apart.
It is not a description of anything happening now. A row that says
`Categorización productos` while the agent is twenty minutes into something else
is not a smaller version of the truth; it is a confident lie, and it is the
reason this feature exists.

### What this supersedes, exactly

- **v2 Decision 1 and the section "The title is a label, not a signal"**, in the
  part that treats a title as a description of current work. The mechanism —
  `#{pane_title}` in the format string, capped at `MaxTitle`, prefix-stripped in
  `withoutAgentPrefix` — is kept unchanged and stays the fallback for a pane with
  no report. Its *meaning* is downgraded from "what it is doing" to "what this
  pane is", and the UI copy should stop implying otherwise.
- **v2's rule that `AgentState` is empty whenever no browser client is
  connected.** That rule was right about hashes and wrong about facts; see
  "State: precedence".
- **The transport sentence that started this design** — that an integration can
  simply write `@wterm_label`. It cannot. See "Not `@wterm_label`".

Everything else in v2 stands. In particular the churn classifier
(`internal/tmux/state.go`), the blocked grammars (`internal/tmux/blocked.go`),
`finishedAt` living on the server and `done` living in the browser, and the rule
that only a positive match ever sets `blocked` — all unchanged, all still load
bearing, because an integration is opt-in and most panes will not have one.

## Prior art: herdr, again, and where this design leaves it

[herdr](https://github.com/herdrdev/herdr) (Apache-2.0) ships exactly this
feature, and reading it is most of the reason this document exists. Four things
it does, and what we do with each:

**They publish state only.** Their integrations report `working | blocked |
idle`. pi's asset sets a `message` field only when blocked; opencode's never sets
one at all. Their default sidebar row is `["state_icon", "machine", "workspace",
"tab"]` plus `["agent"]` — no title, no summary. `terminal_title` and
`terminal_title_stripped` exist as opt-in columns.

We go past them: we publish an activity string as well. That is a deliberate
departure with no precedent to lean on, and the reasons are in "What the label
says", along with the reasons it might be wrong.

**They ship no state integration for Claude Code**, and they used to. Their
current asset registers only `SessionStart` and reports session identity so that
sessions can be restored. `HOOK_REMOVALS` in `src/integration/claude_settings.rs`
lists the hooks they *delete* on install — `PreToolUse`, `UserPromptSubmit`,
`Stop`, `PermissionRequest` among them — which is the wreckage of a hook-based
state machine they removed. Their docs now file Claude Code under "session
identity" and say its state comes from screen-manifest detection.

That deletion is the single most useful signal available about this design, and
it is worth being careful about what it does and does not tell us. See "Why
herdr deleted their Claude hooks (speculation)".

**Their screen rules are declarative TOML**, one file per agent, versioned with
`min_engine_version` (`distribution/agent-detection/*.toml`). pi's whole rule is
`contains = ["Working..."]`. We are not adopting this; see "Rejected: TOML
manifests".

**Their sanitizer** (`src/app/api_helpers.rs`) is: trim, drop every character for
which `is_control()` holds, truncate to 80, and treat empty-after-normalise as
*clear*. We adopt the shape of this, including the empty-means-clear rule, and
not the number. See "Sanitization and bounds".

## What is being transported

### Not `@wterm_label`

The obvious transport is the option that already exists. `@wterm_label` is read
by every poll, is already in `Format`, and already outranks the title in
`paneText`. An integration would need one line.

**It is the wrong option, and the reason is in v2.** `@wterm_label` is *the
user's* field: `PATCH /api/panes/{id}` writes it, and v2 says the label "wins
outright, including over a title a program is rewriting underneath it — that is
the whole point of having one." An integration writing it puts a program back
underneath, in the one field that was defined as being above programs. Renaming a
pane `deploy box` would survive until the agent's next tool call, and the agent's
report would survive until the next rename. Both features would appear
intermittently broken and neither would be at fault.

So integrations write a **new** per-pane option, `@wterm_agent`, and never touch
`@wterm_label`. The naming follows `@wterm_web` and `@wterm_label`.

### The value

One option, one write, one fork per event. Not three options, because every write
happens inside a hook the agent is waiting on (see "Writes must never block") and
three forks is three times the exposure.

```
@wterm_agent = "1;working;1757500000000;running go test ./internal/tmux"
                │ │       │             └ activity text, may contain ";"
                │ │       └ unix ms when the agent produced this
                │ └ working | blocked | idle
                └ schema version
```

Parsed with `strings.SplitN(v, ";", 4)`, so only the first three separators are
structural and the text can contain semicolons freely. Every field before the
text has a shape that can be checked — a decimal version, a word from a closed
set, a decimal timestamp — and **a value that fails any of those checks is
discarded whole**, not repaired. A report we cannot parse is not a report we
wrote.

The leading version is herdr's `min_engine_version` idea moved to where our
version skew actually lives. Their rules ship with the daemon and have to
announce what engine they need; ours are written by an integration that the user
installed once and may not have updated since, into an option read by a daemon
that may be newer or older. A daemon that does not understand version `2` ignores
the report and falls back to the screen, which is the correct degradation and
costs nothing.

**Unset and empty are the same thing.** `#{@wterm_agent}` renders both an unset
option and one set to `""` as the empty string, so the daemon cannot tell them
apart and does not try: empty means no report. Writers must still use
`set -p -u`, because an option that exists holding nothing shows up in
`show-options -p` and wastes the time of whoever is debugging. That is writer
hygiene, not a reader rule.

### The hazard

**tmux does not sanitize user option values.** Verified: both `0x1f` — the
snapshot format's field separator — and a bare newline survive verbatim into
`list-panes -F` output. A `0x1f` adds a field; a newline splits the record in
two. Either way the record is not the record we meant, and the pane can
disappear from the sidebar, which the code's own comments call the worst failure
this project has.

The hardening that landed alongside this design gives `@wterm_label` three
layers, and they are worth restating because the report needs the same treatment
and cannot have all of it:

1. **tmux substitutes the two bytes out before Go sees them.**
   `#{s/[\n\x1f]/ /:@wterm_label}`, with a literal `0x1f` inside the bracket set.
   It replaces with a *space* rather than nothing, so `EV\x1fIL` reads as `EV IL`
   — visibly tampered with, rather than as a label somebody chose.
2. **The field is last**, so a byte that survives layer 1 is bounded by
   arithmetic rather than by layer 1 holding: a surplus separator only adds
   fields past the end, which `ParseRows` rejoins into the label, and a newline
   only truncates the last field, leaving the twelve before it — the whole
   identity of the pane — already complete on the line. `fieldCount` became a
   minimum rather than an equality to make that work.
3. **`sanitizeLabel` in Go**, which is `validateLabel`'s rules applied instead of
   refused: control runes to spaces, invalid UTF-8 to U+FFFD, `MaxLabel` runes,
   trimmed — so one notion of a safe label rather than two.

**This answers the question this design opened with, and the answer is not the
obvious one.** `#{s|[[:cntrl:]]||g:...}` does *not* work, and it fails in the
worst available way: the modifier's variable is introduced by `:`, so the `:`
inside a POSIX class terminates the pattern early and the whole expression
expands to `""` for **every** value, including a perfectly good label. A rule
that silently blanks the field it was meant to protect would have passed every
test that only checks hostile input. A range such as `[\x0a-\x1f]` compiles but
depends on the locale's collation order, and the tmux server's locale is whatever
started it. A set of two literal bytes has neither problem. Anyone extending this
should copy the bracket set and not improve it.

Layer 1 is also the one that can fail **open**: a pattern that stopped compiling
leaves tmux echoing the value untouched and exiting 0, measured with a
deliberately broken `[`. That is not attacker-controllable — it takes a code or
tmux-version change — which means layer 2 is a defence against *our own future
regression*, not against a hostile writer. That distinction decides the next
section.

### Why the report is not a fourteenth field

The obvious way to read `@wterm_agent` is to append it to `formatFields` and get
it in the same `list-panes` the sidebar already runs. That was this design's
assumption until the hardening landed, and it is wrong for a specific reason:
**the last slot has exactly one occupant, and `@wterm_label` has the better claim
to it.**

Layer 2 protects whichever unsanitized field is last. A second unsanitized field
in the middle of the record gets layers 1 and 3 only, and its failure mode is
worse than the label's ever was: a surplus separator at index *k* shifts every
field after it by one, the greedy last field absorbs the overflow, and the row
**parses successfully with the wrong values in it** — a pane wearing another
pane's title. `ParseRows` cannot detect that, because the report field itself
still looks like a report; only the fields behind it are wrong. "The sidebar
shows something other than the truth" is the failure `formatFields`' comment
gives as the reason paths are excluded entirely.

Giving the report the last slot instead and demoting the label is not a trade
worth making. A label is free-form text a human typed, where a stray byte is
plausible content; a report is machine-generated with a checked shape. The
free-form field is the one that needs the position.

**So the report is read by a second `list-panes`, with its own format string:**

```
tmux list-panes -a -F "#{pane_id}<Sep>#{s/[\n\x1f]/ /:@wterm_agent}"
```

joined to the snapshot by pane id. In that format the report is the last and only
variable field, so it gets all three layers; `#{pane_id}` is `%N` and cannot
carry anything. The properties that make this the right shape:

- **The blast radius is a report, never a pane.** A line that will not parse
  costs one pane its state. The identity snapshot — the thing the whole sidebar
  is built from — never sees the option at all.
- **It is one fork per poll, flat**, not one per pane. v2 already accepts one
  `capture-pane` fork *per agent pane* per poll and calls three agents "two forks
  a second"; this is cheaper than the thing it replaces, and it replaces it
  entirely for panes that report.
- **The two format strings stay independent.** `Format` is not touched by this
  design at all, which means the hardening's careful field ordering is not
  something a later feature can quietly break by appending to the list.

The join is a plain map from pane id: a report for a pane the snapshot does not
contain is dropped, and a pane with no line in the second call has no report. If
the second call fails outright, every pane falls back to the classifier for that
poll — the same degradation as a failed capture, which v2 already accepts as an
ordinary race rather than a fault.

## What the label says

This is the central choice, and the recommendation is **activity, not prompt**.

### The options considered

**The user's raw prompt.** Available and exact on all three: pi's `input.text`,
opencode's `chat.message`, claude's `UserPromptSubmit.prompt` (a string — *not*
`user_input.text`, which is what you will write first). Rejected as the default:

- It answers the wrong question. It is the same answer the frozen title gives,
  only fresher — "what was it asked" rather than "what is it doing now". Twenty
  minutes into a task, a prompt is history.
- It publishes the user's own words into a tmux option that anything able to
  talk to the tmux server can read, and then renders them in a web UI. The
  developer holds private client work in these panes. `pi-subagents` already
  refuses this for the same reason, in almost these words: *"raw prompts never
  enter pane metadata."*
- It is unbounded prose. Truncating a prompt to a sidebar row produces a
  sentence fragment; truncating a tool call produces a tool call.

**The current tool call, as verb plus object.** Available on pi
(`tool_execution_start` → `toolName` + `args`, verified) and on opencode (tool
events), and *probably* on Claude Code via `PreToolUse` — see the open question.
This is the recommendation, with a caveat below about arguments.

**opencode's `in_progress` todo.** opencode emits `todo.updated`, and the entry
marked `in_progress` is a short, human-readable description of the step the agent
is on, revised during the turn (verified). This is better than a tool call when
it exists: `Fix the parser bug` beats `read blocked.go`, because it is the
agent's own statement of intent rather than a mechanical trace.

It is also, honestly, closer to the prompt end of the scale than a tool name is —
it is model-generated text derived from the user's request, and it can echo the
request's nouns. Accepted anyway, because it is a *step* description rather than
the request, it is generated for display, and it is short by construction. This
is a judgement call and it is the one place where the privacy argument above is
weakened rather than honoured.

### The ladder

The label is the first of these that is available, per agent:

1. **The question**, when blocked. Exact from events on all three: pi's
   `ui_prompt_start.title` is the literal question; opencode's `permission.asked`
   carries the question plus `metadata`; claude's `Notification` carries
   `notification_type` ∈ `permission_prompt | idle_prompt`. This is what
   `paneText` already puts above everything else when the screen grammar reads a
   dialog, and an event-sourced question enters at the same rung.
2. **The current step**, when opencode reports one (`todo.updated`'s
   `in_progress` entry).
3. **The current tool call**, verb plus object.
4. **Nothing.** The report is cleared at turn end and the row falls back to the
   title, then to the command, exactly as it does today.

### The object of a tool call is not the whole argument

An unremarked hazard in "verb plus object": a tool's arguments are not a safe
thing to publish. `Bash`'s argument is a command line, which routinely contains
paths and occasionally contains a token. `Read`'s argument is an absolute path —
and v1 and v2 both deliberately keep `pane_current_path` out of the snapshot.

That exclusion was about *parsing* (a path can contain a `0x1f`), and our
sanitizer covers that. But widening a web UI from "no paths" to "every path the
agent touches" is a change of kind that nobody asked for. So:

- A path argument is reduced to its **basename**: `read snapshot.go`, not
  `read /home/dev/src/example-app/…/snapshot.go`.
- A shell command is reduced to its **first word**: `run go`, not the command
  line. (`run go test ./internal/tmux` in the example above is what the *pi*
  integration would produce from a structured `args` object; a raw `Bash` string
  gets the first word only.)
- Anything else is the tool name alone.

This is deliberately lossy. `read snapshot.go` is not enough to know which repo
the agent is in — and it does not have to be, because the window name and the
session name above it already say that. The second line describes; the first line
identifies.

### Claude Code starts without activity, and that is not a placeholder

Claude Code's state hooks are measured and complete: `UserPromptSubmit` →
working, `Notification` → blocked, `Stop` → idle, with `last_assistant_message`
available at the end. Its **activity** source is not established. `PreToolUse`
exists and would give verb plus object like the others, but nobody has measured
its payload or its cost, and herdr deleted theirs.

The obvious stopgap is to give claude the prompt, since `UserPromptSubmit`
already carries it and the hook is already registered. **Recommend against it**,
and this is a place where the brief for this document and the document disagree.
Publishing the user's prompt is the thing the previous section decided not to do;
doing it "only for claude, only until `PreToolUse` is measured" is how a
temporary exception becomes the contract, and it would ship the highest-volume
agent with the behaviour the design rejected. Claude ships **state-only**: a dot,
no second line, falling back to the title as it does today.

The middle path, if the owner wants it on their own machine: a config flag
(`--claude-label-from-prompt`, default off) that turns rung 4 into the prompt for
claude alone. The schema does not care where the text came from, so this is a
flag in the integration and nothing else. That is what "design so claude can gain
activity later without a redesign" actually requires — the schema is
source-agnostic, and every rung is a change to one integration file.

## State: precedence, and what happens when only one authority exists

Two authorities now exist for the same field: the event report in
`@wterm_agent`, and the churn classifier plus blocked grammars in
`state.go`/`blocked.go`.

**A fresh report wins.** It is exact, it names states the screen can only infer,
and it costs one flat fork per poll for every pane at once — against one fork per
agent pane for the captures it replaces. The classifier remains for every pane
without one.

The four cases:

| Report | Screen | Result |
| --- | --- | --- |
| fresh | (not taken) | The report. The capture is skipped, so this pane costs zero forks |
| stale or absent | available | The classifier, exactly as v2 |
| stale or absent | not available (no client connected) | Empty, exactly as v2 |
| fresh | — | The report, **even with no client connected** |

That last row supersedes v2's rule that `AgentState` is empty whenever no browser
holds a terminal socket. v2's reasoning was that "stale state presented as
current is worse than none", and it was right *about a hash*: the classifier's
memory is a claim that nothing has changed since we last looked, and we stopped
looking. A report is different in kind. It is a fact the agent published and tmux
is still holding, and reading it does not scale with the number of agents — one
fork per poll, in the same class as the `list-panes` and the `ServerStart` the
poller already runs unconditionally. The freshness rules below are what keep it
from becoming the thing v2 feared.

### The capture is skipped while a report is fresh, and that is the win

Two authorities running in parallel can only agree — in which case the second one
was cost — or disagree, in which case we have already decided which wins. So a
fresh report suppresses the capture entirely: an agent pane with a working
integration is a pane tmux-web never forks `capture-pane` for.

The residual is that skipping the capture also skips `IsBlocked`, which is the
only thing that catches an approval box an integration failed to report. All
three agents *do* report blocking by event, so this only bites when the
integration is wrong or dead — and then the report ages out and the classifier
takes over and finds the box. **The failure mode is "blocked is late by the
freshness window", not "blocked is missed."** That is the property the window is
chosen against.

### One exception: a reported `blocked` keeps capturing

A false `blocked` is the failure v2 says destroys the feature, and a report is
the one way to get one that the screen grammars cannot: an agent whose
integration died while a dialog was up, whose dialog the user then answered in
the terminal, keeps reporting `blocked` forever while it happily works.

So while a report says `blocked` **and** a client is connected, the pane is
captured anyway, and a capture whose hash *changed* is positive evidence that the
agent is running — which drops the report and hands the pane back to the
classifier. This does not violate the house rule that only a positive match sets
`blocked`; it is the mirror of it. We are not guessing at a state, we are
checking a claim against evidence, and only evidence overturns it.

### Working expires; blocked and idle do not

The freshness rule is not a single TTL, because the three states make different
claims.

- **`working` is transient.** It asserts something is happening *now* and must be
  re-asserted. It expires after **60 seconds**.
- **`blocked` and `idle` are resting.** The agent said it has stopped, and by
  definition nothing further will happen until the user acts. There is nothing to
  re-assert. They do not expire on a clock.

This dissolves the tension that makes a single TTL unpickable. A short TTL is
what you want for `working` — a crashed agent must not show as busy — and it is
exactly wrong for `blocked`, where the user is away, no client is connected, and
the badge that says "this one needs you" is the only thing the app is for.

60 seconds is a guess and is flagged as one: nobody has measured the distribution
of gaps between events during real work. The bias is deliberate. A `working`
report that expires too early falls back to the classifier, which for a genuinely
working agent says `working` — the same answer, one fork more expensive. A
`working` report that expires too late is a lie on the sidebar. Cheap failure on
one side, expensive on the other, so err short.

Resting states are instead cleared by three things that are exact:

1. **A later report.** The normal path: every agent has a turn-end event
   (`agent_settled`, `session.idle`, `Stop`) and every integration clears on it.
2. **The pane's command.** If `pane_current_command` is no longer a known agent,
   the agent exited and the report is dropped unconditionally. This catches the
   crash case, which is also the case a clock was supposed to catch.
3. **The pane dying**, which takes the option with it. Free.

Plus the positive-evidence override above for `blocked`, and a manual
`tmux set -p -u @wterm_agent` for anyone stuck.

**No heartbeats.** Nothing inside an agent runs on a timer to keep a report warm.
pi and opencode integrations live in the agent's own event loop and a timer there
is a thing we do not control; claude's hooks are one process per event and cannot
hold one. The command check and the resting/transient split exist so that no
heartbeat is needed.

### `finishedAt`, and the switch between authorities

`finishedAt` still lives on the server and `done` still lives in the browser, per
v2. Two changes:

**A reported working→idle edge stamps `finishedAt` from the report's own
timestamp**, not from `time.Now()`. The report says when the agent finished; the
poll can be up to 1.5s later, and the browser's `seen` comparison is against a
moment the user cares about.

**A change of authority is a first sight and stamps nothing.** This is subtle and
it is the shape of bug this project keeps producing. Install an integration
mid-run: the classifier has been reporting `working` from churn, the integration
comes up and reports `idle` at the end of the turn — and if those two are treated
as one run, the edge stamps normally and every device lights up. Worse in the
other direction: an integration dies mid-turn, the report ages out, the
classifier starts from nothing and reports `working`, then settles two polls
later and stamps an edge for a run it never saw start. v2 has `everChanged` for
exactly this class; this is the same guard applied to the authority instead of to
the hash. An edge stamps only when the previous state and the current state came
from the same authority.

## What crosses the wire

`Row` gains two fields:

```go
// Activity is what the agent's own integration says it is doing. "" when no
// integration is installed, when its report is stale, and for every pane that
// is not an agent. Sanitised and capped on write and again on read.
Activity string `json:"activity"`

// StateSource is which authority decided AgentState: "event", "screen", or ""
// when nothing did.
StateSource string `json:"stateSource"`
```

`AgentState`, `FinishedAt` and `Question` are unchanged in shape. `Question` may
now be filled from an event rather than from `ExtractQuestion`, which is
invisible to the frontend and intended to be.

**`StateSource` is on the wire mainly so that tests can see it.** v2 learned this
the hard way with `finishedAt`: the blocked override made the row read `blocked`
whether or not an edge had been stamped underneath, so a test asserting on the
state alone could not see the bug at all. Precedence has the same property — a
report and the classifier agreeing on `working` is indistinguishable from the
precedence being backwards — and a test that cannot distinguish them is a test
that will stay green through the rewrite that breaks it.

The UI may use it for a tooltip ("reported by the agent" / "from the screen") and
must **not** branch the row's appearance on it. Two visibly different kinds of
state dot teaches the user to trust one and ignore the other, which is the badge
integrity failure arriving through a third door.

### The row

`paneText`'s ladder becomes: **question → user label → activity → title →
command**.

The user's label stays above the activity for v2's reason — a label is a name the
user gave this pane on purpose, and it wins over programs. But that leaves a
labelled pane with no activity readout, which is a real loss, and the fix is a
layout change rather than a precedence change: **when both exist, the label joins
the window name on the first line and the activity keeps the second.** The label
is identity, the activity is description, and `PaneLines` already has exactly
that split — it is what the capsule-versus-second-line decision is about. This is
a change to `PaneLines`, not only to `paneText`, and should be planned as one.

## One binary, three integrations

The security-critical part of this feature is the sanitizer, and the naive shape
of the feature implements it three times: once in TypeScript for pi, once in
JavaScript for opencode, once in something for claude. Three implementations of
one security boundary, drifting apart, is not a thing to ship.

**So all three integrations shell out to a new `wterm-web report` subcommand.**
It reads the agent's hook payload on stdin (or takes flags), decides the state,
sanitizes the text, and performs one `tmux set-option`. The integration files are
then ~30 lines of "map this event to these arguments", which is what makes them
short enough for a user to read before trusting them — and reviewability is the
whole of the trust model here.

Consequences worth stating:

- **`report` talks to tmux, never to the daemon.** No socket, no auth, no
  dependency on tmux-web running. The state lives in the pane, so a daemon
  restart loses nothing — which is the one thing the churn classifier could never
  manage, since v2 has to reset its whole map on restart to avoid a badge storm.
- **The server is addressed as `tmux -S "${TMUX%%,*}"`.** `$TMUX`'s first
  comma-separated field is the socket path, and a bare `tmux` can reach a
  different server than the one the agent is running inside. The pane is
  `-t "$TMUX_PANE"`, which held on all three agents: it is present in every
  integration's environment and identifies the right pane. There is no
  alternative for Claude Code in particular — a hook's stdin is a socket and it
  has no controlling tty, so no tty-derived pane id is available.
- **`report` exits 0. Always.** A nonzero exit from a Claude Code `PreToolUse`
  hook can block the tool call. A reporting integration that can stop the agent
  from working is worse than no reporting integration, so every failure — no
  `$TMUX`, no `$TMUX_PANE`, tmux missing, pane gone, unparseable stdin — is a
  silent no-op with status 0. Running an agent outside tmux is not an error, it
  is a no-op.
- **The binary's path is resolved at install time** and written into the
  generated integration file, with a `PATH` lookup as fallback. If it is missing
  at run time, the integration does nothing and says nothing.

## Writes must never block

Every one of these events is delivered inside something the agent is waiting on,
and all three agents were measured:

- **pi** awaits handlers with **no timeout**. A 3s stall in `input` delayed the
  turn by 3.03s; 4s in `tool_execution_start` delayed the tool result by 4.00s.
  There is no `hookTimeout` setting.
- **opencode** awaits hooks **sequentially across plugins**, also with no
  timeout. 4s of stall with two plugins registered delayed `session.status busy`
  to 8956ms.
- **Claude Code** waits and adds the full hook duration to the turn: baseline
  2771ms became 7735ms with a `sleep 5`. Unless `"async": true`, which is
  undocumented in the offline reference but present in 2.1.250 and measured at
  2251ms.

So: **fire and forget, everywhere.** Spawn, do not await. The spawn itself is a
few milliseconds and that is the budget.

**A single-slot queue in pi and opencode**, adopting herdr's shape: at most one
`report` in flight per pane, and if a new state arrives while one is running,
keep only the latest and drop what it replaced. A burst of tool calls must not
become a queue of forks. Both of those runtimes are long-lived and can hold the
slot in module scope.

**Claude Code gets no queue**, because each hook is a fresh process and there is
nowhere to put one. It gets a *small hook set* instead — the four measured events
and nothing else — which is why the unmeasured cost of a per-tool-call
`PreToolUse` is a genuine open question rather than an implementation detail.

`"async": true` is used where available and **not depended on**. It is
undocumented, which means it can vanish in a version bump without anyone
announcing it, so the hook has to be fast enough that async is an optimisation.
A `set-option` fork is; anything that waits on the network is not, which is
another reason `report` never talks to the daemon.

## Subagents leak into hooks on every agent

Verified on all three, and it is the same bug each time: `TMUX_PANE` is the same
for the root session and its children, so an unfiltered child event overwrites
the root's state. The specific damage is a child's turn-end firing while the root
is still working — an idle report on a busy agent, which becomes a `done` badge,
which is the failure v2 spent most of its complexity avoiding, reintroduced
through a new door.

**A report must only ever describe the root session in its pane.** The guards
differ per agent and each one is the agent's own:

- **opencode**: child sessions arrive in every hook. Track
  `properties.info.parentID` and report only for the root.
- **claude**: guard on `agent_id` being present in the hook input. `Stop` is
  root-only, but `SubagentStop` exists and must not be registered.
- **pi**: gate on `ctx.mode !== "tui"` in `session_start`. This is herdr's guard
  and their comment gives the reason: RPC reports `hasUI=true`, so `mode` is the
  reliable discriminator and `hasUI` is not.

One more pi-specific detail worth carrying over: pi re-derives activity as
`ctx.isIdle() === false` on `session_start`, because a reload can replace the
extension mid-run without another `agent_start`. An extension that only ever sets
state on transitions comes back from a reload believing nothing is happening.

## Sanitization and bounds

In `wterm-web report`, in this order, before the value reaches tmux:

1. **Strip whole escape sequences** — CSI, OSC and friends — as sequences. Doing
   this by removing the ESC byte alone leaves the literal `[31m` behind in the
   text, which is a thing that has already been observed. Sequence first, byte
   second.
2. **Drop every remaining control character.** C0, DEL, and the C1 range
   (U+0080–U+009F) — the last because tmux's own checks elsewhere are
   byte-oriented and let C1 through, which `validateLabel` already had to learn.
   This is what removes the `0x1f` and the newline.
3. **Collapse whitespace runs to one space, and trim.**
4. **Truncate on a rune boundary** — `truncateAtRuneBoundary`'s reason:
   `s[:max]` halves a multi-byte rune and puts invalid UTF-8 on the wire, and
   whether it does depends on where the runes land, so the naive version is right
   most of the time.
5. **Empty after all of that means clear**, `set -p -u`, not a write of `""`.
   herdr's rule, adopted for their reason: a label that normalises away is not a
   label.

The daemon re-runs 2 and 4 on what it reads, on the standing assumption that a
writer's promise is not a guarantee — anything that can talk to the tmux socket
can set this option, which is the same uid that can already drive tmux directly.
It is the third of the same three layers the label now has: the tmux-side
substitution takes the two record-breaking bytes, the report's position as the
only variable field in its own format string bounds what a survivor could do, and
this repairs everything neither of those is a promise about — C1 controls, a lone
`0x7f`, invalid UTF-8, and length.

`sanitizeLabel` is the model and should be reused rather than paralleled: the
one difference is that a report is dropped where a label is repaired. A label
that sanitises to nothing is an unlabelled pane, which is a legitimate thing to
be. A report that sanitises to nothing failed its shape check on the way past,
and there is no such thing as a partially trustworthy state.

**The cap is 128 runes**, matching `MaxLabel`.

- Not herdr's **80**: that number is a column budget for their row, and copying a
  magic number is copying the answer to a different question. Ours is the second
  line of a `PaneLines`, and `MaxLabel` is the cap that was chosen for exactly
  that line.
- Not `MaxTitle`'s **256**: a title is at least normalised by tmux's OSC parser
  before we see it, and this is not. A smaller cap on the less trustworthy field
  is the right way round.
- Runes, not bytes, for `MaxLabel`'s stated reason: the cap is a column budget,
  and a byte cap cuts a readable string in half in any language that is not
  English.

Separately, the **whole option value is capped at 1 KiB on read**, before
parsing. The rune cap only applies to a field we have successfully parsed out,
and nothing stops a hostile writer from storing a megabyte that the daemon would
then carry through every 1.5s poll. Over the cap, the report is discarded.

## Trust and installation

These files execute inside the user's agents, with the user's credentials, in the
user's client repositories. The rules follow from that.

**Nothing is installed implicitly.** tmux-web never writes into a project
directory as a side effect of polling, of the user opening the sidebar, or of
noticing an agent that has no integration. There is no "we detected claude, shall
we…" prompt in the web UI at all.

**Installation is a CLI act**: `wterm-web install-integration --agent
claude|opencode|pi [dir]`. It prints the exact paths it will write and requires
confirmation unless `--yes`. Not a button in the web UI, because the web UI is
reachable over the network from a phone, and "write executable code into a repo"
is not a thing a network request should be able to do however well authenticated
it is. The Origin middleware is the boundary for tmux operations; this is not a
tmux operation.

**What goes where**, and one of these is not like the others:

| Agent | Path | Kind |
| --- | --- | --- |
| pi | `<proj>/.pi/extensions/wterm.ts` | A file we own. TypeScript via jiti, no build step. Needs project trust |
| opencode | `<proj>/.opencode/plugin/wterm.js` | A file we own. Auto-loaded, no config entry needed |
| claude | `<proj>/.claude/settings.json` | **A file the user owns**, merged into |

The claude case is different in kind and the installer must treat it that way: it
refuses if the file is not valid JSON, never rewrites the whole file, adds only
entries whose command is recognisably ours, and removes exactly those on
uninstall. A user's own hooks in that file are not ours to reformat.

**Every file we own carries a managed header**, adopting herdr's practice with
our own wording, plus a schema line:

```
// managed by tmux-web (wterm-schema: 1)
// Reinstalling or updating the integration overwrites this file.
// It does one thing: `tmux set-option -p @wterm_agent`. Nothing else.
```

The schema line lets `install` tell three cases apart: ours and current
(overwrite silently), ours and outdated (overwrite, say so), and **not ours**
(refuse, and say which file). The last case is the one the header exists for.

**Uninstall is `wterm-web install-integration --remove`**, and for the two
file-based agents `rm` also works and is documented as working. An integration
you cannot remove with `rm` is an integration you have to trust more than this
one deserves.

**What the integration is permitted to do** is one thing: spawn `wterm-web
report`. No network, no filesystem writes, no reading the repository, no
dependencies. Small enough to read in a minute — which, again, is the trust
model, not a nicety.

**Scope.** pi and opencode are project-local by mechanism, so an install is per
project, and pi additionally requires the project to be trusted. Claude Code
hooks can also live in `~/.claude/settings.json`, so `--global` is offered for
claude. opencode may have a global plugin directory
(`~/.config/opencode/plugin/`); that is **unverified here** and must not be
implemented on the strength of this sentence.

**One consequence to warn about at install time:** opencode auto-creates
`.opencode/package.json`, `node_modules/` and a `.gitignore` in the project on
first plugin load. Installing into a client repository therefore adds files that
repository did not have, and a `.gitignore` tmux-web did not write and does not
control. The installer says so before writing, because "why is there a
`.gitignore` in my client's repo" is a question the user should be able to answer
without archaeology.

## Why herdr deleted their Claude hooks (speculation)

Their `HOOK_REMOVALS` list is the wreckage of a hook-based Claude state machine
they removed in favour of screen detection. Only one reason is actually attested;
the rest is inference and is marked as such.

**Attested**, quoted in the v2 design from their own docs: Claude Code's hooks
"do not cover the whole lifecycle" and can miss permission results and
interrupts. A state machine whose only authority is hooks desynchronises
permanently the first time an event does not arrive, and there is nothing to put
it back.

**Speculation**, in descending order of how plausible it looks:

- **Cost.** A hook on every tool call adds its latency to every tool call, and
  before `"async": true` existed there was no way around that. 2771ms → 7735ms
  for a `sleep 5` is what that tax looks like at the extreme.
- **Coverage of the tool cycle.** `PreToolUse` fires when a tool starts and
  nothing fires when it finishes, so a state machine built on it needs
  `PostToolUse` as well — double the hooks and double the cost, for one signal.
- **Churn in a file they do not own.** Every hook is an entry in the user's
  `settings.json`. Four entries to remove cleanly is a smaller promise than
  eight.

**Why this design is less exposed to the attested reason than theirs was:** their
integration is the state authority, so a desync is permanent. Ours is one of two,
with a screen classifier underneath it and a freshness rule above it. A missed
event ages the report out and the classifier resumes; the failure is *late*, not
*wrong*. That is a real difference and it is the argument for trying `PreToolUse`
rather than inheriting their conclusion — but it is an argument for **measuring**
it, not for assuming it.

There is also a happy accident waiting there. Under this design `PreToolUse`'s
state contribution is only "still working", which is precisely the thing the
60-second `working` window wants re-asserted. Claude's activity source and
claude's keepalive would be the same hook.

## Rejected: TOML manifests for the screen rules

herdr's screen rules are declarative TOML, one file per agent, versioned with
`min_engine_version`. Replacing `blocked.go`'s hardcoded Go with the same idea is
tempting and is rejected, for four reasons.

**The two things are not the same size.** herdr's pi rule is
`contains = ["Working..."]` — a substring test that answers *state*.
`blocked.go` holds three structurally different grammars behind a `dialog`
interface, each of which also **extracts the question and its choices**. The
comment there records why they cannot be one table: over the three captured
screens, no two agents share a structural signal (claude has rules and numbered
choices, opencode has a gutter and a header line, pi has box corners and a
selector), and "bending any of them through machinery written for another would
mean loosening that machinery until it fit". Expressing that in TOML means either
dropping extraction or inventing a configuration language capable of three
grammars — a config language nobody asked for, in which the strictness that makes
the feature trustworthy is now user-editable.

**A `contains` rule is the loose kind v2 already rejected.** `Permission
required` typed into an input box already produces a false blocked in one
carefully-argued case; a substring matcher would produce a class of them.

**The screen path is becoming the fallback.** Investing in making it data-driven
is investing in the path this design exists to stop relying on. If it ever
becomes worth it, the reason will be "we support eight agents now", and that is a
better time to design the format.

**Licence.** herdr is Apache-2.0 and this repo is MIT. This is a reading of the
licences and not legal advice, but: the *idea* of declarative detection rules is
not copyrightable and can be reimplemented freely — this design already borrows
their managed header, their empty-means-clear rule, their single-slot queue and
their `min_engine_version` concept, and credits them. Their **files** are another
matter. Vendoring `distribution/agent-detection/*.toml` would bring Apache-2.0
material into an MIT repository: permitted, but it requires shipping their
LICENSE and NOTICE, marking modifications, and accepting that the repo is no
longer uniformly MIT. That is a real cost for a set of substring rules we could
write ourselves in an afternoon. So: **credit in prose, never a copied file** —
which is what the v2 design already does, and this document continues.

## Failure modes

| Case | Handling |
| --- | --- |
| No integration installed anywhere | Nothing changes. v2's behaviour exactly, which is the point of keeping the classifier |
| Integration installed, agent crashes mid-turn | `working` expires after 60s; the command check drops the report as soon as the pane is no longer running an agent. With a client connected the classifier takes over and agrees |
| Integration dies while its agent lives, mid-turn | The report ages out and the classifier resumes. State is *late*, not wrong. The authority switch stamps no `finishedAt` |
| Integration dies while the agent is `blocked`, user then answers in the terminal | The capture keeps running for reported-blocked panes; a changed hash is positive evidence the agent is running and drops the report |
| A subagent's turn-end event | Filtered per agent (`parentID`, `agent_id`, `ctx.mode`). Unfiltered, it is a false `done` on a working agent — v2's central failure through a new door |
| A report containing `0x1f` or a newline | Impossible from our writer, which strips control characters. From a hostile writer, tmux substitutes both to spaces before Go sees them, and the report is the only variable field in its own format string, so a survivor costs one report and never a pane |
| A second, unsanitized field appended to `Format` by a later feature | The thing this design deliberately does not do. `Format`'s last slot is `@wterm_label`'s and the hardening's comments say why; the report reads through its own format string instead |
| A report with an unknown schema version | Ignored whole. The classifier decides, as if no integration were installed |
| A report from the future, or with an unparseable timestamp | Treated as stale. A report we cannot date is a report we cannot age |
| An integration installed mid-run | The first report is a first sight. No `finishedAt` edge, so no badge storm |
| Daemon restart | Reports are unaffected: they live in tmux, not in the daemon's memory. This is strictly better than v2, which has to reset the whole classifier map |
| tmux server restart | The options die with the panes. No generation-keyed state to reset, unlike the classifier's map |
| `wterm-web` binary missing or moved after install | The integration spawns nothing and says nothing. The pane falls back to the classifier |
| A hook that would fail | `report` exits 0 unconditionally. A `PreToolUse` hook exiting nonzero can block the tool call, and a reporting feature that can stop an agent working is worse than no reporting feature |
| Two integrations installed for one agent (a stale herdr asset, say) | Both write their own option; ours is `@wterm_agent` and theirs is not. No collision. A second *tmux-web* integration in a parent directory is a real risk for opencode and pi and is not handled — see the open questions |

## Testing

Following v1 and v2:

- **Golden tests for the sanitizer**, as data: the `\x1b[31m` → `[31m` trap, a
  `0x1f`, a bare newline, a C1 control, invalid UTF-8, a multi-byte rune
  straddling the cap, a 10 KiB value, and a value that normalises to empty.
- **Table tests for precedence**, over report present/absent × fresh/stale ×
  screen available/unavailable, asserting `AgentState`, `StateSource` **and**
  `FinishedAt` together. Asserting the state alone cannot see a precedence bug at
  all, for the same reason v2's blocked override hid `finishedAt`.
- **A regression test that an authority switch stamps no `finishedAt` edge**, in
  both directions.
- **A regression test that a hostile `@wterm_agent` cannot remove a pane from the
  snapshot**, sibling to `TestSnapshotHostileLabelCannotRemoveAPane`. It should
  pass trivially, because the option is not in `Format` — and it is worth having
  precisely so that the day somebody appends it there, this goes red.
- **A test that the report's format string round-trips a value containing a raw
  `0x1f` and a newline as spaces**, against a real server. The `[[:cntrl:]]`
  finding is the reason: a substitution pattern can fail by expanding to `""` for
  every value, which no hostile-input test would catch. Assert on a *benign*
  value surviving intact, not only on a hostile one being cleaned.
- **Integration tests against real tmux** for the option round-trip: set, read,
  unset, and the empty-versus-unset equivalence. On an isolated socket via
  `internal/tmux/testutil`, whose `Args()` includes `-f /dev/null`.
- **Recorded hook payloads as fixtures**, one file per agent per event, including
  a subagent payload for each. These do not exist yet and capturing them is part
  of implementation, not of this design.
- **What cannot be tested in CI**: that a real agent, running a real integration,
  produces the events we mapped. That is a manual check per agent per upgrade,
  and the honest mitigation is that a wrong mapping degrades to no report, which
  degrades to v2.

## Out of scope

Reporting to anything other than the local tmux server. Cross-machine
aggregation. Integrations for agents not on the `Agents` list. Anything that
makes tmux-web able to *drive* an agent rather than observe it — the reporting
channel is one-way by construction, and a two-way one is a different design with
a different threat model.

## Open questions, requiring measurement before a plan can be written

1. **Claude Code's `PreToolUse`.** The blocking one. Its payload shape (does
   `tool_input` give a usable object per tool, or a blob?), its per-call cost
   with and without `"async": true`, and whether `"async"` is present in the
   versions we intend to support at all — it is undocumented in the offline
   reference and measured only in 2.1.250. Until this is measured, claude ships
   state-only.
2. **The 60-second window for a transient `working` report.** Needs the
   distribution of inter-event gaps during real work on all three agents. The
   number is currently a guess biased short on the argument that early expiry is
   cheap; that argument should be checked against a long `Bash` tool call with no
   client connected.
3. ~~Whether tmux can strip control bytes at read time.~~ **Answered by the
   hardening while this was being written**, and left here because the answer is
   a trap rather than a yes: `#{s/[\n\x1f]/ /:var}` works with a bracket set of
   the two literal bytes, `[[:cntrl:]]` blanks every value including good ones,
   and a byte range is locale-dependent. Recorded in "The hazard" so the next
   person does not re-derive it the expensive way.
4. **opencode's global plugin directory.** Whether `~/.config/opencode/plugin/`
   is auto-loaded. If it is, `--global` becomes available for opencode and the
   `.gitignore` consequence stops applying to client repos.
5. **How often opencode's todo ladder rung is empty.** Whether `todo.updated`
   fires for sessions that never use todos decides whether the tool-call rung is
   the common case or the rare one — and therefore how much the argument-reduction
   rules matter.
6. **What pi's `tool_execution_start.args` actually contains**, per tool. The
   basename and first-word reductions are designed against a guess about its
   shape; they need checking against the real thing, particularly for tools that
   take structured input.
7. **Nested project installs.** opencode and pi load project-local files; it is
   not established what happens when a repository contains an installed
   integration and a working directory below it does too, or whether a parent
   directory's plugin is loaded at all. Two integrations writing one pane option
   is a race nobody has looked at.
