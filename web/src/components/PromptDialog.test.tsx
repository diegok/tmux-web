/**
 * The dialog behind every rename and every create: its state machine, and the
 * one thing its markup has to get right -- a submit button that cannot send a
 * blank required field.
 *
 * Rendered as KillDialog.test.tsx explains: inside a bare `<Dialog open>` for
 * the Radix context, never inside `DialogContent`, which is a portal.
 */

import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'

import {
  PROMPT_CLOSED,
  PromptDialogBody,
  promptAction,
  promptDialogReducer,
} from './PromptDialog'
import { Dialog } from '@/components/ui/dialog'
import type { PromptSpec } from '@/lib/manage'

function spec(over: Partial<PromptSpec> = {}): PromptSpec {
  return {
    title: 'Rename window "api"',
    description: 'tmux renames the window itself.',
    fields: [{ name: 'name', label: 'Name', initial: 'api', required: true }],
    submitLabel: 'Rename',
    build: (v) => ({ verb: 'rename-window', window: '@7', name: v.name }),
    ...over,
  }
}

function render(over: { values?: Record<string, string>; busy?: boolean; spec?: PromptSpec } = {}) {
  const s = over.spec ?? spec()
  return renderToStaticMarkup(
    <Dialog open>
      <PromptDialogBody
        spec={s}
        values={over.values ?? { name: 'api' }}
        busy={over.busy ?? false}
        onChange={() => {}}
        onSubmit={() => {}}
      />
    </Dialog>,
  )
}

/** Whether the submit button is inert. The attribute, not the Tailwind class. */
function submitDisabled(markup: string): boolean {
  const buttons = markup.match(/<button[^>]*>[^<]*<\/button>/g) ?? []
  const found = buttons.find((b) => /type="submit"/.test(b))
  if (!found) throw new Error(`no submit button in ${markup}`)
  return / disabled=""/.test(found)
}

describe('promptDialogReducer', () => {
  it('opens on the current values, not on whatever was typed last time', () => {
    const typed = promptDialogReducer(
      promptDialogReducer(PROMPT_CLOSED, { type: 'open', spec: spec() }),
      { type: 'set', field: 'name', value: 'billing' },
    )
    expect(typed.values).toEqual({ name: 'billing' })
    // Reopening on another window: a dialog holding the previous target's name
    // is a rename waiting to happen on the wrong object.
    const other = spec({ fields: [{ name: 'name', label: 'Name', initial: 'notes', required: true }] })
    expect(promptDialogReducer(typed, { type: 'open', spec: other }).values).toEqual({
      name: 'notes',
    })
  })

  it('keeps what was typed when the daemon refuses it', () => {
    const sending = promptDialogReducer(
      promptDialogReducer(
        promptDialogReducer(PROMPT_CLOSED, { type: 'open', spec: spec() }),
        { type: 'set', field: 'name', value: 'a:b' },
      ),
      { type: 'send' },
    )
    const settled = promptDialogReducer(sending, { type: 'settle' })
    // tmux refuses a ":" in a window name, and the toast says so. The fix is
    // one character in the field it was typed in, so the dialog stays open
    // holding it rather than making the owner start again.
    expect(settled.busy).toBe(false)
    expect(settled.spec).not.toBeNull()
    expect(settled.values).toEqual({ name: 'a:b' })
  })

  it('drops everything on dismiss', () => {
    const open = promptDialogReducer(PROMPT_CLOSED, { type: 'open', spec: spec() })
    expect(promptDialogReducer(open, { type: 'dismiss' })).toEqual(PROMPT_CLOSED)
  })

  it('builds the action from what was typed', () => {
    const typed = promptDialogReducer(
      promptDialogReducer(PROMPT_CLOSED, { type: 'open', spec: spec() }),
      { type: 'set', field: 'name', value: 'billing' },
    )
    expect(promptAction(typed)).toEqual({ verb: 'rename-window', window: '@7', name: 'billing' })
  })

  it('builds nothing from a blank required field, or while one is in flight', () => {
    const blank = promptDialogReducer(
      promptDialogReducer(PROMPT_CLOSED, { type: 'open', spec: spec() }),
      { type: 'set', field: 'name', value: '  ' },
    )
    expect(promptAction(blank)).toBeNull()
    expect(promptAction(PROMPT_CLOSED)).toBeNull()

    const sending = promptDialogReducer(
      promptDialogReducer(PROMPT_CLOSED, { type: 'open', spec: spec() }),
      { type: 'send' },
    )
    // Otherwise a double-tap on a phone sends two renames.
    expect(promptAction(sending)).toBeNull()
  })
})

describe('<PromptDialogBody>', () => {
  it('cannot be submitted with a blank required field', () => {
    expect(submitDisabled(render({ values: { name: '' } }))).toBe(true)
    expect(submitDisabled(render({ values: { name: 'api' } }))).toBe(false)
  })

  it('can be submitted blank when the field is optional', () => {
    // Clearing a pane's name is the point of having one.
    const label = spec({
      fields: [{ name: 'label', label: 'Name' }],
      build: (v) => ({ verb: 'label-pane', pane: '%3', label: v.label }),
    })
    expect(submitDisabled(render({ spec: label, values: { label: '' } }))).toBe(false)
  })

  it('shows the field opened on the current value', () => {
    expect(render()).toContain('value="api"')
  })

  it('is inert while the request is away', () => {
    expect(submitDisabled(render({ busy: true }))).toBe(true)
  })
})
