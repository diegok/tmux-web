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
 * Claude's 158-point starburst is written out in full, all 1.8KB of it. Pinning
 * it by its ends and its length was tried first and read much better, and it
 * let a single digit change in the middle of the path through: the mark would
 * have been quietly wrong with the suite green. A second copy is the price of
 * a pin that actually holds.
 */
const expected = {
  claude: {
    label: 'Claude Code',
    viewBox: '0 0 24 24',
    path:
      'm4.7144 15.9555 4.7174-2.6471.079-.2307-.079-.1275h-.2307l-.7893-.0486-2.6956-.0729-2.3375-.0971-2.2646-.1214-.5707-.1215-.5343-.7042.0546-.3522.4797-.3218.686.0608 1.5179.1032 2.2767.1578 1.6514.0972 2.4468.255h.3886l.0546-.1579-.1336-.0971-.1032-.0972L6.973 9.8356l-2.55-1.6879-1.3356-.9714-.7225-.4918-.3643-.4614-.1578-1.0078.6557-.7225.8803.0607.2246.0607.8925.686 1.9064 1.4754 2.4893 1.8336.3643.3035.1457-.1032.0182-.0728-.164-.2733-1.3539-2.4467-1.445-2.4893-.6435-1.032-.17-.6194c-.0607-.255-.1032-.4674-.1032-.7285L6.287.1335 6.6997 0l.9957.1336.419.3642.6192 1.4147 1.0018 2.2282 1.5543 3.0296.4553.8985.2429.8318.091.255h.1579v-.1457l.1275-1.706.2368-2.0947.2307-2.6957.0789-.7589.3764-.9107.7468-.4918.5828.2793.4797.686-.0668.4433-.2853 1.8517-.5586 2.9021-.3643 1.9429h.2125l.2429-.2429.9835-1.3053 1.6514-2.0643.7286-.8196.85-.9046.5464-.4311h1.0321l.759 1.1293-.34 1.1657-1.0625 1.3478-.8804 1.1414-1.2628 1.7-.7893 1.36.0729.1093.1882-.0183 2.8535-.607 1.5421-.2794 1.8396-.3157.8318.3886.091.3946-.3278.8075-1.967.4857-2.3072.4614-3.4364.8136-.0425.0304.0486.0607 1.5482.1457.6618.0364h1.621l3.0175.2247.7892.522.4736.6376-.079.4857-1.2142.6193-1.6393-.3886-3.825-.9107-1.3113-.3279h-.1822v.1093l1.0929 1.0686 2.0035 1.8092 2.5075 2.3314.1275.5768-.3218.4554-.34-.0486-2.2039-1.6575-.85-.7468-1.9246-1.621h-.1275v.17l.4432.6496 2.3436 3.5214.1214 1.0807-.17.3521-.6071.2125-.6679-.1214-1.3721-1.9246L14.38 17.959l-1.1414-1.9428-.1397.079-.674 7.2552-.3156.3703-.7286.2793-.6071-.4614-.3218-.7468.3218-1.4753.3886-1.9246.3157-1.53.2853-1.9004.17-.6314-.0121-.0425-.1397.0182-1.4328 1.9672-2.1796 2.9446-1.7243 1.8456-.4128.164-.7164-.3704.0667-.6618.4008-.5889 2.386-3.0357 1.4389-1.882.929-1.0868-.0062-.1579h-.0546l-6.3385 4.1164-1.1293.1457-.4857-.4554.0608-.7467.2307-.2429 1.9064-1.3114Z',
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

        expect(d).toBe(want.path)
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
