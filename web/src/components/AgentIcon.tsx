/**
 * The little mark that says *which* agent a row is running.
 *
 * The sidebar row used to read `claude`. It now reads the pane title --
 * "Categorización productos southafrica" -- which is far more useful and says
 * nothing at all about who is doing the work. This is what puts that back, in
 * ~16px, beside the state dot.
 *
 * ## Only real marks, never an invented one
 *
 * Every entry below is the project's *own* mark, reduced to one colour. Drawing
 * something "close enough" from memory would misrepresent someone else's
 * project; a two-letter monogram is the honest fallback when a mark cannot be
 * established, because a monogram is obviously a label rather than a claim.
 *
 * No monogram is shipped today: all three marks were establishable from the
 * projects' own sites and repositories, recorded per entry below. An unreachable
 * fallback branch is not worth carrying -- the rule lives here, in prose, for
 * whoever adds the fourth agent.
 *
 * ## Monochrome, and why `currentColor`
 *
 * Each upstream mark is coloured (Claude's terracotta, a dark rounded square
 * behind Pi's glyph, a two-tone cursor inside opencode's frame). All of that is
 * dropped: the path is filled with `currentColor`, so the mark inherits the
 * row's own text colour and is legible in both themes for free, and -- more to
 * the point -- it stays *neutral* next to the state dot, which is the one thing
 * in the row that is allowed to carry colour meaning. A brand colour here would
 * read as a second, contradictory status.
 *
 * ## A command this file does not know renders nothing
 *
 * Not a placeholder, not a question mark: `null`. A pane running `zsh` is a
 * shell, and decorating it would say it is an agent. The keys below are exact
 * `pane_current_command` values and must stay a *subset* of Go's `tmux.Agents`
 * -- the list that gates capture, state and logo together -- so a logo can
 * never appear on a pane nothing computed a state for. `AgentIcon.test.tsx`
 * reads `internal/tmux/agent.go` and pins that.
 */

/** One agent's mark: the artwork, and the name a screen reader gets. */
interface AgentMark {
  /** Spoken by assistive tech as part of the row's name. */
  name: string
  /** Sized to the artwork, not forced square -- see `size-4` below. */
  viewBox: string
  /** A single path. `fill` comes from the `<svg>`, so it is `currentColor`. */
  path: string
  /** Only where upstream sets it. */
  fillRule?: 'evenodd'
}

/**
 * Agent command → its mark, with where each one came from.
 *
 * Keep this a subset of `tmux.Agents` in `internal/tmux/agent.go`.
 */
export const AGENT_MARKS: Record<string, AgentMark> = {
  /**
   * Claude Code.
   *
   * Source: Anthropic's own Claude mark, published at https://claude.ai as
   * `https://claude.ai/favicon.svg` (fetched 2026-09-10, one path in a
   * `0 0 248 248` box). The 24-unit normalisation below is the same path as
   * shipped by simple-icons v16.30.0 (`icons/claude.svg`, CC0-1.0, whose
   * recorded source is https://claude.ai); the two were compared by parsing
   * both paths to absolute points and scaling each to its own bounding box --
   * 158 points each, maximum deviation 0.001 of the box, i.e. rounding.
   * Reduction: `fill="#D97757"` → `currentColor`.
   */
  claude: {
    name: 'Claude Code',
    viewBox: '0 0 24 24',
    path: 'm4.7144 15.9555 4.7174-2.6471.079-.2307-.079-.1275h-.2307l-.7893-.0486-2.6956-.0729-2.3375-.0971-2.2646-.1214-.5707-.1215-.5343-.7042.0546-.3522.4797-.3218.686.0608 1.5179.1032 2.2767.1578 1.6514.0972 2.4468.255h.3886l.0546-.1579-.1336-.0971-.1032-.0972L6.973 9.8356l-2.55-1.6879-1.3356-.9714-.7225-.4918-.3643-.4614-.1578-1.0078.6557-.7225.8803.0607.2246.0607.8925.686 1.9064 1.4754 2.4893 1.8336.3643.3035.1457-.1032.0182-.0728-.164-.2733-1.3539-2.4467-1.445-2.4893-.6435-1.032-.17-.6194c-.0607-.255-.1032-.4674-.1032-.7285L6.287.1335 6.6997 0l.9957.1336.419.3642.6192 1.4147 1.0018 2.2282 1.5543 3.0296.4553.8985.2429.8318.091.255h.1579v-.1457l.1275-1.706.2368-2.0947.2307-2.6957.0789-.7589.3764-.9107.7468-.4918.5828.2793.4797.686-.0668.4433-.2853 1.8517-.5586 2.9021-.3643 1.9429h.2125l.2429-.2429.9835-1.3053 1.6514-2.0643.7286-.8196.85-.9046.5464-.4311h1.0321l.759 1.1293-.34 1.1657-1.0625 1.3478-.8804 1.1414-1.2628 1.7-.7893 1.36.0729.1093.1882-.0183 2.8535-.607 1.5421-.2794 1.8396-.3157.8318.3886.091.3946-.3278.8075-1.967.4857-2.3072.4614-3.4364.8136-.0425.0304.0486.0607 1.5482.1457.6618.0364h1.621l3.0175.2247.7892.522.4736.6376-.079.4857-1.2142.6193-1.6393-.3886-3.825-.9107-1.3113-.3279h-.1822v.1093l1.0929 1.0686 2.0035 1.8092 2.5075 2.3314.1275.5768-.3218.4554-.34-.0486-2.2039-1.6575-.85-.7468-1.9246-1.621h-.1275v.17l.4432.6496 2.3436 3.5214.1214 1.0807-.17.3521-.6071.2125-.6679-.1214-1.3721-1.9246L14.38 17.959l-1.1414-1.9428-.1397.079-.674 7.2552-.3156.3703-.7286.2793-.6071-.4614-.3218-.7468.3218-1.4753.3886-1.9246.3157-1.53.2853-1.9004.17-.6314-.0121-.0425-.1397.0182-1.4328 1.9672-2.1796 2.9446-1.7243 1.8456-.4128.164-.7164-.3704.0667-.6618.4008-.5889 2.386-3.0357 1.4389-1.882.929-1.0868-.0062-.1579h-.0546l-6.3385 4.1164-1.1293.1457-.4857-.4554.0608-.7467.2307-.2429 1.9064-1.3114Z',
  },

  /**
   * opencode.
   *
   * Source: the project's own mark file, `packages/identity/mark.svg` at
   * https://github.com/anomalyco/opencode (commit 1251a870, fetched
   * 2026-09-10) -- reached from https://opencode.ai, and the org `sst/opencode`
   * 301-redirects to, so this is the opencode whose binary is `opencode` and
   * not the unrelated project that was renamed away from that name.
   *
   * Upstream is a white frame with a two-tone cursor block inside it, on a
   * `#131010` field, drawn at 512. Reduction: the field and the grey block are
   * dropped -- monochrome cannot carry two tones -- leaving upstream's own
   * even-odd frame path, translated by (-128, -96) and scaled by 24/320. That
   * is exact: every upstream coordinate is a multiple of 64, and 64 x 24/320
   * is 4.8. simple-icons makes the same reduction, which is corroboration, but
   * it also widens the frame by ~4% to sit in a square box; this does not.
   */
  opencode: {
    name: 'opencode',
    viewBox: '0 0 19.2 24',
    path: 'M19.2 24H0V0h19.2zM14.4 4.8H4.8v14.4h9.6z',
    fillRule: 'evenodd',
  },

  /**
   * Pi.
   *
   * Source: the Pi Coding Agent's own favicon, `https://pi.dev/favicon.svg`
   * (fetched 2026-09-10). https://pi.dev is titled "Pi Coding Agent", installs
   * via `curl -fsSL https://pi.dev/install.sh | sh`, and ships
   * `@earendil-works/pi-coding-agent` from github.com/earendil-works/pi -- i.e.
   * it is the `pi` in `tmux.Agents`, and not π, Pi Network or Pi-hole.
   *
   * Upstream is a white glyph on a `#09090b` rounded square, drawn at 800.
   * Reduction: the square is dropped. The path below is simple-icons v16.30.0
   * (`icons/pi.svg`, CC0-1.0, recorded source `https://pi.dev/favicon.svg`),
   * which is upstream's two shapes re-cut as one non-zero contour: every
   * coordinate lands on upstream's own 0/6/12/18/24 grid once (v - 165.29) is
   * scaled by 24/469.43, and its signed area is 288, exactly the 324 of the
   * staircase less the 36 of the notch -- so the notch is a real hole.
   */
  pi: {
    name: 'Pi',
    viewBox: '0 0 24 24',
    path: 'M0 0v24h6v-6h6v-6H6V6h6v6h6V0Zm18 12v12h6V12Z',
  },
}

/**
 * The mark for a pane's `pane_current_command`, or nothing if it is not an
 * agent this file has a mark for.
 *
 * `size-4` is 16px in *both* directions and `preserveAspectRatio` defaults to
 * fitting inside it, so opencode's taller-than-wide frame is centred rather
 * than stretched, and all three agree optically.
 *
 * `role="img"` with `aria-label` rather than an SVG `<title>`: a `<title>` is
 * also a native tooltip, and the row already has a `title` attribute carrying
 * the full pane title. Two tooltips fighting over one row is worse than none.
 * The label folds into the row button's accessible name, so a screen reader
 * reads the agent and then the task.
 */
export function AgentIcon({ command }: { command: string }) {
  const mark = AGENT_MARKS[command]
  if (!mark) return null
  return (
    <svg
      viewBox={mark.viewBox}
      className="size-4 shrink-0"
      fill="currentColor"
      fillRule={mark.fillRule}
      role="img"
      aria-label={mark.name}
      xmlns="http://www.w3.org/2000/svg"
    >
      <path d={mark.path} />
    </svg>
  )
}
