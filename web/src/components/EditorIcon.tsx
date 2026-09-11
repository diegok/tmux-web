/**
 * The mark that says a row is an **editor** -- the other thing that is open all
 * day in a tmux development session.
 *
 * ## Why this is not another row in the agents table
 *
 * An agent's mark travels with a *state*: a dot beside it saying working,
 * blocked, done or idle, and a daemon that captures the pane to work that out.
 * `AGENT_MARKS` is therefore a subset of Go's `tmux.Agents`, the one list that
 * gates capture, state and logo together.
 *
 * An editor has no state. Nothing is ever waiting on you inside vim, there is
 * no run to finish, and the daemon must never start classifying its screen to
 * decide otherwise. Adding `vim` to the agents table is the obvious way to get
 * this mark and it would do exactly that -- a status badge on a text editor,
 * and the badge integrity the sidebar's whole design rests on spent on it.
 * Hence a second table, in a second file, looked up separately: **agents are
 * things with state; this is identity only.** `EditorIcon.test.tsx` reads
 * `internal/tmux/agent.go` and fails if the two ever overlap.
 *
 * ## One glyph for the category, not a logo per editor
 *
 * Vim's and Neovim's own marks are real artwork, and `AgentIcon` states the
 * rule that covers them: only a project's own mark, never one drawn from
 * memory, because a wrong logo is confidently wrong. Neither could be
 * established here the way the three agent marks were -- each of those was
 * fetched from the project's own site and compared against a second source --
 * so nothing here claims to be either project's mark.
 *
 * What is honest at 16px is a glyph for *what kind of thing this is*: a file
 * with a pen on it. Which editor it actually is, the row says in words a few
 * pixels away -- the command capsule, or the window name itself now that a
 * window named after its shell shows what it is running -- and the mark says it
 * to a screen reader, which is where the two entries below differ.
 *
 * Adding an editor is one line in the table. Adding a *pile* of tools is not
 * the point: each one is a claim that a glyph is worth the row's 16px, and only
 * these two were asked for.
 *
 * ## Both themes, for free
 *
 * lucide draws with `stroke="currentColor"` and no fill, so the mark inherits
 * the row's own text colour in light and dark alike -- the same reason
 * `AgentIcon` reduces every upstream mark to `currentColor`. Nothing here may
 * carry a colour of its own: colour in a sidebar row means state, and this is
 * the one kind of mark that has none.
 */

import { FilePen } from 'lucide-react'

/**
 * Editor command → the name it is announced by.
 *
 * Keyed by exact `pane_current_command`, and **disjoint from `AGENT_MARKS` and
 * from Go's `tmux.Agents`** -- see the note above and the contract test.
 *
 * Exact, like the agent lookup: `vi` may be a symlink to vim on any given
 * machine and `vimdiff` is vim, but neither was asked for and neither was
 * checked, and a mark on an unchecked command is a guess dressed as a fact.
 */
export const EDITOR_MARKS: Record<string, string> = {
  vim: 'Vim',
  nvim: 'Neovim',
}

/**
 * The mark for a pane's `pane_current_command`, or nothing if it is not an
 * editor this file knows.
 *
 * `role="img"` with `aria-label` rather than an SVG `<title>`, exactly as
 * `AgentIcon` does it and for the same reason: a `<title>` is also a native
 * tooltip and the row already has a `title` attribute of its own. The label
 * folds into the row button's accessible name, so a screen reader reads the
 * editor and then the window -- and it is the whole of what a phone gets, where
 * there is no hover at all.
 *
 * `Object.hasOwn` rather than a bare lookup: a command is whatever binary the
 * user happened to run, and `constructor` is a legal filename.
 */
export function EditorIcon({ command }: { command: string }) {
  if (!Object.hasOwn(EDITOR_MARKS, command)) return null
  return <FilePen className="size-4 shrink-0" role="img" aria-label={EDITOR_MARKS[command]} />
}
