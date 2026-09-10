# tmux-web v3 — agent-side reporting

Date: 2026-09-10
Status: design, not agreed. Revision 5, written against **a measurement rather
than a review**: 88 turns across the three agents, 44 of them with the agent's
own turn-end event wired up, and 1,440 replays of the 1.5s poll grid at every
phase offset. It confirms revision 4's derivation, fixes `N_idle` at a measured
value, and finds two things no review had: a late repaint that can re-light a
cleared badge, and mid-turn stillness that makes the classifier agree with an
idle report before the turn has ended. See "Revision 5"; the four revision
sections below it are the rounds they answer.
Follows: `2026-09-10-tmux-web-v2-design.md` (v2), which it partly supersedes.
Depends on: the hardening of `internal/tmux/snapshot.go` against hostile
`@wterm_label` values, **committed as `f25e066`** — in history, not pending, and
not a thing this design is waiting on. It is a prerequisite, it answers one of
the questions this document opened with, and it constrains the transport more
than expected — see "The hazard" and "Why the report is not a fourteenth field".

## Revision 5

Revision 4 left `N_idle` explicitly pending a measurement and named what that
measurement had to be. It landed, and the first thing to say about it is that
its method matched this document's assumptions rather than approximating them:
an isolated tmux server per agent, panes at 194x54, the capture and the hash
taken exactly as `Client.Capture` and `Client.Run` take them (`capture-pane -p
-J`, `TrimRight`, FNV-64a over the **whole** capture, no tail slicing), `Observe`
reimplemented line for line with `settleAfter = 2`, and `pipe-pane` logging
per-write wall-clock times so that repaint instants are known to the millisecond
instead of quantised to the sampling grid. The 1.5s poll grid was then replayed
at **every phase offset**, because the daemon's grid has arbitrary phase against
a turn end and a single phase is not a result. Most important of all: **the
turn-end event was measured, not assumed** — Claude Code's `Stop`, opencode's
`session.idle` and pi's `agent_end` were each wired to a log and each fired
reliably, so "polls to settle" is counted from the instant the integration would
have written its report rather than from a proxy for it.

Four things it settles, then the two it opens.

- **`N_idle = 4`, and the floor revision 4 derived is now also a measured
  relationship.** Polls-to-settle from the agent's own turn-end event was **3 or
  4 in every one of the replays** — claude 471×3 / 9×4, opencode 469×3 / 11×4,
  pi 477×3 / 3×4 — and *every* turn on *every* agent had at least one poll phase
  at which it took 4. The 3-versus-4 split is purely where the grid lands. The
  mechanism is the thing revision 4 could not have known: **the turn-end event
  fires 7–52 ms before the agent's last repaint** (claude 13–39, opencode 15–52,
  pi 7–11). A poll landing in that gap is spent on the pre-final screen, the
  repaint lands on window poll 2, and `settleAfter = 2` then needs polls 3 and 4.
  So `R = 1` is a measurement rather than an inspection, `N_idle = settleAfter +
  2 = 4`, and `N = 3` would silently discard about 1 turn end in 50 on claude and
  opencode and 1 in 170 on pi — invisibly, because the badge would just fall back
  to the classifier's slower verdict. Revision 4's `settleAfter + 1` reasoning
  was sound and rested on a false premise: that the report and the last repaint
  are simultaneous. They are not, and **the report always comes first**. See
  "Reports are checked against the screen"; question 3 closes for `N_idle` and
  survives only for `N_blocked`, which the measurement does not touch.
- **Revision 2's rule-3 variant is refuted empirically, not just argued
  against.** Revision 3 rejected "drop the idle report on *any* `working` verdict
  inside the window" by inspection and wrote that nobody had measured how often
  it would fire. It would have fired on **100% of all 88 turns**: every
  post-report sequence begins `W W`. That is now a number in the text rather than
  a worry.
- **The classifier's premise holds, measured.** About 580 seconds of post-turn
  idle across ten runs — 150 s per agent plus seven shorter tails — produced
  **zero hash changes and zero pane output** on all three agents. No spinner,
  clock or animation survives a turn; pi's powerbar and opencode's context panel
  are static when idle; cursor blink is invisible to `capture-pane`. Every place
  this document leans on "an idle screen is static" now cites that rather than
  assuming it — including the places where it is *bad* news, since a static
  screen is exactly what makes a false resting badge unclearable.
- **`N_blocked` is untouched by all of this and stays `settleAfter + 1`.** Rule 1
  drops anything that moves before rule 2 sees it, so rule 2 never has to absorb
  a repaint and the measurement bears on it not at all. Nobody should read
  "`N_idle` measured at 4" as "N is 4".
- **Finding A: a late repaint can re-light a cleared badge.** On 2 of 30 claude
  turns a single line near the input box repainted **5.0 s and 9.0 s after
  everything else had stopped** — both on a "write a file, then reply done"
  prompt, not periodic, not reproducible on demand. It does not stop the turn
  settling, but through the classifier it produces a second working→idle edge and
  a fresh `finishedAt` about 13 s after the real turn end. **It cannot reach
  `finishedAt` on a pane whose report is the authority**, for two independent
  reasons, so it is not a defect this design introduces; it is a v2 classifier
  defect this design inherits wherever it hands a pane back to the classifier.
  The scoping, the reason no hash-level rule can separate it from a genuinely
  short turn, and the minimum mitigation are in "A late repaint re-lights a
  cleared badge".
- **Finding B: an idle verdict inside the window corroborates, it does not
  verify.** On 4 of 88 turns the screen sat still long enough *while the agent
  waited on the model* that the classifier reported `idle` **before the turn
  ended**. Rule 3 accepts an idle report on exactly that verdict, so rule 3 is a
  backstop with a measured leak rather than a proof. Two consequences: the rule
  now states its own limit, and **revision 4's provisional rule-3 tombstone is
  withdrawn**, because mid-turn stillness can clear a rejection that was correct
  and the measurement has emptied the case the provisional clearing was built
  for. See "Reports are checked against the screen" and "What "dropped" means".

One thing worth noting about what the measurement did *not* find: in 88 turns
across three agents it met **none** of the four screens question 10 is waiting on
and did not provoke them. That is not an answer to question 10, but it is
evidence about how rare those screens are, and it is recorded there.

**Where that leaves the document.** Nothing here is now waiting on a measurement
in order to be built. The one number this design could not choose has been
chosen; the two findings are one scoping answer and one withdrawal, neither of
which adds a mechanism; and every remaining open question is either a value that
biases a guess (2, 13), a promotion path that needs a screen nobody has (10, and
`N_blocked` in 3), or a hazard recorded rather than solved (9, 11), which is the
same standing they had when revision 4 was told this was the last design round it
needed. Two things the plan inherits as tasks rather than as questions: the
`state.go` dwell of finding A, which is separable and should not gate anything,
and the sizing of `N_blocked`, which is a constant expressed against
`settleAfter` and changeable by editing one line.

## Revision 4

The third review found four blockers and one false plank, all of them in revision
3's new material, and every one of them yielded to the same instrument: **state
the criterion, then classify against it**. Each was a list, a floor or a guard
derived by example rather than from a rule — correct for the cases its author had
in mind and silently wrong just outside them. That is the successor to revision
3's lesson about compositions and it is worth naming before the list: a rule
justified by the cases it was written against will be extended by whoever reads
it next, and they will extend it to the cases it was never checked on.

- **Rule 3's floor was off by one, and the tombstone made the resulting false
  drop permanent.** `Observe` returns idle only at the `settleAfter`-th
  *identical* comparison, so a window that must contain one repaint needs
  `settleAfter + 2` polls, not `settleAfter + 1`. `N = 3` tolerated a repaint
  only if it was observed at the very first window capture — and if a
  capture-skipped pane's classifier entry is a first sight, zero repaints. Worse,
  a rule-3 rejection was tombstoned with rules 1 and 2's "do not re-evaluate the
  evidence" semantics, which for rule 3 refuses the very confirmation the report
  was missing. Revision 2 dropped true idles transiently; revision 3 dropped
  fewer and made them permanent. Fixed three ways — the floor as a relationship,
  the classifier baseline for a capture-skipped pane specified, and rule-3
  rejections made provisional. See "Reports are checked against the screen" and
  "What "dropped" means". **The third of those three is withdrawn by revision 5**,
  on a measurement that empties the case it was built for and a finding that gives
  its trigger a failure mode; the first two stand and the first is now a measured
  relationship as well as a derived one.
- **"Promoted later by a data change to `blockedRules`" was false, and the test
  written to stop round two's blocker recurring could not fail for it.**
  `blockedRules` is `map[string]dialog` — one grammar per agent, and claude's
  slot is taken by a grammar that requires horizontal rules and numbered choices.
  A second claude screen form is a new grammar type *and* a restructured map: a
  code change. And "every whitelist entry mapping to `blocked` must name an agent
  with a `blockedRules` grammar" passes for every claude type, including the
  three the same revision demoted. The whitelist names a **form** now, not an
  agent — see "Which event means which state".
- **The claude "structurally guarded" claim over-reached by two doors.** `Stop`
  is root-only against **Task-tool subagents**; a `claude` CLI spawned from a
  Bash tool call in the same pane inherits `TMUX_PANE` and fires its own `Stop`
  as its own root, and teammates and background sessions are not subagents at
  all — they have their own hook events, carry no `agent_id`, and run under a
  supervisor process with no terminal attached. Whether those processes carry
  `TMUX_PANE` is unmeasured and decides whether the class needs a guard. See
  "Subagents leak into hooks" and questions 9 and 11.
- **The edge/re-assertion split was classified by semantics and asserted to be
  exhaustive.** The safety criterion is stated now — *a write is a re-assertion
  if the state it would write is resting and the event can fire more than once
  within one resting period* — the split is per (event, state) pair rather than
  per event, pi's `session_start` re-derivation is classified under it (a
  re-assertion; as an edge it re-badges every device on every extension reload),
  the invariant that makes the turn-end events edges is named rather than
  assumed, and the lists are no longer claimed to be exhaustive.
- **One plank corrected**: `agent_needs_input` also fires when *this* session asks
  a teammate's terminal setup question, which is this pane's root session waiting
  on this pane's screen. It joins the other three demotions in the
  pending-a-capture class, so the whitelist waits on four screen captures rather
  than three.

Confirmed sound by the same review and deliberately unchanged here: the bounded
demotion cost, the single rejection slot's capacity (the flaw was semantic, not
capacity), the ordering filter, the missed-`Stop` repair, the six-seconds-slower
connected trade, the `agent_id` wording, the twelve-type table and both timings,
`labelField` at `internal/tmux/snapshot.go:108`, the `AppSidebar.tsx` citation,
`useSnapshot.ts`'s `seen`-stores-the-shown-value mechanics, and the `asyncRewake`
reasoning — including that "even exit 2 is ignored under async" is a
documented-adjacent inference and is labelled as one.

## Revision 3

Revision 2 was reviewed adversarially a second time and found **not ready for a
plan**. All four blockers were in the material revision 2 had just written to
close round one, which is the pattern worth naming before the list: closing a
contradiction by adding a mechanism relocates the contradiction into the new
mechanism unless the new mechanism is composed against everything the old one
touched. Revision 2 congratulated itself on hunting compositions and then made
four more.

- **The badge storm was moved, not defeated.** Stateless `finishedAt` derivation,
  plus `idle_prompt` reporting `idle`, plus `seen` storing the `finishedAt`
  *value* a device was shown, compose into: every device that saw a finish
  re-badges sixty seconds after every Claude turn. "A no-op re-assertion" is
  false under this document's own derivation rule. Fixed at the writer — see
  "Re-assertion is not a report".
- **Evidence rule 2 nullified most of the whitelist.** Four of the five
  `notification_type`s revision 2 mapped to `blocked` have no screen form
  `blocked.go` can match, and rule 2 deletes a reported `blocked` the grammar
  cannot confirm. The whitelist guaranteed membership in the set rule 2 erases,
  so those badges would have stood only until a client connected. The whitelist
  and the grammars are one decision now — see "Which event means which state".
- **"Every one of these filters fails closed" was unimplementable.** For Claude
  and opencode the root session is coded by the *absence* of a field, so "missing
  means silence" silences all normal reporting. Restated per filter, and the
  residual protection re-derived honestly — see "Subagents leak into hooks".
- **"Dropped" was undefined against a transport that never forgets.** Now a
  per-pane tombstone with specified semantics across a restart, and revision 2's
  "almost certainly a real turn end" re-acceptance is withdrawn. Related, and
  found in the same place: **evidence rule 3 as written fired on ordinary turn
  ends**, because `state.go` reports `working` on a *single* changed hash and a
  real turn end repaints. Rule 3 now uses the project's own noise threshold --
  see "Reports are checked against the screen" and "What "dropped" means".

Four corrections in the same pass:

- **A false plank** in the whitelist's fail-closed argument, and a second
  instance of the same error six paragraphs away: at a permission wait the
  standing report is a fresh `working` from `PreToolUse`, so the capture is
  skipped and the screen grammar is **not** running. Both sentences said it was.
- **A comparison the document never made.** Installing the integration makes
  Claude's connected-case blocked detection *slower* — about six seconds against
  about 1.5 — because that same fresh `working` suppresses the capture that
  `IsBlocked` would have read. The trade is now argued rather than omitted.
- **The printed bracket set is uncopyable.** Rendered with a two-character `\n`
  it leaves a real newline alive and turns every lowercase `n` into a space
  (`SECOnD` → `SECO D`). The document now points at `labelField`
  (`internal/tmux/snapshot.go:108`) as the thing to copy, from source.
- **The rule-2 test as specified was vacuous**, testing a counter over a premise
  the whitelist made false. Fixed with the rule.

One fact in the design's favour that revision 2 missed, and a new installer
constraint that arrives with it: a command hook registered `"async": true` has
its **exit code ignored, including exit 2**, unless it also sets
`"asyncRewake": true` — the docs give "exit code 2 wakes Claude" as what
`asyncRewake` adds, and the timeout section says enforcement is skipped for an
async command hook. That is documented *adjacent* rather than verbatim, so it is
belt to the exit-0 rule's braces and not a replacement for it. The constraint:
**`asyncRewake` is never set**, on any hook this project installs.

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

One option, one write, one fork per event — with one exception, added in
revision 3 and confined to the two events that would otherwise lie about when a
turn ended: see "Re-assertion is not a report". Not three options, because every
write happens inside a hook the agent is waiting on (see "Writes must never
block") and three forks is three times the exposure.

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

1. **tmux substitutes the two bytes out before Go sees them.** The pattern is
   `labelField` at `internal/tmux/snapshot.go:108`, and **that source line is the
   thing to copy — not the rendering in this document.** It is a bracket set
   holding a literal newline byte and a literal `0x1f` byte, which prose has to
   render as `#{s/[\n\x1f]/ /:@wterm_label}` and which a reader who copies that
   rendering gets wrong twice: a two-character `\n` leaves a real newline alive,
   *and* it puts a literal `n` in the set, so every lowercase `n` in a perfectly
   good label becomes a space (`SECOnD` → `SECO D`). Measured in revision 3. The
   committed Go works because `"\n"` in a Go string literal *is* the byte. The
   substitution replaces with a *space* rather than nothing, so `EV\x1fIL` reads
   as `EV IL` — visibly tampered with, rather than as a label somebody chose.
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
should copy `labelField` from `internal/tmux/snapshot.go:108` **verbatim and from
the source file**, and not improve it. A third failure mode joins the two above
and is the one this document itself walked into: a bracket set retyped from a
rendered escape covers neither of the two bytes and silently mangles benign
values, which — like the `[[:cntrl:]]` trap — no hostile-input test would see.

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
   \; list-panes -a -F "A<Sep>#{pane_id}<Sep>#{s/<two-byte set>/ /:@wterm_agent}"
```

`<two-byte set>` is deliberately not spelled out here. It is `labelField`'s
bracket set, built the same way from the same constants — a real newline byte
and a real `0x1f` — and layer 1 of "The hazard" says what happens to somebody
who copies a rendered `\n` instead.

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

**Claude's `Notification` is a whitelist on `notification_type`, and the
whitelist is not free to say `blocked`.** Revision 2 mapped five types to
`blocked`. Four of them have no screen form any grammar in `blocked.go` matches
— an MCP form, a URL request, a press-Enter banner, and a question from
somewhere that is not this pane's session — and evidence rule 2 exists precisely
to delete a reported `blocked` the grammar cannot confirm. Two sections written
to close two different round-one findings, each correct alone, cancelling each
other: those four badges would have stood exactly until a client connected, then
been erased about 4.5 seconds later while Claude was still waiting. Revision 3
makes them one decision.

**A whitelist entry may map to `blocked` only if a `blocked.go` grammar can
confirm it.** The whitelist and the grammars are one claim seen from two sides —
the event says the agent is waiting, the grammar says the screen shows it
waiting — and a resting claim for which no evidence can exist does not get to
rest indefinitely. An entry whose screen form nobody has captured is *ignored*
until somebody captures it: the same fail-closed direction as the unknown-type
default.

**The promotion path is a code change, and revision 3 described it wrongly.** It
said "a data change to `blockedRules`, which is the extension path that map's own
comment already describes" — but that comment is about adding a new *agent*.
`blockedRules` is `map[string]dialog`: **exactly one grammar per agent**, and
claude's slot is occupied by `claudeDialog`, which requires horizontal rules, a
cursor on a numbered choice, at least two numbered choices, and a line ending in
`?`. A quota press-Enter banner and an MCP elicitation form match none of that.
Promoting either needs a new type implementing `dialog` **and** a map that can
hold more than one grammar per agent. That is a code change, structurally unlike
what revision 3 promised, and it is the reason the demoted types are named in an
open question rather than waved at by a table row.

**So the whitelist names a form, not an agent.** A `blocked` mapping carries the
identifier of the screen form that confirms it — `claude/permission` today, which
is the only one that exists — and forms are registered in `blockedRules`, which
becomes a registry that can hold several per agent (`map[string][]dialog` or a
keyed equivalent; the plan picks). Two things fall out, and both were latent
holes in revision 3:

- **The consistency test can now fail.** "Every `blocked` entry names an agent
  with a grammar" is vacuous, because claude *is* in the map: the three types
  revision 3 demoted would all have passed it, which is to say the test written
  to stop round two's blocker recurring could not have seen it. "Every `blocked`
  entry names a **registered form**" goes red the moment somebody maps a type to
  `blocked` without writing its grammar. See "Testing".
- **Rule 2 asks the right question by construction.** It drops a reported
  `blocked` when the settled screen matches **no registered form for that
  agent** — not when it fails to match the one grammar the agent happens to have.
  Those are the same question today and stop being the same question the day a
  second claude form is promoted, at which point a standing `permission` report
  on a screen showing an elicitation form must *not* be dropped: the agent is
  waiting, and which form it waits at is not rule 2's business. `IsBlocked`
  already has the right shape for this once the registry holds a list. Revision 3
  had this gap latently and only avoided it because exactly one claude type
  mapped to `blocked`.

The documented values, and what each one reports **today**:

| `notification_type` | Reports | Why |
| --- | --- | --- |
| `permission_prompt` | `blocked` | "Claude needs you to approve a tool use or a sandboxed command's network request, and the prompt has waited about six seconds" — and it is the dialog `blocked.go`'s claude grammar was written against, so rule 2 can adjudicate it rather than merely erase it |
| `quota_auto_resume_fired` | `working` | Claude Code is continuing the task |
| `idle_prompt` | `idle`, **as a repair only** | "Claude finished responding about 60 seconds ago and you haven't typed since". It re-asserts what `Stop` already said, so the writer suppresses it unless the standing report disagrees — see "Re-assertion is not a report" |
| `quota_auto_resume_disabled` | `idle`, **as a repair only** | the wait ended and the task was not continued. Same suppression, same reason |
| `quota_auto_resume_stale` | *ignored, pending a capture* | "waits for you to press Enter instead of continuing" — a true root-session block drawn on the root screen, and no grammar has been written for that banner. Promotes to `blocked` the day one is |
| `elicitation_dialog` | *ignored, pending a capture* | an MCP server has opened a form and is waiting on the user. On the root screen; no grammar |
| `elicitation_url_dialog` | *ignored, pending a capture* | an MCP server is asking the user to open a URL. Same |
| `agent_needs_input` | *ignored, pending a capture* | Two situations under one type, and revision 3 saw only one. "A background session or a teammate needs input" is not this pane's root session. But the *current* session asking a teammate's terminal **setup question** is this pane's root session waiting, drawn on this pane's screen — so revision 3's categorical "not this pane's root session" is half false, and this is the fourth member of the pending-a-capture class rather than a permanent exclusion. If its form is ever captured, rule 2 is what keeps the other half honest: no registered form on the settled root screen, no badge |
| `auth_success` | *ignored* | not a state of the session |
| `elicitation_complete` | *ignored* | bookkeeping between Claude and an MCP server |
| `elicitation_response` | *ignored* | as above |
| `agent_completed` | *ignored* | "a background session finishes or fails" — **not** this session's turn end. Reported as `idle` it is a false `done` badge on a working agent, which is v2's central failure arriving through yet another door |
| **anything else** | ***ignored*** | the default, and it matters more than the table |

**What the demotions cost, plainly.** All four are real waits the screen cannot
currently see — the fourth, `agent_needs_input`'s setup-question form, only since
revision 4 corrected the claim that it was never this pane's session at all — and
demoting them means an MCP elicitation form or a quota banner left overnight
produces **no badge at all**: the pane reads `working`
until the 60-second expiry and then nothing, because with no client connected
there is no classifier either. That is v2's behaviour, which is the floor this
whole design degrades to elsewhere. It is worse than revision 2 *claimed* to
offer and identical to what revision 2 would actually have delivered from the
moment anyone opened the app. What buys them back is four screen captures and four
registered forms — named in the open questions, and each of them a code change
rather than the data change revision 3 advertised.

**The default is the decision here, not the enumeration.** The list above is
what the docs currently carry; it is not documented as closed, it has plainly
grown before, and it will grow again. So an unrecognised `notification_type`
**writes nothing at all** — it does not change the state, it does not refresh the
timestamp, it is not an error. The asymmetry that settles this:

- Ignoring a notification that *should* have meant `blocked` costs a badge that
  is late and may never come. Revision 2 said it costs only a late badge,
  "because the screen grammar is still running (a fresh `blocked` report is not
  in play, so the pane is captured normally)" — and that is **false**, for a
  reason its own precedence table supplies: at a permission wait the standing
  report is a fresh `working` from `PreToolUse`, which fires *before* the
  permission is evaluated, and a fresh `working` skips the capture. `IsBlocked`
  is not run at all. The true cost is up to 60 seconds of `working` expiry and
  then whatever the grammar can do, which for an uncaptured dialog is nothing.
  Still the cheap direction; not the free one.
- Defaulting an unknown notification to `blocked` costs a *permanent false
  badge*. `blocked` is a resting state, so it never expires; if the screen is
  static, the changed-hash escape hatch never fires either. **"If" is now
  "when":** about 580 s of post-turn idle across ten runs produced zero hash
  changes and zero pane output on all three agents, so an idle agent's screen is
  measured to be perfectly still — no spinner, no clock, no animation survives a
  turn, pi's powerbar and opencode's context panel are static, and cursor blink
  is invisible to `capture-pane`. The escape hatch does not merely *risk* never
  firing on a false resting badge; on a finished agent it cannot fire.

`idle_prompt` still deserves its own note, and it now has its own section,
because revision 2's treatment of it was wrong in a way that is invisible until
three sections are read together. See "Re-assertion is not a report".

Two consequences worth stating rather than discovering, and the second is a cost
revision 2 never put on the page at all:

- **Claude's event-sourced `blocked` is about six seconds late by
  construction**, because `permission_prompt` waits about six seconds before
  firing. That is four polls. Revision 2 called it "bounded, because it fails
  towards the screen", and the second half is the same error as the bullet above:
  during those six seconds the standing report is a fresh `working` from
  `PreToolUse`, and a fresh `working` **skips the capture**, so the screen is not
  being read. The bound is real but it comes from the six seconds themselves and
  from nothing else, and for those six seconds the row says `working` about a
  pane that is waiting. It remains an argument against ever raising the `working`
  window on the theory that "the event will arrive".
- **Installing the integration makes Claude's blocked detection *slower* in the
  connected case.** With a client attached, a non-integrated Claude pane is
  captured every poll and `IsBlocked` matches the permission dialog within one
  poll, about 1.5 seconds. An integrated one takes about six, because that same
  fresh `working` suppressed the capture that would have found it. The trade is
  accepted: the integration's genuine value is the **disconnected** case, where
  it is the only source of anything at all, and 4.5 seconds on a wait that lasts
  until the user acts is not what the badge is for. But it is a trade, it was
  never stated, and it has a lever — the state-only fallback for Claude removes
  `PreToolUse`, which restores the 1.5-second connected-case badge and costs the
  activity line. That makes open question 1 a design question and not only a
  fork count.

The whitelist lives in `wterm-web report`, in Go, not in the hook configuration —
which is the point of having one binary. It is a table, it is testable, and when
the list grows it grows in one file rather than in three integrations and a
`settings.json` the user owns.

### Re-assertion is not a report

This is revision 3's first blocker, and it is revision 1's badge storm relocated
one layer up rather than defeated. Three sentences the document held at once:

- `finishedAt` is **derived**, not stamped — "equal to that report's own
  timestamp", recomputed from the current accepted report at every poll, with no
  memory of a previous one;
- `idle_prompt` reports `idle`, "a no-op re-assertion of what `Stop` already
  said", and it fires on **every turn the user does not type into**;
- each browser's `seen[paneId]` stores the `finishedAt` **value it was shown**,
  not the time it looked — `web/src/lib/useSnapshot.ts`, where `isDone` is
  `pane.finishedAt > (seen[key] ?? 0)`.

Compose them. `Stop` at T writes `1;idle;T`; a device glances; `seen = T`; the
badge clears. Sixty seconds later `idle_prompt` writes `1;idle;T+60`. It is
strictly newer, so the ordering filter takes it. The screen has been settled for
a minute, so evidence rule 3 has nothing to object to. Stateless derivation gives
`finishedAt = T+60 > seen = T`. **Every device that saw the finish re-badges, one
minute after every Claude turn**, and `quota_auto_resume_disabled` does it again.
"No-op re-assertion" is false under this document's own derivation rule, and a
badge that fires once per turn on a pane nothing happened to is the exact failure
`blocked.go`'s comments call the worst one available: it teaches the owner to
ignore the badge.

The underlying error is semantic rather than mechanical. **The timestamp field
means "when the agent entered this state", and a re-assertion's write time is not
that.** `Stop`'s is. `idle_prompt`'s is sixty seconds late.

**The fix is writer-side suppression, and it is the one that keeps the repair.**
Revision 3 sorted the events into two kinds by what they *mean*, listed them, and
asserted the lists were complete. Revision 4 states the criterion first, because
this classification is a safety property and "these are the ones I thought of" is
not one:

> **A write is a re-assertion if the state it would write is a resting state and
> the event that produces it can fire more than once within one resting period.**
> Everything else is an edge. An edge writes unconditionally; a re-assertion
> **reads the standing option first** and writes only if the standing report's
> state differs from the one it would write.

Three things follow, and the third is a bug revision 3 was carrying.

**The classification is per (event, state) pair, not per event.** An event that
writes `working` on one branch and a resting state on another is an edge on the
first and a re-assertion on the second. `working` is transient and cannot badge —
the worst a redundant `working` does is refresh a 60-second expiry, which is what
the keepalive wants anyway — so the extra read is paid only where it buys
something.

**What makes the turn-end events edges is an invariant, not their semantics.**
`Stop`, `agent_settled` and `session.idle` all write a resting state, so under
the criterion they are edges only because they cannot fire twice inside one
resting period — and *that* holds only because a new turn writes a non-resting
state before its turn end can fire. So each integration must write `working` at
turn start: Claude's `UserPromptSubmit`, opencode's `session.status busy`, pi's
`input`. All three are already in the design (they are the `working` events in
the table, and `UserPromptSubmit` is already specified as a state hook whose
`prompt` is not published) — what is new is that this is now load-bearing and
named as such. A turn that could end with no `working` written before it would
have its turn end suppressed and lose that turn's badge. Revision 3 relied on
this without noticing it was relying on anything.

**pi's `session_start` re-derivation is a re-assertion, and revision 3 had it in
neither list.** The document already carries the mechanism — a pi extension
reload can replace the extension mid-run, so `session_start` re-derives state
from `ctx.isIdle()` — and it appears in no classification. Classify it: a reload
can happen any number of times while the agent sits idle, and the `isIdle()`
branch writes a resting state, so that pair is a re-assertion. Implemented as an
edge, **every extension reload on an idle pane writes `idle;<now>` and re-badges
every device** — round one's badge storm arriving through an event nobody had
classified, which is the whole argument for having a criterion instead of a list.
Its `isIdle() === false` branch writes `working`, which is transient, and stays
an edge.

The lists as they now stand, and they are **not claimed to be exhaustive**:

- **Edge**: `Stop`, `agent_settled`, `session.idle` (each by the turn-start
  invariant above), `UserPromptSubmit`, pi's `input`, `session.status busy`,
  `PreToolUse`, tool events, `permission_prompt`, `permission.asked`,
  `ui_prompt_start`, `quota_auto_resume_fired`, and `session_start`'s working
  branch.
- **Re-assertion**: `idle_prompt`, `quota_auto_resume_disabled`, and
  `session_start`'s idle branch.

Exhaustiveness is not ours to claim. The event inventories are per-agent
documentation we do not control, and revision 3's "nothing else on any agent
today" rested on the once-per-idle cardinality of `session.idle` and
`agent_settled`, which nobody has measured. So the lists ship with a **default**
instead of a guarantee: **an event whose cardinality within a resting period is
unknown, and which writes a resting state, is treated as a re-assertion.** The
read costs one fork on a path nobody is waiting on; the alternative costs a badge
storm. That is the same asymmetry that settles the unknown-`notification_type`
default two sections up, and it is why the classification lives in a table in
`report` with a test, rather than in three integration files.

In the normal case the re-assertion reads `1;idle;T`, sees `idle`, and does
nothing: no write, no newer timestamp, no badge. In the case revision 2 wanted it
for — a `Stop` that never landed, which is the attested reason herdr gave up on
Claude hooks — it reads `1;working;T`, sees the disagreement, and writes
`1;idle;T+60`, repairing the pane from the agent itself. The repair survives
whole; only the no-op goes away. Note what the repair is worth, which revision 2
never spelled out: a missed `Stop` leaves the pane on `working` until the
60-second expiry, and with **no client connected** the expiry hands it to a
classifier with no screen to read, so the pane shows nothing at all until the
user next types. That is the overnight case, which is the case the app is for.

The edge/re-assertion classification lives in `wterm-web report`, in Go, in the
same table as the whitelist — not in the three integration files. One table, one
test.

**What it costs:**

- **One extra `tmux show-options -p -v @wterm_agent` fork**, on re-assertion
  writes only. Today that is two `Notification` types on claude, at most once per
  turn each, plus pi's `session_start` idle branch, which fires on an extension
  reload rather than on a turn. `PreToolUse` — the hot hook, and the whole
  subject of open question 1 — pays nothing, and opencode pays nothing at all.
  Revision 3 said pi paid nothing either; that was true only because it had not
  classified `session_start`.
- **"One option, one write, one fork per event" is no longer exactly true**, and
  that sentence in "The value" is amended rather than quietly left standing.
- **A stale or buggy integration still storms.** The daemon cannot tell a
  re-assertion from a genuine finish — both are `1;idle;<ts>` — so this is a
  promise the writer keeps and the reader cannot check. It is the one place in
  this design where a reader-side invariant rests on writer-side behaviour, and
  it is stated here rather than discovered later.

**Why not the other two candidates**, since each contradicts a sentence this
document holds and the choice has to be explicit:

- *Edge memory in the daemon* — remember the previous accepted report and collapse
  a resting run to its first timestamp — works while the daemon lives and fails
  on restart, where the memory is empty and the standing `1;idle;T+60` derives
  `T+60` all over again. It converts one storm per turn into one storm per
  restart, and it spends the restart story that stateless derivation was chosen
  to buy. Rejected as a replacement, and not needed as a backstop: with
  suppression the option never holds the later timestamp at all.
- *Demoting `idle_prompt` to ignored* costs nothing to build and loses the repair
  described above. Rejected because that repair is the answer to the one attested
  objection anybody has raised against hook-sourced Claude state, and one fork
  per turn is cheap for it.
- *A fifth field carrying the finish time*, so a re-assertion could restate the
  original timestamp instead of skipping, is the same mechanism in a costume: the
  writer still has to read the standing report to learn what to restate. It adds
  a wire field and buys nothing.

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

**Claude ships with activity from `PreToolUse`, registered with `"async": true`
and never `"asyncRewake"`, with state-only as the specified fallback** if the
volume measurement comes out badly. The fallback is not a redesign — it is
deleting one hook entry from the generated settings block, and every other Claude
behaviour in this document is unchanged by it.

Revision 3 adds a second reason the fallback might win, which is not a cost at
all but a benefit: **`PreToolUse` is what makes Claude's connected-case blocked
badge six seconds slow instead of 1.5**, because its fresh `working` suppresses
the capture `IsBlocked` would have read. So the choice is not "activity line
versus fork count" but "activity line in the disconnected case versus a
four-and-a-half-second faster badge in the connected one". The recommendation
still goes to `PreToolUse` — the disconnected case is the product — but the
measurement in open question 1 now decides a trade rather than a threshold.

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
| fresh `idle`, inside the verification window | captured | The report, unless the classifier fails to report `idle` even once within the window — see evidence rule 3. At most `N_idle` polls, and the window closes at the first `idle` verdict rather than running out the count: measured, that is three polls in about 98% of poll phases |
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
three agents *do* report the blocking they can see, so for a dialog the agent
raises itself **the failure mode is "blocked is late by the freshness window",
not "blocked is missed"** — and on Claude "late" is now a measured six seconds
against the screen path's 1.5, which is the comparison two sections up rather
than a shrug. The exception is a wait reported through a `notification_type` the
whitelist demotes for want of a grammar: there both authorities are blind and the
badge is genuinely missed. That is the cost recorded under the whitelist, and it
is deliberately the *same set* on both sides — the demoted types are exactly the
ones rule 2 could not have adjudicated.

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
   after `N_blocked` consecutive polls agree. *New in revision 2.* Revision 1's failure
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
   else. `N_blocked` polls rather than one because the first capture after a reconnect
   can catch a repaint mid-frame and the grammars are deliberately strict.

   The premise under "has stopped moving" is measured rather than assumed as of
   revision 5: an agent that has finished a turn produces a byte-identical
   capture indefinitely (about 580 s across ten runs, zero hash changes, all
   three agents). Rule 2 is therefore adjudicating a screen that really is final,
   not one that is between frames — with the single caveat finding A supplies,
   that "final" was violated twice in 30 claude turns by a repaint arriving up to
   9 s late. Composed with rule 1 that is survivable rather than free: a late
   repaint on a pane reporting `blocked` is a changed hash, so rule 1 drops the
   report and tombstones it. The badge does not go with it, because a dropped
   report hands the pane to the grammars and the dialog is still on the screen
   for `IsBlocked` to match positively. Both rules require a connected client, so
   there is no case where the drop happens and the grammar is not there to catch
   it.

3. **A reported `idle` is dropped if the screen never settles**, over a
   verification window of `N_idle` polls after the report arrives. Within the window
   the pane is still captured. The report **stands** if the classifier — the same
   code, the same hash, the same rules — says `idle` at any poll inside the
   window, and the window **closes at that verdict**: the report is accepted, the
   derivation happens, and the captures stop rather than running out the count.
   It is **dropped** only if the classifier says `working` for the whole
   of it, and then the pane goes back to the classifier with no `finishedAt`
   derived. That is the unfiltered-subagent case caught by evidence rather than
   by a discriminator holding.

   **The limit of this rule, measured, and it is not a small one: an `idle`
   verdict inside the window is *corroboration*, not verification.** On **4 of 88
   measured turns** the screen sat still long enough *while the agent was waiting
   on the model* that the classifier reported `idle` **before that turn's
   turn-end event fired** — in one opencode turn, at 31 of 60 poll phases, with a
   run of two consecutive `idle` verdicts, which is about six seconds of a
   perfectly static screen on an agent that had not finished. So the verdict rule
   3 accepts on does not mean "the turn ended"; it means "the screen was still
   for `settleAfter` polls", and those are different claims. Everything rule 3 is
   asked to carry has to be sized against that:

   - **The rule is a backstop with a measured leak, not a proof.** A subagent's
     false `idle` survives rule 3 whenever the root's own mid-turn stillness
     happens to fall inside the window. The measured rate at which a turn offers
     such a window at all is about 1 in 20; the rate at which one *coincides*
     with a false report is smaller and is not measured.
   - **This is the argument against padding `N_idle`.** A longer window is not
     free caution: it accepts more mid-turn stillness as corroboration, so
     widening `N` trades one failure for another rather than buying safety. That
     is the reason `N_idle` is set at exactly the measured `settleAfter + 2` and
     not at `settleAfter + 2` plus a margin, and the reason finding A is
     explicitly *not* answered by widening it — that would take `N ≈ 8`.
   - **It is also why the rule-3 rejection is no longer provisional.** Revision 4
     had a rejection clear "the moment the classifier reports `idle`"; under this
     finding, mid-turn stillness clears a rejection that was correct. See "What
     "dropped" means".
   - **No sustained-idle threshold turns corroboration into verification.** The
     tempting repair — require the classifier to stay idle for *k* polls — is a
     rule derived from the example that provoked it. Waiting on a model has no
     upper bound, and neither does a silent tool call, which is question 2's
     case; a threshold that separates the four turns this run caught says
     nothing about the fifth.

   *Revision 3 rewrote this rule, because revision 2's version fired on ordinary
   turn ends.* Revision 2 dropped the report if the classifier said `working` at
   **any** poll in the window — and `state.go` says `working` on a **single**
   changed hash, which is precisely what `settleAfter = 2` exists to declare is
   noise. A real turn end repaints at least once immediately after `Stop`: the
   spinner clears, the prompt redraws, and that repaint races a fire-and-forget
   write. So revision 2's rule would have dropped a large fraction of *true* idle
   reports whenever a client was connected, quietly handing `finishedAt` back to
   the classifier's `now`-stamp — in exactly the case anybody would be watching.
   Revision 3 wrote that nobody had measured how often. **It is measured now, and
   the fraction is all of it: revision 2's rule would have discarded 100% of all
   88 measured turns on all three agents**, because every post-report poll
   sequence begins `W W` — the pre-final screen, then the repaint. The point
   revision 3 made without the number stands and is sharper with it: the rule as
   written could not have been measured green, and the per-section test it
   implied would have passed.

   The rewrite borrows the project's own noise threshold instead of inventing a
   second one: one repaint is noise, `settleAfter` consecutive identical captures
   is stillness, and a working agent does not go still — `state.go`'s own comment
   is that "a working agent redraws its spinner and elapsed-time". **That last
   clause is the one the measurement qualifies**: an *idle* screen is now
   measured to be perfectly static (about 580 s of post-turn idle across ten
   runs, zero hash changes, zero pane output, all three agents), but a *working*
   screen is not measured to be perfectly animated — see the corroboration limit
   below.

   **Revision 3 then got the floor wrong by one, in the direction that breaks the
   property it advertises.** Read `Observe` (`internal/tmux/state.go`): a changed
   hash sets `still = 0`, and idle is returned only when `still == settleAfter`,
   where `still` counts consecutive *identical* comparisons. So a capture that
   differs at window poll *k* yields idle no earlier than poll *k + settleAfter*.
   Revision 3's `N >= settleAfter + 1` is the floor for a window containing
   **zero** repaints, and `N = 3` therefore tolerated a repaint only if it was
   observed at the very first window capture. The general relationship:

   > **`N_idle >= settleAfter + R + 1`**, where `R` is the number of *polls*
   > inside the window at which the capture differs from the one before it,
   > following a turn end. `R` counts polls and not repaints: several redraws
   > inside one 1.5s interval collapse into a single changed hash.

   `R >= 1` by inspection — the spinner clears and the prompt redraws after every
   turn end, and that write races a fire-and-forget report — so the floor for the
   "one repaint is noise" property this document advertises is `settleAfter + 2`,
   which at `settleAfter = 2` is **four polls, about six seconds**, not three.

   > **`N_idle = settleAfter + 2`. Measured, not floored: `R = 1`.**

   This is both the derived floor and a measured relationship, and it should be
   cited as both, because they are two different claims that happen to agree.
   88 turns across the three agents, 44 of them counted from the agent's own
   turn-end event, replayed at every phase of the 1.5s grid: **polls-to-settle
   was 3 or 4 and never anything else** — claude 471×3 / 9×4 (1.9%), opencode
   469×3 / 11×4 (2.3%), pi 477×3 / 3×4 (0.6%) — and every turn on every agent had
   at least one poll phase at which it took 4. The 3-versus-4 split is not a
   property of the agent, it is where the grid lands.

   **The mechanism is what revision 4 could not have known, and it is why the
   `settleAfter + 1` reasoning was sound and still wrong.** The turn-end event
   fires **7–52 ms before the agent's last repaint** — claude 13–39 ms, opencode
   15–52 ms, pi 7–11 ms — so the report is written to a screen that is not yet
   final. Revision 4 assumed the report and the last repaint are simultaneous;
   they are not, and **the report always comes first**. A poll landing in that
   gap is spent on the pre-final screen, the repaint lands on window poll 2, and
   `settleAfter` then needs polls 3 and 4. `R >= 1` is therefore not a property
   of *some* turns but of every one measured, and the cost of getting it wrong is
   the invisible kind: `N = 3` discards about 1 true turn end in 50 on claude and
   opencode and 1 in 170 on pi, and discards it silently, because the badge
   merely falls back to the classifier's slower verdict rather than failing.

   Two details of the replay, because they are what make it a measurement of
   *this* design rather than of a similar one. It reimplemented `Observe` line for
   line at `settleAfter = 2` and hashed the whole `capture-pane -p -J` output the
   way `Client.Capture` does, so the numbers are this classifier's numbers. And
   it was run both ways round the first-sight question — a classifier created
   fresh at the first window poll, which is this design's capture-skipped
   baseline rule, and one carrying a retained baseline from before the report —
   with **identical histograms**. The claim two subsections down, that the floor
   is the same either way, is checked rather than argued.

   **`N_blocked` is a different number, does not need `R`, and this measurement
   does not bear on it at all.** Rule 2's precondition is a screen that has
   stopped moving, and anything that moves is dropped by rule 1 first — so rule 2
   never has to absorb a repaint and its floor is `settleAfter + 1`, three polls.
   Nothing measured here changes that, and nobody should read "`N_idle` measured
   at 4" as "N is 4": the two constants are separate, and `N_blocked` remains a
   derived floor with an unmeasured value, in question 3. Revision 3 guessed that
   the two rules "may want two constants rather than one"; they do, and the
   reason is now a derivation for one of them and a measurement for the other.
   Both are expressed against `settleAfter` in code, not written as literals —
   which matters more now, not less: a literal `4` would survive a change to
   `settleAfter` and be wrong, and it would also read as a number somebody chose
   rather than one two independent arguments arrived at.

   **A capture-skipped pane's classifier baseline is dropped, not kept.** This is
   the thing revision 3 never specified and on which its arithmetic silently
   depended. A pane whose captures have been skipped for five minutes either
   keeps its `paneState` entry or does not, and the two do not behave alike. The
   rule: a pane the poller did not capture is not passed to `Retain`, so its
   entry is dropped and the first capture when a window opens is a **first
   sight**, which `Observe` answers with `working` and no comparison at all. Two
   reasons, and neither of them is the arithmetic — the floor above is the same
   either way, because a retained baseline from minutes ago is all but guaranteed
   to differ at poll 1 and so burns the same poll a first sight does:

   - **A retained hash is not what the classifier means by one.** `Observe`'s
     contract is "changed since the previous poll", at a fixed 1.5s interval.
     Comparing a fresh capture against a baseline from an arbitrary time ago
     answers a different question, and answers it in whichever direction the
     screen happened to land.
   - **`everChanged` must not be armed by the gap.** A retained baseline that
     differs sets `everChanged`, which is the flag that licenses the classifier
     to stamp a `finishedAt` of `time.Now()`. A first sight leaves it false, so
     through the whole verification window the only finish time available is the
     report's own — which is this design's rule that a change of authority stamps
     nothing, holding in the one place it would otherwise have leaked.

   The hole this leaves, stated rather than left for the next reviewer: a
   subagent's false `idle` survives if the root's screen happens to go still for
   `settleAfter` polls inside the window — about three seconds of a static screen
   on an agent that is working. Revision 3 wrote that "all three agents animate
   while working, which is what makes that unlikely; it is not what makes it
   impossible". **The measurement withdraws the first clause.** Agents animate
   while they are *drawing*; they do not animate while they are *waiting on the
   model*, and 4 of 88 turns went still for long enough during that wait for the
   classifier to say `idle` before the turn had ended. So the hole is not
   improbable, it is merely uncorrelated: it needs a false report standing at the
   moment a mid-turn still window opens, and the still windows are measured at
   about 1 in 20 turns. This is finding B, it is stated with the rule above, and
   it is the reason the rule-3 rejection stopped being provisional.

`N_idle` is **measured**, at `settleAfter + 2`. `N_blocked` is floored as above at
`settleAfter + 1` and is otherwise a guess of the same kind as the 60-second
window, flagged as such and still in open question 3. Both are relationships
against `settleAfter` in code, never literals.

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
- **The cost is bounded and shaped correctly.** Rule 3 costs at most `N_idle`
  captures per turn end on a reporting pane — measured, three of them in about
  98% of poll phases, because the window closes at the verdict rather than
  running out the count — against one capture per poll forever without
  the integration. Rule 2 costs captures only while a `blocked` report stands,
  which is the state where the user is waiting anyway.

**What rule 2 costs, and what revision 2 got wrong about it:** an agent genuinely
blocked at a dialog whose *screen form* no grammar in `blocked.go` matches loses
its reported `blocked` after `N_blocked` polls, and the classifier — which also cannot
see the dialog — reads a static screen as idle. That is a downgrade of a true
`blocked` to `idle`.

Revision 2 called that "bounded to dialogs we have not captured, the same set v2
already fails on". The bound was real; the reassurance was not, because revision
2's own whitelist **guaranteed membership in that set** for four of its five
`blocked` mappings. A rule that deletes a class of report and a whitelist that
manufactures exactly that class are not two independent decisions that happen to
sit in one document. Revision 3 makes them one: an entry may claim `blocked` only
if a grammar can confirm it, so by construction every reported `blocked` is one
rule 2 can adjudicate. What remains inside the bound is what it always should
have been — a dialog captured once and since restyled — which is a real risk, is
the same risk `blocked.go` already carries for every non-integrated pane, and
gives that file a second reason to be kept current. It is the price of not
letting a dead integration pin a badge forever, and that trade goes this way.

### What "dropped" means, against a transport that never forgets

The evidence rules say a report is "dropped", and revision 2 never said what that
means against an option that still holds the same report on the next poll. Left
unspecified, the daemon re-reads it, re-evaluates it, and flips it back to
accepted the moment the screen settles again. Specified:

- **The daemon keeps two timestamps per pane**, in the map the ordering filter
  already needs: the timestamp of the last report it **accepted**, and the
  timestamp of the standing report if that report was **rejected on evidence**.
  One slot each rather than a set — the option holds exactly one value, so the
  only report that can be re-seen is the current one.
- **A report whose timestamp matches the rejection slot is re-rejected without
  re-evaluating the evidence**, whichever rule condemned it — revision 4 split
  this per rule and revision 5 puts it back, for a measured reason below. The
  evidence that condemned it was a screen that has since moved on;
  re-running the test against a screen that has since settled is exactly how a
  dropped report comes back to life.
- **A rejection does not advance the ordering filter.** A dropped report was
  never accepted, so the next genuine report must still be strictly newer than
  the last *accepted* one, not than the dropped one. The two slots are
  independent, and a test says so.
- **A newer value clears the rejection slot.** It is a different report and earns
  its own verdict.
- **The slot has one semantic for all three rules, and revision 4's second one is
  withdrawn.** This is the one place where the measurement overturns a decision
  rather than confirming it, so both halves are worth keeping on the page.

  For **rules 1 and 2** the tombstone was always right and its justification is
  the one above: the report claims the agent is waiting at a dialog, the evidence
  is a screen that moved or a settled screen with no form on it, and nothing the
  screen does later makes that claim true again. Later settling is resurrection
  noise. Cleared only by a newer value.

  **Revision 4 made rule 3's rejection provisional** — cleared "the moment the
  classifier reports `idle` for that pane" — and the argument was good against
  the premises it had. Rule 3 drops a resting `idle` for want of a settle inside
  the window; a later settle looks like the confirmation the report was missing;
  and at `N = 3` a window one poll too short was going to happen often, with
  revision 3's stated escape ("a newer value clears the slot") closed by the
  `idle_prompt` re-assertion, which reads the standing *option*, sees `idle`,
  agrees and writes nothing while the rejection sits unreachable in the daemon.

  **Both premises are gone, and one of them has been replaced by its opposite.**

  - *The case it was built for is measurably empty.* At the measured `N_idle =
    settleAfter + 2`, polls-to-settle from the agent's own turn-end event was 3
    or 4 in **every one of 1,440 exact-timing phase replays** and never 5. The
    "window one poll too short" that provisional clearing exists to repair did
    not occur once. It is not impossible — finding A supplies the shape of an
    `R = 2` turn, where a late repaint lands on the fourth window poll of a
    4-poll phase — but that needs two measured rarities to coincide, where
    revision 4 had to assume a routine one.
  - *Clearing on a classifier `idle` is no longer a safe trigger.* Finding B:
    4 of 88 turns went still long enough while waiting on the model for the
    classifier to report `idle` **before the turn ended**. That verdict is
    exactly what revision 4 made the clearing key on, so mid-turn stillness on a
    root that is genuinely working can clear a rejection that was **correct** —
    resurrecting the subagent's false `idle`, deriving `finishedAt` from it, and
    landing the false done badge this whole rule exists to prevent. The trigger
    the provisional clearing chose is the one signal finding B showed to be
    unreliable.

  **So a rule-3 rejection is cleared only by a newer value, like the other
  two.** What that costs is much less than revision 4 feared, and the reason is
  worth stating because revision 4 missed it: **a rejected report hands the pane
  back to the classifier, and the classifier is an authority that can stamp a
  finish.** For rule 3 to have fired at all the screen must have churned through
  the whole window, which sets `everChanged`; when it does settle, `Observe`
  stamps `finishedAt = now` in the ordinary way. So a wrongly-dropped true turn
  end does not lose its badge — it gets one dated by when the daemon noticed
  instead of by when the agent finished, a few seconds late and from the weaker
  authority. Revision 4 wrote "a permanent loss of that turn's `finishedAt`";
  the accurate version is "that turn's finish is dated by the classifier".
  That is the ordinary v2 degradation, which is the floor this whole design
  degrades to everywhere else, and it is a far better thing to spend than a
  false badge.

  Two consequences, both better stated here than found later. The rejection is
  not permanent in the sense that word usually carries: the **next turn's start**
  writes `working`, which is an edge and writes unconditionally, which is a newer
  value, which clears the slot. So a rejection lasts until the agent next does
  anything, and never longer. And the writer/daemon disagreement about what
  "standing" means is harmless in the direction that remains: a `blocked`
  rejected by rule 1 or 2 is seen by a re-assertion event as a state it
  *disagrees* with, so it writes, and the newer value clears the slot the
  ordinary way.

  One thing the plan should not do: reintroduce the split as a "small
  improvement". The slot is one field, the difference was a flag on it, and the
  flag is now known to have a measured failure mode rather than merely an
  unproven one.

**Across a restart both slots are empty, and revision 2's answer to that is
withdrawn.** Revision 2 said a report that arrived before the restart "is almost
certainly a real turn end" and re-derived `finishedAt` from it immediately —
which resurrects precisely the subagent false-idle rule 3 exists to catch, with
the word "almost" carrying the argument. Instead: **after a restart the standing
report is a first sight, and a first sight is unverified.**

- A standing resting `idle` **enters the verification window** rather than
  skipping it. With a client connected the done badge is up to `N_idle` polls late
  after a restart; with no client there is nothing to verify and the derivation
  is immediate, unchanged — which is the case the app exists for.
- A standing `blocked` that had been dropped on evidence is accepted again and
  re-adjudicated: rule 1 drops it on the next changed hash, rule 2 after
  `N_blocked` settled polls. So a restart can re-show a false `blocked` badge for
  up to `N_blocked` polls. That is the honest cost of holding the rejection in daemon memory rather
  than in tmux: bounded, one-shot, and only on restart.

The alternative — writing the rejection back into the option, so it survives with
the report — is rejected on a rule this design has held since "Not
`@wterm_label`": the daemon reads that option, it does not write it. A reader
that edits the channel it reads cannot be reasoned about when two of them run,
and nothing promises `wterm-web` is a singleton.

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
is never unset as part of normal operation.** Turn end is an *edge* event, so it
writes unconditionally; the only events that check before writing are the two
that re-assert a state the agent is already in, and they are a different thing
with a different section — "Re-assertion is not a report".

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
  daemon has no better information. *Accepted by this filter is not the same as
  believed* — a first-sight resting report still has to earn its way past the
  evidence rules, which after a restart it has not yet done. See "What
  "dropped" means".
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
  the app exists for, so it is the right way round. On a restart the standing
  report is a first sight and **enters the window rather than skipping it**.
  Revision 2 re-derived immediately there, on the grounds that a pre-restart
  report "is almost certainly a real turn end", which is the subagent false-idle
  waved through by an adverb. See "What "dropped" means".
- **Derivation is safe only because a resting state is never re-asserted with a
  later timestamp.** `seen` stores the value a device was shown, so a second
  `1;idle;<later>` on an unchanged pane re-badges every device that had already
  looked. That is prevented at the writer and not here — "Re-assertion is not a
  report" — which makes this the one reader-side invariant in the design that
  rests on writer-side behaviour. Named, rather than left to compose quietly with
  the next thing somebody adds.

### A late repaint re-lights a cleared badge, and it cannot reach a reported finish

New in revision 5, from the measurement, and it is round one's badge storm
arriving through a door this document had not counted: not through a report and
not through a re-assertion, but through the classifier.

**What was measured.** On 2 of 30 claude turns, a single line near the input box
repainted **5.0 s and 9.0 s after everything else on the screen had stopped**.
Both were on a "write a file, then reply done" prompt; it is not periodic and it
does not reproduce on demand. It does not stop the turn settling — the classifier
had already said idle. What it does is produce a **second working→idle edge and a
fresh `finishedAt` stamp about 13 s after the real turn end**: the repaint at
+9.0 s, plus `settleAfter` polls, plus wherever the grid lands. Since
`finishedAt` is compared against each browser's stored `seen` *value*, a stamp
13 s later than the one a device was shown re-lights a done badge the user has
already cleared. That is a badge-integrity failure independent of `N`, and
widening `N` is not the answer: covering a 9 s late repaint takes `N ≈ 8`, and a
window that wide starts accepting mid-turn stillness as corroboration, which is
finding B.

**First the question that decides whose problem this is: can it reach
`finishedAt` on an integrated pane?** It cannot, and the two reasons are
independent, which is worth having because either alone would be a coincidence.

- **A fresh report outranks the classifier for the derivation.** A pane whose
  accepted report is a resting `idle` has `finishedAt` equal to *that report's
  own timestamp*. `idle` is a resting state and does not expire, so the report
  stays the authority until a newer report, the command check, or pane death.
  Whatever the classifier computes underneath is not what the row carries.
- **The capture is skipped, so the classifier does not even see it.** Outside the
  verification window a fresh report suppresses the capture entirely. A repaint
  at +5.0 s or +9.0 s on a pane whose window has closed is not observed at all,
  and the pane's `paneState` entry has already been dropped by `Retain` — it was
  not captured, so it was not retained.

**And the one boundary where it could have leaked is already closed, by
machinery this document already has.** Inside the verification window the pane
*is* captured, so a late repaint at +5.0 s can land on window poll 3 or 4. It
cannot stamp anything, because a capture-skipped pane's classifier baseline is
dropped and window poll 1 is therefore a **first sight**, which leaves
`everChanged` false — and `everChanged` is precisely the flag that licenses a
`time.Now()` stamp. That rule was written for a different reason (a retained hash
does not answer the question `Observe` asks) and it turns out to cover this one.
Of the four candidate mechanisms, that is the only one that applies: the
**ordering filter** works on report timestamps and a classifier edge has none;
the **rejection slot** needs a report to reject and the panes this bites have
none; and the browser's **`seen`** is not a defence here, it is the thing being
defeated, since `isDone` is `finishedAt > seen` and a fresh stamp is strictly
greater than the value the device stored.

**So this bites where the classifier is the authority**, which is: a pane with no
integration at all — the common case, and pure v2 — and a pane whose report is
not in force, which this design creates in three places it already documents (a
report dropped by an evidence rule, a `working` report expired after a missed
turn-end event, and a `notification_type` demoted for want of a grammar). **It is
a v2 defect this design inherits rather than one it introduces**, and the
inheritance is deliberate: the classifier is kept precisely so that a pane
without a report still gets a state. So it is recorded here and **scoped as its
own task** rather than folded into the reporting mechanism: the fix is a change
to `state.go`'s stamping rule, this design otherwise does not touch that file,
and its constant is a number nobody has measured yet (question 13). A design that
is otherwise ready should not wait on it.

**The reason it cannot be fixed by looking harder at the hash, which is the thing
to write down so nobody tries.** The obvious repair is to require *more* movement
before arming an edge — a run of changed polls rather than a single one, by
symmetry with `settleAfter`. It does not work, and the same measurement refutes
it: **a genuinely short turn presents exactly one changed poll too.** In 29 of 72
claude turn-phase replays and 19 of 72 opencode ones, the entire turn — prompt
echo, answer, prompt box redrawn — changed the screen at exactly one poll of the
1.5s grid before settling. A late repaint and a two-second turn are the same
signal at the hash level, and any threshold that discards the first discards the
second. That is not a limitation of the threshold; it is what the classifier is:
**it dates our noticing, where a report dates the finish.** Finding A is
therefore one more argument for the integration rather than a defect in it.

**The minimum that closes it, and it is one conjunct.** The stamp guard in
`Observe` already reads `p.still == settleAfter && p.everChanged && !blocked`. Add
a **re-stamp dwell**: do not stamp a new finish within `lateRepaintDwell` of the
one already recorded on that pane.

- It needs no new state — `p.finishedAt` is already there — and no clock inside
  the classifier, because `now` is already a parameter and the purity rule holds.
- It is not symmetric with the other guards and should not be described as one:
  the others decide whether *this* run earned an edge; this one decides whether a
  second edge so soon after the first can be a different finish at all.
- **The constant is a guess, flagged as one, of the same kind as the 60-second
  window.** It is floored by the measurement — the second edge landed 12 to 13.5 s
  after the first — and the sample behind that floor is **two events**, which is
  a bound and not a distribution. 15 s is the value to start from, biased long,
  and question 13 is the measurement it wants.
- **Biasing long has a cost and it is the right one to pay.** Two genuine
  finishes inside the dwell collapse to a single badge — the user would have to
  have looked at the pane between them, and then walked away within seconds, for
  that to lose anything. Against that, the failure it prevents was measured at 2
  of 30 claude turns.
- **It applies to the classifier's stamp only, never to a report's derivation.**
  A report dates its own finish, and a resting state is never re-asserted with a
  later timestamp (writer-side, "Re-assertion is not a report"), so there is
  nothing there for a dwell to protect against and adding one would silently
  swallow a genuine second turn end.

The plan should carry this as its own small task against `state.go`, separable
from everything else here and testable on its own: a fixture of one turn's worth
of changed captures, a settle, a stamp, then a single changed capture and a
second settle, asserting the second stamp does not happen — and its sibling, two
real runs separated by more than the dwell, asserting that it does.

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
sanitizes the text, and performs one `tmux set-option` — preceded, on the two
events that can re-assert a resting state, by one `show-options -p -v` read; see
"Re-assertion is not a report". The integration files are
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
  no-op. And on the hook that fires most often the sharp edge is not even
  reachable: with `"async": true` and no `"asyncRewake"`, Claude ignores the exit
  code entirely. That is a reason to keep exiting 0 rather than a reason to stop
  — two independent things would have to change before an exit code could hurt,
  and the second one is a line in a file the user owns.
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
  built an argument on that; the argument is withdrawn below. Revision 3 adds the
  part that makes the exit-0 rule doubly safe: an async command hook's **exit
  code is ignored, including exit 2**, unless it also sets `"asyncRewake": true`
  — the docs give "exit code 2 wakes Claude" as the thing `asyncRewake` adds, and
  the timeout section says enforcement is skipped for an async command hook.
  Documented *adjacent* rather than verbatim, so it is belt to the exit-0 braces
  and not a replacement for them, and it carries one hard installer rule:
  **`asyncRewake` is never set**, on any hook this project writes.

So: **fire and forget, everywhere.** Spawn, do not await. The spawn itself is a
few milliseconds and that is the budget.

The one place `report` does two tmux calls rather than one — the read before a
re-assertion write — costs a second few-millisecond fork inside a hook nobody is
waiting on: the two re-assertion events are `Notification`s, which fire while
Claude is already idle, and the hook is registered async. It is not on any turn's
critical path, and it does not apply to `PreToolUse`, which is the hook whose
volume this section is actually about.

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

**"Every one of these filters fails closed" was revision 2's claim, and it is not
implementable.** It cannot be, and the reason is in the payloads: for two of the
three agents the root session is coded by the **absence** of a field. Claude
documents `agent_id` as "present only when the hook fires inside a subagent
call"; opencode's root is the session with no `parentID`. For those two, "missing
means silence" silences the root — which is all normal reporting. Revision 2 held
that sentence and, three paragraphs from it, "absent means root, anything else
means silence", which fails *open* on absence. Both stood. Restated per filter,
because they are not the same kind of thing:

| Filter | Coding | Failure direction |
| --- | --- | --- |
| pi: `ctx.mode === "tui"` | presence | **Closed.** A missing or unexpected `mode` reports nothing |
| claude: `agent_id` absent | absence | **Open.** A payload that lost the field — renamed, nested, dropped in a refactor — reads as root |
| opencode: no `parentID` | absence | **Open.** Same |

An absence-coded filter can be *tightened* but not inverted. The integration can
require that the payload parsed, that it is the shape it expects, and that the
other fields it knows about are present, so that a wholesale schema change is
caught rather than read as a root event. It cannot distinguish "this field is
absent because this is the root" from "this field is absent because it moved."
Nobody can, and a design that says otherwise is describing a filter it has not
written.

**So what actually protects the badge has to be re-derived, and honestly.** The
only failure that matters is a subagent producing a resting `idle`: a subagent's
`working` on a working root is true, and a subagent's `blocked` is off Claude's
whitelist entirely now and adjudicated by rules 1 and 2 on the other two. So the
analysis is per *event*, not per agent:

- **claude's turn end is `Stop`, and the structural guard is real but narrower
  than revision 3 claimed.** `SubagentStop` is a *different hook* and we do not
  register it, so for a **Task-tool subagent**'s turn end to reach us, subagent
  completions would have to start firing the `Stop` hook — a far larger break
  than a field moving. That is the whole of what the structure buys. Revision 3
  wrote it as though it covered everything that is not the root session, and two
  neighbouring doors are open:

  - **A nested `claude` CLI in the same pane. Certain**, and already recorded
    under another name in open question 11 as "any agent started from another
    agent's shell". A `claude` spawned from a Bash tool call inherits
    `TMUX_PANE`, loads the same project `.claude/settings.json`, and is the
    **root of its own session** — so it fires its own `Stop`, correctly, and
    writes `idle;<now>` onto a pane whose outer agent is still working. No field
    test can catch it: both processes are roots, `agent_id` is absent for both
    because it is absent for every root, and the two share nothing to compare.
    This is not hypothetical in this repo's own development.
  - **Teammates and background sessions, which are not subagents. Unknown**, and
    never considered before revision 4. They have their own hook events —
    `TeammateIdle`, `TaskCreated`, `TaskCompleted`, carrying `teammate_name` —
    none of which we register; they carry no `agent_id`, which is documented as
    subagent-only; and they run under a **separate supervisor process with no
    terminal attached**, described as a full conversation that keeps running
    without a terminal. What the docs do not specify is that supervisor's
    environment inheritance. If those hook processes carry no `TMUX_PANE`,
    `report` no-ops and the entire class is out of scope for free. If they
    inherit a **stale** `TMUX_PANE` from the pane that dispatched them, it is the
    nested-CLI door again, and `Stop` is not the only hook that could come
    through it. Nobody knows which, so it is a measurement before it is a design:
    questions 9 and 11.

  One more thing the payload says, because "`Stop` means nothing is happening" is
  the natural misreading: the `Stop` hook input carries a `background_tasks`
  array whose entries have a `type` — `shell`, `subagent`, `monitor`,
  `workflow`, `teammate`, `cloud session`, `MCP task`. A `Stop` can therefore
  fire with work still running under the session. It does **not** change what we
  report: the root session is waiting on the user, which is exactly what `idle`
  means here and exactly what the sidebar is being asked. It is noted because it
  is also the clearest evidence in the payload that those supervisor processes
  exist at all.

  The `agent_id` guard remains a second pass over `PreToolUse`, `Notification`
  and `UserPromptSubmit`. It is worth having, and Claude's idle path rests on it
  no more than revision 3 said — but it now rests on rather less than revision 3
  implied, because the structure it was contrasted against covers one of the
  three classes rather than all of them.
- **pi's is `agent_settled` and opencode's is `session.idle`, and those are field
  tests that fail open.** Behind them there is exactly one thing: evidence rule
  3, which needs a connected client, needs the pane to keep churning for a whole
  window, and is now floored by `settleAfter` so an ordinary repaint does not
  trigger it. **With no client connected, a subagent false-idle on pi or opencode
  is undefended.** That is the true state of it. It is not two independent
  mechanisms; it is one mechanism that only runs when somebody is watching — and
  since revision 5, one mechanism that leaks at a measured rate even then, because
  a root waiting on the model goes still often enough for the classifier to
  corroborate a false idle on about 1 turn in 20. Finding B does not change what
  should be built here; it changes how much this backstop is worth, and therefore
  how much the writer-identity field below is worth.

Two things follow rather than being asserted. First, the fixtures that matter
most are a real `agent_settled` and a real `session.idle` captured *while a
subagent is running*, which is why "Testing" names them specifically instead of
asking for a subagent payload per agent in general. Second, the shape of a real
fix is already in the open questions under another name: a **writer-identity
field**, so a resting report can be refused when the standing `working` came from
a different writer. It was raised for cross-agent nesting (question 9); it
answers this too, and the read-before-write machinery added for re-assertions is
already most of what it needs. Revision 3 hoists it and does not adopt it: "which
session" needs defining per agent and measuring, and adopting it would put a
second reader-side invariant on writer-side behaviour.

One more pi-specific detail worth carrying over: pi re-derives activity as
`ctx.isIdle() === false` on `session_start`, because a reload can replace the
extension mid-run without another `agent_start`. An extension that only ever sets
state on transitions comes back from a reload believing nothing is happening.
**That re-derivation's idle branch is a re-assertion, not an edge** — a reload
can recur arbitrarily often inside one resting period, so as an edge it would
write `idle;<now>` and re-badge every device on every reload. See the criterion
in "Re-assertion is not a report"; revision 3 carried this event here and
classified it nowhere.

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
| The same, but with **no client connected**, and the agent finishes before anyone reconnects | Evidence rule 2, new in revision 2. On reconnect the screen never changes, so rule 1 can never fire; a settled screen matching no registered form over `N_blocked` polls drops the report. Revision 1 listed this row as handled when only the row above it was |
| An agent genuinely blocked at a dialog no registered form matches | Rule 2 drops the report after `N_blocked` polls and the classifier reads the static screen as idle. A true `blocked` downgraded to `idle` — the accepted cost of rule 2. Revision 3 stops the whitelist from *manufacturing* this case: an entry may claim `blocked` only where a grammar can confirm it, so what is left inside the bound is a dialog captured once and since restyled |
| An MCP elicitation form, or the quota press-Enter banner, waiting overnight | **No badge.** The type is demoted to *ignored* for want of a grammar, so the pane reads `working` until the 60s expiry and then nothing, with no client connected. This is a real loss against what revision 2 claimed and is identical to what revision 2 would have delivered once anyone opened the app. Four screen captures and four registered forms buy it back, and each is a code change rather than the data change revision 3 promised — `blockedRules` holds one `dialog` per agent and claude's slot is taken |
| `Notification(agent_needs_input)` | Ignored, **pending a capture** — not permanently, which is revision 3's plank corrected. One half of the type is a background session or a teammate, which is not this pane's root session; the other half is *this* session asking a teammate's terminal setup question, which is this pane's root session waiting on this pane's screen. So it joins the pending-a-capture class, and if its form is captured, rule 2 keeps the first half honest |
| A **Task-tool subagent**'s turn-end event, claude | Structural: `SubagentStop` is a different hook and is not registered, so `Stop` is root-only *against this class*. The `agent_id` guard is a second pass over the other three hooks. Revision 3 wrote this row as covering everything that is not the root session; the two rows below are what it did not cover |
| A nested `claude` CLI in the same pane (spawned from a Bash tool call) | **Not handled, and certain.** It inherits `TMUX_PANE`, loads the same project `.claude/settings.json`, and is the root of its own session, so it fires a legitimate `Stop` and writes `idle` onto a pane whose outer agent is working. Both processes are roots, so `agent_id` is absent for both and no field test can separate them. Question 11; not hypothetical in this repo's own development |
| A teammate, background session or its supervisor firing a hook | **Unknown.** They are not subagents: their own hook events (`TeammateIdle`, `TaskCreated`, `TaskCompleted`) are not registered, they carry no `agent_id`, and they run under a supervisor with no terminal attached whose environment inheritance is undocumented. No `TMUX_PANE` means `report` no-ops and the class is out of scope; a stale `TMUX_PANE` means it is the nested-CLI door again. A measurement, in questions 9 and 11 |
| A subagent's turn-end event, pi or opencode | Filtered on `ctx.mode` (presence-coded, fails closed) and `parentID` (absence-coded, **fails open**). Behind the opencode filter there is only evidence rule 3, which needs a connected client. **With no client connected this is undefended**, which revision 2 obscured by calling the filter and the rule "two independent mechanisms" |
| A `Notification` whose `notification_type` we do not recognise | Ignored. No write, no state change, no timestamp refresh. Revision 1 would have written `blocked` — a permanent false badge on a resting state |
| `Notification(idle_prompt)`, ~60s after every turn | A **re-assertion** event: the writer reads the standing option and writes only if it disagrees. Standing `idle` — silence, no newer timestamp, no badge. Standing `working` (a missed `Stop`) — writes `idle` and repairs the pane from the agent. Revision 1 reported `blocked` here; revision 2 reported `idle` unconditionally and thereby re-badged every device once per turn, because `finishedAt` is derived from the report's own timestamp and `seen` stores the value it was shown |
| A report dropped on evidence, still standing in the option next poll | Re-rejected from the per-pane rejection slot without re-evaluating the evidence, whichever rule condemned it. A rejection never advances the ordering filter; a newer value clears the slot — and the next turn's `working` write is one, so a rejection lasts until the agent next does anything and never longer. See "What "dropped" means" |
| A true turn end whose repaint races the turn-end write | Rule 3 does not drop it, at `N_idle = settleAfter + 2`. **Measured rather than reasoned**: the turn-end event fires 7–52 ms *before* the agent's last repaint, so a poll can be spent on the pre-final screen, and polls-to-settle was 3 or 4 in every one of 1,440 exact-timing phase replays across 88 turns and never 5. Every turn on every agent had some poll phase at which it took 4, so `N = 3` would discard ~1 true turn end in 50 on claude and opencode and ~1 in 170 on pi — silently, since the badge just falls back to the classifier. Revision 2 dropped on a single changed hash, which would have discarded 100% of the 88 turns; revision 3 fixed that and set the window one poll too short |
| Rule 3 drops a **true** idle anyway — the window was one poll too short | The report stays rejected until a newer value, like a rule-1 or rule-2 drop; revision 4's provisional clearing is **withdrawn** in revision 5. The badge is not lost: a rejected report hands the pane to the classifier, which churned all through the window and so has `everChanged` set, and stamps its own `finishedAt` when the screen settles. The turn's finish is dated by when the daemon noticed instead of by when the agent finished — ordinary v2 degradation. Measured: 0 of 1,440 replays needed a fifth poll |
| Mid-turn stillness makes the classifier agree with an idle report before the turn ended | **Finding B, measured at 4 of 88 turns.** Rule 3's `idle` verdict is corroboration, not verification, so a subagent's false `idle` survives whenever the root's own model-wait stillness falls inside the window. Not fixed — sized. It is the reason `N_idle` is not padded above the measured value, and the reason a rule-3 rejection is no longer cleared by a classifier `idle` verdict |
| A late repaint 5–9 s after a turn has visibly ended | **Finding A, measured on 2 of 30 claude turns.** On a pane whose report is in force it reaches nothing: the report outranks the classifier for `finishedAt`, and the capture is skipped so the classifier never sees the repaint; inside the verification window it cannot stamp either, because window poll 1 is a first sight and `everChanged` is false. On a pane the classifier owns — no integration, or a report dropped, expired or demoted — it stamps a second finish about 13 s late and re-lights a cleared badge. A v2 defect this design inherits; the minimum fix is a re-stamp dwell in `Observe`, and no hash-level rule can do better, because a two-second turn presents the same single changed poll |
| Two writes landing out of order (a delayed `working` after a `blocked`) | The daemon refuses a report whose timestamp is not strictly newer than the last it accepted. Ordinary scheduling jitter, not a broken integration, and revision 1 did not account for it |
| Claude and pi in the same pane (an agent run inside another agent's shell) | **Not handled.** Both integrations see the same `TMUX_PANE` and write the same option, and the last writer wins with no way to tell whose turn ended. See the open questions |
| A report containing `0x1f` or a newline | Impossible from our writer, which strips control characters. From a hostile writer, tmux substitutes both to spaces before Go sees them, and the report is the only variable field in its own format string, so a survivor costs one report and never a pane |
| A second, unsanitized field appended to `Format` by a later feature | The thing this design deliberately does not do. `Format`'s last slot is `@wterm_label`'s and the hardening's comments say why; the report reads through its own format string instead |
| A report with an unknown schema version | Ignored whole. The classifier decides, as if no integration were installed |
| A report with an unparseable timestamp | Discarded whole. A report we cannot date is a report we cannot age |
| A report dated in the future | Discarded whole if it is more than a few seconds ahead, rather than treated as stale. A resting `idle` derives `finishedAt` from its own timestamp, and a future `finishedAt` is a `done` badge that `seen` can never catch up with |
| An integration installed mid-run | The first report is a first sight. No `finishedAt` edge, so no badge storm |
| Daemon restart | Reports are unaffected: they live in tmux, not in the daemon's memory, and `finishedAt` is *derived* from a resting `idle` report rather than stamped on an edge — so a finished agent still badges after a restart. That is better than v2, which has to reset the whole classifier map. Cost, revised: the ordering filter **and** the rejection slot are empty, so the standing report is a first sight — accepted by the ordering filter and then **verified from scratch**. A resting `idle` enters the verification window instead of being re-derived immediately (revision 2's "almost certainly a real turn end" is withdrawn), and a `blocked` that had been dropped can re-badge for up to `N_blocked` polls before rule 1 or 2 drops it again |
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
- **The whitelist and the form registry asserted consistent, at form
  granularity.** Revision 3 specified this as "every whitelist entry that maps to
  `blocked` must name an agent with a `blockedRules` grammar" — which is
  **vacuous**, because claude *is* in the map: the three types revision 3 demoted
  would all have passed, so the test written to stop round two's blocker
  recurring could not fail for it. Respecified: every `blocked` mapping names a
  **registered screen form** (`claude/permission` today), and the test goes red
  when somebody maps a `notification_type` to `blocked` without registering that
  form's grammar. It is still a composition test on purpose — the two tables live
  in different files, each is correct alone, and the contradiction is only
  visible from above — but it is now at the granularity that can actually fail.
  A second assertion at the same granularity: rule 2 drops only when **no**
  registered form for the agent matches, so a report naming one form standing
  while another form is on screen must not drop.
- **The edge/re-assertion table, keyed by (event, state) pair**: an edge writes
  without reading; a re-assertion reads first and writes **nothing** when the
  standing report already carries that state, and writes when it carries a
  different one. Assert on the tmux calls made, not only on the value that ends
  up stored, because "no write" and "a write of the same state with a newer
  timestamp" store values that look alike and badge differently. Three rows carry
  more weight than the rest: pi's `session_start` **idle** branch must read
  before writing (as an edge it re-badges every device on every extension
  reload — the bug revision 3 shipped by leaving the event unclassified), its
  **working** branch must not, and a turn-end event must write unconditionally,
  which is only safe because turn start writes `working` first. That last
  invariant gets its own test: a turn with no `working` write before its turn end
  is the shape that would lose a badge, and the test asserts each integration's
  turn-start event is present in the table as a `working` edge.
- **The `working` expiry as a pure function with an injected clock.** 60 seconds
  is a number that will be changed; it should be changeable by editing one
  constant and re-reading one table, with no sleeping and no real time anywhere
  near it.
- **The blocked-verification drops**, both of them, and rule 2's test needs a
  true premise rather than a counter. Revision 2 specified it as "`N` settled
  captures drop, `N-1` do not", which tests the counter and cannot see the
  blocker: its premise, that a reported `blocked` has a matchable dialog, was
  false for four fifths of the whitelist. The fixtures are the **captured claude
  permission screen** with the dialog present (does not drop, at any `N`) and the
  same screen after the dialog is answered (drops at exactly `N`, not at `N-1`).
  The off-by-one is still the point; it is no longer the only point.
- **The idle verification window** (rule 3), and specifically the turn-end race
  that revision 2's version failed *and revision 3's window was too short to
  survive*. The fixture is the measured one: a first sight, then one changed
  capture — the last repaint, which the turn-end event precedes by 7–52 ms — then
  `settleAfter` identical ones, `settleAfter + 2` polls in total, which **keeps**
  the report and derives `finishedAt` from its timestamp. Written
  against the constants and never against literals: the same fixture at
  `N_idle = settleAfter + 1` drops the report, which is exactly the off-by-one
  revision 3 shipped, so a test hard-coding `3` and `4` would pass through the
  next change to `settleAfter`. Also: a screen that changes at every poll for the
  whole window drops it and derives nothing; the window **closes at the verdict**
  rather than running out the count, asserted on the captures the poller makes
  and not only on the state it ends with, since a report accepted at poll 3 and
  one accepted at poll 4 look identical from the outside; and with no client
  connected the derivation is immediate, because there is nothing to verify.
- **The classifier baseline for a capture-skipped pane is dropped.** A pane whose
  capture is skipped for several polls and then enters a verification window is a
  **first sight** to `Observe` — asserted by the poller passing only the panes it
  actually captured to `Retain`, and by `everChanged` being false on the first
  window poll, so no `time.Now()` finish edge can be stamped inside the window.
  This is the premise rule 3's arithmetic rests on and revision 3 never stated
  it, which is how the floor came out wrong in two different ways at once.
- **A rejection means the same thing whichever rule made it, and a classifier
  `idle` does not clear one.** This is the test that holds revision 5's
  correction of revision 4 in place, and it has to be written as the negative:
  after a rule-3 drop, a later poll at which the classifier reports `idle`
  changes nothing — the report stays rejected and no `finishedAt` is derived from
  it — and the same is true after a rule-1 or rule-2 drop. What *does* clear the
  slot is a newer value, and the fixture for that is the next turn's `working`
  write. Assert the three rules in one test: the slot is one field, and the
  behaviour revision 4 wanted is exactly the kind a reader will re-add as an
  improvement unless the test says why it is wrong.
- **A rule-3 rejection does not cost the badge**, which is the claim that makes
  the rule above affordable. Same fixture, continued: the pane is back on the
  classifier, the classifier's `everChanged` is set because the screen churned
  through the whole window, and when the screen finally settles the classifier
  stamps its own `finishedAt`. Assert that it lands, and that it is dated `now`
  rather than the report's timestamp — the two are different facts and the test
  should be able to tell which one it got.
- **The late-repaint dwell in `Observe`** (finding A), against `state.go`
  directly and independent of everything else here: a run of changed captures, a
  settle, a stamp; then a **single** changed capture and a second settle, which
  must **not** re-stamp; then the same again beyond `lateRepaintDwell`, which
  must. Written against the constant, and paired with the test that keeps the
  obvious wrong fix out — a turn whose whole visible life is one changed capture
  followed by a settle **does** stamp, because a short turn and a late repaint
  are the same signal and only the dwell separates them.
- **The ordering filter and the rejection slot, together**: a report older than
  the last accepted one is refused and the previously accepted state stands; a
  strictly newer one is taken; a report rejected on evidence is re-rejected on
  the next poll **without the evidence being re-evaluated**, and does not advance
  the accepted-timestamp; a newer value clears the slot; and after a simulated
  restart the standing report is accepted by the ordering filter but a resting
  `idle` enters the verification window rather than deriving immediately.
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
  `0x1f` and a newline as spaces**, against a real server, with a **benign
  fixture that contains a lowercase `n` and a real newline byte**. Both traps are
  the reason and neither is hypothetical: `[[:cntrl:]]` expands to `""` for every
  value, and a bracket set retyped from a rendered `\n` leaves the newline alive
  while turning every `n` into a space. A benign fixture without an `n` in it
  passes both. Assert on the benign value surviving byte-intact, not only on the
  hostile one being cleaned.
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
- **Recorded hook payloads as fixtures**, one file per agent per event, and for
  Claude one payload per `notification_type` in the table. Two of them carry more
  weight than the rest and should be captured first: a real **`agent_settled`**
  (pi) and a real **`session.idle`** (opencode) taken *while a subagent is
  running*, because those are the two turn-end filters that fail open and the
  only evidence anybody will ever have about what they actually contain. These
  do not exist yet and capturing them is part of implementation, not of this
  design.
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

Revision 1 listed seven, one of them already answered. Revision 2 closed most of
question 1 on the documentation and added two. Revision 3 changed what question 1
*decides*, floored question 3, promoted the writer-identity idea out of question
9, and added two more. Revision 4 added no new questions and sharpened three.
**Revision 5 closes half of question 3 with a measurement, records what the same
run says about question 10's rarity, and adds one question that finding A brought
with it.**

1. **`PreToolUse`'s per-call cost, and nothing else about it.** Revision 1 asked
   three things here — the payload shape, the cost, and whether `"async"` exists
   at all. The first and third are documented: `tool_input` is a structured
   per-tool object (`Bash.command`, `Read.file_path`, …), and `"async": true` is
   a documented field that runs the hook in the background. What remains is a
   number: **fork volume under a burst of tool calls**, one hook process plus one
   tmux client each. Latency is off the critical path by construction, so this is
   about whether a tight `Bash` loop makes the machine notice. If it does, Claude
   drops to state-only by deleting one hook entry.

   **Revision 3 changes what this decides.** `PreToolUse` is also what makes
   Claude's connected-case blocked badge about six seconds slow instead of about
   1.5, because its fresh `working` suppresses the capture `IsBlocked` would read.
   So the measurement no longer decides a threshold ("is the fork volume
   tolerable") but a trade: the activity line and the disconnected-case keepalive
   against a four-and-a-half-second faster badge whenever somebody is watching.
   The recommendation still goes to `PreToolUse`; the fallback is now a
   defensible position rather than a retreat.
2. **The 60-second window for a transient `working` report.** Needs the
   distribution of inter-event gaps during real work on all three agents. Revision
   2 sharpens what to measure: the failure of expiring early is a false `idle` on
   a quiet screen, not merely an extra fork, so the case to measure is **a single
   long tool call that emits no sub-events, with no client connected**. The
   number is still a guess biased short. Revision 5 adds one data point from the
   other end and it is not reassuring: finding B measured screens sitting
   perfectly still for about six seconds *while the agent was waiting on the
   model*, which is the same silence this question is about, arriving from a
   cause nobody had listed. Six seconds is far inside the window; the question is
   what the tail looks like, and it is the same tail both questions want.
3. ~~`N_idle`~~ **`N_blocked` — and `N_idle` is answered.** `N_idle =
   settleAfter + 2`, **measured**: 88 turns across the three agents, 44 counted
   from the agent's own turn-end event, 1,440 exact-timing replays of the poll
   grid at every phase, polls-to-settle 3 or 4 and never 5, with every turn on
   every agent showing some phase at which it took 4. `R = 1` is a measurement
   now rather than an inspection, and the mechanism is that the turn-end event
   fires 7–52 ms *before* the agent's last repaint, so a poll can be spent on the
   pre-final screen. Revision 4's `settleAfter + 1` reasoning was sound on a
   false premise — that the report and the last repaint are simultaneous — and
   `N = 3` would have discarded about 1 true turn end in 50 on claude and
   opencode, silently. Recorded in "Reports are checked against the screen"; the
   value is expressed as `settleAfter + 2` in code and never as `4`.

   **What remains is `N_blocked` alone, and the measurement says nothing about
   it.** Rule 2 never has to absorb a repaint, because rule 1 drops anything that
   moves before rule 2 sees it, so `N_blocked` needs no `R` and its floor stays
   `settleAfter + 1`. Nobody should carry `4` across from `N_idle`. The trade is
   unchanged: too small and rule 2 drops true `blocked` on slow-repainting
   screens, too large and the overnight case takes longer to correct itself.
   Measuring it needs the thing question 10 is also waiting on — real dialog
   screens — which is why it is still open.
4. ~~Whether tmux can strip control bytes at read time.~~ **Answered by
   `f25e066`**, and left here because the answer is a trap rather than a yes.
   Three ways to get it wrong: `[[:cntrl:]]` blanks every value including good
   ones, a byte range is locale-dependent, and a bracket set retyped from a
   rendered `\n` covers neither target byte while turning every `n` into a space.
   What works is a set of the two *literal* bytes. Copy `labelField`
   (`internal/tmux/snapshot.go:108`) from source. Recorded in "The hazard" so the
   next person does not re-derive it the expensive way.
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
9. **A writer-identity field, now wanted by two problems rather than one.**
   Revision 3 promotes this out of the nesting question below, because the honest
   re-derivation of the subagent filters left pi's and opencode's turn-end guards
   failing open with only a client-dependent evidence rule behind them. A report
   that said *which session wrote it* would let a resting report be refused when
   the standing `working` came from somebody else — and the read-before-write
   added for re-assertions is already most of the machinery. Not adopted: "which
   session" has to be defined per agent and measured, it is a wire change, and it
   would be the second reader-side invariant resting on writer-side behaviour.
   What to measure first: whether pi, opencode and Claude each expose a stable
   session identifier in the payloads the integrations already receive.

   **Revision 4 adds a second measurement to this question, and it may make the
   first one unnecessary for one whole class:** does a **teammate, background
   session or nested-CLI hook process carry `TMUX_PANE`** — and if it does, is it
   its own or a stale one inherited from the pane that dispatched it? Claude's
   teammates and background sessions run under a supervisor process with no
   terminal attached, carry no `agent_id`, and have hook events of their own that
   we do not register; the docs do not specify what that supervisor's environment
   inherits. No `TMUX_PANE` and the class is out of scope for free, with no guard
   to write. A stale one and it is the nested-CLI door, where a writer-identity
   field is one of the few things that could help.
10. **Four screen captures the whitelist is waiting on, and a code change per
    capture.** `quota_auto_resume_stale`, `elicitation_dialog`,
    `elicitation_url_dialog` and — since revision 4 corrected the claim that it
    never describes this pane's root session — `agent_needs_input`'s
    teammate-setup-question form are demoted to *ignored* because no registered
    grammar can confirm them. Each promotes to `blocked` the day somebody
    captures its screen **and writes a grammar**, which revision 3 described as
    "a data change to `blockedRules`" and which is not: that map holds exactly
    one `dialog` per agent and claude's slot is taken, so a second claude form is
    a new type plus a registry that can hold several per agent. The quota banner
    is the cheapest capture to get (exhaust a quota), the two elicitation dialogs
    need an MCP server that asks for something, and the setup question needs a
    teammate. Until then those four waits are invisible to both authorities,
    which is the largest single hole in this feature's coverage.

    **Unchanged by revision 5's measurement, and that is itself the datum.** A
    3-agent, 88-turn run met **none** of the four screens and did not provoke
    them — no quota banner, no elicitation of either kind, no teammate setup
    question. That is not an answer and it does not shrink the hole: it is
    evidence that these screens are rare enough that ordinary use will not
    capture them by accident, which means somebody has to go and manufacture each
    one deliberately. Anyone budgeting this work should assume that rather than
    hoping a screenshot turns up.
11. **Cross-agent nesting**, which nobody had considered until the second review:
    Claude running inside a pi pane, or any agent started from another agent's
    shell. Both integrations see the same `TMUX_PANE`, both write `@wterm_agent`,
    and the last writer wins — so the inner agent's turn end can report `idle`
    for a pane whose outer agent is still working, which is the subagent failure
    again with no `parentID` or `agent_id` available to filter on because the two
    processes share nothing. The ordering rule does not help; both are
    legitimately fresh. Worth noting that this configuration is not hypothetical
    in this repo's own development. Nothing is proposed here beyond recording it.
    The candidates are the writer-identity field now standing as question 9,
    refusing to report when the pane's `pane_current_command` is not the agent
    doing the reporting, or accepting that the innermost agent owns the pane. All
    three want measuring first.

    **Revision 4 sharpens the claude half of this and widens it.** The
    nested-CLI case is not speculative and is not only cross-*agent*: a `claude`
    spawned from a Bash tool call in the same pane is a `claude` inside a
    `claude`, inheriting `TMUX_PANE` and the same project
    `.claude/settings.json`, firing a perfectly legitimate `Stop` as its own
    root. `agent_id` is absent for both, because it is absent for every root, so
    the guard revision 3 leaned on cannot see it — and neither can
    `SubagentStop` not being registered, which covers Task-tool subagents and
    nothing else. Teammates and background sessions may or may not be a third
    instance of the same door; see the measurement now attached to question 9.
12. **Whether `notification_type`'s documented list is closed.** It is not
    documented as closed, which is why the whitelist's default is "ignore". If it
    ever becomes closed, the default could tighten — but the default is cheap
    enough that this is curiosity rather than a blocker.
13. **How late a late repaint can be — the constant behind finding A's dwell.**
    Two observations, 5.0 s and 9.0 s after the screen had otherwise stopped,
    both on one claude prompt shape ("write a file, then reply done"), both on 2
    of 30 turns of one run. That is a bound, not a distribution, and the
    `lateRepaintDwell` proposed in "A late repaint re-lights a cleared badge" is
    floored by it at about 13.5 s and set at 15 s biased long. What to measure:
    whether the tail is a property of that prompt shape, of file writes, of
    claude's input box specifically, or of terminal apps generally; and whether
    any agent produces one beyond ten seconds. The measurement is cheap — it is
    the same harness with a longer idle watch and a per-line diff — and until it
    is done the dwell is a guess of the same standing as the 60-second window in
    question 2. It is also the reason the dwell is a separable task rather than
    part of this design: a constant nobody has measured should not be able to
    hold up the mechanism it protects.
