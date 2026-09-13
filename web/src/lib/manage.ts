/**
 * Driving tmux from the browser: create, rename, split, zoom and kill.
 *
 * Everything the management UI decides lives here rather than in a component,
 * for the reason the rest of this app already gives -- `renderToStaticMarkup`
 * runs no effects and fires no handlers, so a rule inside a click handler is a
 * rule no test reaches. The components below this file are wiring: a menu maps
 * over `rowMenu`, a dialog renders a `KillPlan`, and the one place a request is
 * built is `manageRequest`.
 *
 * ## Three things the daemon insists on
 *
 * **Ids, never names.** `%3`, `@7`, `$1`. A row is up to 1.5s old, and a stale
 * id targets nothing while a stale name may target something else -- v1
 * measured `kill-session -t _web-` killing `_web-abcd` and exiting 0.
 *
 * **Ids are percent-encoded.** A pane id contains `%`, and `%3` is an invalid
 * percent-escape: net/http rejects the request line before the mux is
 * consulted, so the failure is a 400 with no route ever reached. Every id goes
 * through `encodeURIComponent`, including `@7` and `$1`, which do not need it
 * -- one rule is cheaper to keep right than three.
 *
 * **Every DELETE carries `{"confirm": true}`.** It mirrors the kill dialog
 * rather than replacing it, so a request that never passed through the dialog
 * cannot destroy a window. It is an anti-footgun, not a security control: the
 * exact-Origin check on the daemon is the boundary.
 *
 * ## Nothing is retried
 *
 * A failed kill that silently succeeded on retry is worse than one that failed.
 * Every failure surfaces as a toast naming what was attempted and tmux's own
 * words, and the sidebar refreshes immediately -- so a row that no longer
 * exists disappears along with the error rather than waiting out a poll.
 *
 * ## "Immediately" takes both sides
 *
 * This side re-fetches `/api/snapshot`, and the daemon forces a poll before it
 * answers a management verb (`front.settle`, `tmux.Poller.PollNow`). Both
 * halves are load-bearing, and for a while only this one existed:
 * `/api/snapshot` serves the poller's CACHED tree and nothing re-polled, so the
 * re-fetch was answered from a read taken up to an interval before the verb
 * ran. A killed row lingering was the mild half. The sharp half is a split,
 * which returns a pane id that is then missing from the very snapshot this side
 * just asked for -- a tab pointed at a pane the daemon has never heard of,
 * which is worse than a stale row.
 */

import type { FetchLike, PaneNode, SessionNode, WindowNode } from '@/lib/useSnapshot'

/** The three collections the daemon exposes. Pinned against its routes in the tests. */
export const SESSIONS_URL = '/api/sessions'
export const WINDOWS_URL = '/api/windows'
export const PANES_URL = '/api/panes'

/**
 * Which way a split goes, in the words the daemon takes.
 *
 * "right" and "down" describe where the *new pane* lands. tmux's own flags
 * (`-h`, `-v`) describe which way the split line runs, which is the opposite of
 * how anyone says it; the mapping is the daemon's, and this side never sends a
 * tmux flag.
 */
export type SplitDirection = 'right' | 'down'

/**
 * One request to the management API, as the UI thinks of it.
 *
 * The fields are tmux ids and the names the owner typed -- there is no URL, no
 * method and no JSON here. `manageRequest` turns one of these into an HTTP
 * request, and it is the only place that knows how.
 */
export type ManageAction =
  | { verb: 'new-session'; name: string; path: string }
  | { verb: 'new-window'; session: string; name: string; fromPane: string }
  | { verb: 'split'; pane: string; direction: SplitDirection }
  | { verb: 'rename-session'; session: string; name: string }
  | { verb: 'rename-window'; window: string; name: string }
  | { verb: 'label-pane'; pane: string; label: string }
  | { verb: 'zoom'; pane: string }
  | { verb: 'kill-session'; session: string }
  | { verb: 'kill-window'; window: string }
  | { verb: 'kill-pane'; pane: string }

/** The HTTP shape of a `ManageAction`. */
export interface ManageRequest {
  method: 'POST' | 'PATCH' | 'DELETE'
  url: string
  /** The JSON body, or null when the endpoint reads none (zoom). */
  body: Record<string, unknown> | null
}

/**
 * A tmux id in a URL path.
 *
 * `encodeURIComponent('%3')` is `%253`. Without it the browser sends `/api/panes/%3`,
 * `%3` is not a valid percent-escape, and net/http answers 400 before the mux
 * runs -- a failure that looks like a broken endpoint rather than a broken
 * caller.
 */
function encodeId(id: string): string {
  return encodeURIComponent(id)
}

/** The `{confirm: true}` every DELETE carries. */
const CONFIRM = { confirm: true }

/** Turn an action into the request that carries it out. */
export function manageRequest(action: ManageAction): ManageRequest {
  switch (action.verb) {
    case 'new-session':
      return { method: 'POST', url: SESSIONS_URL, body: { name: action.name, path: action.path } }
    case 'new-window':
      return {
        method: 'POST',
        url: WINDOWS_URL,
        // Both optional on the daemon's side, and empty means "unset" there:
        // an empty name leaves tmux to name the window itself, and an empty
        // fromPane leaves the working directory to tmux.
        body: { session: action.session, name: action.name, fromPane: action.fromPane },
      }
    case 'split':
      return {
        method: 'POST',
        url: PANES_URL,
        body: { pane: action.pane, direction: action.direction },
      }
    case 'rename-session':
      return {
        method: 'PATCH',
        url: `${SESSIONS_URL}/${encodeId(action.session)}`,
        body: { name: action.name },
      }
    case 'rename-window':
      return {
        method: 'PATCH',
        url: `${WINDOWS_URL}/${encodeId(action.window)}`,
        body: { name: action.name },
      }
    case 'label-pane':
      // An empty label is the dialog's "erase this", not a malformed request,
      // so the field is always sent rather than omitted when blank.
      return {
        method: 'PATCH',
        url: `${PANES_URL}/${encodeId(action.pane)}`,
        body: { label: action.label },
      }
    case 'zoom':
      return { method: 'POST', url: `${PANES_URL}/${encodeId(action.pane)}/zoom`, body: null }
    case 'kill-session':
      return { method: 'DELETE', url: `${SESSIONS_URL}/${encodeId(action.session)}`, body: CONFIRM }
    case 'kill-window':
      return { method: 'DELETE', url: `${WINDOWS_URL}/${encodeId(action.window)}`, body: CONFIRM }
    case 'kill-pane':
      return { method: 'DELETE', url: `${PANES_URL}/${encodeId(action.pane)}`, body: CONFIRM }
  }
}

/**
 * What was attempted, in the words the toast uses.
 *
 * Ids rather than names, deliberately: the message beside it is tmux's own
 * ("can't find pane: %7"), and a sentence naming `%7` next to one naming `%7`
 * is a sentence the owner can act on. The pretty name belongs in the dialog,
 * where there is room for it.
 */
export function describeAction(action: ManageAction): string {
  switch (action.verb) {
    case 'new-session':
      return `create session "${action.name}"`
    case 'new-window':
      return `create a window in ${action.session}`
    case 'split':
      return `split ${action.pane} to the ${action.direction === 'right' ? 'right' : 'bottom'}`
    case 'rename-session':
      return `rename ${action.session} to "${action.name}"`
    case 'rename-window':
      return `rename ${action.window} to "${action.name}"`
    case 'label-pane':
      return action.label === '' ? `clear the label on ${action.pane}` : `label ${action.pane}`
    case 'zoom':
      return `zoom ${action.pane}`
    case 'kill-session':
      return `kill ${action.session}`
    case 'kill-window':
      return `kill ${action.window}`
    case 'kill-pane':
      return `kill ${action.pane}`
  }
}

/** What the daemon said. `id` is the created object on a 201, null on a 204. */
export type ManageResult = { ok: true; id: string | null } | { ok: false; message: string }

const browserFetch: FetchLike = (url, init) => globalThis.fetch(url, init)

/**
 * Send one action and report what happened. This never throws.
 *
 * A rejected fetch is a failure with a message, exactly like a 400, because
 * every caller does the same thing with both: name the attempt, quote the
 * reason, refresh the tree. A thrown error here would only move that decision
 * into a catch block in a component, where no test runs it.
 */
export async function performManage(
  action: ManageAction,
  fetchImpl: FetchLike = browserFetch,
): Promise<ManageResult> {
  const req = manageRequest(action)
  try {
    const res = await fetchImpl(req.url, {
      method: req.method,
      // No CSRF token: the daemon checks Origin against its own exact host,
      // which the browser sets on this request and a page on another host
      // cannot forge. `credentials` keeps the device cookie on it.
      credentials: 'same-origin',
      headers: req.body
        ? { 'Content-Type': 'application/json', Accept: 'application/json' }
        : { Accept: 'application/json' },
      body: req.body ? JSON.stringify(req.body) : undefined,
    })
    if (!res.ok) return { ok: false, message: await refusal(res) }
    return { ok: true, id: await createdId(res) }
  } catch (err) {
    return { ok: false, message: err instanceof Error ? err.message : String(err) }
  }
}

/**
 * The daemon's own sentence for a refusal.
 *
 * It answers `{"error": "..."}` and passes tmux's words through unlaundered --
 * "can't find pane: %7" tells the owner what happened where "management
 * failed" does not. `Protect` answers 401 in plain text, though, so a body that
 * is not JSON leaves the status as all there is to say.
 *
 * Exported for `capture.ts`, which reads the same `{"error": ...}` from the
 * same middleware: a second copy of this is a copy that drifts.
 */
export async function refusal(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { error?: unknown }
    if (typeof body?.error === 'string' && body.error !== '') return body.error
  } catch {
    /* not JSON; fall through to the status */
  }
  return `the daemon answered ${res.status}`
}

/** The id on a 201. A 204 carries no body, and neither does an unparseable one. */
async function createdId(res: Response): Promise<string | null> {
  if (res.status !== 201) return null
  try {
    const body = (await res.json()) as { id?: unknown }
    return typeof body?.id === 'string' && body.id !== '' ? body.id : null
  } catch {
    return null
  }
}

/** A message for the owner: what was tried, and what tmux said about it. */
export interface ManageNotice {
  title: string
  description: string
}

/**
 * Run an action and deal with the outcome: report a failure, refresh either
 * way.
 *
 * Both halves are here rather than in the component so that both are covered.
 * The refresh is unconditional on purpose -- a *successful* split changes the
 * tree just as much as a failed kill does, and waiting out a poll for either
 * makes the sidebar look like it ignored the click.
 */
export async function runManage(
  action: ManageAction,
  deps: {
    notify: (notice: ManageNotice) => void
    refresh: () => void
    fetchImpl?: FetchLike
  },
): Promise<ManageResult> {
  const result = await performManage(action, deps.fetchImpl)
  if (!result.ok) {
    deps.notify({
      title: `Could not ${describeAction(action)}`,
      description: result.message,
    })
  }
  deps.refresh()
  return result
}

// --- what a row offers -------------------------------------------------------

/**
 * A window the wire named no `@N` for.
 *
 * The snapshot carries `windowId` now, so this is the degraded case rather than
 * the normal one: a daemon too old to send the field. Renaming and killing a
 * window are addressed by `@N` on the daemon (`ValidateWindowID`), which is the
 * only thing either verb could send, so `rowMenu` leaves both out rather than
 * offering an entry that cannot work. Everything else -- new window, split,
 * zoom, and every session and pane verb -- is addressed by a session or a pane
 * id and is offered regardless.
 */
export const NO_WINDOW_ID = ''

/**
 * The thing a menu entry was opened on.
 *
 * Every id a verb sends is read off the nodes in here -- `session.sessionId`,
 * `window.id`, `pane.paneId` -- and never passed alongside them, so a menu
 * built for one row cannot address another.
 */
export type RowTarget =
  | { kind: 'session'; session: SessionNode }
  | { kind: 'window'; session: SessionNode; window: WindowNode }
  | { kind: 'pane'; session: SessionNode; window: WindowNode; pane: PaneNode }

/**
 * What a window *row* in the sidebar is acting on.
 *
 * A window with one pane is that pane: the row already carries the pane's badge
 * and the pane's agent mark, and treating it as a window would leave a lone
 * pane with no kill of its own on any row it has. A split window is a window,
 * and its panes have rows of their own underneath.
 */
export function rowTargetForWindow(session: SessionNode, window: WindowNode): RowTarget {
  const lone = window.panes.length === 1 ? window.panes[0] : undefined
  return lone
    ? { kind: 'pane', session, window, pane: lone }
    : { kind: 'window', session, window }
}

/** What a menu entry does when it is chosen. */
export type MenuIntent =
  | { kind: 'run'; action: ManageAction }
  | { kind: 'prompt'; prompt: PromptSpec }
  | { kind: 'kill'; plan: KillPlan }

/** One row of a context menu, and one row of the palette's action group. */
export interface MenuEntry {
  /** Unique within a menu, and the React key. */
  id: string
  label: string
  /**
   * A second line, for the one action whose effect reaches past this browser.
   * Rendered, not just a tooltip: zoom is a window property, so it moves the
   * terminal on the host too, and someone finding that out afterwards is the
   * failure this line exists to prevent.
   */
  hint?: string
  /** Draw it red and separate it from everything above. */
  danger?: boolean
  intent: MenuIntent
  /** Everything the palette fuzzy-matches on. */
  search: string
}

/** The zoom copy, in one place, because two surfaces show it. */
export const ZOOM_HINT = 'Zooms the window for every client, including the terminal on the host'

/**
 * What the owner can do to the thing they right-clicked.
 *
 * Scoped to the row: a pane row offers pane verbs, a window row window verbs,
 * and both offer the session-level "new window" because that is where a new
 * window would go. The kill is last, marked `danger`, and it is the only entry
 * that opens a dialog before anything is sent.
 */
export function rowMenu(target: RowTarget, activeSession: string | null): MenuEntry[] {
  const { session } = target
  const entries: MenuEntry[] = []

  if (target.kind === 'session') {
    // An app-owned group is this app's own throwaway session, and the daemon
    // refuses to rename or kill one -- killing it would drop a live tab's
    // socket for no reason the owner could understand. Left out rather than
    // shown disabled: the row is already unreachable in the sidebar.
    if (!session.appOnly) {
      entries.push({
        id: 'rename-session',
        label: `Rename session "${session.name}"…`,
        intent: {
          kind: 'prompt',
          prompt: {
            title: `Rename session "${session.name}"`,
            description: 'tmux renames the session itself, so the new name shows up everywhere.',
            fields: [{ name: 'name', label: 'Name', initial: session.name, required: true }],
            submitLabel: 'Rename',
            build: (v) => ({ verb: 'rename-session', session: session.sessionId, name: v.name }),
          },
        },
        search: `rename session ${session.name} ${session.sessionId}`,
      })
    }
  }

  // Read off the node the row was built from rather than passed alongside it,
  // exactly as a session's id is: two spellings of "which window" is one more
  // than can ever disagree, and a rename dialog titled "api" must not send the
  // `@N` of the window beside it.
  if (target.kind === 'window' && target.window.id !== NO_WINDOW_ID) {
    const windowId = target.window.id
    entries.push({
      id: 'rename-window',
      label: `Rename window "${target.window.name}"…`,
      intent: {
        kind: 'prompt',
        prompt: {
          title: `Rename window "${target.window.name}"`,
          description: 'tmux renames the window itself, so the new name shows up everywhere.',
          fields: [{ name: 'name', label: 'Name', initial: target.window.name, required: true }],
          submitLabel: 'Rename',
          build: (v) => ({ verb: 'rename-window', window: windowId, name: v.name }),
        },
      },
      search: `rename window ${target.window.name} ${windowId}`,
    })
  }

  if (target.kind === 'pane') {
    const pane = target.pane
    entries.push({
      id: 'label-pane',
      label: pane.label === '' ? `Name pane ${pane.paneIndex}…` : `Rename pane ${pane.paneIndex}…`,
      intent: {
        kind: 'prompt',
        prompt: {
          title: `Name pane ${pane.paneId}`,
          // The one rename that is this app's rather than tmux's, and the
          // reason is worth a sentence: a pane's tmux title is rewritten
          // constantly by a shell prompt and by Claude Code, so a name kept
          // there would not survive the next redraw.
          description:
            'Kept as a tmux option on the pane, so a shell prompt cannot overwrite it. An empty name clears it.',
          fields: [
            { name: 'label', label: 'Name', initial: pane.label, placeholder: pane.command },
          ],
          submitLabel: 'Save',
          build: (v) => ({ verb: 'label-pane', pane: pane.paneId, label: v.label }),
        },
      },
      search: `rename label pane ${pane.paneIndex} ${pane.paneId} ${pane.command}`,
    })
  }

  // New window goes in the session the row belongs to, and opens in the
  // working directory of the pane in hand -- the daemon resolves that itself
  // from the pane id, so no path crosses the wire.
  if (!session.appOnly) {
    entries.push({
      id: 'new-window',
      label: 'New window',
      intent: { kind: 'run', action: newWindowAction(target) },
      search: `new window ${session.name}`,
    })
  }

  const pane = splitTarget(target)
  if (pane) {
    entries.push(
      {
        id: 'split-right',
        label: 'Split right',
        intent: { kind: 'run', action: { verb: 'split', pane, direction: 'right' } },
        search: `split right vertical ${pane}`,
      },
      {
        id: 'split-down',
        label: 'Split down',
        intent: { kind: 'run', action: { verb: 'split', pane, direction: 'down' } },
        search: `split down horizontal below ${pane}`,
      },
      {
        id: 'zoom',
        label: 'Zoom window',
        hint: ZOOM_HINT,
        intent: { kind: 'run', action: { verb: 'zoom', pane } },
        search: `zoom fullscreen ${pane}`,
      },
    )
  }

  const plan = planKill(target, activeSession)
  if (plan) {
    entries.push({
      id: 'kill',
      label: `${plan.confirmLabel}…`,
      danger: true,
      intent: { kind: 'kill', plan },
      search: `kill ${plan.title}`,
    })
  }

  return entries
}

/**
 * The pane a split or a zoom acts on.
 *
 * A window row splits the pane that window would show -- tmux's active pane for
 * it, else the first in layout order -- which is the same pane clicking the row
 * navigates to. A session row has no pane in hand and so offers neither.
 */
function splitTarget(target: RowTarget): string | null {
  if (target.kind === 'pane') return target.pane.paneId
  if (target.kind === 'window') {
    const pane = target.window.panes.find((p) => p.active) ?? target.window.panes[0]
    return pane?.paneId ?? null
  }
  return null
}

/**
 * Create a window in the session a row belongs to.
 *
 * Exported because the session row's `+` is the same action as the menu entry,
 * and two spellings of it would be two things to keep in step. The name is
 * empty -- tmux applies its automatic one -- and `fromPane` is the pane in
 * hand, so the window opens where the owner was working. Only an id is sent:
 * the daemon resolves `#{pane_current_path}` itself, which is what keeps
 * working directories off the wire entirely.
 */
export function newWindowAction(target: RowTarget): ManageAction {
  return {
    verb: 'new-window',
    session: target.session.sessionId,
    name: '',
    fromPane: splitTarget(target) ?? '',
  }
}

// --- the two-step kill -------------------------------------------------------

/** Everything the kill dialog needs, decided before it opens. */
export interface KillPlan {
  /** Unique per target: the dialog is remounted when it changes. */
  id: string
  action: ManageAction
  /** `kill window "api"`. */
  title: string
  /** `3 panes, one running claude`. */
  detail: string
  /**
   * What else goes with it, most consequential last. Empty for a kill that
   * takes only what it names.
   */
  warnings: string[]
  /** `Kill window`, on the red button and on the menu entry. */
  confirmLabel: string
}

/**
 * What a kill would destroy, spelled out.
 *
 * The count and the agents come from the snapshot rather than from a fresh
 * read, so they are up to 1.5s old -- which is why the dialog says what it
 * *believes* dies and the daemon is what decides. An id that went stale in
 * between kills nothing and reports it.
 *
 * Returns null when there is nothing addressable to kill: a window with no id
 * on the wire, or a group that is only this app's own sessions, which the
 * daemon refuses to touch.
 */
export function planKill(target: RowTarget, activeSession: string | null): KillPlan | null {
  const { session } = target

  if (target.kind === 'session') {
    if (session.appOnly) return null
    const panes = session.windows.flatMap((w) => w.panes)
    return {
      id: `kill-session:${session.sessionId}`,
      action: { verb: 'kill-session', session: session.sessionId },
      title: `kill session "${session.name}"`,
      detail: join([
        count(session.windows.length, 'window'),
        count(panes.length, 'pane'),
        agentPhrase(panes),
      ]),
      warnings: killWarnings(target, activeSession),
      confirmLabel: 'Kill session',
    }
  }

  if (target.kind === 'window') {
    // Nothing to send: see NO_WINDOW_ID. `rowMenu` drops the entry on the same
    // answer, so the dialog is never reached with one -- and this is the check
    // that makes that true rather than a comment saying it is.
    if (target.window.id === NO_WINDOW_ID) return null
    return {
      id: `kill-window:${target.window.id}`,
      action: { verb: 'kill-window', window: target.window.id },
      title: `kill window "${target.window.name}"`,
      detail: join([count(target.window.panes.length, 'pane'), agentPhrase(target.window.panes)]),
      warnings: killWarnings(target, activeSession),
      confirmLabel: 'Kill window',
    }
  }

  return {
    id: `kill-pane:${target.pane.paneId}`,
    action: { verb: 'kill-pane', pane: target.pane.paneId },
    title: `kill pane ${target.pane.paneIndex} of "${target.window.name}"`,
    detail: join([`${target.pane.paneId}`, `running ${target.pane.command}`]),
    warnings: killWarnings(target, activeSession),
    confirmLabel: 'Kill pane',
  }
}

/**
 * The consequences a kill has beyond the thing it names.
 *
 * The cascade is the one the design verified on a live server: a window loses
 * its last pane and closes, a session loses its last window and is destroyed --
 * **and a session group is destroyed with it, this app's own member included**,
 * which drops the socket of every tab attached to that group. The daemon's
 * refusal to kill an app session guards only the direct path; the sanctioned
 * window and pane paths reach the same end, so the dialog is where it has to be
 * said.
 *
 * Killing the *session* is the one case that does not disconnect this tab, and
 * the difference is worth getting right rather than warning uniformly. This tab
 * is attached to a throwaway session grouped with that one, so the windows
 * survive the kill exactly as they do when the user's session dies on the host
 * -- the state the sidebar already labels "(orphaned)". What is lost is the
 * ability to attach to that name again.
 */
export function killWarnings(target: RowTarget, activeSession: string | null): string[] {
  const { session } = target
  const attached = session.key === activeSession
  const warnings: string[] = []

  if (target.kind === 'session') {
    warnings.push(
      attached
        ? 'This tab is attached to this session. Its windows stay open while this tab holds them, but nothing can attach to it by name again.'
        : 'Its windows close with it, unless another session in the group is holding them open.',
    )
    return warnings
  }

  const lastPane = target.kind === 'pane' && target.window.panes.length === 1
  const lastWindow = session.windows.length === 1

  if (lastPane) {
    warnings.push(`This is the last pane in "${target.window.name}", so the window closes too.`)
  }
  if (lastWindow && (lastPane || target.kind === 'window')) {
    warnings.push(`It is the last window in "${session.name}", so the session closes with it.`)
    warnings.push(
      attached
        ? 'That closes the session this tab is attached to, so this tab disconnects.'
        : 'Any tab attached to that session disconnects.',
    )
  }
  return warnings
}

/** `3 panes`, `1 pane`. */
function count(n: number, noun: string): string {
  return `${n} ${noun}${n === 1 ? '' : 's'}`
}

/** Drop the empty parts and comma-join what is left. */
function join(parts: readonly string[]): string {
  return parts.filter((p) => p !== '').join(', ')
}

/**
 * `one running claude`, or "" when none of these panes is an agent.
 *
 * Whether a pane is an agent is the daemon's answer, never this file's: a pane
 * has a state exactly when the daemon recognised it as an agent and captured
 * it. Matching `command` against a list here would be a second copy of the Go
 * `Agents` list, and the copy is what eventually disagrees.
 */
export function agentPhrase(panes: readonly PaneNode[]): string {
  const agents = panes.filter((p) => p.agentState !== '')
  if (agents.length === 0) return ''
  const names = [...new Set(agents.map((p) => p.command))]
  const which =
    names.length === 1 ? names[0] : `${names.slice(0, -1).join(', ')} and ${names.at(-1)}`
  return agents.length === 1 ? `one running ${which}` : `${agents.length} running ${which}`
}

// --- asking for a name -------------------------------------------------------

/** One text field in a prompt dialog. */
export interface PromptField {
  /** Key in the values object handed to `build`. */
  name: string
  label: string
  initial?: string
  placeholder?: string
  /** Empty is not an answer: the submit button stays disabled. */
  required?: boolean
  description?: string
}

/** A dialog that collects text and turns it into one action. */
export interface PromptSpec {
  title: string
  description?: string
  fields: PromptField[]
  submitLabel: string
  build: (values: Record<string, string>) => ManageAction
}

/** The starting values for a prompt: what each field says it opens with. */
export function promptValues(spec: PromptSpec): Record<string, string> {
  const values: Record<string, string> = {}
  for (const field of spec.fields) values[field.name] = field.initial ?? ''
  return values
}

/**
 * Whether the prompt can be submitted.
 *
 * The only rule this side keeps is "a required field is not blank", which
 * spares a round trip that could only fail. Everything else tmux and the
 * daemon decide -- a leading `-`, a `:`, a control byte, the length cap -- and
 * a second copy of those rules here is a copy that drifts from the one that is
 * enforced.
 */
export function promptReady(spec: PromptSpec, values: Record<string, string>): boolean {
  return spec.fields.every((f) => !f.required || (values[f.name] ?? '').trim() !== '')
}

/** Create a session: the one dialog reachable when there is no tmux at all. */
export function newSessionPrompt(): PromptSpec {
  return {
    title: 'New tmux session',
    description: 'Starts a detached session on the host. This tab can then attach to it.',
    fields: [
      { name: 'name', label: 'Name', placeholder: 'work', required: true },
      {
        name: 'path',
        label: 'Directory',
        placeholder: '$HOME',
        description: 'Optional. The daemon checks it exists rather than letting tmux land in $HOME.',
      },
    ],
    submitLabel: 'Create',
    build: (v) => ({ verb: 'new-session', name: v.name.trim(), path: v.path.trim() }),
  }
}
