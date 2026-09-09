# tmux-web — design

Date: 2026-09-09
Status: agreed, not yet implemented
Revision: 2 — corrections from adversarial review, verified against tmux 3.7b

## Purpose

A single-user web app that fronts the tmux server on a development box, so a
remote laptop can drive several coding agents at once. Later it also publishes
local ports as subdomains and shows git context for the pane in view.

The box is a powerful server holding all of the user's dev environments. The app
is the remote door into it.

## Scope

v1 ships the load-bearing part only: reliable remote shell over TLS, with a
sidebar that navigates tmux.

| In v1 | Deferred |
| --- | --- |
| Wildcard TLS, single-user auth | Publishing local ports as subdomains |
| Sidebar: sessions → windows → panes | Git context panel |
| Terminal attached to a per-tab tmux client | Git manipulation from the UI |
| Scrollback via tmux copy-mode | Several windows on screen at once |
| Reconnect after network drop | |

Publishing and the git panel are the reason several v1 decisions look
over-built (wildcard cert, routing table, `pane_current_path` in the snapshot).
They are hooks, not implementations.

Note on the deferred item: `tmux attach` already renders every pane and split in
a window. What is deferred is showing several *windows or sessions* side by side
in the browser, which the per-tab client model cannot do.

## Decisions

1. **Client per browser tab.** tmux renders; the web app navigates.
2. **The app owns the front door.** One process on :443, ACME wildcard, auth,
   UI, and later the published-port proxy.
3. **Go backend, TypeScript frontend.** One static binary with the UI embedded.
4. **Laptop first, phone as a bonus.** No design effort spent on soft keyboards.
5. **Enrollment links, no passwords.**
6. **shadcn/ui** on Tailwind v4.
7. **Exact-Origin checks are the CSRF boundary**, not `SameSite`.

## Architecture

One Go binary. Three concerns: front door, tmux control, asset serving.

```
wterm-web (single static binary)
├─ certmagic           wildcard *.example.com via ACME DNS-01
├─ net/http            UI, JSON API, WebSocket
├─ net/http/httputil   reverse proxy for published ports   (deferred)
├─ creack/pty          per-tab tmux attach clients
├─ os/exec             tmux control commands
├─ unix socket         local admin, SO_PEERCRED authenticated
└─ go:embed web/dist   React + shadcn + @wterm/react
```

**Deployment constraint.** The daemon must run as the same uid that owns the
tmux server, since tmux's socket lives in that user's `$XDG_RUNTIME_DIR`. A
dedicated service user cannot see the developer's tmux. `SO_PEERCRED` proves the
*caller's* uid; it does not create access to another user's server.

### Attach model

Opening a session in a browser tab creates a throwaway tmux session grouped onto
the real one. **Order matters and is not obvious:**

```sh
# 1. create, grouped onto the real session
tmux new-session -d -t work -s _web-<uuid>
tmux set -t _web-<uuid> status off
tmux set -t _web-<uuid> mouse on

# 2. attach via creack/pty  — TERM=xterm-256color
#    tmux attach -t _web-<uuid>

# 3. ONLY NOW, once a client is attached
tmux set -t _web-<uuid> destroy-unattached on
```

`destroy-unattached on` is not an on-detach hook. It is a "zero clients ⇒
destroy" invariant that fires the instant it becomes true, and a freshly created
`-d` session has zero clients. Setting it before attaching destroys the session
about 8ms later, reliably, before the attach lands. Verified on 3.7b: with the
corrected order the session survives while attached and is reaped on detach.

Belt and braces: also `kill-session` explicitly when the WebSocket closes.
`destroy-unattached` is the safety net for crashes, not the primary path.

Grouped sessions share a window list but keep an independent current window
(verified). So two browser tabs can watch two different agents.

`aggressive-resize` is deliberately **not** set. It is a window option defined in
terms of `largest`/`smallest` sizing and is a no-op under `window-size latest`.

`creack/pty` spawns `tmux attach -t _web-<uuid>`. That PTY's bytes are the
WebSocket payload, fed straight into `@wterm/react`.

### Window sizing: a real limitation, not a solved problem

An earlier draft claimed a locally attached terminal is unaffected by the browser
client. That is true of *current window* but false of *size*. Window dimensions
are a property of the window, shared across every client viewing it, and
`window-size latest` gives them to whichever client was most recently active.

So when the local terminal and a browser tab view the **same** window, the last
one to type sets the size for both, and the local terminal gets yanked to the
laptop's dimensions. Debouncing resize to ~150ms reduces the frequency of this;
it does not prevent it. There is no per-client sizing in the grouped-session
model.

This is accepted for v1. It only bites when co-viewing one window, which is not
the normal remote workflow. It must not be described as solved.

### Scrollback, copy-mode, and the mouse

Supervising agents means reading back through output, so this is a v1 concern.
wterm renders live PTY bytes only; history lives in tmux's copy-mode.

`mouse on` is set **on the throwaway session only** — `mouse` is a session
option, verified — so the wheel enters copy-mode and scrolls in the browser
without changing the local session's behavior. wterm reports SGR mouse events,
which is what tmux needs.

The command palette also carries an explicit "enter copy mode" action, since
mouse reporting is exactly what a full-screen agent TUI may want to grab.

### Sidebar state

One command, polled every ~1.5s **globally** — a single poll fanned out to every
connected client, never one poll per tab.

```sh
tmux list-panes -a \
  -f '#{!=:#{m:_web-*,#{session_name}},1}' \
  -F '#{session_name}␟#{window_index}␟#{window_name}␟#{pane_id}␟#{pane_active}␟#{pane_current_command}␟#{pane_current_path}'
```

Two corrections over the first draft:

**The filter is mandatory.** Grouped sessions share a window list, so a bare
`list-panes -a` returns every pane once per session in the group — with two tabs
open, three copies of everything, plus the `_web-*` sessions themselves. Verified.
The filter restores exactly one row per pane.

**`␟` above stands for a literal 0x1f byte**, emitted directly in the `-F` argument. tmux does not
expand `\x1f` in a format string. `\t` is wrong regardless: window names and
paths may contain tabs.

The snapshot renders directly to the sidebar tree, so the client keeps no model
of tmux that could drift. `pane_current_path` is the hook the git panel will use.

Sidebar clicks issue `select-window` / `select-pane` against the tab's own
grouped session, so navigation never disturbs other clients.

Cost of polling: a forked tmux process every 1.5s, and up to 1.5s of lag before a
newly spawned agent appears. If that lag is annoying in practice, the upgrade is
a single read-only `tmux -C` control-mode sidecar, which also removes the fork
loop. Worth reconsidering early rather than treating as far-future.

### Reconnect

Reconnect creates a fresh throwaway session, then **restores position**:
the target `session:window.pane` is held client-side and re-selected after
attach. Without this, every network blip drops the user back to window 0 instead
of the agent they were watching.

tmux redraws on attach, so there is no output to replay and no server-side buffer
to size or leak. tmux is the persistence layer; the app holds no terminal state.

### WebSocket protocol

Binary frames carry PTY bytes. Splitting UTF-8 across frames is safe because
wterm consumes bytes, but text frames would force validation, so binary only.

Control messages (resize, select-pane, copy-mode) travel as a separate JSON
channel — either a second WebSocket or a one-byte frame-type prefix. Resize is
debounced ~150ms.

**Ping/pong keepalive is required**, not optional. On a half-open connection —
laptop lid closed, NAT timeout — the `tmux attach` process stays alive and
attached, so `destroy-unattached` never fires and reconnect spawns a second
throwaway session. Orphans accumulate on flaky links. Keepalive detects the dead
peer and tears down the attach.

## Auth

There is no OS identity to inspect on a remote TCP connection. There is one on a
local unix socket: `SO_PEERCRED` yields an unforgeable pid/uid/gid. The design
uses the socket as the trust anchor and enrollment links to project that local
trust onto a remote device. No password is ever created, stored, or typed.

### Admin socket

`$XDG_RUNTIME_DIR/wterm-web.sock`, mode 0600. Every connection is checked with
`unix.GetsockoptUcred`; a uid other than the service's own is refused. The CLI
speaks only to this socket, so "can you run this as me on this box" *is* the
authorization proof.

```
$ wterm-web enroll --name laptop
https://tmux.example.com/enroll#Ck9tR2p…    single use, expires in 10m
```

### Why the fragment

A token after `#` is never sent to the server. It stays out of access logs and
`Referer` headers, and — the practical reason — is not consumed by the link
scanners messaging apps run when you paste yourself a URL. The page reads
`location.hash` and POSTs it once.

### Device sessions

Redeeming mints a 32-byte device token. Only its hash is stored, with name,
user-agent, created-at, last-seen. The cookie is `HttpOnly; Secure;
SameSite=Lax`, scoped to the exact host `tmux.example.com`.

**Never `.example.com`.** A wildcard-domain cookie would be readable by whatever
service is later published on `test.example.com`, handing a shell to any app
under test.

No absolute expiry; revocation is the control. An **idle expiry** based on the
recorded `last-seen` is applied as defence in depth, since a stolen cookie is
otherwise permanent shell access and hashing at rest protects the store, not a
live credential.

### CSRF: exact-Origin is the boundary

`SameSite=Lax` provides **nothing** against the published-ports feature.
`SameSite` is computed on the registrable domain, not the host, so
`test.example.com` and `tmux.example.com` are the *same site*. A service under
test — untrusted code, by definition — can serve a page whose same-site POSTs to
the app carry the device cookie. CORS blocks reading the response; it does not
block executing the request.

Therefore **every state-changing request is checked against an exact-host Origin
allowlist**: the WebSocket handshake and every mutating JSON endpoint alike.
`Origin` is scheme + host + port, so `https://test.example.com` does not match
`https://tmux.example.com`. Requests with no `Origin` are rejected on mutating
routes.

`SameSite=Lax` is kept — it still helps against genuinely cross-site attackers —
but it is not load-bearing and must not be described as the defence.

### Recovery

Lost every device: SSH in, run `enroll` again. From any live session: list
devices, mint links, revoke — including revoking the device previously used.

### Other checks

- Constant-time token comparison; rate-limit redemption.
- Append-only audit log of enroll, redeem, revoke, and sign-out events.

## Frontend

Vite + React + TypeScript, shadcn/ui on Tailwind v4, built to `web/dist` and
embedded with `go:embed`.

```
SidebarProvider
├─ Sidebar
│   ├─ SidebarGroup per tmux session
│   │   └─ SidebarMenu: windows
│   │       └─ SidebarMenuSub: panes (only when >1)
│   │           Badge = pane_current_command   [claude] [vim] [npm]
│   └─ SidebarFooter → user menu
└─ SidebarInset
    ├─ header: breadcrumb  work › api › agent-3    ● connected
    └─ <Terminal/>   @wterm/react, ResizeObserver → debounced resize
```

shadcn's `Sidebar` collapses to icons on desktop and becomes a `Sheet` drawer on
mobile by itself. That satisfies the phone-as-a-bonus decision with no second
layout to maintain.

### Command palette

A focused terminal swallows nearly every key, and the natural candidates are
taken: `Ctrl+K` is readline kill-line, `Ctrl+B` is the tmux prefix, `Alt+*` is
Meta. The app-level chord is **`Ctrl+Alt+K`**, captured in the capture phase
ahead of wterm's handler, with a visible palette button in the header so the
chord is a shortcut rather than the only way in. The palette is shadcn `Command`,
fuzzy-matching `session/window/pane`, and also exposes "enter copy mode".

### Sidebar footer

```
├────────────────────────┤
│ ◒ diegok@devbox     ⌃  │
│   laptop · connected   │
└────────────────────────┘
```

The identity line is functional, not decoration: there is exactly one user and it
is the uid the daemon runs as, so the footer shows `os/user.Current()` and the
hostname — which box am I driving. The second line is the enrolled device name
and connection state.

Menu: **Devices…**, **Appearance**, **Sign out**. Later: Published ports, Git
panel.

- The new-link dialog shows a **QR code** beside the copyable URL. The realistic
  path is enrolling a phone, and a 43-character token is miserable to type.
- **Sign out revokes this device**, not just the cookie. A cookie-only logout
  leaves a live, fully-privileged credential in the store that no UI can still
  identify.

### Theme and TERM

wterm themes are CSS custom properties and so is shadcn. One palette feeds both,
so the terminal does not look like an iframe dropped into an app.

`TERM` on the attach PTY is pinned to match what wterm actually emulates, and the
terminfo entry must exist on the box. A mismatch produces broken colors, keys,
and mouse reporting inside agents — cheap to get wrong, tedious to debug.

### Connection state

`sonner` toasts on drop and reconnect, plus the header dot. Reconnect is
automatic with backoff.

## Failure modes

| Case | Handling |
| --- | --- |
| No tmux server running | First load offers "create session `main`" |
| `destroy-unattached` set too early | Set it only after attach; see attach model |
| Orphaned `_web-*` after `SIGKILL` | Sweep at startup: kill `_web-*` with zero clients |
| Orphaned `_web-*` from half-open TCP | WS ping/pong tears down the attach |
| Resize contention on a co-viewed window | Debounced; accepted limitation |
| Slow or stuck client | Bounded buffer; on overflow **close the WebSocket** |
| Corrupted terminal state | Never drop bytes mid-stream on a live socket |
| Multibyte splitting | Binary WebSocket frames, not text |
| Cert renewal failure | Serve the existing cert, log loudly, surface in the UI |

**On backpressure.** The earlier draft said "stop reading the PTY." That is
wrong: the PTY is a tmux client sharing the server with the user's local session,
so stalling it can wedge the local terminal. A stuck browser client is dropped
instead — the socket closes, the client reconnects, and tmux redraws. Never drop
bytes to a live client; never stall tmux for a dead one.

## Storage

One JSON file under `$XDG_STATE_HOME/wterm-web/`, written by atomic rename, with
all writes serialized through a single goroutine. Atomic rename prevents torn
files; it does not prevent lost updates from concurrent read-modify-write.
Devices and the audit log now, published routes later. No SQLite: nothing here
has a query.

## Testing

**Integration against real tmux** is the high-value layer. Use an isolated
socket (`tmux -L wterm-test`): assert the corrected attach ordering survives,
assert the snapshot filter returns exactly one row per pane with a group member
present, assert independent current-window, assert reaping on detach. Fast and
hermetic, with no mock of a protocol we do not control.

**Unit tests** for enrollment (expiry, single use, replay, wrong token), the
`SO_PEERCRED` uid check, exact-Origin rejection on every mutating route, and
cookie scoping.

**End to end** with Playwright driving the real binary: enroll, attach, type
`echo hi`, assert the row. Because wterm renders to the DOM rather than a canvas,
this is an ordinary DOM assertion — a canvas-based terminal would not allow it.

## Repository layout

```
cmd/wterm-web/        serve, enroll, devices
internal/tmux/        snapshot, session lifecycle, command builders
internal/ptybridge/   attach + WebSocket bridge
internal/auth/        peercred, enrollment, device store, origin checks
internal/front/       http mux, embedded assets
web/                  vite + react + shadcn
docs/plans/
```

## Deferred, with the hooks already in place

### Publishing local ports

The front door already terminates a wildcard cert, so a new subdomain needs no
certificate work — it starts routing. A routes table maps `test` →
`127.0.0.1:4444` with a per-route public/gated flag, served by
`httputil.ReverseProxy`.

### Gated routes: redirect token exchange

A gated route cannot reuse the device cookie, because that cookie is scoped to
`tmux.example.com` and is never sent to a sibling host. Widening it to
`.example.com` is exactly what the design forbids. So gating is a redirect
exchange:

```
GET https://test.example.com/          no gate cookie
  → 302 https://tmux.example.com/authorize?rd=…      device cookie present
  → 302 https://test.example.com/__gate?t=<short-lived, route-scoped>
  → sets a gate cookie scoped to test.example.com, short TTL
  → 302 to the original path
```

Three properties this must hold:

- The gate cookie authorizes **one route** and grants no access to the app.
- The exchange token is single-use, short-lived, and bound to that route.
- The proxy **strips the gate cookie before forwarding upstream**, so the
  service under test never sees it.

This keeps the strict host scoping of the device cookie intact while still
letting a published service be private.

### Group lifecycle edges to document

- A session group outlives its namesake: kill `work` locally and the `_web-*`
  members keep the group and its windows alive. Killing the "real" session does
  not release it while a browser tab is open.
- If every member dies, a later `new-session -t work` creates a fresh empty
  group rather than restoring the old windows.

### Git context

`pane_current_path` is already in every snapshot. Walk to the repo root and show
branch, ahead/behind, dirty files, and stashes. Read-only first; manipulation
only if using the shell for it proves annoying.

### ACME blast radius

DNS-01 wildcard means zone-editing API credentials live on the box, and a single
private key covers the shell and every published service. Out of v1 scope, but
the concentration of risk is deliberate and should stay visible.
