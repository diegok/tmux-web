# Agents driving tmux — what was left for later

Date: 2026-11-05
Companion to `2026-11-05-tmux-web-agent-control-design.md` (revision 6) and
`-implementation.md`. Written at the end of the branch, from the reviews, so the
next reader does not re-derive it.

Fifty-two minor findings came out of the per-task reviews. Eleven were fixed in
the final wave; the whole-branch review judged the other forty-one not worth the
merge. What follows is only the part with a future in it.

## Teaching opencode and pi — both done

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

**opencode is taught too, and it owns its own copy.** The port unblocked it and
a measurement settled where it goes. MEASURED on opencode 2.2.5 against
opencode's own loaded-skill list — `GET /api/skill?location[directory]=<dir>`,
which answers with no model and no credentials:

- `$XDG_CONFIG_HOME/opencode/skills/tmux-web/SKILL.md` is listed, in a project
  that carries nothing of its own.
- `.opencode/skills/tmux-web/SKILL.md` is listed in that project.
- With opencode's own global copy AND a `~/.claude/skills/tmux-web/SKILL.md`
  both present, the list carries exactly ONE tmux-web and it is opencode's. So
  the install does not have to care whether claude's copy is there: it wins, and
  the agent is never shown two.

That third row is also the answer to the question this section used to ask.
opencode reads claude's global skills directory, which is why the owner's
opencode knew the verbs before anything was installed for it — `--agent claude
--global` had been teaching it all along. It is no longer load-bearing: opencode
installs its own file now, so an opencode-only user is taught, and a claude user
who removes claude's install does not silently un-teach opencode as well.

**The oracle is in the suite.** `TestAGlobalOpencodeInstallIsLoadedAndRemovable`
now asserts the skill's presence after the install and its absence after
`--remove`, from a real opencode's own list. It carries a POSITIVE CONTROL,
because the first call after a cold start lists nothing at all — opencode's own
builtins included — so the poll waits for the builtins before reading anything
into the absence of ours. Without that control the assertion would pass on a
race and prove nothing, which is how the first version of this measurement
briefly concluded that none of the paths worked.

A further agent costs one `ownedFile` entry, one embedded source, and one `case`
in `renderOwned` if the file is not a plain copy. opencode's skill cost exactly
that: one entry, no new source, no new code path.

**The shared skill source is still called `claude-skill.md`**, which is now a
lie three times over: all three agents install the same bytes of it, and only
one of them is claude. Renaming it moves the `//go:embed` line in
`internal/integrations`, the `Files` list beside it and the three tests that
assert that list -- `pivalidate_test.go`'s source table now names it for every
agent. It waited for opencode's v2 port, which was working in that directory;
the port has landed, so the rename is free to happen whenever somebody wants it.

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

   *Since then:* the opencode v2 port took the constant out of `queue.ts`
   altogether. Each integration declares its own `TMUX_WEB_SCHEMA` next to its
   own header — `pi.ts` at 1, `opencode.js` at 2 — because the port changed one
   install's shape and not the other's, and one shared number would have made a
   reinstall tell pi's users their file had changed when it had not. The Go
   suite holds each file's constant to that file's number. Nothing forces the
   two to agree again: the two runtimes never meet.

4. **`pivalidate_test.go`'s "last green on pi 1.3.0" is now also green on
   1.5.0.** Add the version rather than replacing it: that comment is a history
   of green runs, which is what makes it worth anything. And the measurement
   this document asked for — whether a fresh pi loads a skill from
   `~/.pi/agent/skills/<name>/SKILL.md` — is answered: it does, measured on pi
   1.5.0 with the file placed by hand.

Both pi skill paths are in the spec and both are measured — see the top of this
document for how, and for the subcommand that does not exist.

## What the opencode v2 port changed (2026-11-06, opencode 2.2.5)

Reported by the owner, ported by an opencode, reviewed here. It is recorded
because almost everything this feature knew about opencode was measured against
1.18.30, and a reader who trusts those measurements on 2.2.5 will be wrong four
times over. The code comments carry each measurement where it is used; this is
the map.

- **The plugin's shape.** A v1 factory function fails v2's schema check and the
  plugin never loads — which is what the owner saw. The default export is now an
  object with an `id` and a `setup`, registering `session.hook('prompt')`,
  `tool.hook('execute.before')` and an `event.subscribe` iteration, and
  returning the function that aborts it.
- **The turn lifecycle moved.** `session.status` and `session.idle` are off the
  public event stream entirely — neither reaches a plugin subscription, with a
  client connected or without one — and `todo.updated` went with the todo
  feature's retreat from the bus. The turn now starts on
  `session.execution.started`, keeps saying so on `session.step.started` (once
  per model step, the rate `status(busy)` used to arrive at) and ends on exactly
  one of `session.execution.succeeded` / `.failed` / `.interrupted`.
- **Two things that existed only because of v1 are gone.** Rung 2 of the
  activity ladder (`todo.updated`'s `in_progress` entry) has no member on any
  agent now, and opencode's turn end is an ordinary edge: the one re-assertion
  that dated from now, and the only one any agent had that was also a genuine
  turn end, retired with the event it was about.
- **The repository footprint is gone, at both scopes.** A dependency-free plugin
  leaves the project empty and puts nothing beside itself in the configuration
  directory. The project-scope warning and its test are retired; if a future
  opencode brings the bootstrap back, they come back with a capture that shows
  it.
- **Two test oracles had to be replaced, not merely adjusted.** `opencode debug
  config` no longer prints a per-plugin `spec` and `scope`, only the directories
  it scans — which cannot tell you that *our* file was accepted — so the install
  test reads the service's own log for `loading plugin` against our path. And
  the v2 service binds a **fixed default port (49374)**, the one the developer's
  own service is already holding, so an isolated-HOME run fails to bind and
  exits before any plugin loads; every test that boots opencode now writes a
  `service.json` with a port of its own.
