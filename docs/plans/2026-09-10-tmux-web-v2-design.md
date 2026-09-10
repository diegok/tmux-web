# tmux-web v2 — agent state and management

Date: 2026-09-10
Status: agreed, not yet implemented
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

1. **The pane title is the primary signal**, not the screen.
2. **Working is decided by title churn**, not by matching glyphs.
3. **The screen is read only to detect `blocked`**, only for known agents, only
   while a client is connected.
4. **`done` is computed in the browser** from a server-reported finish time.
5. **Full management** — create, rename, split, kill, zoom — with kill behind a
   two-step dialog.
6. **Notification is a tab badge**, not Web Push.
7. **One machine.** No cross-daemon aggregation.
8. **Agents are identified by a small bundled logo.**

## Agent state

### The title, free, every poll

`#{pane_title}` joins the existing `list-panes` format string. It costs nothing:
the poller already makes exactly one tmux call every 1.5s regardless of how many
tabs are open, and this is one more field in it.

Verified on this machine: Claude Code sets the title to what it is working on.

```
%6   title=[✳ Categorización productos southafrica]
%9   title=[✳ Product-join tasks southafrica]
```

That is a far better sidebar label than `claude`, independent of any state
detection.

**Titles are safe to parse, unlike paths.** A program can set its own title
through OSC 0/2, so this looked like the injection hazard that got
`pane_current_path` removed from v1's snapshot. It is not: tmux normalises
titles through its own OSC parser. Verified — a title set to
`EVIL\x1fFORGED\nSECONDLINE\x1fMORE` is reported as `EVILFORGEDSECONDLINEMORE`,
one line, no `0x1f`. Paths come from the filesystem and never pass through that
parser, which is why they remain excluded.

Consequence to accept: whatever a shell writes into its title is now visible in
the sidebar. A typical zsh prompt writes `user@host`.

### Churn, not glyphs

The poller keeps the previous title per pane id.

- Title changed since the last poll → `working`.
- Title identical for N consecutive polls → `idle`. N=2, so ~3s at the current
  interval: long enough not to flicker between two spinner frames that happen to
  render the same, short enough to feel live.

This needs no per-agent glyph table, so an agent restyling its UI cannot break
it, and opencode and Pi work without anyone writing rules for them. The
alternative — a pattern table per agent — was rejected because it rots on every
upstream restyle, and because the rules could not be derived empirically here
anyway.

Its weakness, accepted: an agent that never animates its title reads as
permanently idle, and a long silent step reads as idle. The screen check below
is what recovers the case that matters.

### The screen, only for blocked

Churn cannot separate "waiting for your approval" from "finished" — both are
still. So a pane that is **already idle**, **is a known agent**, and **has a
connected client** gets `capture-pane -p -S -8`, matched against approval-UI
rules.

Only a positive match promotes idle → blocked. No match means idle, never a
guess.

What the bottom of a Claude Code pane looks like at rest, structurally:

```
✻ <activity> 1m <…> · <clock>          activity line
※ <label>: <the agent's last message>  prose
────────────────────────────
❯                                      input box, empty = waiting
────────────────────────────
  ⏵⏵ auto <mode> on · …                footer: permission mode
```

When it asks permission, that region becomes a bordered box with the request and
numbered choices. **The question and its choices are extracted and shown**; that
is the whole reason for reading the screen. From a phone, "asking: allow
`rm -rf build/`?" is often enough to decide without opening the pane.

Explicitly not extracted: the `※` prose line (the title is a better, shorter
summary of the same thing) and the activity/footer chrome. The next candidate if
more is ever wanted is the permission mode, not the prose.

Captured text is matched in memory and discarded. It is never written to the
state file, never logged, and never included in an error message.

**Non-agents get no state.** A shell or an editor is neither idle nor working;
it is a shell. Those rows are unchanged from v1.

## What crosses the wire

`Row` gains four fields:

```go
Title      string `json:"title"`      // sanitised by tmux; "" if unset
Label      string `json:"label"`      // @wterm_label, user-set; "" if unset
AgentState string `json:"agentState"` // "" | working | blocked | idle
FinishedAt int64  `json:"finishedAt"` // unix ms of the last working→idle edge
```

Plus, only when blocked, a `question` carrying the extracted request and choices.

`AgentState` is empty for anything not recognised as an agent, so the frontend
never decides what counts.

### Server state

The poller gains one in-memory map keyed by pane id: previous title, unchanged
count, last working→idle timestamp. Pane ids are unique per server and stable
for a pane's life. The map is pruned to whatever the current snapshot contains,
so a closed pane's entry disappears on the next poll.

It is rebuilt from nothing on restart, so every agent reads as `working` for one
poll and then settles. That is honest: we do not know what happened while we
were not looking.

### `done` lives in the browser

The server reports `finishedAt`. Each browser keeps `seen[paneId]` in
`localStorage` and shows the badge when `finishedAt > seen`; viewing the pane
sets it.

No per-device state reaches the daemon, and the phone keeps its badge after the
laptop has cleared its own. Clearing browser data forgets it and the badge
reappears once, which is harmless.

## Management

### Endpoints

All behind the existing cookie + exact-Origin middleware.

```
POST   /api/sessions            create  {name, path?}
POST   /api/windows             create  {session, name?, fromPane?}
POST   /api/panes               split   {pane, direction: right|down}
PATCH  /api/sessions/{name}     rename  {name}
PATCH  /api/windows/{id}        rename  {name}
PATCH  /api/panes/{id}          label   {label}          // "" clears
POST   /api/panes/{id}/zoom     toggle
DELETE /api/sessions/{name}     kill    {confirm: true}
DELETE /api/windows/{id}        kill    {confirm: true}
DELETE /api/panes/{id}          kill    {confirm: true}
```

### Safety

**Ids, not names, wherever tmux has one** — `%3` for panes, `@7` for windows —
validated against their shapes before reaching tmux. A stale row from a poll
1.5s old therefore kills nothing rather than the wrong thing.

**Sessions are exact-matched.** They have only names, so every target is passed
with tmux's `=` prefix. v1 established why: `kill-session -t _web-` silently
kills `_web-abcd` and exits 0.

**The daemon refuses to touch its own sessions.** Anything carrying
`@wterm_web` is the app's, not the user's; killing one would drop a live tab's
socket for no reason the user could understand.

**`confirm: true` on every delete.** This mirrors the UI's two-step rather than
replacing it, so a stray request that never passed through the dialog cannot
destroy a window either.

**The working directory never crosses the wire.** The browser sends "split
`%6`"; the daemon resolves `#{pane_current_path}` for `%6` itself and passes it
as `-c`. New windows and splits therefore open where you were working, without
paths appearing in the snapshot — preserving v1's decision to keep them out.

### Pane labels

A renamed pane cannot use the tmux pane title: both a zsh prompt and Claude
overwrite it constantly. Labels are stored as a per-pane tmux user option,
`@wterm_label`, which is durable, readable from the same format string, dies
with the pane, and needs no new server state. Verified: it survives the title
being clobbered and clears with `set -pu`.

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
| A wrong `blocked` | Worst outcome: it trains the user to ignore the badge. Strict matching; fallback is always idle |
| An agent that never animates its title | Reads idle forever. Accepted; the screen check still promotes it to blocked |
| A clock or timer inside a title | Would read as permanently working. No agent in use does this; worth watching |
| Rename or kill racing the poll | Ids are stable, so a stale row targets nothing |
| Killing the last pane | Closes the window; the last window closes the session. The dialog says so before arming |
| `capture-pane` on a huge pane | Bounded to the last 8 lines, and only for idle known agents with a client |

## Testing

Following v1:

- **Integration against real tmux** for every management verb, including that a
  kill refuses an `@wterm_web` session and that `=` prevents prefix matching.
- **Table tests for classification**, driven by recorded title sequences and
  captured screens, so a rule change is a data change.
- **Playwright** for the two-step kill, the roll-up, and the tab badge.

## Out of scope

Web Push, several machines in one sidebar, agent-to-agent orchestration, layout
restore, and a general parsed-screen panel. Each was considered and dropped;
the reasons are in the decisions above.
