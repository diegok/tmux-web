/**
 * Reading a pane's scrollback: `GET /api/panes/{id}/capture`.
 *
 * One bounded `capture-pane` behind the device cookie, and the only way this
 * app reads back further than the visible screen. It is a GET because it
 * changes nothing -- the daemon's exact-Origin rule is scoped to state-changing
 * requests -- so unlike everything in `manage.ts` it needs no Origin of its
 * own, and unlike everything in `manage.ts` it forces no poll on the daemon.
 *
 * ## The depth lives on the daemon
 *
 * A caller that does not name a depth sends no `lines` parameter at all, and
 * the daemon applies `defaultCaptureLines`. A default spelled here as well
 * would be a second one, and the day they disagreed the panel would say "the
 * last N lines" about a capture of some other depth. The answer carries the
 * depth that was actually used -- after the daemon's clamp -- and that is what
 * a caller should render.
 *
 * `lines` is validated on the daemon before it reaches tmux: a value that is
 * not a whole number is a 400 and never becomes part of an argument list.
 * Nothing here needs to guard it, and nothing here should pretend to.
 *
 * ## Nothing is retried
 *
 * As in `manage.ts`: a failure comes back with the daemon's own sentence --
 * tmux's words for a pane that is gone -- and the caller decides what to show.
 * This never throws, because every caller would otherwise wrap it in the same
 * try/catch inside a component, where no test reaches it.
 */

import type { FetchLike } from '@/lib/useSnapshot'
import { PANES_URL, refusal } from '@/lib/manage'

/** One capture, exactly as the daemon sends it. */
export interface Capture {
  /** The pane it came from, as the daemon decoded it: `%3`. */
  paneId: string
  /** The scrollback plus the visible screen, plain text, no escape sequences. */
  text: string
  /** How deep the daemon actually went, after its own clamp. */
  lines: number
  /**
   * Whether the oldest part was dropped to fit the daemon's byte cap. The
   * capture then begins mid-screen, and a panel that did not say so would be
   * showing a conversation that silently starts in the middle.
   */
  truncated: boolean
  /** Unix milliseconds, on the daemon's clock. */
  capturedAt: number
}

/** What the daemon said. Never a thrown error; see the module comment. */
export type CaptureResult = { ok: true; capture: Capture } | { ok: false; message: string }

/**
 * The URL one capture is fetched from.
 *
 * The pane id is percent-encoded for the same reason every management route's
 * is: `%` is not legal raw in a path, so `/api/panes/%3/capture` is a 400 from
 * net/http before the mux is ever consulted. `lines` is left off entirely when
 * the caller names no depth -- see the module comment.
 */
export function captureUrl(paneId: string, lines?: number): string {
  const base = `${PANES_URL}/${encodeURIComponent(paneId)}/capture`
  return lines === undefined ? base : `${base}?lines=${encodeURIComponent(String(lines))}`
}

const browserFetch: FetchLike = (url, init) => globalThis.fetch(url, init)

/** Fetch one pane's scrollback. This never throws. */
export async function fetchCapture(
  paneId: string,
  opts: { lines?: number; signal?: AbortSignal; fetchImpl?: FetchLike } = {},
): Promise<CaptureResult> {
  const fetchImpl = opts.fetchImpl ?? browserFetch
  try {
    const res = await fetchImpl(captureUrl(paneId, opts.lines), {
      signal: opts.signal,
      headers: { Accept: 'application/json' },
      // The device cookie is same-origin and HttpOnly; spelled out so a future
      // absolute URL does not quietly drop it.
      credentials: 'same-origin',
    })
    if (!res.ok) return { ok: false, message: await refusal(res) }
    const capture = asCapture(await res.json())
    if (!capture) return { ok: false, message: 'the daemon sent something that was not a capture' }
    return { ok: true, capture }
  } catch (err) {
    return { ok: false, message: err instanceof Error ? err.message : String(err) }
  }
}

/**
 * The body, checked field by field.
 *
 * A 200 whose body is missing `truncated` is not a capture with `truncated:
 * false` -- it is a daemon this build does not understand, and treating the
 * absent field as "nothing was dropped" is the one wrong answer. Every field
 * is required for the same reason.
 */
function asCapture(body: unknown): Capture | null {
  if (typeof body !== 'object' || body === null) return null
  const c = body as Record<string, unknown>
  if (typeof c.paneId !== 'string' || c.paneId === '') return null
  if (typeof c.text !== 'string') return null
  if (typeof c.lines !== 'number' || typeof c.capturedAt !== 'number') return null
  if (typeof c.truncated !== 'boolean') return null
  return {
    paneId: c.paneId,
    text: c.text,
    lines: c.lines,
    truncated: c.truncated,
    capturedAt: c.capturedAt,
  }
}
