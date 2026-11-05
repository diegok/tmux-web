# Agents driving tmux — what was left for later

Date: 2026-11-05
Companion to `2026-11-05-tmux-web-agent-control-design.md` (revision 6) and
`-implementation.md`. Written at the end of the branch, from the reviews, so the
next reader does not re-derive it.

Fifty-two minor findings came out of the per-task reviews. Eleven were fixed in
the final wave; the whole-branch review judged the other forty-one not worth the
merge. What follows is only the part with a future in it.

## Teaching opencode and pi

**pi is taught.** It owns two files now, the extension and
`skills/tmux-web/SKILL.md`, from the same embedded prose claude installs. Both
catches this section used to list are closed:

- `validate` is per file, and so is `schema`. The first was the prerequisite
  (pi's pre-flight would otherwise have been handed the Markdown); the second
  turned out to matter more, and is written up below.
- The paths are measured, on pi 1.5.0, in a throwaway tmux with a HOME and a
  `PI_CODING_AGENT_DIR` of its own: `<agent dir>/skills/<name>/SKILL.md` loads
  (the startup screen lists it under `[Skills]`), and `.pi/skills/<name>/SKILL.md`
  loads once the project is trusted — untrusted, pi asks `Trust project folder?`
  and loads no `.pi` resources at all. `.agents/skills/` works too and is
  deliberately not used: it is shared with other agents and searched up the
  ancestors, so a drop there is not one agent's to own.

**Beware a measurement that cannot have been made.** A pi reviewing this
integration reported `pi skills list` as the deterministic way to see what the
discovery loaded. There is no such subcommand in pi 1.5.0 — `pi skills --help`
falls through to the general help, and the command list is
install/remove/uninstall/update/list/config/auth/mcp. The paths above were
measured another way. Every other finding in that review checked out; this one
did not, and it is the reason the rest were checked at all.

There is also no model-free lever for this: an unknown `--provider` makes pi
exit before skills load, so the trick the extension pre-flight uses does not
work for a skill. A TUI in a throwaway tmux, read with `capture-pane`, is what
works.

**opencode is the one left**, and it is blocked rather than pending: opencode
v2 broke the plugin, and the port comes first. opencode reads
`~/.config/opencode/skills/<name>/SKILL.md`, `.opencode/skills/`, and also
`.claude/skills` — so a project install may already be seen through the file
claude owns, which is worth measuring before writing a third path.

A further agent costs one `ownedFile` entry, one embedded source, and one `case`
in `renderOwned` if the file is not a plain copy.

**The shared skill source is still called `claude-skill.md`**, which is now a
lie: claude and pi install the same bytes of it. Renaming it moves the
`//go:embed` line in `internal/integrations`, the `Files` list beside it and the
two tests that assert that list — and that directory is where opencode's v2 port
is working, so the rename waits for it rather than racing it.

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

## What a pi reviewing the pi integration found (2026-11-05, pi 1.5.0)

The owner started a pi in a pane of its own and asked it to review its own
integration, using the skill this branch installs. It found no fault with what
ships: every Go test green against the real pi on this machine (the probe and
its four refusal shapes, the end-to-end wiring against a real pi TUI, the
non-TUI silence gate), 916 vitest, and no drift — what the installer renders
today is byte for byte what is installed in `~/.pi/agent/extensions/`.

Four findings came out of it. All four stand; the third needed its framing
corrected, and it is the one that matters.

1. **A relative `PI_CODING_AGENT_DIR` is resolved where opencode's analogue is
   refused.** `piAgentDir` takes `filepath.Abs` of the value against the
   INSTALLER's working directory. Measured twice on pi 1.5.0 — once by the
   reviewing pi, once independently — pi resolves the same value against ITS
   OWN working directory at startup: from two different directories it created
   `<cwd>/relconf` and nothing under `HOME`. So a global install run from one
   directory writes an extension that a pi started from another never reads.
   `opencodeConfigDir` already refuses the analogous relative
   `XDG_CONFIG_HOME`, for exactly this reason and with the measurement in its
   comment. Refuse this one the same way, with the same shape of message.

2. **The pre-flight belongs to the scope, not to the file** — the entry above
   says it, and the review confirms the analysis. One companion the first
   writing missed: `runPiProbe` writes the artifact to a hardcoded
   `agent/extensions/tmux-web.ts`. Once `validate` is per-file that name is
   right by construction, but passing the file's own `globalFile` costs one
   parameter and removes an assumption that is wrong the day pi owns two
   `.ts` files.

3. **The schema is per agent, and sharing claude's skill file would weld two
   numbers that have to stay apart.** `queue.ts`'s `TMUX_WEB_SCHEMA` is a
   RUNTIME number, not a header: `claimReporter` compares it so that when a
   global copy and a project copy load in one process, the newer one does the
   reporting. `pivalidate_test.go` pins it to `agents["pi"].schema`, and
   opencode shares `queue.ts`. Meanwhile `claude-skill.md`'s embedded header
   says 2, and `control_test.go` pins that header to `claudeSchema`. So giving
   pi the shared file verbatim forces `piSchema` to 2, which forces
   `TMUX_WEB_SCHEMA` to 2, which changes which installed copy of the OPENCODE
   integration claims the reporter — set off by a prose edit to a claude skill.
   The false `older tmux-web` note on every reinstall is the smaller half of
   it. The fix is the reviewer's: the schema moves to `ownedFile`, each
   embedded file carries its own number, and `ownerOf` compares the file's
   rather than the agent's. That resolves this and the companion in 2 together.

4. **`pivalidate_test.go`'s "last green on pi 1.3.0" is now also green on
   1.5.0.** Add the version rather than replacing it: that comment is a history
   of green runs, which is what makes it worth anything. And the measurement
   this document asked for — whether a fresh pi loads a skill from
   `~/.pi/agent/skills/<name>/SKILL.md` — is answered: it does, measured on pi
   1.5.0 with the file placed by hand.

Still open: where pi looks for a PROJECT skill. The pi spec needs both paths,
and the project one is unmeasured.

**Reported by the owner, unverified here:** the opencode integration no longer
works on opencode v2, and an opencode is porting it. Nothing in this document
has been checked against v2.
