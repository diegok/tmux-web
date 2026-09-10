/**
 * The two-step kill: what dies, a toggle to arm it, then a red button.
 *
 * ## Why two steps
 *
 * Everything else in this app is undoable by doing it again -- a split can be
 * killed, a zoom is its own undo, a rename can be renamed back. A kill is the
 * one action that destroys work, and on a phone the whole surface is a
 * fingertip away from every row. So the button is inert until a toggle is
 * flipped, and **dismissing the dialog resets the toggle**: an armed dialog
 * reopened later would be a loaded gun on a screen someone put down.
 *
 * The daemon takes the same position independently -- every DELETE needs
 * `{"confirm": true}` -- and the two are deliberately not the same check. This
 * one is about the owner's intent; that one is about a request that never
 * passed through this dialog at all.
 *
 * ## Why the copy is so specific
 *
 * `kill window "api" — 3 panes, one running claude` is the difference between
 * killing the window you meant and the window you were looking at 1.5 seconds
 * ago. The counts come from the snapshot, so they are as old as the sidebar
 * beside them; the daemon decides what actually happens, and a stale id kills
 * nothing and says so.
 *
 * The warnings under it are the cascade the design verified on a live server:
 * a window losing its last pane closes, a session losing its last window is
 * destroyed, and the *group* goes with it -- this app's own member included --
 * which drops the socket of every tab attached to it. That is why a kill three
 * levels down can end with "this tab disconnects".
 *
 * ## Shape
 *
 * The body is a separate export with no Radix in it, because the suite renders
 * with `react-dom/server` in a node environment and an open Radix dialog is a
 * portal into a `document` that does not exist there. Everything worth pinning
 * -- that the button is inert unarmed, that the counts and the warnings are on
 * screen -- is in the body.
 */

import { TriangleAlert } from 'lucide-react'

import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import type { KillPlan } from '@/lib/manage'

/** Nothing is being killed, and nothing is armed. */
export interface KillDialogState {
  /** The kill being considered, or null when the dialog is closed. */
  plan: KillPlan | null
  /** The toggle. Never survives a dismissal -- see `killDialogReducer`. */
  armed: boolean
  /** A request is in flight; the button is inert and says so. */
  busy: boolean
}

export const KILL_CLOSED: KillDialogState = { plan: null, armed: false, busy: false }

export type KillDialogEvent =
  | { type: 'open'; plan: KillPlan }
  | { type: 'dismiss' }
  | { type: 'arm'; armed: boolean }
  | { type: 'send' }

/**
 * The dialog's whole state machine, out here where a test can run it.
 *
 * The rule this exists to protect is `dismiss` clearing `armed`. Held in a
 * reducer rather than in a `setArmed(false)` inside an `onOpenChange` handler,
 * because a handler is exactly the kind of code `renderToStaticMarkup` never
 * runs -- and "the toggle survived a dismissal" is a bug that leaves a red
 * button live under a fingertip.
 *
 * `open` resets it too, so a dialog opened on a *different* target can never
 * inherit an arming meant for the previous one.
 */
export function killDialogReducer(
  state: KillDialogState,
  event: KillDialogEvent,
): KillDialogState {
  switch (event.type) {
    case 'open':
      return { plan: event.plan, armed: false, busy: false }
    case 'dismiss':
      return KILL_CLOSED
    case 'arm':
      return { ...state, armed: event.armed }
    case 'send':
      return { ...state, busy: true }
  }
}

/** Whether the red button does anything. */
export function canKill(state: KillDialogState): boolean {
  return state.plan !== null && state.armed && !state.busy
}

export interface KillDialogBodyProps {
  plan: KillPlan
  armed: boolean
  busy: boolean
  onArmedChange: (armed: boolean) => void
  onConfirm: () => void
}

/** The dialog's contents, with no portal around them. */
export function KillDialogBody({
  plan,
  armed,
  busy,
  onArmedChange,
  onConfirm,
}: KillDialogBodyProps) {
  // One rule, shared with the handler in App: `canKill`.
  const disabled = !canKill({ plan, armed, busy })
  return (
    <>
      <DialogHeader>
        <DialogTitle className="first-letter:uppercase">{plan.title}</DialogTitle>
        <DialogDescription>{plan.detail}</DialogDescription>
      </DialogHeader>

      {plan.warnings.length > 0 && (
        <ul className="text-destructive space-y-1.5 text-sm" role="alert">
          {plan.warnings.map((warning) => (
            <li key={warning} className="flex gap-1.5">
              <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden />
              <span>{warning}</span>
            </li>
          ))}
        </ul>
      )}

      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={armed}
          disabled={busy}
          onChange={(e) => onArmedChange(e.target.checked)}
          className="accent-destructive size-4"
        />
        <span>Yes, {plan.title}</span>
      </label>

      <DialogFooter>
        <Button
          type="button"
          variant="destructive"
          // Inert until the toggle is flipped. This is the two-step, and it is
          // the one attribute the tests below assert on both sides of.
          disabled={disabled}
          onClick={onConfirm}
        >
          {busy ? 'Killing…' : plan.confirmLabel}
        </Button>
      </DialogFooter>
    </>
  )
}

export interface KillDialogProps {
  state: KillDialogState
  onDismiss: () => void
  onArmedChange: (armed: boolean) => void
  onConfirm: () => void
}

export function KillDialog({ state, onDismiss, onArmedChange, onConfirm }: KillDialogProps) {
  return (
    <Dialog
      open={state.plan !== null}
      onOpenChange={(next) => {
        if (!next) onDismiss()
      }}
    >
      <DialogContent className="sm:max-w-md">
        {state.plan && (
          <KillDialogBody
            plan={state.plan}
            armed={state.armed}
            busy={state.busy}
            onArmedChange={onArmedChange}
            onConfirm={onConfirm}
          />
        )}
      </DialogContent>
    </Dialog>
  )
}

export default KillDialog
