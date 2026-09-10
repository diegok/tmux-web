# tmux-web v3 — agent-side reporting

Date: 2026-09-10
Status: design, not agreed. Revision 2, rewritten against an adversarial review
that found three broken compositions and several errors of fact; see "Revision 2".
Follows: `2026-09-10-tmux-web-v2-design.md` (v2), which it partly supersedes.
Depends on: the hardening of `internal/tmux/snapshot.go` against hostile
`@wterm_label` values, **committed as `f25e066`** — in history, not pending, and
not a thing this design is waiting on. It is a prerequisite, it answers one of
the questions this document opened with, and it constrains the transport more
than expected — see "The hazard" and "Why the report is not a fourteenth field".

## Revision 2

Revision 1 was reviewed adversarially, and its three worst faults were
*compositions*: each section read correctly on its own, and the per-section
tests each one implied would all have passed green over the contradiction. That
is the failure mode this document should be most afraid of, so the changes are
listed rather than quietly folded in.

- **Claude's `Notification` is a whitelist now, not a synonym for `blocked`.**
  Revision 1 mapped every `Notification` to `blocked`, believing
  `notification_type` to be `permission_prompt | idle_prompt`. It is twelve
  documented values and the list is not documented as closed. `idle_prompt` in
  particular fires about sixty seconds after a turn ends — so revision 1 turned
  every idle Claude pane into a permanent "needs you" badge one minute after
  every turn: a *resting* state on a *static* screen, which the changed-hash
  escape hatch can never clear. See "Which event means which state".
- **Turn end writes an idle report; it does not clear the option.** Revision 1
  said "cleared" in two places, "empty means unset the option" in a third, and
  then stamped `finishedAt` from a report the other three sentences had just
  deleted. Under revision 1's own sanitizer rule Claude — which ships
  state-only — would have unset the option on every event and never reported at
  all. See "Turn end is a report".
- **`finishedAt` from a report is derived statelessly**, so it does survive a
  daemon restart, and the first-sight guard stays where the authority has no
  clock: on the classifier. Revision 1 claimed both and could have had neither.
- **A reported `idle` gets an evidence backstop too.** Revision 1 gave one to
  `blocked` alone and defended `idle` — the failure it calls central — with
  per-agent filters on fragile discriminators.
- **A reported `blocked` gets a second evidence rule**, for the settled screen
  with no dialog on it. Revision 1's failure table claimed that case as handled
  when only the churning sub-case was.
- **The two format reads are batched into one tmux invocation**, verified on
  3.7b, so the cost claim is true from the first poll rather than once some
  pane reports.
- **A trailing `;` does not survive `set-option`** — measured, 3.7b, and it lands
  exactly on the state-only report. See "The hazard".
- Errors of fact corrected: `"async": true` is documented; only exit code `2`
  blocks a `PreToolUse` call; `PreToolUse`'s `tool_input` is a documented
  structured per-tool object; a sentence was attributed to v2 that is a comment
  in `AppSidebar.tsx`; `MaxTitle` is 256 *bytes*; the example timestamp was in
  2025.

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
*clear*. We adopt the shape of this and not the number — and we adopt the
empty-means-clear rule **scoped to the text field**, which is a distinction
their design does not have to make because their state and their message are
separate fields on a wire protocol, where ours share one option value. Revision 1
copied the rule without the scope and thereby unset the option on every
state-only report. See "Sanitization and bounds" and "Turn end is a report".

## What is being transported

### Not `@wterm_label`

The obvious transport is the option that already exists. `@wterm_label` is read
by every poll, is already in `Format`, and already outranks the title in
`paneText`. An integration would need one line.

**It is the wrong option, and the reason is in v2.** `@wterm_label` is *the
user's* field: `PATCH /api/panes/{id}` writes it, and v2's "Rows" section is
explicit that "an agent pane shows its label if set, otherwise its title". The
sharpest statement of *why* is not in v2 at all — it is the comment on
`paneText` at `web/src/components/AppSidebar.tsx:843-845`, which says the label
"wins outright, including over a title a program is rewriting underneath it --
that is the whole point of having one." Revision 1 quoted that sentence as v2's;
it is the code's. The substance is v2's, the wording is the comment's.
An integration writing it puts a program back
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
@wterm_agent = "1;working;1789075200000;running go test ./internal/tmux"
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

**Three fields is a valid report, and it is the common one.** A state-only
report has no text, and it is written `1;idle;1789075200000` — with no trailing
separator, so `SplitN` returns three parts and the text is `""`. That is not a
stylistic choice: **tmux strips a trailing `;` from an option value** (measured
on 3.7b, and `--` does not prevent it, because the `;` is eaten by tmux's own
command parser rather than by the shell). A writer that emits `1;idle;<ts>;`
gets `1;idle;<ts>` stored anyway, so a reader that demands four parts would
reject every state-only report — which, since Claude ships state-only, is every
Claude report. The writer omits the separator and the reader accepts three parts
or four. See "The hazard" for the rest of that measurement.

**A timestamp more than a few seconds in the future fails the shape check** and
the report is discarded whole, rather than being treated as stale. Stale is the
right answer for a `working` report and the wrong one for a resting one: a
resting `idle` stamps `finishedAt` from its own timestamp, and a `finishedAt`
far in the future is a `done` badge the browser's `seen` marker can never catch
up with. A few seconds of tolerance covers the clock jitter available on a
single machine, which is the only machine involved.

The leading version is herdr's `min_engine_version` idea moved to where our
version skew actually lives. Their rules ship with the daemon and have to
announce what engine they need; ours are written by an integration that the user
installed once and may not have updated since, into an option read by a daemon
that may be newer or older. A daemon that does not understand version `2` ignores
the report and falls back to the screen, which is the correct degradation and
costs nothing.

**Unset and empty are the same thing.** `#{@wterm_agent}` renders both an unset
option and one set to `""` as the empty string, so the daemon cannot tell them
apart and does not try: an empty *value* means no report. Note the scope of that
sentence — it is about the whole option value, not about the text field inside
one. `1;idle;<ts>` has an empty text field and is a perfectly good report;
revision 1 conflated the two and thereby deleted the Claude integration. See
"Turn end is a report".

Writers therefore never write `""`. A writer with nothing to say does not write.
`set -p -u` is reserved for teardown — see "What `clear` is for".

### The hazard

**tmux does not sanitize user option values.** Verified: both `0x1f` — the
snapshot format's field separator — and a bare newline survive verbatim into
`list-panes -F` output. A `0x1f` adds a field; a newline splits the record in
two. Either way the record is not the record we meant, and the pane can
disappear from the sidebar, which the code's own comments call the worst failure
this project has.

The hardening is committed — `f25e066`, "a hostile pane label can no longer
delete a pane from the sidebar" — and it gives `@wterm_label` three layers,
verified against a real 3.7b server rather than assumed. They are worth
restating because the report needs the same treatment and cannot have all of it:

1. **tmux substitutes the two bytes out before Go sees them.**
   `#{s/[\n\x1f]/ /:@wterm_label}`, with a literal `0x1f` inside the bracket set.
   It replaces with a *space* rather than nothing, so `EV\x1fIL` reads as `EV IL`
   — visibly tampered with, rather than as a label somebody chose.
2. **The field is last**, so a byte that survives layer 1 is bounded by
   arithmetic rather than by layer 1 holding: a surplus separator only adds
   fields past the end, which `ParseRows` rejoins into the label, and a newline
   only truncates the last field, leaving the twelve before it — the whole
   identity of the pane — already complete on the line. `fieldCount` (13) became
   a **minimum** rather than an equality to make that work, and `ParseRows`
   repairs what arrives rather than rejecting it: surplus fields are rejoined
   into the label, control runes become spaces, invalid UTF-8 becomes U+FFFD.
3. **`sanitizeLabel` in Go**, which is `validateLabel`'s rules applied instead of
   refused: control runes to spaces, invalid UTF-8 to U+FFFD, `MaxLabel` runes,
   trimmed — so one notion of a safe label rather than two. `MaxLabel` is 128
   runes (`= MaxSessionName`, "because the rows are the same width").

One structural detail of `f25e066` that this design leans on: the format string
is no longer a literal. It is `var Format = strings.Join(formatFields, Sep)`,
built from an ordered `formatFields` slice whose last element is the label, with
a test asserting both the count and that the label is last. That is what makes
"do not append a fourteenth field" a rule a test can hold, rather than a comment
someone reads.

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

### A trailing `;` does not survive `set-option`

New in revision 2, measured on 3.7b against an isolated socket, because the
report's own separator is `;` and this lands squarely on it:

```
set -p @wterm_agent "a;"    → show-options: a          (stripped)
set -p @wterm_agent "a;;"   → show-options: "a;"       (one of the two stripped)
set -p @wterm_agent "a\;"   → show-options: "a;"       (escaping works)
set -p @wterm_agent ";"     → error "empty value", and the option keeps its
                              PREVIOUS value
```

`--` does not help; the `;` is consumed by tmux's own command parser, which
reads it as a command separator, and `report` will be exec'ing tmux with an argv
rather than going through a shell, so there is no quoting layer that could have
protected it. Three consequences, all of them design decisions rather than
trivia:

- **The writer never emits a value ending in a bare `;`.** A state-only report
  is `1;idle;<ts>`, not `1;idle;<ts>;`. Escaping as `\;` would also work and is
  rejected: it puts a shell-shaped escape into a value that is not going through
  a shell, and the next person to read it will not know whether the backslash is
  data.
- **The reader accepts three parts as well as four.** This is the one that
  matters, because the state-only report is not an edge case — it is every
  Claude report and every turn-end report from all three agents.
- **A value of exactly `;` is refused by tmux and leaves the previous value
  standing.** So a botched write does not clear a report, it *preserves a stale
  one*, which is the more dangerous of the two outcomes and another reason the
  writer's shape is checked before the write rather than after it.

A trailing `;` inside the *text* field is stripped by the same rule
(`…;run go;` stores as `…;run go`). That is harmless — it costs a semicolon off
the end of a display string — and it is noted so nobody spends an afternoon on
it.

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

**So the report is read by a second `list-panes`, with its own format string —
and both reads ride one tmux invocation.** Revision 1 proposed a second fork;
the review pointed out that it forks unconditionally every 1.5s for every user
forever and replaces nothing until some pane reports fresh, so it is only cheaper
than the captures *once reports exist*. tmux will run both commands in one
invocation, which makes that objection disappear rather than answering it.
Verified on 3.7b:

```
tmux list-panes -a -F "S<Sep>#{pane_id}<Sep>…twelve more…<Sep>#{label}" \
   \; list-panes -a -F "A<Sep>#{pane_id}<Sep>#{s/[\n\x1f]/ /:@wterm_agent}"
```

One fork, two blocks of output in command order, told apart by a literal tag
field (`S` for snapshot, `A` for agent report) rather than by counting fields or
by trusting the order. The tag is a constant in the format string, so nothing a
writer controls can forge one: a report containing a newline would have to
survive layer 1 first, and layer 1 turns it into a space.

In the report format the option is still the last and only variable field, so it
gets all three layers; `#{pane_id}` is `%N` and cannot carry anything. The
properties that make this the right shape:

- **The blast radius is a report, never a pane.** A line that will not parse
  costs one pane its state. The identity block — the thing the whole sidebar is
  built from — never contains the option at all.
- **The marginal cost is zero forks.** Not "one flat fork", which is what
  revision 1 claimed and was fair to challenge: it is one more command inside a
  fork the poller already makes unconditionally, and a slightly longer stdout to
  scan. Against that it removes one `capture-pane` fork *per reporting agent
  pane* per poll. There is no poll at which this feature costs a fork it did not
  save.
- **The two format strings stay independent.** `Format` is not touched by this
  design at all, which means the hardening's careful field ordering is not
  something a later feature can quietly break by appending to the list.

**This is also the decision about the third read.** The roadmap wants
`pane_current_path` for git context, which is the other value v1 and v2
deliberately keep out of `Format` because a path can contain a `0x1f`. Deciding
batching now rather than accreting forks means the rule for it is already
written: **one fork, N format strings, each with at most one unsanitized field
and that field last, each block tagged.** A third read appends `\; list-panes -a
-F "P<Sep>#{pane_id}<Sep>#{s/…:pane_current_path}"` and costs nothing.

**The cost of batching, stated honestly:** the sidebar's identity snapshot now
shares a process with a feature that is not the sidebar. A malformed report
format string could take the whole invocation down, where two forks would have
failed independently. Three things bound that:

- The commands run in the order given and the snapshot is first, measured. A
  failure in the second command still leaves the first command's complete output
  on stdout — measured by forcing one with a bogus target: the snapshot block
  printed, then the error, then exit 1.
- **So the daemon parses stdout on its own terms and does not gate on the exit
  status.** A nonzero exit with a complete snapshot block is a missing *report*,
  not a missing snapshot. This has to be written down because "check the error
  first" is the reflex, and here the reflex would trade a degraded feature for a
  blank sidebar.
- Neither command takes a target, so the realistic failure is "the tmux server
  is gone", which fails both reads either way and which the poller already
  handles.

The join is a plain map from pane id: a report for a pane the snapshot does not
contain is dropped. **Every pane gets a line in the report block** — a pane with
no integration yields a line whose report field is empty, which is the same
thing as no report (verified: `A<Sep>%1<Sep>` for an unset option). Revision 1
said "a pane with no line in the second call has no report", which described a
case that does not occur. If the report block is missing entirely, every pane
falls back to the classifier for that poll — the same degradation as a failed
capture, which v2 already accepts as an ordinary race rather than a fault.

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

1. **The question**, when blocked. Exact from events on pi and opencode: pi's
   `ui_prompt_start.title` is the literal question; opencode's `permission.asked`
   carries the question plus `metadata`. Claude's `Notification` carries a
   `notification_type` and a message, and *only some of those types are a
   question* — see the next section, which revision 1 got badly wrong. This is
   what `paneText` already puts above everything else when the screen grammar
   reads a dialog, and an event-sourced question enters at the same rung.
2. **The current step**, when opencode reports one (`todo.updated`'s
   `in_progress` entry).
3. **The current tool call**, verb plus object.
4. **Nothing — an empty text field, not an absent report.** At turn end the
   integration reports `idle` with no text, the row falls back to the title and
   then to the command exactly as it does today, and the *state* is still the
   agent's own. Revision 1 said the report is "cleared" here, which contradicted
   two other sections and broke `finishedAt`. See "Turn end is a report".

### Which event means which state, and what an unknown event means

Revision 1 gave the three agents' turn-start and turn-end events and then, for
Claude, treated `Notification` as a synonym for `blocked`. That was wrong, and
wrong in the direction that destroys the badge.

pi and opencode are unchanged and are simple, because their events say what they
mean:

| Agent | working | blocked | idle |
| --- | --- | --- | --- |
| pi | `tool_execution_start` | `ui_prompt_start` | `agent_settled` |
| opencode | tool events, `session.status busy` | `permission.asked` | `session.idle` |
| claude | `UserPromptSubmit`, `PreToolUse` | some `Notification`s | `Stop` |

**Claude's `Notification` is a whitelist on `notification_type`.** The
documented values, and what each one reports:

| `notification_type` | Reports | Why |
| --- | --- | --- |
| `permission_prompt` | `blocked` | "Claude needs you to approve a tool use or a sandboxed command's network request, and the prompt has waited about six seconds" |
| `elicitation_dialog` | `blocked` | an MCP server has opened a form and is waiting on the user |
| `elicitation_url_dialog` | `blocked` | an MCP server is asking the user to open a URL |
| `agent_needs_input` | `blocked` | a background session or a teammate setup question is waiting on the user |
| `quota_auto_resume_stale` | `blocked` | "waits for you to press Enter instead of continuing" — the user has to act |
| `idle_prompt` | `idle` | "Claude finished responding about 60 seconds ago and you haven't typed since" |
| `quota_auto_resume_disabled` | `idle` | the wait ended and the task was not continued |
| `quota_auto_resume_fired` | `working` | Claude Code is continuing the task |
| `auth_success` | *ignored* | not a state of the session |
| `elicitation_complete` | *ignored* | bookkeeping between Claude and an MCP server |
| `elicitation_response` | *ignored* | as above |
| `agent_completed` | *ignored* | "a background session finishes or fails" — **not** this session's turn end. Reported as `idle` it is a false `done` badge on a working agent, which is v2's central failure arriving through yet another door |
| **anything else** | ***ignored*** | the default, and it matters more than the table |

**The default is the decision here, not the enumeration.** The list above is
what the docs currently carry; it is not documented as closed, it has plainly
grown before, and it will grow again. So an unrecognised `notification_type`
**writes nothing at all** — it does not change the state, it does not refresh the
timestamp, it is not an error. The asymmetry that settles this:

- Ignoring a notification that *should* have meant `blocked` costs a late
  badge. The screen grammar is still running (a fresh `blocked` report is not in
  play, so the pane is captured normally), and positively matching a dialog on
  screen is the one thing v2's classifier is genuinely good at.
- Defaulting an unknown notification to `blocked` costs a *permanent false
  badge*. `blocked` is a resting state, so it never expires; if the screen is
  static, the changed-hash escape hatch never fires either.

`idle_prompt` deserves its own note, because it is the one that would have
destroyed the feature. It fires about sixty seconds after a turn ends, on a pane
that `Stop` has already reported idle. Mapping it to `idle` therefore makes it a
no-op re-assertion in the normal case — and a **repair** in the abnormal one:
the attested reason herdr gave up on Claude hooks is that they "do not cover the
whole lifecycle" and events can be missed, and a missed `Stop` leaves a pane
stuck on `working` until the 60s expiry hands it to the classifier. `idle_prompt`
puts it back, from the agent, exactly once. An event that was revision 1's worst
bug turns out to be a small piece of the answer to herdr's objection.

Two consequences worth stating rather than discovering:

- **Claude's event-sourced `blocked` is about six seconds late by
  construction**, because `permission_prompt` waits about six seconds before
  firing. That is four polls. It is acceptable, and it is bounded, because it
  fails towards the screen: during those six seconds the pane's last report is a
  fresh `working` from `PreToolUse`, so the badge is late rather than wrong.
  It is also an argument against ever raising the `working` window on the theory
  that "the event will arrive".
- **`agent_needs_input` is the one whitelist entry that can describe something
  other than the root session in the pane.** It is kept, because "something in
  this pane wants your input" is true and is what the badge means, and because
  both evidence rules below still apply to it.

The whitelist lives in `wterm-web report`, in Go, not in the hook configuration —
which is the point of having one binary. It is a table, it is testable, and when
the list grows it grows in one file rather than in three integrations and a
`settings.json` the user owns.

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

### Claude Code's activity, re-argued on true premises

Revision 1 shipped Claude **state-only** — a dot, no second line — and rested
that on two factual planks. The review found both false, so the decision has to
be re-argued rather than restated. It comes out **half the same**.

The planks, corrected:

- **`"async": true` is documented.** Revision 1 said twice that it is
  undocumented and open question 1 doubted it existed outside 2.1.250. It is in
  the command-hook schema (alongside `timeout`, defaulting to `false`), the docs
  recommend it for long-running hooks, and an async hook runs in the background
  and cannot block or control the tool call. For a reporting hook, "cannot block
  or control behaviour" is not a limitation, it is the entire specification.
  The "it could vanish in a version bump" argument is gone.
- **`PreToolUse`'s payload is documented, not unmeasured.** `tool_name` names the
  tool and `tool_input` is a structured per-tool object — `Bash` carries
  `command`, `Read`/`Edit`/`Write` carry `file_path`. That is exactly the verb
  plus object the ladder wants, in exactly the shape the reduction rules were
  designed against. (The docs do not enumerate `tool_input` for *every* tool;
  that costs nothing, because a tool whose field names we do not recognise falls
  to "the tool name alone", which is already rung 3's last case.)
- **Only exit code `2` blocks a `PreToolUse` call**, absent a JSON
  `permissionDecision`. Revision 1 said "a nonzero exit can block the tool call".
  Every other nonzero code is a non-blocking error and the action proceeds. The
  hazard of a reporting hook stopping an agent from working was therefore
  narrower than revision 1 claimed.

**What survives unchanged: the prompt is still not the label.** That decision
never rested on either plank. It rests on the three arguments in "The options
considered" — a prompt answers "what was it asked", it publishes the user's own
words about private client work into an option any process on the box can read,
and it truncates into a sentence fragment — plus the argument that "only for
claude, only until `PreToolUse` is measured" is how a temporary exception becomes
the contract. All four still hold. `UserPromptSubmit` remains a *state* hook for
Claude and its `prompt` field is not published.

**What does not survive: deferring `PreToolUse`.** Revision 1 held it back
because "nobody has measured its payload or its cost" and because async might not
exist. The payload is documented and async is documented, so what is left is a
single number — per-call cost, and specifically fork *volume* under a burst
rather than turn *latency*, since async takes the latency off the critical path
by construction. That is a measurement, not an unknown design. So:

**Claude ships with activity from `PreToolUse`, registered with `"async": true`,
with state-only as the specified fallback** if the volume measurement comes out
badly. The fallback is not a redesign — it is deleting one hook entry from the
generated settings block, and every other Claude behaviour in this document is
unchanged by it.

This also collects the "happy accident" noted further down: `PreToolUse`'s state
contribution is "still working", which is exactly what the 60-second window wants
re-asserted. Claude's activity source and Claude's keepalive are the same hook,
so the volume being measured buys two things rather than one.

The `--claude-label-from-prompt` flag stays as described — default off, a flag in
the integration and nothing else — for an owner who wants it on their own
machine. It is now what it always should have been: an escape hatch, not a
stopgap waiting on a measurement.

## State: precedence, and what happens when only one authority exists

Two authorities now exist for the same field: the event report in
`@wterm_agent`, and the churn classifier plus blocked grammars in
`state.go`/`blocked.go`.

**A fresh report wins.** It is exact, it names states the screen can only infer,
and it is read for every pane at once inside a fork the poller already makes —
against one fork per agent pane for the captures it replaces. The classifier
remains for every pane without one.

The cases:

| Report | Screen | Result |
| --- | --- | --- |
| fresh `working` | (not taken) | The report. The capture is skipped, so this pane costs no forks at all |
| fresh `idle`, outside the verification window | (not taken) | The report. Capture skipped |
| fresh `idle`, inside the verification window | captured | The report, unless the classifier says `working` — see evidence rule 3. `N` polls, then the capture stops |
| fresh `blocked` | captured | The report, unless the screen churns or settles with no dialog on it — evidence rules 1 and 2. Captured for as long as the report stands |
| stale or absent | available | The classifier, exactly as v2 |
| stale or absent | not available (no client connected) | Empty, exactly as v2 |
| fresh, any state | no client connected | The report, **even with no client connected**. Nothing to verify against, so nothing is verified |

That last row supersedes v2's rule that `AgentState` is empty whenever no browser
holds a terminal socket. v2's reasoning was that "stale state presented as
current is worse than none", and it was right *about a hash*: the classifier's
memory is a claim that nothing has changed since we last looked, and we stopped
looking. A report is different in kind. It is a fact the agent published and tmux
is still holding, and reading it does not scale with the number of agents — it
is one more command inside the `list-panes` the poller already runs
unconditionally. The freshness rules below are what keep it
from becoming the thing v2 feared.

### The capture is skipped while a report is fresh, and that is the win

Two authorities running in parallel can only agree — in which case the second one
was cost — or disagree, in which case we have already decided which wins. So a
fresh report suppresses the capture entirely: an agent pane with a working
integration is a pane tmux-web mostly never forks `capture-pane` for. "Mostly",
because the two evidence rules below buy some of it back on purpose.

The residual is that skipping the capture also skips `IsBlocked`, which is the
only thing that catches an approval box an integration failed to report. All
three agents *do* report blocking by event, so **the failure mode is "blocked is
late by the freshness window", not "blocked is missed."** That is the property
the window is chosen against.

Revision 1 then said this only bites "when the integration is wrong or dead",
and that was incomplete. There is a third cause and it is ordinary: **scheduling
jitter**. The writes are fire-and-forget, so a `working` write from an earlier
`PreToolUse` can land *after* a `blocked` write from a later `Notification`, and
a fresh `working` would then suppress the capture for up to 60 seconds on a pane
that is sitting at a dialog. That is not a broken integration, it is two
processes racing. It is answered by an ordering rule rather than by an evidence
rule; see "Ordering" below.

### Reports are checked against the screen — both resting states, not just one

Revision 1 gave `blocked` an evidence backstop and gave `idle` none, defending a
reported `idle` with per-agent filters alone. The review's objection is correct
and is worth stating in full, because it is the strongest argument in the whole
review: a false `idle` is the failure this project calls central, the filters
that prevent it (`ctx.mode`, `parentID`, `agent_id`) are fragile discriminators
on payloads we do not control, and given this project's history at least one of
them will be plausibly wrong and test green. A reported `idle` whose pane keeps
churning is the same contradiction that earned `blocked` its override. It gets
the same treatment.

The general rule, which is what revision 1 was missing: **a resting report makes
a claim about the screen, and while the screen is available the claim is
checked.** We are never guessing at a state — the house rule that only a positive
match sets `blocked` is untouched — we are checking a claim against evidence, and
only evidence overturns it.

Three rules, all of them requiring a connected client, because with no client
there is no screen to check and the report is the only authority there is:

1. **A reported `blocked` on a churning screen is dropped.** A capture whose hash
   changed is positive evidence the agent is running. This is revision 1's rule,
   unchanged. It covers the integration that died at a dialog the user then
   answered, on a pane that visibly resumed work.

2. **A reported `blocked` on a settled screen with no dialog on it is dropped**,
   after `N` consecutive polls agree. *New in revision 2.* Revision 1's failure
   table claimed the died-at-a-dialog case as handled when only the churning
   sub-case was. The real sequence is the one this app exists for: the
   integration dies at a dialog, the user answers at the terminal with **no
   client connected**, the agent finishes before anyone reconnects. On reconnect
   the first capture is the baseline, nothing ever changes again, and rule 1 —
   which needs a *changed* hash — never fires. `blocked` rests forever on a
   finished agent, which is the overnight-and-phone case, which is the case the
   badge is for.

   The evidence here is the *absence* of a dialog on a screen that has stopped
   moving, judged by the same `blocked.go` grammars that decide it everywhere
   else. `N` polls rather than one because the first capture after a reconnect
   can catch a repaint mid-frame and the grammars are deliberately strict.

3. **A reported `idle` on a churning screen is dropped**, during a verification
   window of `N` polls after the report arrives. *New in revision 2.* Within
   that window the pane is still captured, and if the **classifier** — the same
   code, the same hash, the same rules — says `working`, the idle report is
   contradicted, dropped, and the pane goes back to the classifier with no
   `finishedAt` derived. That is the unfiltered-subagent case caught by evidence
   rather than by a discriminator holding.

`N = 3` (about 4.5s). It is a guess of the same kind as the 60-second window and
is flagged as one.

**Why `idle` gets a window and `blocked` does not.** This is the asymmetry the
review asked to see argued or removed, and it is real:

- **Verify the claim for as long as the claim stands.** `blocked` rests
  indefinitely — it is true until the user acts, which may be tomorrow — so it
  is checked for as long as it is reported.
- **`idle`'s damage is concentrated at one moment.** A false `idle` on the
  sidebar is self-correcting: a genuinely working agent emits another `working`
  at its next tool call, usually seconds later. What is *not* self-correcting is
  the `finishedAt` stamp, because a `done` badge that has landed on three
  devices does not un-land. So the window covers exactly the moment the stamp is
  derived, and stops.
- **The cost is bounded and shaped correctly.** Rule 3 costs three captures per
  turn end on a reporting pane, against one capture per poll forever without the
  integration. Rule 2 costs captures only while a `blocked` report stands, which
  is the state where the user is waiting anyway.

**What rule 2 costs, said plainly:** an agent genuinely blocked at a dialog whose
*screen form* no grammar in `blocked.go` matches loses its reported `blocked`
after `N` polls, and the classifier — which also cannot see the dialog — reads a
static screen as idle. That is a downgrade of a true `blocked` to `idle`. It is
bounded to dialogs we have not captured, which is the same set v2 already fails
on for every non-integrated pane, and it gives `blocked.go` a second reason to be
kept current as the agents' dialogs change. It is the price of not letting a
dead integration pin a badge forever, and that trade goes this way.

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
of gaps between events during real work. Revision 1's bias argument was that
early expiry is cheap, and **that argument rests on an assumption revision 1 did
not mark**:

> *Assumption.* "A `working` report that expires too early falls back to the
> classifier, which for a genuinely working agent says `working`." That holds
> **only while the screen churns.** v2's own caveat is that a quiet agent reads
> idle — and a quiet agent is precisely the long silent tool call that outlives
> the 60-second window with nothing to re-assert.

So early expiry is not free. Its true cost is a false `idle` on the sidebar, not
one extra fork. Two things keep it from being worse than that, and they are the
reason the bias still stands, just less comfortably:

- **It cannot produce a false badge.** Report → classifier is a change of
  authority, and an authority change stamps no `finishedAt` (below). The row is
  wrong; the badge is not. This is the first-sight rule earning its keep in a
  case it was not designed for.
- **The keepalive is the same hook as the activity.** For Claude, `PreToolUse`
  re-asserts `working` at every tool call; pi and opencode emit tool events. The
  gap is the *single* long tool call with no sub-events, which is a narrower
  target than "any quiet minute" and is exactly what open question 2 should be
  measured against.

Resting states are ended by three things that are exact:

1. **A later report.** The normal path: every agent has a turn-end event
   (`agent_settled`, `session.idle`, `Stop`) and every integration reports
   **`idle`** on it — it supersedes the previous report, it does not unset the
   option. See below.
2. **The pane's command.** If `pane_current_command` is no longer a known agent,
   the agent exited and the report is dropped unconditionally. This catches the
   crash case, which is also the case a clock was supposed to catch.
3. **The pane dying**, which takes the option with it. Free.

Plus the evidence rules above, and a manual `tmux set -p -u @wterm_agent` for
anyone stuck.

### No heartbeats — and the reason is not the one revision 1 gave

Nothing inside an agent runs on a timer to keep a report warm. Revision 1's
reason was that "pi and opencode integrations live in the agent's own event loop
and a timer there is a thing we do not control", and that is simply false: a pi
extension or an opencode plugin can hold a `setInterval` in module scope
trivially, which is the same place the single-slot queue lives. Right
conclusion, wrong reason.

**The real reason is that a heartbeat proves the timer is alive, not that the
agent is.** A hung agent's `setInterval` keeps re-asserting `working` forever,
which converts the 60-second bound on a lie into an unbounded one. That is the
wrong direction for a design whose whole freshness argument is "the failure mode
is *late*, not *wrong*" — a heartbeat trades one for the other, and trades the
cheap failure for the expensive one.

Two supporting reasons, neither of them load-bearing on its own:

- **It would be inconsistent across agents.** Claude's hooks are one process per
  event and genuinely cannot hold a timer, so a heartbeat would exist for two of
  the three integrations and the freshness rule would still have to work without
  it.
- **It is unnecessary.** The command check drops a report the moment the pane is
  no longer running an agent, and the resting/transient split means the only
  state that needs re-assertion is the one the agent naturally re-asserts by
  doing more work.

### Turn end is a report, not a clear

This is the composition revision 1 broke, and it is worth naming the four
sentences that could not all be true, because each one read correctly alone and
each one implied a test that would have passed:

- ladder rung 4: "the report is **cleared** at turn end";
- resting-state rule 1: "every integration **clears** on it";
- `finishedAt` "stamps from the report's own timestamp" on a reported
  working→idle edge — which requires turn end to **write an idle report**, not to
  delete one;
- sanitization step 5: "empty after all of that means clear, `set -p -u`" —
  which, read literally and applied to the whole value, means Claude (state-only,
  therefore empty text on every event) unsets the option every time and never
  reports at all.

The resolution, in one sentence: **turn end writes `1;idle;<ts>`, and the option
is never unset as part of normal operation.**

- Rung 4 is "no *text*", not "no report". The row falls back to the title; the
  state dot is still the agent's own.
- Resting-state rule 1 is "a later report supersedes an earlier one", which is
  what it always meant mechanically.
- Sanitization step 5 applies to **the text field**, not to the value. An empty
  text field produces a three-part value. See "The value" and the trailing-`;`
  measurement, which is what makes the three-part form the written one.
- `finishedAt` has a report to derive from, which is what it needed all along.

### What `clear` is for

`set -p -u @wterm_agent` — a genuine unset — is reserved for teardown, and
nothing in the normal event path performs one:

- **The user's escape hatch.** A stuck report is one command away, documented.
- **An integration's own shutdown**, where the agent provides a shutdown event.
  This is a nicety: the daemon does not need it, because the command check drops
  the report the moment the pane is no longer running an agent, and pane death
  takes the option with it.
- **Nothing else.** In particular not turn end, not an empty label, and not
  "state unchanged". A writer with nothing to say does not write.

The daemon never distinguishes unset from empty and does not need to. Both mean
"no report", and both are ordinary rather than exceptional.

### Ordering: the daemon refuses a report older than the one it accepted

Fire-and-forget with no queue means writes can land out of order. The single-slot
queue bounds this on pi and opencode; Claude, where every hook is a fresh
process, has no queue and no way to have one. So the ordering has to be enforced
by the reader, and the timestamp is already in the value:

**The daemon keeps, per pane, the timestamp of the last report it accepted, and
refuses any report whose timestamp is not strictly newer.** A stale `working`
landing after a fresh `blocked` is read once, refused, and the pane keeps
reporting `blocked` until a genuinely newer report appears.

Three details that make this work rather than merely sound right:

- **The timestamp is the event's, not the write's.** `report` stamps from the
  moment it starts — or from an event timestamp in the payload where one exists
  — never from when the `set-option` returned. Stamping at write time would
  reintroduce exactly the reordering this rule exists to undo.
- **The per-pane timestamp is a filter, not state to be restored.** On a daemon
  restart it is empty and the first report read is accepted, whatever it is. That
  is correct: whatever is in the option is the last write that landed, and the
  daemon has no better information.
- **The cost is one bounded window of wrongness after a restart.** If the
  out-of-order `working` was the last write to land, a restarted daemon accepts
  it and shows `working` on a blocked pane until the next event — bounded by the
  60-second expiry, after which the classifier finds the dialog. That is the
  ordinary degradation, not a new failure.

The single-slot queue interacts with this correctly by construction: when a
queued report is dropped in favour of a newer one, the newer one carries the
newer event's timestamp, so collapsing a burst never resurrects an older state.

### `finishedAt`, and the switch between authorities

`finishedAt` still lives on the server and `done` still lives in the browser, per
v2: each browser keeps `seen[paneId]` in `localStorage` and badges when
`finishedAt > seen`.

Revision 1 tried to have this both ways. It claimed reports survive a daemon
restart "strictly better than v2", *and* it derived `finishedAt` from a
working→idle **edge**, which needs daemon memory of the previous report — *and*
it ruled that a change of authority is a first sight that stamps nothing. Those
three cannot hold together: after a restart the persisted idle report is a first
sight, stamps no edge, and the `done` badge does not survive after all. The
review was right to make this pick one.

**It picks stateless derivation, and it does not cost what the review expected it
to.** The rules:

- **From a report: `finishedAt` is derived, not stamped.** A pane whose current
  accepted report is a resting `idle` has `finishedAt` equal to *that report's
  own timestamp*. No memory of a previous report, no edge, no daemon state. Read
  the option, get the answer. It survives a daemon restart, a poller restart, and
  a `wterm-web` upgrade, because the fact lives in tmux.
- **From the classifier: unchanged from v2**, `everChanged` and all. That
  authority has no clock — its only way to date a finish is `time.Now()` at the
  moment it first noticed — so first sight must stamp nothing there, or every
  restart is a badge storm on every pane that happens to be sitting still.
- **The authority switch guard stays, on the side that needs it.** Classifier →
  report needs no guard: the report is timestamped, so it can date its own
  finish. Report → classifier keeps the guard, because that is the direction
  where a timestamp-less authority starts from nothing — an integration dies
  mid-turn, the report ages out, the classifier reports `working` from churn and
  settles two polls later, and without the guard it would stamp an edge for a run
  it never saw start.

**Why the mid-install badge storm does not happen.** The review's objection was
that stateless derivation means installing an integration mid-run stamps a badge
storm, which is what the first-sight rule exists to prevent. It does not, and the
reason is the difference between the two authorities: **a report carries the time
the agent actually finished, and the classifier can only carry the time we
noticed.** The classifier's first sight of a pane that has been sitting idle
since yesterday invents `finishedAt = now`, which beats every browser's `seen`
and lights up every device — a fiction. A report's first sight says "this agent
finished at 21:20", and the browser badges only if that device has not looked at
that pane since 21:20, which is exactly what the badge is supposed to mean. The
`seen[paneId]` marker is persisted in `localStorage` and keyed by the server
generation, so it is still there to answer.

**What it does cost, stated rather than waved at:**

- A mid-run install produces **one** badge per reporting pane whose first idle
  report is newer than that device's `seen` for it — and each of those badges
  names a true fact, that the agent has finished and this device has not looked.
  It is one-shot, it is bounded by the number of panes with an integration, and
  it is not the unbounded fiction the classifier's first sight would produce.
- A device that has cleared its browser storage sees badges for every reporting
  pane once, which is already v2's documented behaviour for `seen`.
- The derivation trusts the report's clock. That is the same machine's clock, so
  skew is not a real threat — but a *future* timestamp would produce a
  `finishedAt` that `seen` can never catch, so a report dated more than a few
  seconds ahead fails its shape check and is discarded whole. See "The value".
- The idle verification window (rule 3 above) **suppresses the derivation while
  it is pending**, rather than stamping and retracting. With no client connected
  there is nothing to verify and the derivation is immediate — which is the case
  the app exists for, so it is the right way round. On a restart mid-window the
  report is re-derived immediately, which is the safe direction: a report that
  arrived before the restart is almost certainly a real turn end.

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
integrity failure arriving through a third door. A prohibition with no test is a
prohibition that gets violated silently, so this one is named in "Testing": a
component test that renders the same row twice, once per `StateSource`, and
asserts the rendered class lists are equal.

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
- **`report` exits 0. Always.** Revision 1's premise was that "a nonzero exit
  from a Claude Code `PreToolUse` hook can block the tool call". The accurate
  version: **exit code `2` blocks it** (absent a JSON `permissionDecision` on
  stdout); every other nonzero code is a non-blocking error and the action
  proceeds. So the sharp edge is one specific value, not the whole nonzero
  range. Exiting 0 unconditionally is kept anyway, and the reason survives the
  correction: the distance between `exit 1` and `exit 2` is one character in a
  wrapper nobody will re-read, a reporting integration that can stop an agent
  from working is worse than no reporting integration, and belt-and-braces here
  costs nothing. Every failure — no `$TMUX`, no `$TMUX_PANE`, tmux missing, pane
  gone, unparseable stdin, an unrecognised `notification_type` — is a silent
  no-op with status 0. Running an agent outside tmux is not an error, it is a
  no-op.
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
  2771ms became 7735ms with a `sleep 5`. Unless `"async": true`, which is a
  **documented** field of the command-hook schema — it runs the hook in the
  background, and an async hook cannot block or control behaviour. Measured at
  2251ms, i.e. no measurable tax. Revision 1 called it undocumented twice and
  built an argument on that; the argument is withdrawn below.

So: **fire and forget, everywhere.** Spawn, do not await. The spawn itself is a
few milliseconds and that is the budget.

**A single-slot queue in pi and opencode**, adopting herdr's shape: at most one
`report` in flight per pane, and if a new state arrives while one is running,
keep only the latest and drop what it replaced. A burst of tool calls must not
become a queue of forks. Both of those runtimes are long-lived and can hold the
slot in module scope.

**Claude Code gets no queue**, because each hook is a fresh process and there is
nowhere to put one. It gets a *small hook set* instead — `UserPromptSubmit`,
`PreToolUse`, `Notification`, `Stop`, and nothing else — and the reader-side
ordering rule ("Ordering") is what stands in for the queue it cannot have. The
residual cost of a per-tool-call `PreToolUse` is fork volume under a burst, which
is the one thing open question 1 still asks for.

`"async": true` is used, and the hook is still fast enough that async is an
**optimisation rather than a requirement**. Revision 1 justified that belt and
braces with "it is undocumented and can vanish in a version bump", which was
false and is withdrawn. The correct justification is narrower and still good: an
async hook is a hook whose failure nobody watches, one installed integration file
can outlive several agent versions, and a `set-option` fork is fast enough that
we never have to find out. Anything that waits on the network is not, which is
another reason `report` never talks to the daemon.

The one thing async does not buy is fork *volume*. An async `PreToolUse` takes
the latency off the turn, but a burst of tool calls is still a burst of
processes — one hook plus one tmux client each. That is the residue of open
question 1, and it is the whole of it.

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
- **claude**: `agent_id` and `agent_type` are documented common hook-input
  fields, carrying a subagent id and name. Report only when `agent_id` is
  **absent**. `Stop` is root-only, but `SubagentStop` exists and must not be
  registered.
- **pi**: gate on `ctx.mode !== "tui"` in `session_start`. This is herdr's guard
  and their comment gives the reason: RPC reports `hasUI=true`, so `mode` is the
  reliable discriminator and `hasUI` is not.

**Every one of these filters fails closed.** This is the rule that answers the
review's objection that these are fragile discriminators on payloads we do not
control, and that at least one of them will be plausibly wrong and test green:
when the discriminator is missing, unexpected, or a shape the integration does
not recognise, the integration **does not report**. It never falls through to
"probably the root". The cost of failing closed is a missing report, which ages
out into the classifier — v2's behaviour, which is the floor this whole design
degrades to. The cost of failing open is a false `idle` on a working agent,
which is a `done` badge on every device. The docs do not enumerate which events
carry `agent_id`, which is exactly why the guard has to be "absent means root,
anything else means silence" rather than a positive test for a subagent.

Failing closed is a *filter*, not a *backstop*, and it is not the only defence:
evidence rule 3 above catches a reported `idle` on a churning pane whichever
filter let it through. Two independent mechanisms, because one filter on an
undocumented field is not a thing to bet the badge on.

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
5. **Empty after all of that means an empty *text field*** — not an empty value,
   and not an unset option. herdr's rule is adopted for their reason (a label
   that normalises away is not a label) and its *scope* is corrected: it applies
   to the fourth field, not to the whole thing. The value written is
   `1;<state>;<ts>` with no trailing separator, which is a valid three-part
   report carrying the state and no text.

   Revision 1 said "empty means clear, `set -p -u`". Applied to the whole value —
   which is how it reads — that unsets the option on every state-only report,
   which is every Claude report and every turn-end report from all three agents.
   The integration would have deleted its own state on every event and never
   reported at all, and a unit test of the sanitizer in isolation would have been
   perfectly green about it.

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
- Not `MaxTitle`'s **256 bytes**: a title is at least normalised by tmux's OSC
  parser before we see it, and this is not. A smaller cap on the less
  trustworthy field is the right way round. Note the units differ and that is
  deliberate rather than sloppy — `MaxTitle` is a *byte* cap applied by
  `truncateAtRuneBoundary`, `MaxLabel` is a *rune* cap; see the next bullet for
  why the report follows the label.
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

**One consequence to warn about at install time** (**measured**, not inferred —
unlike the global-plugin-directory sentence above it, which is explicitly not):
opencode auto-creates `.opencode/package.json`, `node_modules/` and a
`.gitignore` in the project on first plugin load. Installing into a client
repository therefore adds files that repository did not have, and a
`.gitignore` tmux-web did not write and does not control. The installer says so
before writing, because "why is there a `.gitignore` in my client's repo" is a
question the user should be able to answer without archaeology.

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
  for a `sleep 5` is what that tax looks like at the extreme. Note the tense:
  async is documented and available now, so this reason is largely historical,
  which weakens the case for inheriting their conclusion rather than
  strengthening it.
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

There is also a happy accident, and revision 2 banks it. Under this design
`PreToolUse`'s state contribution is only "still working", which is precisely the
thing the 60-second `working` window wants re-asserted. Claude's activity source
and Claude's keepalive are the same hook, so the one measurement still
outstanding buys both.

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
LICENSE — and their NOTICE, if they have one; Apache-2.0 only obliges you to
carry a NOTICE file that exists — marking modifications, and accepting that the
repo is no longer uniformly MIT. That is a real cost for a set of substring rules we could
write ourselves in an afternoon. So: **credit in prose, never a copied file** —
which is what the v2 design already does, and this document continues.

## Failure modes

| Case | Handling |
| --- | --- |
| No integration installed anywhere | Nothing changes. v2's behaviour exactly, which is the point of keeping the classifier |
| Integration installed, agent crashes mid-turn | `working` expires after 60s; the command check drops the report as soon as the pane is no longer running an agent. With a client connected the classifier takes over and agrees |
| Integration dies while its agent lives, mid-turn | The report ages out and the classifier resumes. State is *late*, not wrong. The authority switch stamps no `finishedAt` |
| Integration dies while the agent is `blocked`, user then answers in the terminal, agent resumes visibly | Evidence rule 1: the capture keeps running for reported-blocked panes, and a changed hash is positive evidence the agent is running, which drops the report |
| The same, but with **no client connected**, and the agent finishes before anyone reconnects | Evidence rule 2, new in revision 2. On reconnect the screen never changes, so rule 1 can never fire; a settled screen with no dialog grammar match over `N` polls drops the report. Revision 1 listed this row as handled when only the row above it was |
| An agent genuinely blocked at a dialog no `blocked.go` grammar matches | Rule 2 drops the report after `N` polls and the classifier reads the static screen as idle. A true `blocked` downgraded to `idle` — the accepted cost of rule 2, bounded to dialog forms we have not captured, which v2 already fails on for every non-integrated pane |
| A subagent's turn-end event | Two defences, deliberately independent. Filtered per agent (`parentID`, `agent_id`, `ctx.mode`), and every filter **fails closed** — an unrecognised payload reports nothing. If a filter is wrong anyway, evidence rule 3 catches a reported `idle` on a churning pane within the verification window and no `finishedAt` is derived |
| A `Notification` whose `notification_type` we do not recognise | Ignored. No write, no state change, no timestamp refresh. Revision 1 would have written `blocked` — a permanent false badge on a resting state |
| `Notification(idle_prompt)`, ~60s after every turn | Reports `idle`, which is a no-op re-assertion of what `Stop` already said, and a repair when `Stop` was missed. Revision 1 reported `blocked` here, which turned every idle Claude pane into a permanent "needs you" badge one minute after every turn |
| Two writes landing out of order (a delayed `working` after a `blocked`) | The daemon refuses a report whose timestamp is not strictly newer than the last it accepted. Ordinary scheduling jitter, not a broken integration, and revision 1 did not account for it |
| Claude and pi in the same pane (an agent run inside another agent's shell) | **Not handled.** Both integrations see the same `TMUX_PANE` and write the same option, and the last writer wins with no way to tell whose turn ended. See the open questions |
| A report containing `0x1f` or a newline | Impossible from our writer, which strips control characters. From a hostile writer, tmux substitutes both to spaces before Go sees them, and the report is the only variable field in its own format string, so a survivor costs one report and never a pane |
| A second, unsanitized field appended to `Format` by a later feature | The thing this design deliberately does not do. `Format`'s last slot is `@wterm_label`'s and the hardening's comments say why; the report reads through its own format string instead |
| A report with an unknown schema version | Ignored whole. The classifier decides, as if no integration were installed |
| A report with an unparseable timestamp | Discarded whole. A report we cannot date is a report we cannot age |
| A report dated in the future | Discarded whole if it is more than a few seconds ahead, rather than treated as stale. A resting `idle` derives `finishedAt` from its own timestamp, and a future `finishedAt` is a `done` badge that `seen` can never catch up with |
| An integration installed mid-run | The first report is a first sight. No `finishedAt` edge, so no badge storm |
| Daemon restart | Reports are unaffected: they live in tmux, not in the daemon's memory, and `finishedAt` is *derived* from a resting `idle` report rather than stamped on an edge — so a finished agent still badges after a restart. That is better than v2, which has to reset the whole classifier map, and revision 1 claimed it while deriving `finishedAt` in a way that could not deliver it. Cost: the ordering filter and the idle verification window are also empty on restart, so the first report read for each pane is accepted unverified |
| tmux server restart | The options die with the panes. No generation-keyed state to reset, unlike the classifier's map |
| `wterm-web` binary missing or moved after install | The integration spawns nothing and says nothing. The pane falls back to the classifier |
| A hook that would fail | `report` exits 0 unconditionally. Exit code `2` specifically blocks a `PreToolUse` call (revision 1 said "nonzero"), and a reporting feature that can stop an agent working is worse than no reporting feature, so the belt-and-braces stands on a corrected premise |
| The second `list-panes` in the batch fails | The daemon parses stdout on its own terms and does not gate on the exit status. Measured: the first command's output is complete on stdout before the error, so a failed report read is a missing report, never a blank sidebar |
| Two integrations installed for one agent (a stale herdr asset, say) | Both write their own option; ours is `@wterm_agent` and theirs is not. No collision. A second *tmux-web* integration in a parent directory is a real risk for opencode and pi and is not handled — see the open questions |
| A `set-option` value ending in a bare `;` | tmux strips it, and a value of exactly `;` is refused with "empty value" while the option keeps its previous contents. The writer never emits one and the reader accepts three parts as well as four |

## Testing

Following v1 and v2. Revision 2 adds most of the second half of this list, and
the reason is uncomfortable enough to write down: every one of revision 1's three
broken compositions would have passed a full suite of per-section tests. The
tests that catch a composition are the ones that assert two sections' outputs
*together*, and they have to be asked for by name.

- **Golden tests for the sanitizer**, as data: the `\x1b[31m` → `[31m` trap, a
  `0x1f`, a bare newline, a C1 control, invalid UTF-8, a multi-byte rune
  straddling the cap, a 10 KiB value, and a value whose *text* normalises to
  empty — which must produce a three-part state-only report, **not** an unset.
- **A round-trip for the state-only report**, `1;idle;<ts>`, against a real
  server: written, read back through the report format string, parsed, and
  yielding state `idle` with text `""`. This is the exact case revision 1's
  ambiguity destroyed, and it is also where the trailing-`;` measurement bites,
  so the test asserts on what `show-options` holds as well as on what the parser
  returns.
- **The `notification_type` whitelist as a table test**, one row per documented
  value plus at least two invented ones, asserting the reported state — and
  asserting that an unrecognised value produces **no write at all**, not a write
  of the previous state.
- **The `working` expiry as a pure function with an injected clock.** 60 seconds
  is a number that will be changed; it should be changeable by editing one
  constant and re-reading one table, with no sleeping and no real time anywhere
  near it.
- **The blocked-verification drops**, both of them: a changed hash drops a
  reported `blocked` (rule 1), and `N` consecutive settled captures with no
  dialog match drop one (rule 2) while `N-1` do not. The off-by-one is the
  point.
- **The idle verification window** (rule 3): a reported `idle` on a pane the
  classifier reads as `working` is dropped within the window and derives no
  `finishedAt`; the same report on a settled pane derives one; and with no client
  connected it derives one immediately, because there is nothing to verify.
- **The ordering filter**: a report older than the last accepted one is refused
  and the previously accepted state stands; a strictly newer one is taken; and
  after a simulated restart the first report read is accepted whatever its
  timestamp.
- **Table tests for precedence**, over report present/absent × fresh/stale ×
  screen available/unavailable, asserting `AgentState`, `StateSource` **and**
  `FinishedAt` together. Asserting the state alone cannot see a precedence bug at
  all, for the same reason v2's blocked override hid `finishedAt`.
- **`finishedAt` derivation, and specifically that it survives a restart.** A
  resting `idle` report yields `finishedAt` equal to its own timestamp with the
  daemon's per-pane memory empty — which is the assertion revision 1's failure
  table claimed and revision 1's mechanism could not have satisfied.
- **A regression test that a report → classifier authority switch stamps no
  `finishedAt` edge**, and that a classifier → report switch *does* produce one
  when the report is a resting `idle`. Both directions, because revision 2
  deliberately made them asymmetric and the next reader will assume they are not.
- **A regression test that a hostile `@wterm_agent` cannot remove a pane from the
  snapshot**, sibling to `TestSnapshotHostileLabelCannotRemoveAPane`. It should
  pass trivially, because the option is not in `Format` — and it is worth having
  precisely so that the day somebody appends it there, this goes red.
- **A test that the report's format string round-trips a value containing a raw
  `0x1f` and a newline as spaces**, against a real server. The `[[:cntrl:]]`
  finding is the reason: a substitution pattern can fail by expanding to `""` for
  every value, which no hostile-input test would catch. Assert on a *benign*
  value surviving intact, not only on a hostile one being cleaned.
- **A test for the batched invocation**: one tmux call, two tagged blocks, split
  correctly; every pane present in the report block including panes with the
  option unset; and a hostile report value leaving the snapshot block byte-intact.
- **Integration tests against real tmux** for the option round-trip: set, read,
  unset, and the empty-versus-unset equivalence. On an isolated socket via
  `internal/tmux/testutil`, whose `Args()` includes `-f /dev/null`.
- **The single-slot queue, in TypeScript.** This is logic that will live in the
  pi extension and the opencode plugin, and today it has no test story at all —
  which is how it would ship untested. It goes in a tiny pure module with the
  spawn injected, unit-tested under the existing `vitest` setup in `web/`
  (`npm test`) rather than invented per integration: a burst of five states with
  one in flight leaves exactly one queued and it is the newest, and the collapsed
  report carries the newest event's timestamp.
- **`StateSource` must not change the row's appearance**, and this is the test
  that says so: render the same row twice, once with `stateSource: "event"` and
  once with `"screen"`, and assert the rendered class lists are equal. Without
  it the prohibition in "What crosses the wire" is a sentence nobody runs.
- **Recorded hook payloads as fixtures**, one file per agent per event, including
  a subagent payload for each, and for Claude one payload per `notification_type`
  in the whitelist. These do not exist yet and capturing them is part of
  implementation, not of this design.
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

## Open questions, as they now stand

Revision 1 listed seven, one of them already answered. Revision 2 closes most of
question 1 on the documentation, and adds two the review found nobody had
considered.

1. **`PreToolUse`'s per-call cost, and nothing else about it.** Revision 1 asked
   three things here — the payload shape, the cost, and whether `"async"` exists
   at all. The first and third are documented: `tool_input` is a structured
   per-tool object (`Bash.command`, `Read.file_path`, …), and `"async": true` is
   a documented field that runs the hook in the background. What remains is a
   number: **fork volume under a burst of tool calls**, one hook process plus one
   tmux client each. Latency is off the critical path by construction, so this is
   about whether a tight `Bash` loop makes the machine notice. If it does, Claude
   drops to state-only by deleting one hook entry.
2. **The 60-second window for a transient `working` report.** Needs the
   distribution of inter-event gaps during real work on all three agents. Revision
   2 sharpens what to measure: the failure of expiring early is a false `idle` on
   a quiet screen, not merely an extra fork, so the case to measure is **a single
   long tool call that emits no sub-events, with no client connected**. The
   number is still a guess biased short.
3. **`N`, the number of polls in the two new evidence rules.** Set at 3 (~4.5s)
   by the same kind of guess as the 60 seconds, and it wants the same kind of
   check: how many consecutive captures a settled agent screen actually produces
   before something repaints, and how quickly a resumed agent starts churning
   again. Too small and rule 2 drops true `blocked` reports on slow-repainting
   screens; too large and the overnight case takes longer to correct itself.
4. ~~Whether tmux can strip control bytes at read time.~~ **Answered by
   `f25e066`**, and left here because the answer is a trap rather than a yes:
   `#{s/[\n\x1f]/ /:var}` works with a bracket set of the two literal bytes,
   `[[:cntrl:]]` blanks every value including good ones, and a byte range is
   locale-dependent. Recorded in "The hazard" so the next person does not
   re-derive it the expensive way.
5. **opencode's global plugin directory.** Whether `~/.config/opencode/plugin/`
   is auto-loaded. If it is, `--global` becomes available for opencode and the
   `.gitignore` consequence stops applying to client repos.
6. **How often opencode's todo ladder rung is empty.** Whether `todo.updated`
   fires for sessions that never use todos decides whether the tool-call rung is
   the common case or the rare one — and therefore how much the argument-reduction
   rules matter.
7. **What pi's `tool_execution_start.args` actually contains**, per tool. The
   basename and first-word reductions are designed against a guess about its
   shape; they need checking against the real thing, particularly for tools that
   take structured input.
8. **Nested project installs.** opencode and pi load project-local files; it is
   not established what happens when a repository contains an installed
   integration and a working directory below it does too, or whether a parent
   directory's plugin is loaded at all. Two integrations writing one pane option
   is a race nobody has looked at.
9. **Cross-agent nesting**, which nobody had considered until the review: Claude
   running inside a pi pane, or any agent started from another agent's shell.
   Both integrations see the same `TMUX_PANE`, both write `@wterm_agent`, and the
   last writer wins — so the inner agent's turn end can report `idle` for a pane
   whose outer agent is still working, which is the subagent failure again with
   no `parentID` or `agent_id` available to filter on because the two processes
   share nothing. The ordering rule does not help; both are legitimately fresh.
   Worth noting that this configuration is not hypothetical in this repo's own
   development. Nothing is proposed here beyond recording it: the candidates are
   a writer-identity field in the value, refusing to report when the pane's
   `pane_current_command` is not the agent doing the reporting, or accepting that
   the innermost agent owns the pane. All three want measuring first.
10. **Whether `notification_type`'s documented list is closed.** It is not
    documented as closed, which is why the whitelist's default is "ignore". If it
    ever becomes closed, the default could tighten — but the default is cheap
    enough that this is curiosity rather than a blocker.
