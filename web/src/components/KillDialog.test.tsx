/**
 * The two-step kill: the state machine behind the toggle, and what the dialog
 * puts on screen on both sides of it.
 *
 * Rendered with `react-dom/server`, as everything else in this suite is. The
 * body is rendered inside a bare `<Dialog open>` -- the Radix root is a context
 * provider with no DOM of its own, and `DialogTitle` throws outside one -- but
 * *not* inside `DialogContent`, which is a portal into a `document` that does
 * not exist under vitest's node environment.
 */

import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'

import { KILL_CLOSED, KillDialogBody, canKill, killDialogReducer } from './KillDialog'
import { Dialog } from '@/components/ui/dialog'
import type { KillPlan } from '@/lib/manage'

function plan(over: Partial<KillPlan> = {}): KillPlan {
  return {
    id: 'kill-window:@7',
    action: { verb: 'kill-window', window: '@7' },
    title: 'kill window "api"',
    detail: '3 panes, one running claude',
    warnings: [],
    confirmLabel: 'Kill window',
    ...over,
  }
}

function render(over: { armed?: boolean; busy?: boolean; plan?: KillPlan } = {}): string {
  return renderToStaticMarkup(
    <Dialog open>
      <KillDialogBody
        plan={over.plan ?? plan()}
        armed={over.armed ?? false}
        busy={over.busy ?? false}
        onArmedChange={() => {}}
        onConfirm={() => {}}
      />
    </Dialog>,
  )
}

/**
 * Whether the red button is inert.
 *
 * The attribute, not the word: the button's Tailwind classes contain
 * `disabled:pointer-events-none`, so a `toContain('disabled')` on the markup
 * passes whether or not the button is actually disabled -- a vacuous assertion
 * on the one thing this dialog exists to get right.
 */
function confirmDisabled(markup: string): boolean {
  const buttons = markup.match(/<button[^>]*>[^<]*<\/button>/g) ?? []
  const found = buttons.find((b) => /Kill|Killing/.test(b))
  if (!found) throw new Error(`no confirm button in ${markup}`)
  return / disabled=""/.test(found)
}

describe('killDialogReducer', () => {
  it('opens disarmed', () => {
    expect(killDialogReducer(KILL_CLOSED, { type: 'open', plan: plan() })).toEqual({
      plan: plan(),
      armed: false,
      busy: false,
    })
  })

  it('resets the toggle when the dialog is dismissed', () => {
    // The rule this reducer exists for. An armed dialog that kept its arming
    // across a dismissal is a loaded button waiting on a screen somebody put
    // down -- and on a phone the whole surface is a fingertip away from it.
    const armed = killDialogReducer(
      killDialogReducer(KILL_CLOSED, { type: 'open', plan: plan() }),
      { type: 'arm', armed: true },
    )
    expect(armed.armed).toBe(true)
    expect(killDialogReducer(armed, { type: 'dismiss' })).toEqual(KILL_CLOSED)
  })

  it('never inherits an arming meant for another target', () => {
    const armed = killDialogReducer(
      killDialogReducer(KILL_CLOSED, { type: 'open', plan: plan() }),
      { type: 'arm', armed: true },
    )
    const other = plan({ id: 'kill-pane:%3', title: 'kill pane 2 of "api"' })
    // Reopening on a different row while the previous arming is still in state:
    // without the reset, the first click on the red button would be live.
    expect(killDialogReducer(armed, { type: 'open', plan: other }).armed).toBe(false)
  })

  it('goes busy while the request is away', () => {
    const open = killDialogReducer(KILL_CLOSED, { type: 'open', plan: plan() })
    expect(killDialogReducer(open, { type: 'send' }).busy).toBe(true)
  })

  it('is only killable armed, open and idle', () => {
    expect(canKill({ plan: plan(), armed: true, busy: false })).toBe(true)
    expect(canKill({ plan: plan(), armed: false, busy: false })).toBe(false)
    expect(canKill({ plan: plan(), armed: true, busy: true })).toBe(false)
    expect(canKill({ plan: null, armed: true, busy: false })).toBe(false)
  })
})

describe('<KillDialogBody>', () => {
  it('leaves the red button inert until the toggle is flipped', () => {
    expect(confirmDisabled(render({ armed: false }))).toBe(true)
    expect(confirmDisabled(render({ armed: true }))).toBe(false)
  })

  it('shows the toggle unchecked, and checked when armed', () => {
    expect(render({ armed: false })).toContain('type="checkbox"')
    expect(render({ armed: false })).not.toContain('checked=""')
    expect(render({ armed: true })).toContain('checked=""')
  })

  it('names exactly what dies, agent included', () => {
    const markup = render()
    expect(markup).toContain('kill window &quot;api&quot;')
    expect(markup).toContain('3 panes, one running claude')
  })

  it('says when the kill takes this tab with it', () => {
    const markup = render({
      plan: plan({
        warnings: [
          'It is the last window in "work", so the session closes with it.',
          'That closes the session this tab is attached to, so this tab disconnects.',
        ],
      }),
    })
    expect(markup).toContain('so this tab disconnects')
    // Marked as an alert, not as prose: it is the one thing in the dialog that
    // is about something other than the row that was right-clicked.
    expect(markup).toContain('role="alert"')
  })

  it('stays quiet when nothing cascades', () => {
    expect(render({ plan: plan({ warnings: [] }) })).not.toContain('role="alert"')
  })

  it('is inert and says so while the kill is in flight', () => {
    expect(confirmDisabled(render({ armed: true, busy: true }))).toBe(true)
    expect(render({ armed: true, busy: true })).toContain('Killing…')
  })
})
