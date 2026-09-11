#!/bin/sh
# managed by tmux-web (wterm-schema: 1)
# Reinstalling or updating the integration overwrites this file.
# It does one thing: `tmux set-option -p @wterm_agent`. Nothing else.
#
# WHAT IS DELIBERATELY NOT HERE. No state name, no mapping from an event to a
# state, no activity text, no sanitizer, no filter. Claude Code hands a hook its
# event name and its own JSON, and both go straight through to `wterm-web
# report`, which owns the one table the three integrations share. If you find
# yourself writing the word "working" or "idle" in this file, something has gone
# wrong.
#
# There is no queue here either, and there is nowhere to put one: every Claude
# hook is a fresh process, so there is no module scope to hold a slot the way
# pi's and opencode's runtimes do. The daemon's ordering filter -- a report is
# ignored unless it is strictly newer than the one standing -- stands in for it.
#
# STDIN IS THE PAYLOAD AND IT HAS TO ARRIVE UNTOUCHED. The hook's stdin is a
# socket carrying the event's JSON, and `report` REFUSES a payload it could not
# parse, on the ground that a payload nobody could read is one where claude's
# `agent_id` is absent for the worst possible reason. So a wrapper that loses
# stdin does not fail loudly: every Stop writes nothing and the pane sits out
# the 60-second working expiry instead of badging a finish. Two consequences,
# and both of them are things somebody will try to "simplify" back:
#
#   - Do NOT background the child. `cmd &` in a non-interactive shell has its
#     stdin redirected from /dev/null by POSIX, which is exactly the silent
#     failure above. Not waiting is the HOOK's job -- every hook this project
#     installs is `"async": true` -- not this script's.
#   - Do NOT read stdin here (`payload=$(cat)` and friends). It costs a fork,
#     it mangles trailing newlines, and it caps the payload at whatever the
#     argument list can hold. `exec` hands the socket over as it stands.
#
# `"async": true` IS WHAT KEEPS THIS OFF THE TURN'S CRITICAL PATH, and it is
# measured rather than assumed: on Claude Code 2.1.267 a turn costing 2771 ms
# baseline took 7735 ms with a `sleep 5` hook registered synchronously and
# 2251 ms with the same hook async. `asyncRewake` is NEVER set alongside it: it
# is documented as the thing that makes exit code 2 wake Claude, and exit 2 is
# the one code that BLOCKS a PreToolUse tool call. This feature must never be
# able to interrupt an agent.
#
# EXIT 0 IS THE CONTRACT. `report` keeps it on every path of its own, including
# a malformed command line; this file keeps it on the one path that is its own.
# Nothing here writes to stdout either -- a PreToolUse hook's stdout is READ,
# and a JSON permissionDecision on it denies the tool call.
BIN='__WTERM_BIN__'

# Moved, uninstalled, never built, or shipped and not yet installed: say
# nothing. Without this guard the shell's own "not found" reaches the hook's
# stderr on every single tool call, and the wrapper exits 127.
[ -x "$BIN" ] || exit 0

# "$1" is the event name. An absent one is not special-cased: `report` answers a
# missing --event with one line on stderr and exit 0, which is the same quiet
# failure as everything else here.
exec "$BIN" report --agent claude --event "$1"
