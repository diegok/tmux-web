# Recorded hook payloads

Every file here is a payload a real agent handed a real hook, captured on
2026-09-11 by running the agent in a throwaway project on a private tmux socket
and driving it with prompts written for the purpose. **Nothing here was written
by hand, and nothing was edited to fit an expectation.** The only change made to
a captured payload is the masking described under "Masking" below.

Tasks 13, 14 and 15 are written against what these files contain. Where a file
disagrees with the design document, the file wins.

## How they were captured

One recorder per agent, in a scratchpad, never in the repo:

- **claude** — a `settings.json` passed with `claude --settings <file>` (so the
  owner's `~/.claude/settings.json` was untouched) registering a shell script on
  `UserPromptSubmit`, `PreToolUse`, `Notification`, `Stop` and `SubagentStop`,
  each `"async": true` and **never** `asyncRewake`. The script appends stdin, the
  `TMUX*`/`CLAUDE_*` environment and `tty` to a per-event log.
- **opencode** — a plugin in `<proj>/.opencode/plugin/` registering the `event`
  hook (which sees every `event.type`) plus `chat.message`, `chat.params`,
  `permission.ask`, `tool.execute.before` and `tool.execute.after`. **Recaptured
  on 2026-11-06 against opencode 2.2.5**, where none of those names exist any
  more: a v2 plugin definition (`{id, setup}`) registering
  `ctx.session.hook('prompt')`, `ctx.tool.hook('execute.before')` and an
  `ctx.event.subscribe` iteration, which is what sees every event type now.
- **pi** — an extension in `<proj>/.pi/extensions/` calling `pi.on(name, fn)` for
  fourteen candidate event names and logging the `event` argument together with a
  summary of `ctx` (`mode`, `hasUI`, `isIdle()`).

Agent versions: **Claude Code 2.1.267**, **opencode 1.18.30**, **pi 0.85.1**.
The opencode files are from the 2026-11-06 recapture on **opencode 2.2.5**; the
1.18.30 ones went with the events that stopped existing (see the opencode
section).

A fixture is the payload as the hook receives it:

- claude — the JSON object on the hook process's stdin.
- opencode bus — the event object the subscription delivers,
  `{id, created, type, data, …}` on 2.2.5 (`{id, type, properties}` on 1.18.30).
- opencode `prompt` / `execute.before` hooks — ONE event argument each on 2.2.5,
  recorded whole, with the tool's arguments on `input`. On 1.18.30 these hooks
  took two arguments and the fixture was `{"input": …, "output": …}`, with the
  arguments under `output.args`.
- pi — the `event` argument only. `ctx` is not part of the payload and is
  recorded in prose below, because the three pi filters live in `ctx`.

## `TMUX_PANE`, and the tty

Recorded on **every** event of **every** agent. The answer was the same
everywhere, and it is the answer open questions 9 and 11 were waiting for:

| Agent | `TMUX_PANE` in the hook process | tty |
| --- | --- | --- |
| claude | **present**, `%0` — the pane the CLI runs in | `not a tty` |
| opencode | **present**, `%0` (the plugin runs inside the TUI process) | a tty |
| pi | **present**, `%0` (the extension runs inside the pi process) | a tty |

**A pi subagent spawned asynchronously is a separate OS process and it inherits
`TMUX_PANE` unchanged** — same value, same pane, no marker in the payload. See
the pi section; this is the sharpest finding of the capture.

## Masking

Applied as plain substring substitution over the serialised JSON, nothing else:

| From | To |
| --- | --- |
| the capture scratchpad path | `/tmp/tmux-web-capture` |
| the same path in claude's project-slug form | `-tmp-tmux-web-capture` |
| `/home/<owner>` | `/home/user` |
| the provider id the owner's config defaults to | `example-provider` |
| the model id that provider defaults to | `example-model` |
| the owner's username | `user` |

Session ids, message ids, tool-call ids, event ids and timestamps are **not**
masked: `parentID` chains and `sessionID` matching are exactly what Task 18's
filter is built on, and rewriting them would break the thing the fixture exists
to prove. None of them identifies anything outside the capture.

No text from any of the owner's live panes appears in any file. Every prompt was
written for this capture ("Write a haiku about tmux panes", "run `wc -l
sample.txt`", "write a two-line poem about terminal windows", and for the 2.2.5
recapture "use the shell tool to run exactly: ls .").

Two of the 2.2.5 fixtures — `session_created_child.json` and
`session_step_started.json` — are the only ones not recorded in the capture
scratchpad: a subagent and a model step could only be provoked from a session
with a working model, which was the porting agent's own. They carry the masking
above (`/home/user`, `example-provider`, `example-model`) rather than the real
home, provider and model. The path hash `projectID` is left as captured: it is a
hash of a directory name and names nothing.

---

## claude

| File | Provoked by | Notes |
| --- | --- | --- |
| `user_prompt_submit.json` | typing the haiku prompt | The raw prompt is under **`prompt`**, a plain string. There is no `user_input.text`. |
| `pre_tool_use_bash.json` | "run `wc -l sample.txt`" | `tool_name` + `tool_input`; `tool_input.description` is Claude's own one-line summary. |
| `pre_tool_use_write.json` | plan mode writing its plan file | A second `tool_input` shape: `file_path` + `content`. |
| `pre_tool_use_agent.json` | "launch one Explore subagent" | The subagent-launching tool is called **`Agent`** (not `Task`), and `tool_input` has `description`, `prompt`, `subagent_type`. Fired on the **root**, so no `agent_id`. |
| `pre_tool_use_subagent_bash.json` | that subagent then running bash | Carries **`agent_id`** *and* **`agent_type`**. |
| `notification_idle_prompt.json` | leaving the prompt untouched | `notification_type: "idle_prompt"`, `message: "Claude is waiting for your input"`, **no `title` key at all**. |
| `notification_permission_prompt.json` | a bash command in manual mode | `notification_type: "permission_prompt"`, `message: "Claude needs your permission"`. |
| `notification_permission_prompt_plan.json` | plan mode's approval dialog | **Same `notification_type`, different `message`.** One type is not one message. |
| `stop.json` | end of the haiku turn | `last_assistant_message`, `background_tasks: []`, `session_crons: []`. |
| `stop_subagent_running.json` | end of a turn that had launched a subagent | `background_tasks` holds `{id, type: "subagent", status: "running", description, agent_type}` — the running-subagent case a `Stop` filter has to see. |
| `subagent_stop.json` | that subagent finishing | `agent_id`, `agent_type`, `agent_transcript_path`, its own `last_assistant_message`, and `background_tasks` still listing itself as `running`. |

Confirmed, measured rather than assumed:

- `agent_id` is **absent** on every root payload and **present** on every
  subagent one. It is absence-coded and fails open, as the design says.
- `permission_mode` rides on `UserPromptSubmit`, `PreToolUse`, `Stop` and
  `SubagentStop` but **not** on `Notification`. Observed values: `auto`,
  `default` (what the TUI calls "manual mode"), `plan`.
- `PreToolUse` also carries `tool_use_id`; `Stop`/`SubagentStop` also carry
  `effort` and `session_crons`.
- `scratchpad_dir` appears on every hook of this version.

### `notification_type`: two of twelve, and that is the result

Only **`idle_prompt`** and **`permission_prompt`** could be provoked. Task 13's
whitelist must not be built out of this file: it is evidence that these two
exist and what they look like, not evidence that the set is two. The design's
twelve-value list stands, and an unknown type writes nothing.

The four the design tracks as open question 10 — `quota_auto_resume_stale`,
`elicitation_dialog`, `elicitation_url_dialog`, and `agent_needs_input`'s
teammate-setup form — **were not produced and were not synthesised.** They stay
ignored until somebody manufactures the screen and writes a grammar.

---

## opencode

Captured on **2026-11-06, opencode 2.2.5**. The 1.18.30 table that stood here
went with its files: see "What 1.18.30 had and 2.2.5 does not" below for what
was lost and why none of it could be kept.

| File | Provoked by | Notes |
| --- | --- | --- |
| `prompt.json` | typing the shell prompt | The `prompt` hook's one argument. The prompt text is `prompt.text`; `delivery` says how it was admitted. |
| `session_created_root.json` | starting the session | `data` has **no `parentID`**. |
| `session_created_child.json` | a subagent | `data.parentID` names the root — one level deep now, where 1.18.30 nested it under `properties.info`. The only event that establishes parentage. |
| `session_execution_started.json` | turn start | `data` is **`{sessionID}`** and nothing else. |
| `session_step_started.json` | each model step within the turn | `data` carries the agent, the model and a snapshot. Arrives as often as 1.18.30's `session.status(busy)` did. |
| `session_execution_succeeded.json` | the root turn ending | `data` is **`{sessionID}`**. |
| `session_execution_succeeded_child.json` | **the subagent's turn ending while the root was still working** | Byte-compatible with the root's shape, child id, **no `parentID`**. |
| `session_execution_failed.json` | a turn that died at the provider | `data.error` with `type`, `message`, `status` and the provider's own body. A real failure: "This model is not available in your country". |
| `tool_execute_before_shell.json` | "run `ls .`" | One event: `tool`, `sessionID`, `agent`, `messageID`, `id`, args under **`input`**. The bash tool is called **`shell`** now. |
| `tool_execute_before_read.json` | reading the capture directory | args key is **`path`**, like pi's. |
| `tool_execute_before_write.json` | writing `hello.txt` | args key is **`path`** too — 1.18.30 spelled it `filePath`. |
| `permission_asked_shell.json` | a shell call with permission set to ask | **No `metadata` at all**: the command is `data.resources[0]`, and `data.action` is `"shell"`. |
| `permission_asked_edit.json` | an edit with permission set to ask | `data.metadata.files[0]` with `file`, `patch`, `status` and counts. That `patch` is a whole patch, over the 1 KiB report cap on its own. |

`session.execution.interrupted` has **no fixture**: three provocations of a real
interruption all finished as `succeeded` instead — the API answered
`{"interrupted": false}` each time, the runs being already over — so the row in
the event table is held to the schema rather than to a capture. Its payload is
`data.sessionID` plus a reason string, the shape `succeeded` and `failed` carry
verbatim.

Confirmed:

- **The turn end carries a session identifier and no parent.** A child's arrives
  before the root's, while the root is still working — the false-`done` this
  would cause is the same one 1.18.30 measured at 2.05 s, and the `sessionID` is
  enough to filter it, provided the `session.created` that named the `parentID`
  was seen first.
- **`session.step.started` fires repeatedly within one turn**, so a handler for
  it must be idempotent or the write must be a re-assertion.

**Where the design is wrong about `permission.asked`:** it is described as
carrying "the question". It does not, on either version. There is no question
string and no title. 2.2.5 carries `data.action` (the class: `"shell"`,
`"edit"`), `data.resources`, `data.save`, `data.source` and — **for some classes
only** — a `data.metadata` whose keys depend on the class. The reduction is per
class, not one field.

### What 1.18.30 had and 2.2.5 does not

These files were deleted rather than updated, because the events they recorded
are not on the public stream any more. MEASURED on 2.2.5, with a client
connected and without one:

- `session_status_busy.json`, `session_status_idle.json` — `session.status`
  does not reach a plugin subscription at all. `session.execution.started` and
  `session.step.started` are what report the turn now.
- `session_idle_root.json`, `session_idle_child.json` — `session.idle` does not
  either. The turn ends on one of `session.execution.succeeded` / `.failed` /
  `.interrupted`, each once.
- `todo_updated.json` — the todo list left the bus with the todo feature's own
  retreat from it, and with it rung 2 of the activity ladder.
- `chat_message.json`, `chat_message_child.json` — the hook is called `prompt`
  and takes one argument.
- `tool_execute_before_bash.json` — the tool is called `shell`.
- `tool_execute_before_todowrite.json`, `tool_execute_before_task.json` — not
  reprovoked. `todowrite` went with the todo feature; the `task` tool simply was
  not provoked on 2.2.5, and a fixture built to prove nothing is worse than no
  fixture. Both tools reduce through the same `tool`/`input` reader as the three
  that were captured.
- `permission_asked_bash.json` — the class is `shell`, and its metadata is gone.

Also note: the **`permission.ask` plugin hook never fired** in any of the 1.18.30
runs — only the `event` hook's `permission.asked` did — and on 2.2.5 there is no
such hook to register.

---

## pi

| File | Provoked by | Notes |
| --- | --- | --- |
| `session_start.json` | launching pi | `{type, reason: "startup"}`. |
| `input.json` | typing the haiku prompt | `text` carries the prompt; there is also a `source` field (`"interactive"`). |
| `tool_execution_start_bash.json` | "run `wc -l sample.txt`" | `args: {command}` |
| `tool_execution_start_read.json` | "read `sample.txt`" | `args: {path}` |
| `tool_execution_start_write.json` | "write `out.txt`" | `args: {path, content}` |
| `tool_execution_start_subagent.json` | the subagent tool | `args: {agent, async, task}` |
| `ui_prompt_start_input.json` | the `ask_user` tool | `{type, reason, kind: "input", title}` — `title` is the literal question. |
| `ui_prompt_start_custom.json` | an extension's own overlay | **`kind: "custom"` and no `title` key at all.** |
| `agent_settled.json` | end of the haiku turn | `{"type": "agent_settled"}` — **that is the whole payload.** |

### Open question 7, answered

`tool_execution_start.args` is the tool's own argument object, unreduced, with a
different key per tool: `command` (bash), `path` (read), `path` + `content`
(write), `agent`/`async`/`task` (subagent). `content` can be arbitrarily large.
The reduction rules in Task 15 are written against these four files.

### `agent_settled` and subagents — say this loudly

- The payload is `{"type": "agent_settled"}`. **No session id, no agent id, no
  parent, nothing.** Nothing in the payload can tell a root settle from a
  subagent settle, so Task 15 cannot filter on it and must not pretend to.
- A **synchronous** subagent (`async: false`) produced no extension events at
  all; only the root's single `agent_settled` at the end of the turn.
- An **asynchronous** subagent (`async: true`) is a **separate pi process**. It
  discovers and loads the same project extension, inherits `TMUX_PANE=%0`, and
  emits its own full event stream — `session_start`, `input`,
  `tool_execution_start`, `turn_end` and **its own `agent_settled`**, which
  landed 7.0 s before the root's final one. Its `agent_settled` payload is
  **byte-identical** to the root's, which is why only one file is landed here.
- `ctx.isIdle()` was **`true` in the subagent process too**, at both its
  `session_start` and its `agent_settled`. The design's `agent_settled` filter
  (`ctx.isIdle() === true`) therefore does **not** exclude it.
- What *does* exclude it: the subagent process reports **`ctx.mode === "print"`
  and `ctx.hasUI === false`** at `session_start`, against `"tui"` / `true` for
  the real root. The `ctx.mode !== "tui"` gate in `session_start` — the one
  filter of the three that fails closed — is the **only** thing standing between
  an async pi subagent and a false `idle` on the owner's pane. It is load-bearing
  and must not be relaxed.

Other `ctx` facts, for the record: the object exposes `ui`, `mode`, `hasUI`,
`cwd`, `sessionManager`, `modelRegistry`, `model`, `scopedModels`,
`thinkingLevel`, `isIdle`, `isProjectTrusted`, `signal`, `abort`,
`hasPendingMessages`, `shutdown`, `getContextUsage`, `compact`,
`getSystemPrompt`. `pi.on(name, fn)` is the registration form and the `pi`
object also exposes `registerTool`, `registerCommand`, `registerFlag`, `exec`,
`setLabel`, `events` and others. `session_start`'s `reason` was `"startup"` in
every run captured.

pi does **not** prompt for project trust: an untrusted project prints "This
project is not trusted. Project `.pi` resources and packages are ignored" and
carries on, so there is no `ui_prompt_start` for it.

---

## One sample versus a confirmed shape

**Confirmed shape** — observed several times, across turns, with the same keys:

- claude `UserPromptSubmit` (9), `PreToolUse` (12), `Stop` (6),
  `SubagentStop` (7)
- opencode `session.status` (38), `session.idle` (6), `tool.execute.before` (10),
  `todo.updated` (4), `chat.message` (6), `session.created` (4)
- pi `input` (7), `tool_execution_start` (11), `agent_settled` (8),
  `session_start` (4)

**One sample, treat the shape as unproven:**

- `claude/notification_idle_prompt.json` — the type recurred (3 times) but always
  from the same cause.
- `claude/notification_permission_prompt.json` and
  `…_plan.json` — one each. Two messages, one type.
- `claude/stop_subagent_running.json` — one turn ended with a subagent still
  running. `background_tasks` has only ever been seen with `type: "subagent"`;
  the design's other six types (`shell`, `monitor`, `workflow`, `teammate`,
  `cloud session`, `MCP task`) were **not** observed.
- `opencode/permission_asked_bash.json`, `…_edit.json` — one each, and the two
  already disagree about `metadata`'s keys, so assume a third class disagrees
  again.
- `opencode/session_idle_child.json` — one subagent run.
- `pi/ui_prompt_start_input.json`, `…_custom.json` — one each. Two `kind` values
  seen; there are certainly more, and one of the two already has no `title`.
- `pi/tool_execution_start_subagent.json` — one sample; `args` here comes from a
  third-party extension's tool, not from pi's own built-ins.
