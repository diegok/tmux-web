# tmux-web — design

Date: 2026-09-09
Status: agreed, not yet implemented
Revision: 3 — corrections from two adversarial review passes, verified against tmux 3.7b

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
| Single-name TLS, single-user auth | Publishing local ports as subdomains |
| Sidebar: sessions → windows → panes | Git context panel |
| Terminal attached to a per-tab tmux client | Git manipulation from the UI |
| Scrollback via tmux copy-mode | Several windows on screen at once |
| | Wildcard cert, audit log, idle expiry |
| Reconnect after network drop | |

Publishing and the git panel are the reason several v1 decisions look
over-built (the routing table, the per-pane path query). They are
hooks, not implementations. The wildcard certificate deliberately is *not* one:
see the TLS decision below.

Note on the deferred item: `tmux attach` already renders every pane and split in
a window. What is deferred is showing several *windows or sessions* side by side
in the browser, which the per-tab client model cannot do.

## Decisions

1. **Client per browser tab.** tmux renders; the web app navigates.
2. **The app owns the front door.** One process on :443, ACME, auth, UI, and
   later the published-port proxy.
3. **v1 issues a single-name certificate, not a wildcard.** v1 serves exactly one
   hostname. A DNS-01 wildcard would put zone-editing API credentials on the box
   from day one to serve a deferred feature. certmagic makes adding the wildcard
   cheap later, so this is not a hook worth paying for now. Use HTTP-01 while
   :80 is reachable; when publishing lands and DNS-01 becomes necessary, delegate
   via CNAME (acme-dns style) so the credential cannot edit the real zone.
4. **Go backend, TypeScript frontend.** One static binary with the UI embedded.
5. **Laptop first, phone as a bonus.** No design effort spent on soft keyboards.
6. **Enrollment links, no passwords.**
7. **shadcn/ui** on Tailwind v4.
8. **Exact-Origin checks are the CSRF boundary**, not `SameSite`.

## Architecture

One Go binary. Three concerns: front door, tmux control, asset serving.

```
tmux-web (single static binary)
├─ certmagic           tmux.example.com via ACME HTTP-01  (wildcard later)
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
the real one. This is a single command, run under a PTY by `creack/pty` with
`TERM=xterm-256color`:

```sh
tmux new-session -t work -s _web-<uuid> \; \
     set destroy-unattached on \; \
     set status off \; \
     set mouse on \; \
     set @tmux_web_owned 1
```

**Why one command and not four.** `destroy-unattached on` is not an on-detach
hook. It is a "zero clients ⇒ destroy" invariant that fires the instant it
becomes true, and a session created with `-d` has zero clients — setting the
option first destroys the session about 8ms later, reliably, before any attach
can land. But deferring the option to a second call after spawning the attach
only narrows the race rather than removing it, and there is no non-guessy way to
observe "the client is up" from outside.

`new-session` without `-d` creates *and* attaches in one client, and the chained
`set` commands run afterwards in that client's context. Verified on 3.7b: the
session comes up `attached=1` with all options applied, and is reaped on client
exit. No race, no polling `#{session_attached}`, no sleep.

Belt and braces: also `kill-session` explicitly when the WebSocket closes.
`destroy-unattached` is the safety net for crashes, not the primary path.

`@tmux_web_owned` is a tmux user option tagging the session as app-created. The
sidebar filter and the startup sweep both match on it rather than on the name —
a user with a real session called `_web-notes` would otherwise have it hidden
from the sidebar and killed by the sweep.

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
one to *act* sets the size for both, and the local terminal gets yanked to the
laptop's dimensions. Debouncing resize to ~150ms reduces the frequency of this;
it does not prevent it. There is no per-client sizing in the grouped-session
model.

This is accepted for v1. It only bites when co-viewing one window, which is not
the normal remote workflow. It must not be described as solved.

#### Measured, against tmux 3.7b, two PTY clients on one grouped session

- **A resize counts as acting.** "Most recently active" is not keyboard
  activity: a bare `SIGWINCH` makes a client the latest one, and the shared
  window follows it in *both* directions. A client sitting at 80x24 beside a
  120x40 one took the window to 100x29 by being resized and nothing else, and
  `aggressive-resize on` changed none of it -- as this document already says,
  that option is defined in terms of `largest`/`smallest` and does nothing under
  `latest`. So "the browser grows on full-screen and never shrinks back" is not
  this limitation. It was ours; see `e2e/sizing.spec.ts`.
- **The smaller client is clipped, not overflowed.** With the window at 160
  columns and the client at 80, every row tmux wrote to that client was exactly
  80 columns wide. Nothing wraps and nothing is corrupted -- the right-hand part
  of the window is simply not sent, which is why a full-width redraw (vim
  opening) looks truncated rather than scrambled.
- **`window-size` cannot be scoped to our own session.** It is a window option,
  and grouped sessions share the window *object*: `set-option -t <our throwaway
  session> -w window-size smallest` was immediately readable as `smallest` from
  the user's own session on the same window. There is no value the daemon can
  set that does not change the user's tmux, and it would have to be re-set on
  every new window besides. This stays a README suggestion -- `set -wg
  window-size smallest` in the user's own config -- and never a thing the app
  does.

### Scrollback, copy-mode, and the mouse

Supervising agents means reading back through output, so this is a v1 concern.
wterm renders live PTY bytes only; history lives in tmux's copy-mode.

`mouse on` is set **on the throwaway session only** — `mouse` is a session
option, verified — so wheel events reach tmux from the browser without turning
on mouse reporting for the local client. wterm reports SGR mouse events, which
is what tmux needs.

**But copy-mode itself is shared, and this is a real limitation.** The mouse
*option* is per-session; copy-mode is a property of the *pane*. Verified: with a
local client and a browser client on the same window, entering copy-mode from the
browser puts both screens into copy-mode and scrolling drags both through
history. The local terminal's live tail freezes while the browser reads back.

This is the same accepted-limitation class as window sizing, and for the same
reason — it only bites when co-viewing one window. Like sizing, it must not be
described as solved.

Three things now sit in this family: window size, copy-mode, and the active
pane. All three are properties of the *window*, which grouped sessions share by
design; only the current window itself is per-session. That is the real
boundary of what this model isolates, and it is worth stating once plainly
rather than rediscovering it one feature at a time.

A header button and a palette action both offer "enter copy mode" explicitly,
since a full-screen agent TUI may itself want mouse reporting.

### Sidebar state

One command, polled every ~1.5s **globally** — a single poll fanned out to every
connected client, never one poll per tab.

```sh
tmux list-panes -a -F '#{?#{session_group},#{session_group},#{session_name}}␟#{pane_id}␟#{pane_index}␟#{@tmux_web_owned}␟#{window_index}␟#{window_name}␟#{pane_active}␟#{pane_current_command}'
```

The rows are then **deduplicated in Go by `pane_id`, preferring a
row that came from a non-app session.** That dedupe, not a tmux filter, is what
makes the snapshot correct. Four things to understand about why:

**Raw `list-panes -a` duplicates everything.** Grouped sessions share a window
list, so every pane is reported once per session in the group — with two tabs
open, three copies of each pane, plus the app's own sessions. Verified.

**A tmux `-f` filter cannot express this.** Filtering out app-created sessions
looks like it works until the user kills the namesake session while a tab is
open. The group survives with only app-created members, so a filtered snapshot
comes back **empty while the agents are still running** and still visible in the
attached tab. Verified — including with the `session_group` fallback in place,
which fixes the label but not the dropped row. Dedupe keeps the pane and just
sources it from whichever session is left.

**`@tmux_web_owned` is the app marker, not the name.** A user with a real session
called `_web-notes` must not be hidden from the sidebar, and must not be killed
by the startup sweep. Both match the user option; verified that `_web-notes`
survives both.

**`␟` above stands for a literal 0x1f byte**, emitted directly in the `-F`
argument; tmux does not expand `\x1f` in a format string. `\t` is wrong
regardless, since window names and paths may contain tabs. Note that tmux
sanitizes session and window names but **not** `pane_current_path`, which can
contain a raw newline or a raw 0x1f — verified, and `#{q:}` escapes neither.

**The snapshot therefore does not carry the path at all.** Making it the last
field does not contain the damage: a newline in it simply starts a fresh line
whose eight fields are all pane-controlled, so a pane can forge a second pane
into the sidebar — verified, a directory name yielded a fabricated `PWNED`
row. A 0x1f in it is worse: the following pane's record is swallowed into the
path field and that live pane disappears from the sidebar — verified. Since no
v1 surface renders the path, dropping the field removes both. A malformed line
is now skipped and counted, never merged into its neighbour.

`#{pane_index}` replaces it. Panes must be ordered by index, not by `pane_id`:
after a split-and-kill cycle the ids run `%0 %4 %2 %1` while the layout the
user sees runs `0 1 2 3` — verified.

The snapshot renders directly to the sidebar tree, so the client keeps no model
of tmux that could drift.

Sidebar clicks issue `select-window` / `select-pane` against the tab's own
grouped session. **Only half of that is isolated**, and the plan's original
target form was wrong on top of it.

The current *window* is per-session, so switching windows really does leave
other clients alone. The active *pane* is a property of the window, which
grouped sessions share, so `select-pane` is visible to every member of the
group including the user's own attached session. Verified: a tab selecting
`%1` moves the base session's active pane too. tmux offers no way to scope it,
and it is the same behavior two attached clients already have, but the UI
should not promise otherwise.

The target form also has to be exact. `select-window -t <session>:<paneID>`
fails outright (`can't find window: %3`) -- a window target takes a name, index
or `@id`, never a pane id. A bare `select-window -t %3` succeeds but picks the
session itself, and with grouped sessions that choice is arbitrary: in testing
it moved a different tab's session. The correct sequence resolves the pane's
window first:

```sh
tmux list-panes -t %3 -F '#{window_id}'      # -> @7
tmux select-window -t '=<session>:@7'
tmux select-pane -t %3
```

`=` matters because tmux falls back to prefix matching and resolves an empty
target to whatever is current, both exiting 0.

Cost of polling: a forked tmux process every 1.5s, and up to 1.5s of lag before a
newly spawned agent appears. If that lag is annoying in practice, the upgrade is
a single read-only `tmux -C` control-mode sidecar, which also removes the fork
loop. Worth reconsidering early rather than treating as far-future.

### Reconnect

Reconnect creates a fresh throwaway session, then **restores position**: the
target `session:window.pane` is held in the tab's `sessionStorage` and
re-selected after attach. Without this, every network blip drops the user back to
window 0 instead of the agent they were watching. If the remembered pane died in
the meantime, fall back to the group's active window rather than erroring.

tmux redraws on attach, so there is no output to replay and no server-side buffer
to size or leak. tmux is the persistence layer; the app holds no terminal state.

### WebSocket protocol

Binary frames carry PTY bytes. Splitting UTF-8 across frames is safe because
wterm consumes bytes, but text frames would force validation, so binary only.

Control messages (resize, select-pane, copy-mode) share the same socket behind a
one-byte frame-type prefix. Not a second WebSocket: that would double the auth,
reconnect, and keepalive surface for nothing. Resize is debounced ~150ms.

**Open question to resolve before implementing:** `@wterm/core` ships its own
WebSocket transport with binary framing and reconnection. If that protocol is
fixed, the backend must speak it and this framing choice is already made. Check
the package before designing a control channel wterm will fight.

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

`$XDG_RUNTIME_DIR/tmux-web.sock`, mode 0600. Every connection is checked with
`unix.GetsockoptUcred`; a uid other than the service's own is refused. The CLI
speaks only to this socket, so "can you run this as me on this box" *is* the
authorization proof.

```
$ tmux-web enroll --name laptop
https://tmux.example.com/enroll#Ck9tR2p…    single use, expires in 10m
```

### Why the fragment

A token after `#` is never sent to the server. It stays out of access logs and
`Referer` headers, and — the practical reason — is not consumed by the link
scanners messaging apps run when you paste yourself a URL. The page reads
`location.hash` and POSTs it once.

### Device sessions

Redeeming mints a 32-byte device token. Only its hash is stored, with name,
user-agent, created-at, last-seen. The cookie is named `__Host-tmux_web_device` and
set `HttpOnly; Secure; Path=/`, with no `Domain` — so it is scoped to the exact
host `tmux.example.com`.

**Never `.example.com`.** A wildcard-domain cookie would be readable by whatever
service is later published on `test.example.com`, handing a shell to any app
under test.

The `__Host-` prefix does a second job the plain host-only cookie cannot: it
stops a sibling subdomain from *setting* a `Domain=.example.com` cookie of the
same name that shadows the real one. Browsers reject a `__Host-` cookie carrying
a `Domain` attribute, so the shadowing attack is closed at the browser rather
than guarded against in parsing code.

No expiry; revocation is the control. **Revocation must sever live connections** —
closing that device's WebSockets and killing its attach PTYs — not merely
invalidate the token for future requests. A revoked device with an established
socket otherwise keeps its shell until it happens to disconnect.

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
ahead of wterm's handler. The header button is the primary affordance and the
chord is a convenience, which matters because `Ctrl+Alt+K` is AltGr+K on several
European layouts — it must never be the only way in. The palette is shadcn `Command`,
fuzzy-matching `session/window/pane`, and also exposes "enter copy mode".

### Sidebar footer

```
├────────────────────────┤
│ ◒ dev@devbox        ⌃  │
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

`TERM` on the attach PTY is pinned to `xterm-256color`, whose terminfo entry is
present on the box. A mismatch produces broken colors, keys, and mouse reporting
inside agents — cheap to get wrong, tedious to debug. tmux itself will advertise
`tmux-256color` to programs running inside it, which is expected.

### Connection state

`sonner` toasts on drop and reconnect, plus the header dot. Reconnect is
automatic with backoff.

## Failure modes

| Case | Handling |
| --- | --- |
| No tmux server running | First load offers "create session `main`" |
| `destroy-unattached` set too early | Set it only after attach; see attach model |
| Orphaned sessions after `SIGKILL` | Sweep at startup: kill `@tmux_web_owned` sessions with zero clients |
| Namesake session killed under a tab | Snapshot keys on `session_group`; windows stay addressable |
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

One JSON file under `$XDG_STATE_HOME/tmux-web/`, written by atomic rename, with
all writes serialized through a single goroutine. Atomic rename prevents torn
files; it does not prevent lost updates from concurrent read-modify-write.
Devices now, published routes later. No SQLite: nothing here has a query.

## Testing

**Integration against real tmux** is the high-value layer. Use an isolated
socket (`tmux -L tmux-web-test`): assert the one-shot create-and-attach comes up
attached with every option set, assert the snapshot returns exactly one row per
pane with group members present, assert a session named `_web-notes` survives
both the dedupe and the sweep, assert the snapshot stays populated after the
namesake session is killed, assert independent current-window, assert reaping on
detach. Fast and
hermetic, with no mock of a protocol we do not control.

**Unit tests** for enrollment (expiry, single use, replay, wrong token), the
`SO_PEERCRED` uid check, exact-Origin rejection on every mutating route, and
cookie scoping.

**End to end** with Playwright driving the real binary: enroll, attach, type
`echo hi`, assert the row. Because wterm renders to the DOM rather than a canvas,
this is an ordinary DOM assertion — a canvas-based terminal would not allow it.

## Repository layout

```
cmd/tmux-web/        serve, enroll, devices
internal/tmux/        snapshot, session lifecycle, command builders
internal/ptybridge/   attach + WebSocket bridge
internal/auth/        peercred, enrollment, device store, origin checks
internal/front/       http mux, embedded assets
web/                  vite + react + shadcn
docs/plans/
```

## Deferred, with the hooks already in place

### Publishing local ports

This is where the wildcard certificate earns its cost: with `*.example.com`
issued, a new subdomain needs no certificate work and simply starts routing. A
routes table maps `test` → `127.0.0.1:4444` with a per-route public/gated flag,
served by `httputil.ReverseProxy`.

**Server-side threat model, stated plainly.** The daemon runs as the developer's
uid and published services run on loopback on the same box. If those services run
as the *same* uid, they need no cookie to compromise anything: they can open the
tmux socket directly, and they pass the `SO_PEERCRED` check on the admin socket.
All the browser-side cookie hygiene below is therefore protection against
*browser* threats — third-party visitors to public routes, and XSS in a service
under test — not against a hostile process. Services that are genuinely untrusted
must run in a container or under another uid. This assumption should be revisited
when publishing is implemented.

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

Properties this must hold:

- The gate cookie authorizes **one route**, grants no access to the app, and
  carries the `__Host-` prefix like the device cookie.
- The exchange token is single-use, short-lived, and bound to that route.
- `rd` is validated against the registered route table — the host must be a
  known gated route, and the path is kept path-only. Without this, `rd` is an
  open redirect and the token rides it to an attacker-chosen host.
- `/__gate` on host X redeems **only** tokens minted for X.
- The proxy **strips the gate cookie before forwarding upstream**, so the
  service under test never sees it.
- `Referrer-Policy` is set on the gate hop, since the token travels in a URL.

Two residual weaknesses to resolve before implementing, both arising from
siblings being same-site:

**The exchange is non-interactive.** `/authorize` redirects with no user
interaction, so a page on any sibling subdomain can iframe it and silently mint a
gate cookie for any gated route into the victim's browser. That alone reads
nothing cross-origin, but combined with XSS or a state-changing GET on the gated
service it defeats the gate. Mitigation is a one-time interstitial per route, or
an explicit acceptance that gating is convenience rather than a security boundary.

**Exact-Origin does not extend to proxied routes.** Once the victim holds a gate
cookie, a sibling can fire same-site requests at the gated service that arrive
upstream authorized. Requiring exact-Origin on non-GET proxied requests closes
this, at the cost of breaking services that legitimately accept cross-origin
posts.

Gating is a real design problem, not a formality. It needs its own pass before
implementation.

### Group lifecycle edges to document

- A session group outlives its namesake: kill `work` locally and the `_web-*`
  members keep the group and its windows alive. Killing the "real" session does
  not release it while a browser tab is open.
- If every member dies, a later `new-session -t work` creates a fresh empty
  group rather than restoring the old windows.

### Git context

Query `pane_current_path` for the selected pane only; it is deliberately not in
the shared snapshot (see "Sidebar state"), and a single-pane result needs no
field splitting, so neither hazard applies. Walk to the repo root and show
branch, ahead/behind, dirty files, and stashes. Read-only first; manipulation
only if using the shell for it proves annoying.

This originally prescribed `display-message -p -t <pane>`, and implementation
found that unusable: given a target it cannot find, `display-message` prints an
empty expansion and exits 0, so a stale pane id silently becomes `""` rather
than an error. `internal/tmux` reads the path with a filtered `list-panes`
instead — see `panePath` in `internal/tmux/manage.go`, which also has to pin
the pane with `-f` because `list-panes -t %3` lists the whole window. Take the
command from there, not from here.

### ACME blast radius

v1 avoids this entirely by issuing a single-name certificate. When publishing
lands, a DNS-01 wildcard means zone-editing API credentials live on the box and a
single private key covers the shell and every published service. Delegating the
`_acme-challenge` record via CNAME to a dedicated zone keeps the credential from
being able to edit real records. The concentration of risk is deliberate and
should stay visible.
