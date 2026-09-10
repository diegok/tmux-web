/**
 * The one dialog behind every rename and every create: a title, one or two text
 * fields, and a button that turns them into a single management call.
 *
 * One component rather than four, because the four differ only in their text.
 * What each one *is* -- its title, its fields, what it builds -- is a
 * `PromptSpec` from `lib/manage.ts`, which is where it can be tested without a
 * renderer; this file is the input elements around it.
 *
 * Nothing here validates a name beyond "a required field is not blank". tmux
 * refuses a leading `-`, a `:`, a `.` and control bytes, and the daemon holds
 * it to a length cap, and every one of those is a rule that exists on the Go
 * side already. A second copy here is a copy that drifts from the one that is
 * enforced; a refusal comes back as a toast in tmux's own words instead.
 *
 * As in KillDialog, the body is exported without the Radix portal around it, so
 * the suite can render it under `react-dom/server` in a node environment.
 */

import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { promptReady, promptValues } from '@/lib/manage'
import type { ManageAction, PromptSpec } from '@/lib/manage'

export interface PromptDialogState {
  spec: PromptSpec | null
  values: Record<string, string>
  busy: boolean
}

export const PROMPT_CLOSED: PromptDialogState = { spec: null, values: {}, busy: false }

export type PromptDialogEvent =
  | { type: 'open'; spec: PromptSpec }
  | { type: 'dismiss' }
  | { type: 'set'; field: string; value: string }
  | { type: 'send' }
  | { type: 'settle' }

/**
 * The prompt's state machine.
 *
 * `open` seeds the values from the spec rather than keeping whatever was typed
 * last time: a rename dialog has to open showing the *current* name, and one
 * that reopened holding the previous target's name is a rename waiting to
 * happen on the wrong object.
 */
export function promptDialogReducer(
  state: PromptDialogState,
  event: PromptDialogEvent,
): PromptDialogState {
  switch (event.type) {
    case 'open':
      return { spec: event.spec, values: promptValues(event.spec), busy: false }
    case 'dismiss':
      return PROMPT_CLOSED
    case 'set':
      return { ...state, values: { ...state.values, [event.field]: event.value } }
    case 'send':
      return { ...state, busy: true }
    case 'settle':
      // A refused rename comes back with the daemon's reason in a toast -- a
      // ":" in the name, a leading "-", a length cap -- and every one of those
      // is fixable in the field it was typed in. So a failure leaves the dialog
      // open holding what was typed, where a dismissal would make the owner
      // reopen it and type the whole thing again.
      return { ...state, busy: false }
  }
}

/** The action a submitted prompt sends, or null when it is not submittable. */
export function promptAction(state: PromptDialogState): ManageAction | null {
  if (!state.spec || state.busy || !promptReady(state.spec, state.values)) return null
  return state.spec.build(state.values)
}

export interface PromptDialogBodyProps {
  spec: PromptSpec
  values: Record<string, string>
  busy: boolean
  onChange: (field: string, value: string) => void
  onSubmit: () => void
}

export function PromptDialogBody({
  spec,
  values,
  busy,
  onChange,
  onSubmit,
}: PromptDialogBodyProps) {
  const ready = promptReady(spec, values)
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        if (ready && !busy) onSubmit()
      }}
      className="contents"
    >
      <DialogHeader>
        <DialogTitle>{spec.title}</DialogTitle>
        {spec.description && <DialogDescription>{spec.description}</DialogDescription>}
      </DialogHeader>

      <div className="space-y-3">
        {spec.fields.map((field, i) => (
          <div key={field.name} className="space-y-1.5">
            <label htmlFor={`prompt-${field.name}`} className="text-sm font-medium">
              {field.label}
            </label>
            <Input
              id={`prompt-${field.name}`}
              value={values[field.name] ?? ''}
              placeholder={field.placeholder}
              // The first field is the one being answered; a rename dialog that
              // opens without the cursor in it costs a click on a phone.
              autoFocus={i === 0}
              disabled={busy}
              onChange={(e) => onChange(field.name, e.target.value)}
            />
            {field.description && (
              <p className="text-muted-foreground text-xs">{field.description}</p>
            )}
          </div>
        ))}
      </div>

      <DialogFooter>
        <Button type="submit" disabled={!ready || busy}>
          {busy ? 'Working…' : spec.submitLabel}
        </Button>
      </DialogFooter>
    </form>
  )
}

export interface PromptDialogProps {
  state: PromptDialogState
  onDismiss: () => void
  onChange: (field: string, value: string) => void
  onSubmit: () => void
}

export function PromptDialog({ state, onDismiss, onChange, onSubmit }: PromptDialogProps) {
  return (
    <Dialog
      open={state.spec !== null}
      onOpenChange={(next) => {
        if (!next) onDismiss()
      }}
    >
      <DialogContent className="sm:max-w-md">
        {state.spec && (
          <PromptDialogBody
            spec={state.spec}
            values={state.values}
            busy={state.busy}
            onChange={onChange}
            onSubmit={onSubmit}
          />
        )}
      </DialogContent>
    </Dialog>
  )
}

export default PromptDialog
