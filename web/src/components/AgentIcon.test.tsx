/**
 * What `AgentIcon` actually puts in the DOM, rendered with `react-dom/server`.
 *
 * Same approach as `AppSidebar.test.tsx`: `renderToStaticMarkup` in the node
 * environment the rest of the suite runs in, no jsdom and no testing library.
 * An icon has no behaviour, so static markup is not a compromise here -- it is
 * the whole of the component.
 *
 * The expected path data is written out *literally* below rather than read back
 * from `AGENT_MARKS`. Asserting a module against itself would pass no matter
 * what the paths became, including two agents swapping marks, which is the
 * failure this file most needs to catch: a wrong logo is worse than no logo,
 * because it is confidently wrong.
 */

import { readFileSync } from 'node:fs'

import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'

import { AGENT_MARKS, AgentIcon } from './AgentIcon'

const render = (command: string) => renderToStaticMarkup(<AgentIcon command={command} />)

/** Repo-root-relative, as the other contract tests read it. */
const goSource = (path: string) => readFileSync(new URL(`../../../${path}`, import.meta.url), 'utf8')

/**
 * What each agent must render, spelled out independently of the component.
 *
 * Claude's mark is a 158-point starburst; pinning 1.5KB of it twice would be
 * unreadable, so it is pinned by both ends and its exact length -- enough that
 * any other agent's mark, or a truncated one, fails.
 */
const expected = {
  claude: {
    label: 'Claude Code',
    viewBox: '0 0 24 24',
    pathStartsWith: 'm4.7144 15.9555 4.7174-2.6471.079-.2307',
    pathEndsWith: '.0608-.7467.2307-.2429 1.9064-1.3114Z',
    pathLength: 1712,
    fillRule: undefined,
  },
  opencode: {
    label: 'opencode',
    viewBox: '0 0 19.2 24',
    path: 'M19.2 24H0V0h19.2zM14.4 4.8H4.8v14.4h9.6z',
    fillRule: 'evenodd',
  },
  pi: {
    label: 'Pi',
    viewBox: '0 0 24 24',
    path: 'M0 0v24h6v-6h6v-6H6V6h6v6h6V0Zm18 12v12h6V12Z',
    fillRule: undefined,
  },
} as const

describe('AgentIcon', () => {
  it('renders nothing at all for a command that is not an agent', () => {
    // A shell is a shell. Decorating it would say an agent lives there, and
    // the row would carry a logo for something no state was ever computed for.
    // Note `claude-helper` and `CLAUDE`: the lookup is exact, matching Go's.
    for (const command of ['zsh', 'bash', 'nvim', 'go', '', 'claude-helper', 'CLAUDE', 'Pi']) {
      expect(render(command)).toBe('')
    }
  })

  for (const [command, want] of Object.entries(expected)) {
    describe(command, () => {
      it("draws that project's own mark and no other", () => {
        const markup = render(command)
        const d = markup.match(/ d="([^"]*)"/)?.[1]
        if (d === undefined) throw new Error(`no path rendered for ${command}: ${markup}`)

        if ('path' in want) {
          expect(d).toBe(want.path)
        } else {
          expect(d.startsWith(want.pathStartsWith)).toBe(true)
          expect(d.endsWith(want.pathEndsWith)).toBe(true)
          expect(d).toHaveLength(want.pathLength)
        }
        // Geometry is meaningless without the box it is measured in, and
        // opencode's frame is deliberately not square.
        expect(markup).toContain(`viewBox="${want.viewBox}"`)
        if (want.fillRule) expect(markup).toContain(`fill-rule="${want.fillRule}"`)
      })

      it('inherits the row text colour and carries no brand colour', () => {
        const markup = render(command)
        // `currentColor` is what keeps the mark legible in both themes without
        // a second palette, and neutral beside the state dot -- the only thing
        // in the row allowed to mean something by being coloured.
        expect(markup).toContain('fill="currentColor"')
        // Every upstream mark is coloured; none of that may come back. A hex,
        // a named colour, or the dark plate behind Pi's glyph would all be a
        // second status signal.
        expect(markup).not.toMatch(/#[0-9a-fA-F]{3}/)
        expect(markup).not.toMatch(/fill="(?!currentColor)/)
        expect(markup).not.toContain('<rect')
      })

      it('is 16px and does not shrink', () => {
        expect(render(command)).toContain('class="size-4 shrink-0"')
      })

      it('names the agent for a screen reader without adding a tooltip', () => {
        const markup = render(command)
        expect(markup).toContain('role="img"')
        expect(markup).toContain(`aria-label="${want.label}"`)
        // An SVG <title> is also a native tooltip, and the row already has a
        // `title` attribute holding the full pane title. Two tooltips over one
        // row is worse than one.
        expect(markup).not.toContain('<title')
      })
    })
  }

  it('gives every agent a mark of its own', () => {
    const drawn = Object.keys(AGENT_MARKS).map((c) => render(c))
    expect(new Set(drawn).size).toBe(drawn.length)
  })

  it('has an expectation above for every mark it ships', () => {
    // So that adding a fourth agent cannot land a logo with no provenance and
    // no test: this fails until the entry is spelled out in `expected`.
    expect(Object.keys(AGENT_MARKS).sort()).toEqual(Object.keys(expected).sort())
  })
})

describe('contract with the daemon', () => {
  it('only draws commands Go classifies as agents', () => {
    // The design's invariant is one-directional: `tmux.Agents` gates capture,
    // state and logo together, so a pane with no state must never get a logo.
    // A *subset*, not equality -- Go's list may name an agent whose mark has
    // not been established yet (e2e adds a scripted fake one), and that pane
    // correctly gets a state dot and no logo.
    const m = goSource('internal/tmux/agent.go').match(/Agents\s*=\s*\[\]string\{([^}]*)\}/)
    if (!m) throw new Error('Agents not found in internal/tmux/agent.go')
    const agents = [...m[1].matchAll(/"([^"]*)"/g)].map((q) => q[1])
    expect(agents.length).toBeGreaterThan(0)
    for (const command of Object.keys(AGENT_MARKS)) {
      expect(agents).toContain(command)
    }
  })
})
