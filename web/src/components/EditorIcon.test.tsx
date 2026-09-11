/**
 * What `EditorIcon` puts in the DOM, rendered with `react-dom/server`.
 *
 * Same approach as `AgentIcon.test.tsx`, and one extra thing to pin that that
 * file does not have to: an editor's mark is **identity only**. The agent marks
 * are drawn beside a state dot and are a subset of Go's `tmux.Agents`, the list
 * that gates capture, state and logo together. These must be a subset of
 * nothing -- an editor has no state to report, and the last test here is the
 * one that keeps it that way.
 */

import { readFileSync } from 'node:fs'

import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'

import { AGENT_MARKS } from './AgentIcon'
import { EDITOR_MARKS, EditorIcon } from './EditorIcon'

const render = (command: string) => renderToStaticMarkup(<EditorIcon command={command} />)

/** Repo-root-relative, as the other contract tests read it. */
const goSource = (path: string) => readFileSync(new URL(`../../../${path}`, import.meta.url), 'utf8')

/**
 * The name each editor must be announced by, spelled out rather than read back
 * from `EDITOR_MARKS`: a module asserted against itself passes whatever it
 * becomes, including the two names swapped.
 */
const expected = { vim: 'Vim', nvim: 'Neovim' } as const

describe('EditorIcon', () => {
  it('renders nothing at all for a command that is not one of these editors', () => {
    // Exact, as the agent lookup is: `vi` may well be a symlink to vim on this
    // machine and `gvim` is not in a terminal at all, but neither is a command
    // the owner asked for, and a mark on a command nobody checked is a guess.
    for (const command of ['zsh', 'claude', 'vi', 'VIM', 'neovim', 'nvim-qt', 'gvim', '']) {
      expect(render(command), command).toBe('')
    }
  })

  it('does not mistake an inherited property for an editor', () => {
    // `'toString' in EDITOR_MARKS` is true -- `in` walks the prototype -- and a
    // pane running a binary called `toString` would be handed a function's name
    // to announce itself with.
    for (const command of ['toString', 'constructor', 'valueOf']) {
      expect(render(command), command).toBe('')
    }
  })

  for (const [command, name] of Object.entries(expected)) {
    describe(command, () => {
      it('names the editor for a screen reader without adding a tooltip', () => {
        // The whole of what a screen reader gets, and the whole of what a phone
        // gets: there is no hover there to reveal anything else. An SVG
        // <title> is not the way to do it -- it is also a native tooltip, and
        // the row already has a `title` attribute of its own.
        const markup = render(command)
        expect(markup).toContain('role="img"')
        expect(markup).toContain(`aria-label="${name}"`)
        expect(markup).not.toContain('<title')
      })

      it('inherits the row text colour and carries none of its own', () => {
        // `currentColor` is what makes the mark legible in both themes without
        // a second palette, and what keeps it neutral beside the state dot --
        // the one thing in a row allowed to mean something by being coloured.
        const markup = render(command)
        expect(markup).toContain('currentColor')
        expect(markup).not.toMatch(/#[0-9a-fA-F]{3}/)
        expect(markup).not.toMatch(/(stroke|fill)="(?!currentColor|none)/)
        // Nor a colour from the palette: a `text-*` utility here would paint
        // the mark in one theme's foreground and fight the other's, and it
        // would mean something beside the state dot besides.
        expect(markup).not.toMatch(/class="[^"]*text-/)
      })

      it('is 16px and does not shrink', () => {
        expect(render(command)).toContain('size-4 shrink-0')
      })
    })
  }

  it('draws one glyph for the category, not a logo per editor', () => {
    // Deliberate: Vim's and Neovim's own marks are real artwork, and drawing
    // either from memory would misrepresent someone else's project -- the rule
    // `AgentIcon` states and every entry there obeys. What is honest at 16px is
    // a glyph that says "a file is being edited here", with the editor's name
    // on it for anything that reads rather than looks. Which editor it is, the
    // row says in words beside the mark.
    const [vim, nvim] = [render('vim'), render('nvim')]
    const glyph = (m: string) => m.replace(/ aria-label="[^"]*"/, '')
    expect(glyph(vim)).toBe(glyph(nvim))
    // And it is nobody's brand mark: no agent's path may appear here.
    for (const mark of Object.values(AGENT_MARKS)) {
      expect(vim).not.toContain(mark.path)
    }
  })

  it('has an expectation above for every mark it ships', () => {
    // So that a fourth editor cannot land unnamed and untested.
    expect(Object.keys(EDITOR_MARKS).sort()).toEqual(Object.keys(expected).sort())
  })
})

describe('contract with the daemon', () => {
  it('draws no command Go classifies as an agent', () => {
    // The invariant this whole file exists for, and it runs the opposite way to
    // `AgentIcon`'s. `tmux.Agents` gates capture, state and the state dot
    // together, so an editor named in that list would be screen-classified and
    // would carry a badge saying it needs you. An editor has no state; keeping
    // the two tables disjoint is what keeps it out of the one that does.
    const m = goSource('internal/tmux/agent.go').match(/Agents\s*=\s*\[\]string\{([^}]*)\}/)
    if (!m) throw new Error('Agents not found in internal/tmux/agent.go')
    const agents = [...m[1].matchAll(/"([^"]*)"/g)].map((q) => q[1])
    expect(agents.length).toBeGreaterThan(0)
    for (const command of Object.keys(EDITOR_MARKS)) {
      expect(agents, command).not.toContain(command)
    }
  })

  it('shares no command with the marks that do carry a state', () => {
    for (const command of Object.keys(EDITOR_MARKS)) {
      expect(Object.hasOwn(AGENT_MARKS, command), command).toBe(false)
    }
  })
})
