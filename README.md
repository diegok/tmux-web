# tmux-web

Drive your dev box's tmux from a browser.

It attaches to the tmux server already running on a machine and serves it as a
web terminal, so you can pick up the coding agents running there from a laptop
or a phone somewhere else. The sidebar lists your sessions, windows and panes
with what each one is running, so several agents are one click apart.

It is a single static binary. The only runtime dependency is `tmux`.

```
┌─────────────┬──────────────────────────────────┐
│ work        │  work › 2: api › claude       ●  │
│  › api      │                                  │
│    [claude] │  ▸ running tests…                │
│  › web      │                                  │
│    [vim]    │                                  │
│ infra       │                                  │
│  › deploy   │                                  │
│    [zsh]    │                                  │
├─────────────┤                                  │
│ ◒ you@box   │                                  │
└─────────────┴──────────────────────────────────┘
```

## Getting started

Build it, then run it on the machine whose tmux you want to reach:

```sh
make build
./wterm-web serve --host tmux.example.com
```

It gets a TLS certificate for that hostname over ACME, so ports 80 and 443 need
to reach it. To try it locally instead, skip the certificate:

```sh
./wterm-web serve --host localhost --dev     # plain HTTP on 127.0.0.1:8080
```

Then enrol a browser. There are no passwords and no login form:

```sh
./wterm-web enroll --name laptop
# https://tmux.example.com/enroll#Ck9tR2p…   single use, expires in 10m
```

Open that link in the browser you want to use. That browser is now enrolled
permanently — you only do this once per device. The dialog inside the app can
mint further links (with a QR code, for a phone) so you never need to come back
to the shell for the next device.

```sh
./wterm-web devices        # what is enrolled
./wterm-web revoke <id>    # cut a device off, including any terminal it has open
```

Lost every device? SSH in and run `enroll` again.

## How the auth works

A browser arriving over the network carries no identity worth trusting, so
nothing about it is used to decide access. A process connecting to a unix socket
does: the kernel reports its uid, and it cannot be forged. So the daemon listens
on a socket only its own user can open, and *being able to run `wterm-web enroll`
on the box* is the entire authorization proof.

An enrolment link projects that one-time proof onto a remote browser, which
exchanges it for a device credential. The token travels in the URL fragment, so
it is never sent to a server, stays out of access logs and `Referer` headers, and
is not consumed by the link scanners that messaging apps run.

Device sessions do not expire. Revocation is the control, and it severs live
connections rather than only failing the next request.

## Using it

- Click a pane in the sidebar to jump to it, or press `Ctrl+Alt+K` for a fuzzy
  palette (the "Jump to…" button does the same, which matters on layouts where
  that chord is AltGr+K).
- Scroll with the mouse wheel to enter tmux's copy mode.
- `Shift+click` or `Ctrl+click` a URL to open it. Real hyperlinks (`gh`, `delta`,
  `eza`) and bare URLs both work; a plain click still goes to tmux.
- Close the tab whenever. tmux is where the state lives, so reconnecting drops
  you back on the same pane.

## What it does not do yet

- **Create sessions.** Make them with `tmux new -s work` on the box; the app
  attaches to what already exists.
- **Publish local ports.** Mapping `test.example.com` to a service on
  `127.0.0.1:4444` is the next thing planned, and the reason the front door is
  built the way it is.
- **Show git context.** Reading the repo behind the pane you are looking at is
  planned after that.

## Things worth knowing

Each browser tab gets its own throwaway tmux session, grouped onto the real one.
That keeps the *current window* independent, so a tab and your local terminal can
look at different things. Three properties belong to the window rather than the
session and are therefore shared with anyone else attached:

- **Size** — tmux sizes a window for whichever client acted most recently, so
  viewing the same window in the browser and locally makes them fight over it.
- **Copy mode** — scrolling back in the browser scrolls the local view too.
- **Active pane** — clicking a pane in the sidebar moves it for every client.

None of these bite unless you are watching the same window from two places at
once. There is no way to scope them per client in tmux; the design document has
the details.

## Requirements

tmux 3.4 or newer (developed against 3.7), and Go 1.26 plus pnpm to build.

## Development

```sh
make test        # go tests with -race, then the frontend suite
make test-e2e    # Playwright against a real browser, daemon and tmux
```

Design and implementation notes are in `docs/plans/`.

## Licence

MIT
