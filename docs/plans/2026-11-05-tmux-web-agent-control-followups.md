# Agents driving tmux — what was left for later

Date: 2026-11-05
Companion to `2026-11-05-tmux-web-agent-control-design.md` (revision 6) and
`-implementation.md`. Written at the end of the branch, from the reviews, so the
next reader does not re-derive it.

Fifty-two minor findings came out of the per-task reviews. Eleven were fixed in
the final wave; the whole-branch review judged the other forty-one not worth the
merge. What follows is only the part with a future in it.

## Teaching opencode and pi

The installer carries N owned files per agent now, and claude's second is
`.claude/skills/tmux-web/SKILL.md`. The owner uses skills in all three agents,
so the other two are a known follow-up rather than a gap. Two catches found
while building the slot, both from Task 10's review:

- **`where.validate` is one hook run on every owned file.** It must become
  per-file before pi gains a skill, or the pi validator will run on Markdown.
- **Each agent's skill path and settings handling need their own
  investigation.** Nothing in this repo can answer where opencode and pi look
  for a skill; the owner's own machines can.

A third agent costs one `ownedFile` entry, one embedded source, and one `case`
in `renderOwned` if the file is not a plain copy.

## Two behaviours that are right but unobvious

- **`--label` with nothing but spaces is accepted and then *clears* the label**
  rather than setting one. That mirrors `SetLabel`'s own clear-the-option
  branch, so the two sides agree; it is the one `--label` input where "accepted"
  does not mean "labelled". Both flag help strings say so.
- **`panes` and `state` could still disagree in one theoretical way.** `panes`
  reads the report through `#{@tmux_web_agent}` in `list-panes`, which inherits
  window, session and global options; `state`'s `standingReport` uses
  `show-options -p`, which is pane-only. Nothing this project writes lives at
  those scopes, so it cannot happen today. If anything ever sets that option
  above the pane, this is where the pair breaks.

## The anti-goal with no artefact behind it

The design names "two agents talk to each other unattended and loop" as a
failure, and nothing in the code prevents it. The only controls are the skill's
"do not spin" and whatever cadence the calling agent picks. That was deliberate
— the design made it etiquette rather than code — but it is the one anti-goal
with no test, no guard and no measurement, and a reader should know that before
assuming something is watching.

## Smaller things, if anyone is ever in the area

- `freeStates`' option-scope note and `runRun`'s twin predicates are now shared
  or cross-referenced, but the pane lookup helpers (`requirePane`,
  `checkPaneTarget`) are new: a sixth verb should use them rather than a fifth
  copy.
- `tmux-web run` has no direction flag; `SplitPaneDetached` already accepts
  `SplitDown` whenever something wants it.
- `controlTimeout` (10s) and `stateSettle` (1.5s, the daemon's own
  `DefaultPollInterval`) are constants with no override. Nothing has asked for
  one.
- There is no `--json`. The design chose line records, and `panes`' first five
  fields are positional and tested; `label` and `title` are free-form and are
  for reading.
- An MCP front end was approach 2 in the design and was deferred with the core
  factored so it could be a thin face over the same functions.
