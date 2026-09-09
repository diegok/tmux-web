# tmux-web — design

Date: 2026-09-09
Status: agreed, not yet implemented

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
| Reconnect after network drop | Multiple panes visible at once in the browser |

Publishing and the git panel are the reason several v1 decisions look
over-built (wildcard cert, routing table, `pane_current_path` in the snapshot).
They are hooks, not implementations.

## Decisions

1. **Client per browser tab.** tmux renders; the web app navigates.
2. **The app owns the front door.** One process on :443, ACME wildcard, auth,
   UI, and later the published-port proxy.
3. **Go backend, TypeScript frontend.** One static binary with the UI embedded.
4. **Laptop first, phone as a bonus.** No design effort spent on soft keyboards.
5. **Enrollment links, no passwords.**
6. **shadcn/ui** on Tailwind v4.

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

### Attach model

Opening a session in a browser tab creates a throwaway tmux session grouped onto
the real one:

```sh
tmux new-session -d -t work -s _web-<uuid>
tmux set -t _web-<uuid> status off
tmux set -t _web-<uuid> destroy-unattached on
tmux set -t _web-<uuid> aggressive-resize on
```

Grouped sessions share a window list but keep an independent current window.
So two browser tabs can watch two different agents, and a terminal attached to
`work` locally is unaffected. `destroy-unattached on` collects the throwaway
session the moment the client leaves, so a dropped connection leaves nothing
behind. `status off` keeps a tmux status bar from competing with the web chrome.

`creack/pty` then spawns `tmux attach -t _web-<uuid>`. That PTY's bytes are the
WebSocket payload, fed straight into `@wterm/react`.

**Reconnect** repeats the sequence with a fresh uuid. tmux redraws on attach, so
there is no output to replay and no server-side buffer to size or leak. tmux is
the persistence layer; the app holds no terminal state.

**Resize.** Browser dimensions reach `pty.Setsize`; tmux's default
`window-size latest` makes the active client win.

### Sidebar state

One polled command, every ~1.5s, fields separated by `\x1f`:

```sh
tmux list-panes -a -F '#{session_name}\x1f#{window_index}\x1f#{window_name}\x1f#{pane_id}\x1f#{pane_active}\x1f#{pane_current_command}\x1f#{pane_current_path}'
```

`\t` is wrong here: a window name or path may contain one.

The snapshot renders directly to the sidebar tree, so the client keeps no model
of tmux that could drift. `pane_current_path` is already present and is the hook
the git panel will later use.

Sidebar clicks issue `select-window` / `select-pane` against the tab's own
grouped session, so navigation never disturbs other clients.

If polling proves too coarse, the upgrade is a single read-only `tmux -C`
control-mode sidecar for push notifications. Not needed for v1.

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

Sessions do not expire. Revocation is the control.

### Recovery

Lost every device: SSH in, run `enroll` again. From any live session: list
devices, mint links, revoke — including revoking the device previously used.

### Other checks

- Validate `Origin` on the WebSocket handshake. Browsers attach cookies to
  cross-origin WS handshakes; without this, any page the user visits can open a
  socket to the shell.
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
    └─ <Terminal/>   @wterm/react, ResizeObserver → pty.Setsize
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
fuzzy-matching `session/window/pane`.

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

### Theme

wterm themes are CSS custom properties and so is shadcn. One palette feeds both,
so the terminal does not look like an iframe dropped into an app.

### Connection state

`sonner` toasts on drop and reconnect, plus the header dot. Reconnect is
automatic with backoff.

## Failure modes

| Case | Handling |
| --- | --- |
| No tmux server running | First load offers "create session `main`" |
| Orphaned `_web-*` sessions after `SIGKILL` | Sweep at startup: kill `_web-*` with zero attached clients |
| Resize thrash yanking the local terminal | Debounce resize to ~150ms |
| Output floods outrunning the socket | Bounded write buffer; when full, **stop reading the PTY** |
| Corrupted terminal state | Never drop bytes — a dropped escape sequence corrupts permanently |
| Multibyte splitting | Binary WebSocket frames, not text |
| Cert renewal failure | Serve the existing cert, log loudly, surface in the UI |

## Storage

One JSON file under `$XDG_STATE_HOME/wterm-web/`, written by atomic rename.
Devices now, published routes later. No SQLite: nothing here has a query.

## Testing

**Integration against real tmux** is the high-value layer. Use an isolated
socket (`tmux -L wterm-test`): create sessions, assert the snapshot parses,
assert a grouped client gets its own current window and vanishes on detach.
Fast and hermetic, with no mock of a protocol we do not control.

**Unit tests** for enrollment (expiry, single use, replay, wrong token), the
`SO_PEERCRED` uid check, `Origin` validation, and cookie scoping.

**End to end** with Playwright driving the real binary: enroll, attach, type
`echo hi`, assert the row. Because wterm renders to the DOM rather than a canvas,
this is an ordinary DOM assertion — a canvas-based terminal would not allow it.

## Repository layout

```
cmd/wterm-web/        serve, enroll, devices
internal/tmux/        snapshot, session lifecycle, command builders
internal/ptybridge/   attach + WebSocket bridge
internal/auth/        peercred, enrollment, device store
internal/front/       http mux, origin check, embedded assets
web/                  vite + react + shadcn
docs/plans/
```

## Deferred, with the hooks already in place

**Publishing local ports.** The front door already terminates a wildcard cert, so
a new subdomain needs no certificate work — it starts routing. A routes table
maps `test` → `127.0.0.1:4444` with a per-route public/gated flag, served by
`httputil.ReverseProxy`.

**Git context.** `pane_current_path` is already in every snapshot. Walk to the
repo root and show branch, ahead/behind, dirty files, and stashes. Read-only
first; manipulation only if using the shell for it proves annoying.
