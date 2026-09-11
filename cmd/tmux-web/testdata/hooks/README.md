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
  `permission.ask`, `tool.execute.before` and `tool.execute.after`.
- **pi** — an extension in `<proj>/.pi/extensions/` calling `pi.on(name, fn)` for
  fourteen candidate event names and logging the `event` argument together with a
  summary of `ctx` (`mode`, `hasUI`, `isIdle()`).

Agent versions: **Claude Code 2.1.267**, **opencode 1.18.30**, **pi 0.85.1**.

A fixture is the payload as the hook receives it:

- claude — the JSON object on the hook process's stdin.
- opencode `event` hook — the `event` object (`{id, type, properties}`).
- opencode `chat.message` / `tool.execute.before` — these hooks take two
  arguments, so the fixture is `{"input": …, "output": …}`, which is what the
  handler actually sees. `tool.execute.before` puts the tool's arguments under
  `output.args`, not under `input`.
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
sample.txt`", "write a two-line poem about terminal windows").

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

| File | Provoked by | Notes |
| --- | --- | --- |
| `chat_message.json` | typing the haiku prompt | Two arguments. The prompt text is `output.parts[].text` for `type: "text"`. |
| `chat_message_child.json` | the subagent's own first message | Same shape, child `sessionID`. `input` here also carries `messageID`, which the root's did not. |
| `session_created_root.json` | starting the session | `properties.info` has **no `parentID`**. |
| `session_created_child.json` | the `task` tool | `properties.info.parentID` names the root. This is the only event that establishes parentage. |
| `session_status_busy.json` | turn start | `properties.status.type == "busy"`. |
| `session_status_idle.json` | turn end | `properties.status.type == "idle"`. Fires in the same millisecond as `session.idle`. |
| `tool_execute_before_bash.json` | "run `wc -l sample.txt`" | `input.tool`, `input.sessionID`, `input.callID`; args under `output.args`. |
| `tool_execute_before_todowrite.json` | the todo list | |
| `tool_execute_before_write.json` | writing `out.txt` | args key is **`filePath`** (camelCase), unlike pi's `path`. |
| `tool_execute_before_task.json` | the subagent launch | `description`, `prompt`, `subagent_type`. |
| `todo_updated.json` | the same todo list, mid-run | `properties.todos[]` with `content`, `status`, `priority`; exactly one `in_progress`. |
| `permission_asked_bash.json` | `"permission": {"bash": "ask"}` in a project config | |
| `permission_asked_edit.json` | `"permission": {"edit": "ask"}` | |
| `session_idle_root.json` | the root turn ending | `properties` is **`{sessionID}`** and nothing else. |
| `session_idle_child.json` | **the subagent's turn ending while the root was still working** | Same shape, child id. |

Confirmed:

- **`session.idle` carries a session identifier.** A child's `session.idle`
  arrived **2.05 s before** the root's, while the root was still `busy`. The
  false-`done` this would cause is real and reproducible, and the `sessionID` is
  enough to filter it — provided the `session.created` that named the
  `parentID` was seen first.
- `session.status` fires `busy` repeatedly within one turn — 17 times in the
  three-tool turn captured here — so a `busy` handler must be idempotent or the
  write must be a re-assertion.

**Where the design is wrong about `permission.asked`:** it is described as
carrying "the question". It does not. There is no question string and no title.
It carries `permission` (the class: `"bash"`, `"edit"`), `patterns`, `always`,
`tool: {messageID, callID}`, and a **`metadata` object whose keys depend on the
permission class**: `{command}` for bash, `{filepath, diff}` for edit — and that
`diff` is a full unified diff, comfortably over the 1 KiB report cap on its own.
Task 15 must reduce per class, not read one field.

Also note: the **`permission.ask` plugin hook never fired** in any of these runs.
Only the `event` hook's `permission.asked` did.

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
