# tmux-web v2 — agent state and management

Date: 2026-09-10
Status: revision 2 — agreed, corrected after adversarial review
Revision 1 was wrong about the central signal; see "Detecting work".
Follows: `2026-09-09-tmux-web-design.md` (v1, shipped)

## Purpose

v1 answers "reach my tmux from a browser". It does not answer the question you
actually have when three coding agents are running: **which one needs me?**

The sidebar today shows `claude`, `claude`, `claude`. This adds what each one is
doing, whether it is working, waiting or finished, and the ability to create,
rename, split and kill without leaving the browser.

## Prior art

[herdr](https://github.com/herdrdev/herdr) solves the same problem from the
other side: it is a multiplexer that replaces tmux, with a Rust TUI, and reaches
remote machines over SSH. It is not a competitor to this app — it owns terminals,
we borrow tmux's, and it cannot be used from a phone because it is a TUI. Two of
its ideas are worth taking, and both are taken here:

- **Agent state on every pane**, rolled up to the container. Their pitch, "never
  hunt for the stuck one", is exactly the gap.
- **Strict blocked detection.** They only mark blocked on a positive match of a
  known approval UI, and fall back to idle otherwise. A false "this one needs
  you" trains the user to ignore the badge, which destroys the feature.

Worth knowing: herdr uses screen detection as the state authority for Claude
Code, not its hooks, because "their hooks do not cover the whole lifecycle" and
can miss permission results and interrupts. Their lifecycle-hook path applies to
opencode and Pi, not to Claude Code.

## Decisions

1. **The pane title is the sidebar label**, not a state signal.
2. **Working is decided by screen churn** — the tail of the pane changing —
   for known agents only.
3. **One capture per known agent pane per poll** answers both "is it working"
   and "is it blocked". There is no separate cadence and no idle gate.
4. **`done` is computed in the browser** from a server-reported finish time.
5. **Full management** — create, rename, split, kill, zoom — with kill behind a
   two-step dialog.
6. **Notification is a tab badge**, not Web Push.
7. **One machine.** No cross-daemon aggregation.
8. **Agents are identified by a small bundled logo.**

## Agent state

### The title is a label, not a signal

`#{pane_title}` joins the existing `list-panes` format string. It costs nothing,
and Claude Code sets it to what it is working on:

```
%6   title=[✳ Categorización productos southafrica]
%9   title=[✳ Product-join tasks southafrica]
```

That is a far better sidebar row than `claude`, and it ships regardless of
everything below.

**Titles are safe to parse, unlike paths.** tmux normalises titles through its
own OSC parser: a title set to `EVIL\x1fFORGED\nSECONDLINE\x1fMORE` reads back
as `EVILFORGEDSECONDLINEMORE`, one line, no `0x1f` (verified, twice). A title
containing `#{session_name}` is not re-expanded, and `#(cmd)` is stored defanged.
Paths come from the filesystem and never pass through that parser, which is why
they remain excluded from the snapshot.

Two properties to handle: the default title is the **hostname**, not empty, so
every plain shell carries one; and titles have no length limit — an 8KB title
was stored and reported in full. Truncate to 256 bytes server-side before it
rides a 1.5s poll into the DOM.

### Detecting work: screen churn, not title churn

**Revision 1 got this wrong, and the error is worth recording.** It claimed
working could be inferred from the title changing, on the reasoning that the
leading `✳` was an animating spinner. It is not. Claude Code rewrites the title
when its *task summary* changes — roughly when you send a new message — and
leaves it byte-identical while it works. Observed directly: a pane showing
`✽ Thinking… (15m 54s · ↓ 34.5k tokens)` with a cycling spinner and a ticking
counter held one identical title across minutes of work, and still held it after
finishing. The `✳` is on idle and working panes alike. opencode's title is a
static `OpenCode` and never changes at all.

So revision 1 verified that titles are a good *label* and silently generalised
that to being a good *state signal*. Everything downstream — working, the
working→idle edge, `finishedAt`, `done`, and the idle gate on blocked detection
— rested on a signal that is not there. Notably, revision 1 quoted herdr using
screen detection as the state authority for Claude Code, and then did not follow
it.

**The signal that is actually there is the screen.** A working agent redraws at
least once a second: the spinner cycles and the elapsed-seconds counter
increments. So for a **known agent pane**, each poll captures the pane and hashes
**the whole visible capture**, not a tail:

- Hash changed since the last poll → `working`.
- Hash identical for N consecutive polls → `idle`. N=2, so ~3s at the current
  interval.

The 1.5s poll cannot alias against a per-second counter, which is what made the
title-churn threshold fragile. Verified on both edges: a working pane's capture
differed at every 1.5s sample (spinner cycling, counter ticking
`16m 48s → 50s → 51s`), and four idle panes -- three Claude Code, one opencode --
hashed identical across eight consecutive polls.

**The whole capture rather than a tail**, because a tail needs a size nobody can
justify, a wrapped footer can push the activity line out of one that is too
small, the blocked matcher wants the full screen anyway, and the full-screen hash
was verified exactly as stable as a tail hash when idle. Hashing 54 lines instead
of 12 is free next to the fork.

Two caveats, stated rather than assumed. opencode has never been *observed
working*, so its busy screen is unverified -- worth confirming the first time one
is seen running. And a TUI that animates only colour would evade this, since
`capture-pane -p` without `-e` drops attributes; Claude Code animates glyphs, so
it is unaffected.

This is the cost the design deliberately accepts, and it is the cost the owner
scoped: **only known agents, only while a client is connected.** Three agent
panes is two forks a second. Non-agent panes — zsh, nvim, a build — are never
captured and never classified.

"A client is connected" means **the registry holds at least one live terminal
socket** — not that anyone is viewing that particular pane, which would make the
sidebar useless for exactly the panes you are not looking at. With no client,
captures stop and `AgentState` reports **empty**, not a frozen last value: stale
state presented as current is worse than none. When a client connects the
per-pane map starts empty, so states settle over the first two polls (~3s) and
stamp no finish edge — the same rule as a daemon restart.

### Blocked comes from the same capture

The capture that answers "is it working" also answers "is it waiting". There is
no separate cadence, and — correcting revision 1 — **no idle gate**. An agent can
raise an approval box while background work continues; gating the check on idle
would mean never seeing it. The live panes here show `← 1 agent` and `1 monitor`
in their footers, so that is not hypothetical.

`capture-pane -p -J` captures the **visible screen only**, and the tail is sliced
in Go. Revision 1 said `-S -8` and called it "the last 8 lines": it is not. On a
6-row pane `-S -8` returns 14 lines — the screen plus 8 lines of *scrollback*,
which is exactly where a just-answered approval box lives. `-J` rejoins a
question wrapped by a narrow pane.

Blocked is decided by matching the bottommost dialog box in the visible screen,
not a fixed-height window: Claude Code's permission dialog — command preview,
question, three numbered choices, hint — is routinely taller than eight lines,
so a fixed window can catch the options without the question.

Only a positive match sets `blocked`. No match means whatever churn decided,
never a guess. A false "this one needs you" trains the owner to ignore the badge,
which destroys the feature.

**Latency, stated rather than discovered:** a box appearing between polls shows
as blocked within one poll (~1.5s). Reaching idle still takes N=2 polls (~3s).

Captured text is matched in memory and discarded — never written to the state
file, never logged, never in an error message.

### The upgrade path, when forks stop being free

A read-only `tmux -C` control-mode sidecar — already named in v1 as the poller's
eventual upgrade — would deliver `%output` push-based, giving churn with no forks
at all. Capture-hashing degrades into it cleanly, since both answer the same
question. Not worth building now: three panes at two forks a second is nothing.

### Ship order

`blocked` as a state ships before question extraction. The extraction grammar is
the most fragile piece in the design and the badge is useful without it, so
parser rot should degrade to a missing quote rather than a wrong state.

### What counts as a known agent

A configured list of `pane_current_command` values, defaulting to `claude`,
`opencode`, `pi`. It gates capture, classification, and the logo together, so it
is one list and not three. A command not on it gets no capture, no state, and no
logo, and shows exactly what v1 shows today.

A pane whose command changes mid-life — an agent exits back to a shell — simply
stops matching on the next poll, and its per-pane state is dropped.

## What crosses the wire

`Row` gains session identity as well as agent fields. Identity first, because
revision 1 missed that it was broken.

**Sessions have ids, and the snapshot must carry them.** Revision 1 said sessions
"have only names". They do not: `$0`, `$1` are stable targets, and
`rename-session -t '$0'` and `kill-session -t '$3'` both work. Worse, verified:
**`session_group` keeps the original name after a rename.** Renaming `work3` to
`api` leaves `group=work3` on every member, and `kill-session -t '=work3'` then
fails with "can't find session" while the session lives on as `api`.

Since v1 keys the sidebar on `#{?#{session_group},#{session_group},#{session_name}}`,
and a group always exists while any browser tab is open, **rename as designed in
revision 1 would appear to do nothing** — the sidebar would show the old name
forever and later name-addressed calls would fail. So the row carries
`#{session_id}` for addressing and the live `session_name` of the preferred
non-app member for display, and every session operation is addressed by `$id`.

**Windows have ids too, and the same argument applies.** Revision 1 listed no
window id here while addressing windows by `@N` throughout the section below —
so the row carried `#{window_index}` and nothing else, and a window index is a
position, not an address: tmux renumbers indices on `move-window` and reuses
them after a kill. `ValidateWindowID` refuses anything but `@N`, so a snapshot
without the id leaves `PATCH /api/windows/{id}` and `DELETE /api/windows/{id}`
implemented on the server and unreachable from the browser — nothing on the wire
names a window. The sidebar is expected to omit the menu entries it cannot
serve, which is why the gap reads as a missing feature rather than a broken one.

With that fixed, `Row` gains:

```go
SessionID  string `json:"sessionId"`  // $N, stable; what operations target
SessionName string `json:"sessionName"` // live name for display, not the group's
WindowID   string `json:"windowId"`   // @N, stable; what window operations target
Title      string `json:"title"`      // sanitised by tmux, truncated to 256B
Label      string `json:"label"`      // @wterm_label, user-set; "" if unset
AgentState string `json:"agentState"` // "" | working | blocked | idle
FinishedAt int64  `json:"finishedAt"` // unix ms of the last working→idle edge
```

Plus, only when blocked and only once extraction ships, a `question` object:

```go
type Question struct {
    Text    string   `json:"text"`    // the request, one line, truncated
    Choices []string `json:"choices"` // e.g. ["Yes", "Yes, and don't ask again", "No"]
}
```

**Truncated means capped, and the choices are capped too** — 256 runes each,
`MaxQuestion`. Nothing on the screen bounds either one: `capture-pane -J`
rejoins a question wrapped across rows, and the extractor rejoins a choice's
continuation lines the same way, so one line of a wide pane is one long string
riding every 1.5s poll into the sidebar and the tooltip. That is the hazard
`MaxTitle` exists for, arriving through the other field. Runes rather than
bytes, for `MaxLabel`'s reason: the cap is a column budget for a row, and a byte
cap cuts a question in half in any language that is not English.

`AgentState` is empty for anything not recognised as an agent, so the frontend
never decides what counts.

### Server state

The poller gains one in-memory map keyed by pane id: **the previous
capture hash**, how many consecutive polls it has been unchanged, and the last
working→idle timestamp. (Revision 1 stored the previous *title* here; that is the
machine this revision exists to remove.) Pane ids are unique per server and stable
for a pane's life. The map is pruned to whatever the current snapshot contains,
so a closed pane's entry disappears on the next poll.

It is rebuilt from nothing on restart, so every agent reads as `working` until
two identical polls settle it — two polls, ~3s, not one.

**It is also rebuilt when the tmux server's generation changes**, for the same
reason the browser's `seen` keys carry that generation: pane ids restart at `%0`
with the server, so an entry keyed on `%1` outlives the machine it describes.
While the server is down the snapshot fails and the poll returns before
classification, so nothing prunes it; on the first poll against the new server a
fresh pane reusing `%1` inherits the dead server's hash and its "a real change
was seen" flag, and stamps a finish edge two polls later. The browser cannot
suppress that one — its map is keyed on the *new* generation and is empty — so
it is an unviewed `done` badge on an agent that has only just started. Only a
*change* of generation resets; a poll gap on the same server does not, because
an edge after a snapshot outage reflects work that really happened.

**And the classifier is told when a capture is a dialog.** A box waiting on the
owner is byte-identical poll after poll, which is exactly the shape stillness
has, so a working agent that raises one draws a single change and then settles —
stamping a working→idle edge for a pane that was never idle on the wire. The
damage lands *after* the box is answered: the agent resumes working and that
stale edge is still the newest, so every device that has not viewed the pane
shows `done` on an agent mid-run. Blocked detection therefore runs *before*
classification and is passed in: while blocked the hash and the still-counter
are kept up to date — the settled state is fine, the caller overrides it with
`blocked` anyway — and only the stamp is withheld. A false `done` is the same
badge-integrity failure a false `blocked` is, reached from the other side.

**That settling must not stamp `finishedAt`.** Otherwise every daemon restart
synthesises a working→idle edge on every agent pane, every edge is newer than
every browser's `seen`, and every device lights up with done badges after every
restart or deploy. This is the same shape as v1's `Sweep` defect: correct pieces,
destructive composition. A working run only produces a finish edge if it was
observed for at least one real poll, so a run that began at map-rebuild produces
none.

### `done` lives in the browser

The server reports `finishedAt`. Each browser keeps `seen[paneId]` in
`localStorage` and shows the badge when `finishedAt > seen`; viewing the pane
sets it.

No per-device state reaches the daemon, and the phone keeps its badge after the
laptop has cleared its own. Clearing browser data forgets it and the badge
reappears once, which is harmless.

**The key includes the tmux server's generation** (`#{start_time}`), because pane
ids restart at `%0` when the tmux server restarts. Without it, a stale
`seen["%3"]` from a previous server would silently suppress the badge on an
unrelated new pane.

One consequence of choosing a badge over push: a hidden tab's polling is
throttled by the browser, and a locked phone stops entirely. The badge is
therefore late or absent exactly while you are away — which is the trade accepted
by not shipping push.

## Management

### Endpoints

All behind the existing cookie + exact-Origin middleware.

```
POST   /api/sessions            create  {name, path?}
POST   /api/windows             create  {session, name?, fromPane?}
POST   /api/panes               split   {pane, direction: right|down}
PATCH  /api/sessions/{id}       rename  {name}          // id is $N
PATCH  /api/windows/{id}        rename  {name}           // id is @N
PATCH  /api/panes/{id}          label   {label}          // "" clears
POST   /api/panes/{id}/zoom     toggle
DELETE /api/sessions/{id}       kill    {confirm: true}  // id is $N
DELETE /api/windows/{id}        kill    {confirm: true}  // id is @N
DELETE /api/panes/{id}          kill    {confirm: true}
```

### Safety

**Ids, not names, wherever tmux has one** — `%3` for panes, `@7` for windows —
validated against their shapes before reaching tmux. A stale row from a poll
1.5s old therefore kills nothing rather than the wrong thing.

**Sessions are addressed by `$id` too.** Revision 1 exempted them on the false
belief that they have no ids, which would have left the rename race the
ids-not-names rule exists to kill — and would have broken outright after a
rename, since the name changes while the group name does not. Where a name must
be used at all it carries tmux's `=` exact-match prefix: v1 established why,
`kill-session -t _web-` silently kills `_web-abcd` and exits 0.

**The daemon refuses to touch its own sessions** — both verbs that take one,
kill *and rename*. Anything carrying `@wterm_web` is the app's, not the user's;
killing one would drop a live tab's socket for no reason the user could
understand, and renaming one is worse, because the bridge tears its session down
**by name** when the tab closes: a renamed session is never found and the
teardown falls through to the `destroy-unattached` net that exists as a crash
net, not as the normal path. The app row is reachable, not hypothetical — the
snapshot deliberately keeps an app-owned session when it is a group's only
member, which is the state the sidebar is in after the base session is killed
with a tab open. Identified by the option and never by the name: `_web-` is a
convention the app follows, not a namespace it owns.

**`confirm: true` on every delete.** This mirrors the UI's two-step rather than
replacing it, so a stray request that never passed through the dialog cannot
destroy a window either. It is an anti-footgun, not a security control; the
Origin middleware is the boundary.

**The refusal to touch app sessions does not stop the cascade, and the dialog
must say so.** Verified: killing the only window of a base session destroys the
whole group, `@wterm_web` member included, which drops every attached tab's
socket. Refusing `DELETE /api/sessions` on an app session guards only the direct
path; the sanctioned window and pane paths reach the same end. This is not a
reason to block the action — the owner asked for it — but the dialog must say
"closes the session and disconnects this tab", and reconnect needs a defined
behaviour for "the session I was attached to no longer exists": report it and
offer the session list, rather than retrying against a group that is gone.

**Pane paths never cross the wire.** The browser sends "split
`%6`"; the daemon resolves `#{pane_current_path}` for `%6` itself and passes it
as `-c`. New windows and splits therefore open where you were working, without
paths appearing in the snapshot — preserving v1's decision to keep them out.

The one path that does cross is the optional `path` on session create, typed by
the owner in the dialog. That is not the snapshot leak the rule guards against,
but it gets the same treatment as `split -c`: the daemon stats it first and
reports a missing directory rather than letting tmux land silently in `$HOME`.

### When a management call fails

A stale id targets nothing and tmux exits 1 — the safety property — but that says
nothing about what the owner sees. Failures surface as a `sonner` toast naming
what was attempted and tmux's own message ("can't find pane: %7"), and the
sidebar refreshes immediately rather than waiting for the next poll, so a row
that no longer exists disappears along with the error. Nothing is retried: a
failed kill that silently succeeded on retry is worse than one that failed.

### Pane labels

A renamed pane cannot use the tmux pane title: both a zsh prompt and Claude
overwrite it constantly. Labels are stored as a per-pane tmux user option,
`@wterm_label`, which is durable, readable from the same format string, dies
with the pane, and needs no new server state. Verified: it survives the title
being clobbered, clears with `set -pu`, and its value is not re-expanded.

**Unlike titles, user option values are NOT sanitised by tmux**, and revision 1
missed it. A label set to `EV\x1fIL\nSECOND` comes back raw: the `0x1f` splits
fields and the newline splits the record, so `ParseRows` drops the line and
**that pane vanishes from the sidebar** — the failure v1's own code comments call
the worst this project has. This is the `pane_current_path` class, reintroduced.

So labels are validated on write: control bytes rejected, length capped. That is
necessary but not sufficient — any process with access to the tmux socket can
set the option out of band, and from v2 things do: the pi extension and the
opencode plugin write this option from third-party code, carrying text derived
from prompts and tool calls.

The residual this section originally accepted — "the damage is confined to one
row disappearing until the option is cleared" — is no longer accepted, because
a row disappearing is the worst failure this app has. The snapshot now defends
itself, in three layers:

1. **tmux strips the two bytes itself.** The format reads
   `#{s/[<0x1f><newline>]/ /:@wterm_label}`. tmux's `s///` pattern can carry
   those bytes literally; `[[:cntrl:]]` cannot be used, because the `:` inside
   the class ends the modifier's pattern and the whole expression then expands
   to empty for every label.
2. **The label is the last field.** A raw separator can then only add fields
   after the last one, and a raw newline can only cut the line short once every
   other field is already on it. Neither can remove a pane, whatever layer 1
   does — which matters because tmux answers a pattern it cannot compile by
   echoing the value unchanged and exiting 0.
3. **`ParseRows` repairs what arrives.** 13 or more fields, surplus rejoined
   into the label; control runes (C1 included) become spaces, invalid UTF-8
   becomes U+FFFD, the result is capped at `MaxLabel` runes and trimmed. The
   rules are `validateLabel`'s, applied instead of refused, so there is one
   notion of a safe label rather than two.

A hostile label therefore degrades to a repaired or empty label. It cannot
remove a pane, forge one, or disturb another pane's row.

Sessions and windows rename through tmux itself, so the new name is visible
everywhere, not only in the browser.

## Interface

**Rows.** An agent pane shows its label if set, otherwise its title, with a small
bundled agent logo and a state dot. A non-agent pane is unchanged: its command,
no dot, no logo.

**Logos.** One inline monochrome SVG per agent, ~16px, so it works in both
themes without fighting the state colour. Each project's official mark where one
exists, with its source recorded beside the file. An agent with no mark
available falls back to a two-letter monogram — an invented logo for someone
else's project is worse than letters.

**Roll-up.** A window shows the most urgent state among its panes and a session
among its windows, ordered `blocked > done > working > idle`. This is what makes
a collapsed sidebar useful.

**Blocked rows show the question**, truncated, with the choices in the tooltip.

**The tab is the notification.** Title becomes `(2) tmux-web` when anything is
blocked or done, and the favicon carries a dot. No push, no service worker, no
subscription.

**Zoom joins v1's shared-property family.** Zoom is a window property, so zooming
from the browser zooms the window for every client viewing it, including the
local terminal — the same family as window size, copy-mode and the active pane
that v1 enumerated. Worth one line in the UI copy, not a blocker.

**Actions live on the rows.** Right-click, long-press on touch, scoped to what
was clicked: rename, new window, split right/down, zoom, then a separated red
kill. The same actions appear in the `Ctrl+Alt+K` palette. A session row carries
a `+` for the case with nothing to right-click yet — an empty tmux server, which
v1 cannot fix from the browser at all.

**Kill is two steps.** A dialog naming exactly what dies — `kill window "api" —
3 panes, one running claude` — a toggle to arm it, then a red button. Dismissing
resets the toggle.

## Failure modes

| Case | Handling |
| --- | --- |
| A wrong `blocked` | Worst outcome: it trains the user to ignore the badge. Strict matching; no match changes nothing |
| An agent that redraws nothing while working | Reads idle. The blocked check is independent of churn, so a question is still caught |
| A clock or spinner in a *non*-agent pane | Never captured, never classified — only known agents are |
| Rename or kill racing the poll | Panes, windows and sessions all have ids, so a stale row targets nothing |
| Killing the last pane or window | Cascades to the session and the whole group, disconnecting attached tabs. The dialog says so; reconnect reports it rather than retrying |
| A label containing control bytes | Rejected on write; a row that still arrives malformed is dropped and counted, as v1 does |
| A resize, a zoom, or a keystroke echoed into an idle agent's box | **Stamps a finish edge, and that is not cosmetic.** One changed capture is indistinguishable from one poll's worth of work, so the pane reads working for a poll, settles in ~3s, and earns a `done` badge on every device that has not viewed it. The *state* self-heals; the edge does not. Accepted, because nothing separates the two: a minimum run length would close the single-poll cases and not the class — typing spans several polls and looks exactly like an agent redrawing — while costing a real badge on every task that finishes inside it. Viewing the pane clears it, as it clears any other |
| Daemon restart | Every agent settles from working to idle in ~3s and stamps no finish edge, so no badge storm |
| tmux server restart | Pane ids restart at `%0`; `seen` keys carry the server generation so old entries cannot suppress new badges, and the daemon's own per-pane map is reset on the generation change so a reused id cannot inherit a dead server's run |
| An agent held at a permission dialog | The classifier is told, so a held box updates the hash without stamping a finish edge. Without that, answering the box leaves a stale `done` on an agent that is mid-run |
| `Permission required` typed as the whole first line of opencode's input box | Reads blocked. The input box *is* the bottommost guttered block, and the header rule matches a whole line with any decoration before it. Accepted: closing it means requiring the `△` — reversing the decision that the glyph is style — which trades a self-inflicted, self-evident, transient false badge for a silently missed one on the load-bearing signal, the same trade the footer rule already refuses |
| A capture that fails for one poll | That pane's `agentState` is empty for one poll by decision, and the badge is gated on `idle`, so an unseen `done` blinks off and returns 1.5s later with the same timestamp. Accepted: the flicker is the empty state, not the edge, so carrying `finishedAt` through the failed poll would change nothing the browser renders. It can only *hide* a badge for one poll, never invent one |
| `split-window -c` on a deleted directory | tmux silently succeeds and lands in `$HOME`. The daemon resolves the path itself, so it stats first and reports rather than surprising you |

## Testing

Following v1:

- **Integration against real tmux** for every management verb, including that a
  kill refuses an `@wterm_web` session and that `=` prevents prefix matching.
- **Table tests for classification**, driven by recorded capture
  sequences -- a working pane's successive screens and an idle pane's identical
  ones -- so a rule change is a data change.
- **Playwright** for the two-step kill, the roll-up, and the tab badge.
- **A regression test for the restart badge storm** — restart the poller with
  agent panes present and assert no finish edge is stamped.
- **A regression test that a held dialog stamps no edge**, asserted on
  `finishedAt` and not only on `agentState`: the blocked override makes the row
  read `blocked` whether or not an edge was stamped underneath it, so a test
  that checks the state alone cannot see this bug at all.
- **A regression test that the tmux generation changing resets the per-pane
  map**, and that an unchanged generation does *not* — a reset that fired every
  poll would make every poll a first sight and kill the done badge outright,
  with the restart test still green.
- **A regression test that a label containing `0x1f` or a newline cannot remove a
  pane from the snapshot** — against a real tmux server, since the bug exists
  only because of what real tmux stores and prints. Done:
  `TestSnapshotHostileLabelCannotRemoveAPane` (the whole stack),
  `TestFormatAloneKeepsEveryRecordWellFormed` (layer 1 on its own terms, the
  only test that fails when the substitution weakens) and
  `TestParserAloneSurvivesARawLabelFromRealTmux` (layers 2 and 3, reading real
  tmux output with layer 1 removed by construction).
- **A test that rename is visible in the snapshot afterwards**, which is what
  revision 1's group-name bug would have failed.

## Out of scope

Web Push, several machines in one sidebar, agent-to-agent orchestration, layout
restore, and a general parsed-screen panel. Each was considered and dropped;
the reasons are in the decisions above.
