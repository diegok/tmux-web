/**
 * The reply box: what one press of a send control puts on the wire, and the
 * three rules about the box itself that are decisions rather than styling.
 *
 * Rendered with `react-dom/server`, as everything else in this suite is, which
 * is why every decision under test is an exported pure function rather than a
 * line inside a handler. `renderToStaticMarkup` fires no handlers: a rule that
 * lives in `onKeyDown` is a rule no test here can reach, and the box is almost
 * entirely handlers.
 *
 * What this suite deliberately cannot prove: that `send` reaches the socket.
 * `ReplyBox` hands its frames to a sink, and the sink App passes is the
 * terminal handle -- whose `send` is `TerminalSession.write` (the transport)
 * and not `useTerminal().write` (the local canvas). Swap those two and the
 * reply is painted into the terminal, character for character, looking exactly
 * like it worked, while the pane receives nothing; the markup is identical
 * either way, so nothing below goes red. What catches it is
 * `internal/front/endmode_integration_test.go` and a human looking at a pane.
 */

import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'

import {
  PASTE_END,
  PASTE_START,
  REPLY_OPEN_KEY,
  REPLY_PLACEHOLDER,
  ReplyBox,
  dispatchReply,
  draftAfterPaneChange,
  draftOrigin,
  focusesOnOpen,
  readReplyOpen,
  replyFrames,
  replyHint,
  replyKey,
  replyOpenFromStored,
  replyToggleLabel,
  showReplyBox,
  willBracket,
  writeReplyOpen,
} from './ReplyBox'
import type { ReplyOpenStore, ReplySink } from './ReplyBox'

/** The bytes of a string, as numbers, which is the only way to see a stray CR. */
function codes(s: string): number[] {
  return [...s].map((c) => c.charCodeAt(0))
}

/** The data frame of a send, or a failure naming what came back instead. */
function dataOf(frames: ReturnType<typeof replyFrames>): string {
  const last = frames.at(-1)
  if (!last || last.kind !== 'data') throw new Error(`no data frame in ${JSON.stringify(frames)}`)
  return last.bytes
}

/** A keydown, as `replyKey` reads one: no modifier held unless it is named. */
function key(k: string, held: Partial<Record<'shiftKey' | 'ctrlKey' | 'altKey' | 'metaKey', boolean>> = {}) {
  return { key: k, shiftKey: false, ctrlKey: false, altKey: false, metaKey: false, ...held }
}

/**
 * One element's class list, as tokens.
 *
 * Tokens and never a substring, which is this project's recorded trap: a
 * `toContain('disabled')` over the class string passes on shadcn's
 * `disabled:pointer-events-none`, and `size-8` is inside `size-8.5`.
 */
function classesOf(tag: string): string[] {
  const m = tag.match(/class="([^"]*)"/)
  return m ? m[1].split(/\s+/).filter(Boolean) : []
}

/** Every `<button>` opening tag, in document order. */
function buttons(markup: string): string[] {
  return [...markup.matchAll(/<button[^>]*>/g)].map((m) => m[0])
}

/** The `<textarea>` opening tag. */
function textarea(markup: string): string {
  const m = markup.match(/<textarea[^>]*>/)
  if (!m) throw new Error('the reply box rendered no textarea')
  return m[0]
}

describe('replyFrames', () => {
  it('sends nothing at all for an empty reply', () => {
    // `[]` and not "no data frame": an empty send has nothing to protect, and
    // an `end-mode` on its own would still pop the owner's copy mode -- his
    // scroll position, thrown away by a stray Enter.
    expect(replyFrames('', { pane: '%3', withReturn: true, origin: 'type' })).toEqual([])
    expect(replyFrames('', { pane: '%3', withReturn: false, origin: 'type' })).toEqual([])
  })

  it('sends a whitespace-only reply, which is not an empty one', () => {
    // A space is a legitimate answer -- it is what "press any key" wants, and
    // what advances a pager. The natural guess is the opposite, so it is
    // pinned here rather than left to whichever `trim()` someone adds later.
    expect(replyFrames(' ', { pane: '%3', withReturn: false, origin: 'type' })).toEqual([
      { kind: 'end-mode', pane: '%3' },
      { kind: 'data', bytes: ' ' },
    ])
  })

  it('cancels the pane s mode before the bytes, in that order', () => {
    // An ordered array and not two membership checks: the order *is* the
    // finding. Task 16 measured it through a real PTY -- with the pane in copy
    // mode, `echo quit PARTIAL\r` loses its `q` to the cancel key and the
    // shell runs `uit PARTIAL`.
    expect(replyFrames('hi', { pane: '%3', withReturn: true, origin: 'type' })).toEqual([
      { kind: 'end-mode', pane: '%3' },
      { kind: 'data', bytes: 'hi\r' },
    ])
  })

  it('ends the line with CR and never LF', () => {
    // The headline. `0x0a` is `C-j`, a different key, and an agent's prompt
    // does not answer to it.
    const [, data] = replyFrames('hi', { pane: '%3', withReturn: true, origin: 'type' })
    expect(data).toEqual({ kind: 'data', bytes: 'hi\r' })
    if (data.kind !== 'data') throw new Error('the second frame is the data')
    expect([...data.bytes].map((c) => c.charCodeAt(0))).toEqual([0x68, 0x69, 0x0d])
  })

  it('sends the bare text with no return when asked', () => {
    // The `y/n` case: one character, no Return, or the prompt takes the answer
    // and the Return as a second keystroke into whatever came next.
    expect(replyFrames('y', { pane: '%3', withReturn: false, origin: 'type' })).toEqual([
      { kind: 'end-mode', pane: '%3' },
      { kind: 'data', bytes: 'y' },
    ])
  })

  it('makes the two send controls differ by exactly the return', () => {
    // So that inverting `withReturn` cannot pass both cases above.
    const withReturn = replyFrames('y', { pane: '%3', withReturn: true, origin: 'type' })
    const without = replyFrames('y', { pane: '%3', withReturn: false, origin: 'type' })
    expect(withReturn).not.toEqual(without)
    expect(withReturn.at(-1)).toEqual({ kind: 'data', bytes: 'y\r' })
    expect(without.at(-1)).toEqual({ kind: 'data', bytes: 'y' })
  })

  it('still sends when the tab has not learned its pane', () => {
    // `end-mode` names a pane and the server refuses an unnamed one, so there
    // is no frame to send -- but the data goes anyway. The bytes reach this
    // tab's client wherever tmux left it, which is where the user is looking.
    expect(replyFrames('hi', { pane: null, withReturn: true, origin: 'type' })).toEqual([
      { kind: 'data', bytes: 'hi\r' },
    ])
  })
})

describe('dispatchReply', () => {
  /** A sink that records the calls, in order, as the wire would see them. */
  function recorder() {
    const calls: string[] = []
    const sink: ReplySink = {
      send(bytes) {
        calls.push(`send ${typeof bytes === 'string' ? bytes : '<bytes>'}`)
        return true
      },
      endMode(pane) {
        calls.push(`end-mode ${pane}`)
        return true
      },
    }
    return { calls, sink }
  }

  it('walks the frames in order, onto the sink s two methods', () => {
    const { calls, sink } = recorder()
    dispatchReply(replyFrames('hi', { pane: '%3', withReturn: true, origin: 'type' }), sink)
    expect(calls).toEqual(['end-mode %3', 'send hi\r'])
  })

  it('touches nothing for an empty reply', () => {
    const { calls, sink } = recorder()
    dispatchReply(replyFrames('', { pane: '%3', withReturn: true, origin: 'type' }), sink)
    expect(calls).toEqual([])
  })

  it('drops the frames when there is no terminal', () => {
    expect(() => dispatchReply([{ kind: 'data', bytes: 'hi' }], null)).not.toThrow()
  })
})

/**
 * Bracketed paste.
 *
 * Measured on tmux 3.7b through the same PTY a tab's bytes take -- Claude Code
 * 2.1.267, opencode 1.18.30 and pi all set `DECSET 2004` at their prompt and
 * all three honour the wrappers, so this is the wrapping branch of Task 18 and
 * not the refusal one. What the measurement also found, against the plan's
 * premise: a bare LF is *not* submit in any of the three -- it inserts a line,
 * and only CR (`0x0d`) submits. The wrappers are still what the bytes carry,
 * because "LF happens not to submit in these three builds today" is an
 * accident of three versions and `ESC[200~` is a contract; and because opencode
 * only draws its `[Pasted ~3 lines]` chip when the wrappers told it a paste
 * happened, which is the agent understanding the input rather than tolerating
 * it.
 */
describe('bracketed paste', () => {
  it('has the real escape byte in both halves, not a retyped one', () => {
    // The recorded trap in this repo is an escape sequence copied out of prose:
    // a two-character `\e`, or a rendered `^[`, both of which are ordinary text
    // and neither of which tmux reads as a paste.
    expect(codes(PASTE_START)).toEqual([0x1b, 0x5b, 0x32, 0x30, 0x30, 0x7e])
    expect(codes(PASTE_END)).toEqual([0x1b, 0x5b, 0x32, 0x30, 0x31, 0x7e])
  })

  it('wraps a multi-line paste in exactly those bytes and no others', () => {
    // The whole feature in one assertion, spelled as bytes: the two wrappers,
    // the interior newline still `0x0a`, and nothing after `ESC[201~`. A
    // trailing CR here is the submit the wrapping exists to withhold, and a
    // missing `ESC[201~` leaves the agent's prompt in paste mode forever.
    const frames = replyFrames('a\nb', { pane: '%3', withReturn: true, origin: 'paste' })
    expect(codes(dataOf(frames))).toEqual([
      0x1b, 0x5b, 0x32, 0x30, 0x30, 0x7e, 0x61, 0x0a, 0x62, 0x1b, 0x5b, 0x32, 0x30, 0x31, 0x7e,
    ])
  })

  it('still cancels the pane s mode first', () => {
    // A paste is prose too, and prose in copy mode loses its first cancel key.
    expect(replyFrames('a\nb', { pane: '%3', withReturn: true, origin: 'paste' })[0]).toEqual({
      kind: 'end-mode',
      pane: '%3',
    })
  })

  it('leaves a single-line paste alone, Return and all', () => {
    // No interior newline, nothing to protect, and a Return the user wants:
    // a one-line paste is the ordinary case and must keep behaving like one.
    // This is also what a rule that fired on every paste would break.
    expect(replyFrames('one line', { pane: '%3', withReturn: true, origin: 'paste' })).toEqual([
      { kind: 'end-mode', pane: '%3' },
      { kind: 'data', bytes: 'one line\r' },
    ])
  })

  it('leaves a typed multi-line draft alone', () => {
    // Same text, two origins, two answers -- the pair is the assertion. A
    // Shift+Enter line is one the user asked for with the pane in front of
    // them, and it still ends in the Return that submits it.
    const pasted = dataOf(replyFrames('a\nb', { pane: '%3', withReturn: true, origin: 'paste' }))
    const typed = dataOf(replyFrames('a\nb', { pane: '%3', withReturn: true, origin: 'type' }))
    expect(typed).toBe('a\nb\r')
    expect(pasted).not.toBe(typed)
    expect(typed).not.toContain(PASTE_START)
  })

  it('gives both send controls the same bytes for a wrapped paste', () => {
    // `withReturn` is what the two buttons differ by everywhere else, and the
    // one place it stops mattering is here: there is no Return to withhold.
    const a = replyFrames('a\nb', { pane: '%3', withReturn: true, origin: 'paste' })
    const b = replyFrames('a\nb', { pane: '%3', withReturn: false, origin: 'paste' })
    expect(a).toEqual(b)
  })

  it('sends nothing for an empty paste, as for an empty anything', () => {
    expect(replyFrames('', { pane: '%3', withReturn: true, origin: 'paste' })).toEqual([])
  })
})

describe('willBracket', () => {
  it('is true only for a pasted draft with a newline in it', () => {
    expect(willBracket('a\nb', 'paste')).toBe(true)
    expect(willBracket('a\nb', 'type')).toBe(false)
    expect(willBracket('a b', 'paste')).toBe(false)
    expect(willBracket('a b', 'type')).toBe(false)
  })

  it('counts a trailing newline, which is what a copied line ends with', () => {
    // Selecting a whole line in a terminal or an editor takes its newline with
    // it, so `"one line\n"` is the commonest paste there is -- and it is a
    // multi-line paste as far as an agent's prompt is concerned, because the
    // newline is still a keystroke the prompt will read.
    expect(willBracket('one line\n', 'paste')).toBe(true)
  })
})

describe('draftOrigin', () => {
  it('takes the paste when this change was one', () => {
    expect(draftOrigin('type', 'a\nb', true)).toBe('paste')
  })

  it('stays a paste while the user types under it', () => {
    // Paste a stack trace, then type "what causes this?" beneath it: the
    // twelve lines above the question are still twelve lines.
    expect(draftOrigin('paste', 'a\nb what causes this?', false)).toBe('paste')
  })

  it('is typing again once the box is empty', () => {
    // Otherwise a draft cleared by hand and retyped would still be wrapped,
    // and the origin would never come back without a remount.
    expect(draftOrigin('paste', '', false)).toBe('type')
  })

  it('leaves a typed draft typed', () => {
    expect(draftOrigin('type', 'hello', false)).toBe('type')
  })
})

describe('replyKey', () => {
  it('sends on a plain Enter', () => {
    expect(replyKey(key('Enter'))).toBe('send')
  })

  it('inserts a newline on Shift+Enter', () => {
    expect(replyKey(key('Enter', { shiftKey: true }))).toBe('newline')
  })

  it('closes back to the terminal on Escape', () => {
    // Task 23 turned this from a blur into a close: the box now owns rows of a
    // window the owner's own client shares, so leaving it open with the caret
    // elsewhere is leaving his terminal short. What Escape means to the *user*
    // is unchanged -- "give the keyboard back to the pane".
    expect(replyKey(key('Escape'))).toBe('close')
  })

  it('lets the palette chord through', () => {
    // Ctrl+Alt+K is installed in the capture phase on `window`, ahead of
    // wterm's own handler. The box must not be a third thing that eats it.
    //
    // Documentation more than a guard, and measured as such: `K` is neither
    // Enter nor Escape, so this stays green under every mutation of the
    // modifier check. What actually protects the chord is the case below --
    // the modifier guard, whose loss shows up on a modified Enter -- together
    // with `onKeyDown` calling `preventDefault` on `send` and `blur` only.
    expect(replyKey(key('k', { ctrlKey: true, altKey: true }))).toBe('pass')
    expect(replyKey(key('K', { ctrlKey: true, altKey: true }))).toBe('pass')
  })

  it('lets a modified Enter through rather than guessing at it', () => {
    expect(replyKey(key('Enter', { ctrlKey: true }))).toBe('pass')
    expect(replyKey(key('Enter', { altKey: true }))).toBe('pass')
    expect(replyKey(key('Enter', { metaKey: true }))).toBe('pass')
  })

  it('lets ordinary typing through', () => {
    expect(replyKey(key('a'))).toBe('pass')
    expect(replyKey(key('c', { ctrlKey: true }))).toBe('pass')
  })
})

describe('draftAfterPaneChange', () => {
  it('keeps the draft while the pane has not moved', () => {
    expect(draftAfterPaneChange('half a sen', '%3', '%3')).toEqual({
      draft: 'half a sen',
      discarded: false,
    })
  })

  it('clears the draft when the pane moves', () => {
    // The bytes go to this tab's client, which follows the selection, so a
    // sentence typed against one agent would land in another one's prompt.
    expect(draftAfterPaneChange('half a sen', '%3', '%7')).toEqual({
      draft: '',
      discarded: true,
    })
  })

  it('says nothing when there was nothing to clear', () => {
    expect(draftAfterPaneChange('', '%3', '%7')).toEqual({ draft: '', discarded: false })
  })

  it('clears when the pane is first learned, and when it is lost', () => {
    expect(draftAfterPaneChange('hi', null, '%3')).toEqual({ draft: '', discarded: true })
    expect(draftAfterPaneChange('hi', '%3', null)).toEqual({ draft: '', discarded: true })
  })
})

describe('replyHint', () => {
  it('names the pane the bytes will reach', () => {
    expect(replyHint('%3', false, false)).toContain('%3')
  })

  it('says the draft was cleared, rather than clearing it silently', () => {
    const said = replyHint('%7', true, false)
    expect(said).not.toBe(replyHint('%7', false, false))
    expect(said.toLowerCase()).toContain('cleared')
  })

  it('says something useful before the tab has learned its pane', () => {
    expect(replyHint(null, false, false)).not.toBe('')
    expect(replyHint(null, false, false)).not.toContain('null')
  })

  it('warns that a wrapped paste carries no Return', () => {
    // The one case where Send does not do what its label says. A user who
    // pastes twelve lines, presses Send and sees the agent sit there needs the
    // box to have told them beforehand, not the pane to tell them after.
    const wrapped = replyHint('%3', false, true)
    expect(wrapped).not.toBe(replyHint('%3', false, false))
    expect(wrapped).toContain('%3')
    expect(wrapped.toLowerCase()).toContain('no return')
  })

  it('still leads with the discarded draft, which is the more urgent news', () => {
    // A cleared draft empties the box, so there is nothing left to wrap; if
    // the two ever disagree the user needs to hear about the lost sentence.
    expect(replyHint('%3', true, true)).toBe(replyHint('%3', true, false))
  })
})

/**
 * Presence: the box is a function of the user having asked for it, and of the
 * pane's agent state never.
 *
 * Four fixtures rather than one loop over a single render, as with the
 * sidebar's branch chip, because the mistake this exists to catch is a box
 * gated on `agentState === 'blocked'` -- and that mistake gets the blocked
 * fixture accidentally right. Three of the four below go red on it.
 *
 * `showReplyBox` takes the agent state and ignores it, which is the whole point
 * of its signature: the argument is there so the mutant is expressible and this
 * test can kill it. Three reasons the box does not move on that state, in the
 * order they matter: a control that vanishes under you while you are typing
 * costs the sentence; `blocked` is 1.5-6 s late, so a box gated on it arrives
 * after you wanted it; and the box is also how you answer the four Claude
 * notifications the README lists that produce no badge on either authority.
 *
 * The `open` flag is not a hole in that rule and the pair of `it.each` blocks
 * below is what says so: the box is the same at all four states with the toggle
 * on, and absent at all four with it off. What moves it is a person pressing
 * something, which is the one input a control is allowed to move on.
 */
const everyState = ['', 'working', 'blocked', 'idle']

/** Exactly the composition App makes: the rule decides, the box renders. */
function footer(agentState: string, open = true): string {
  return showReplyBox({ attached: true, agentState, open })
    ? renderToStaticMarkup(<ReplyBox pane="%3" onSend={() => {}} onClose={() => {}} />)
    : ''
}

describe('the box itself', () => {
  it.each(everyState)('is on screen on a %s pane once it is open', (agentState) => {
    expect(footer(agentState)).toContain('<textarea')
  })

  it.each(everyState)('is off screen on a %s pane while it is closed', (agentState) => {
    // The owner's complaint, as a rule: "no debería mostrarse a menos que yo
    // decida verlo". Closed is closed whatever the agent is doing -- an agent
    // going `blocked` must not bring the box back any more than it may take it
    // away.
    expect(footer(agentState, false)).toBe('')
  })

  it('renders identically at every agent state', () => {
    // The property Task 17 pinned, still pinned: same markup, not merely
    // "present in all four". A box that changed its hint, its size or its
    // buttons on the state would be a box that moves on the state.
    const rendered = everyState.map((state) => footer(state))
    expect(new Set(rendered).size).toBe(1)
  })

  it('is not on screen when this tab is attached to nothing', () => {
    // Even with the toggle remembered on: there is no pane to reply to.
    expect(showReplyBox({ attached: false, agentState: 'blocked', open: true })).toBe(false)
  })

  it('is closed by default, before anyone has decided anything', () => {
    // The whole of Task 23 in one line. On a desktop the pane *is* the text
    // box, and this one costs rows of a window the owner's own tmux client is
    // attached to.
    expect(replyOpenFromStored(null)).toBe(false)
    expect(showReplyBox({ attached: true, agentState: 'blocked', open: replyOpenFromStored(null) })).toBe(
      false,
    )
  })

  it('does not take focus on mount', () => {
    // The attribute must be *absent*, which is a different assertion from it
    // being false: React omits `autoFocus={false}` and renders `autofocus=""`
    // for a truthy one, so only absence rules out an autofocusing box. This is
    // the remembered-open box on page load: it is on screen because this
    // browser last left it on, which is not the same as being asked for now.
    expect(textarea(footer(''))).not.toMatch(/autofocus/i)
  })

  it('takes the keyboard when the toggle is what opened it', () => {
    // The other half, and the reason `autoFocus` is a prop rather than a
    // constant: pressing "Reply box" is a person asking to type, and making
    // them then click the box would be the toggle doing half its job.
    const asked = renderToStaticMarkup(
      <ReplyBox pane="%3" onSend={() => {}} onClose={() => {}} autoFocus />,
    )
    expect(textarea(asked)).toMatch(/autofocus/i)
  })

  it('does not offer Ctrl+C, which the browser has already taken', () => {
    // Ctrl+C in a text field is the platform's copy and cannot be reclaimed.
    // The interrupt stays something you send by focusing the terminal, and the
    // box must not imply otherwise.
    expect(REPLY_PLACEHOLDER).not.toMatch(/ctrl|\^c|interrupt/i)
    expect(footer('').toLowerCase()).not.toContain('ctrl+c')
  })

  it('offers two send controls, the no-return one smaller', () => {
    const [send, keys] = buttons(footer(''))
    // `data-size` is a real attribute shadcn's Button writes; a size looked
    // for in the class string is the trap this file's `classesOf` exists for.
    expect(send).toContain('data-size="sm"')
    expect(keys).toContain('data-size="xs"')
    expect(classesOf(send)).not.toEqual(classesOf(keys))
  })

  it('takes its own space rather than lying over the terminal', () => {
    // Task 21 made the box an overlay and Task 23 undoes it: it covered the
    // bottom two rows, which on a settled pane are the prompt and the agent's
    // question -- exactly what you are reading while you answer. `shrink-0` is
    // the other half of being a flex sibling: the box keeps its height and the
    // terminal above it gives up the rows.
    //
    // Tokens, never a substring: `absolute` is inside `md:absolute` and inside
    // the connection pill's own classes further down the tree.
    const wrapper = footer('').match(/<div[^>]*>/)![0]
    expect(classesOf(wrapper)).not.toContain('absolute')
    expect(classesOf(wrapper)).toContain('shrink-0')
    // What the e2e suite proves and this cannot: with the box open, the
    // terminal's last row is still on screen. See `e2e/reply.spec.ts`.
  })

  it('says where the bytes go', () => {
    const hint = replyHint('%3', false, false)
    expect(hint).not.toBe('')
    expect(footer('')).toContain(hint)
  })
})

/**
 * The memory: one browser's answer to "do I want this box", and nothing else's.
 *
 * `localStorage` and not `sessionStorage`, which is what the pane and the
 * session are filed under: those are properties of a tab's current position and
 * are meant to be forgotten, while this is a property of the *device*. A phone
 * keeps the box open across every visit and a laptop keeps it shut, which is
 * the whole of what the owner asked for.
 *
 * Every read and write goes through a try/catch, because a Safari private
 * window throws on the property access itself rather than returning null. The
 * cost of a refusal is the default -- closed -- and never a page that does not
 * render.
 */
describe('remembering the toggle', () => {
  /** A store that answers, and records what it was told. */
  function memory(seed: Record<string, string> = {}) {
    const map = new Map(Object.entries(seed))
    const store: ReplyOpenStore = {
      getItem: (k) => map.get(k) ?? null,
      setItem: (k, v) => void map.set(k, v),
    }
    return { map, store }
  }

  /** A store that throws on both halves, as a browser blocking site data does. */
  const hostile: ReplyOpenStore = {
    getItem() {
      throw new Error('SecurityError: the operation is insecure')
    },
    setItem() {
      throw new Error('SecurityError: the operation is insecure')
    },
  }

  it('is closed for a browser that has never said otherwise', () => {
    expect(readReplyOpen(memory().store)).toBe(false)
    expect(readReplyOpen(null)).toBe(false)
  })

  it('reads back what it wrote, both ways round', () => {
    // A round trip and not two assertions about a literal: what is stored is
    // this module's business, and a test that spelled the value would pass a
    // reader that had stopped agreeing with the writer.
    const { store } = memory()
    writeReplyOpen(store, true)
    expect(readReplyOpen(store)).toBe(true)
    writeReplyOpen(store, false)
    expect(readReplyOpen(store)).toBe(false)
  })

  it('files it under a key of its own, alongside the tab s other memories', () => {
    const { map, store } = memory()
    writeReplyOpen(store, true)
    expect([...map.keys()]).toEqual([REPLY_OPEN_KEY])
    expect(REPLY_OPEN_KEY).toContain('tmux-web:')
  })

  it('treats anything it does not recognise as closed', () => {
    // A value from an older build, a half-written key, a user poking at
    // devtools. None of those may leave the box open, and none may throw.
    expect(replyOpenFromStored('')).toBe(false)
    expect(replyOpenFromStored('yes')).toBe(false)
    expect(replyOpenFromStored('true')).toBe(false)
    expect(replyOpenFromStored('null')).toBe(false)
    expect(readReplyOpen(memory({ [REPLY_OPEN_KEY]: 'banana' }).store)).toBe(false)
  })

  it('renders closed rather than throwing when storage refuses', () => {
    // A private window is a supported way to use this app, not a crash.
    expect(() => readReplyOpen(hostile)).not.toThrow()
    expect(readReplyOpen(hostile)).toBe(false)
    expect(() => writeReplyOpen(hostile, true)).not.toThrow()
  })
})

describe('focusesOnOpen', () => {
  it('focuses a box the user just asked for, and not one merely restored', () => {
    // The pair is the assertion. A remembered-open box on load must leave the
    // keyboard where it was -- on a phone, focusing it would raise the soft
    // keyboard on every visit -- while the toggle is a person saying "I want to
    // type now".
    expect(focusesOnOpen('toggle')).toBe(true)
    expect(focusesOnOpen('restore')).toBe(false)
  })
})

describe('replyToggleLabel', () => {
  it('says what pressing it will do, not what the box is', () => {
    // The palette row reads as a command. A label naming the current state is
    // the recorded way to write a toggle nobody can predict.
    expect(replyToggleLabel(false).toLowerCase()).toContain('show')
    expect(replyToggleLabel(true).toLowerCase()).toContain('hide')
    expect(replyToggleLabel(true)).not.toBe(replyToggleLabel(false))
  })
})
