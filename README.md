# tmux-web

Drive your dev box's tmux from a browser.

It attaches to the tmux server already running on a machine and serves it as a
web terminal, so you can pick up the coding agents running there from a laptop
or a phone somewhere else. The sidebar lists your sessions, windows and panes
with what each one is running, so several agents are one click apart, and marks
which of them are working, which are blocked and which have finished. Install a
small integration in a project and its agent says what it is doing right now.

It is a single static binary. The only runtime dependency is `tmux`.

```
┌────────────────────┬──────────────────────────────┐
│ ● work             │  work › 2: api › claude   ●  │
│  ✳● › api          │                              │
│      run go test   │  ▸ running tests…            │
│  › web             │                              │
│    [vim]           │                              │
│ ● infra            │                              │
│  › deploy          │                              │
│    [zsh]           │                              │
├────────────────────┤                              │
│ ◒ you@box          │                              │
└────────────────────┴──────────────────────────────┘
```

## Getting started

Build it, then run it on the machine whose tmux you want to reach:

```sh
make build
./tmux-web serve --host tmux.example.com
```

It gets a TLS certificate for that hostname over ACME, so ports 80 and 443 need
to reach it. To try it locally instead, skip the certificate:

```sh
./tmux-web serve --host localhost --dev     # plain HTTP on 127.0.0.1:8080
```

### On a private network

ACME cannot help with a name no public CA can validate -- an invented hostname
like `devbox.ss`, or anything that only resolves on your VPN. The daemon issues
its own certificate instead:

```sh
./tmux-web serve --host devbox.ss --self-signed --tls-port 8443
```

`--dev` is not an alternative here: it binds loopback only, and the session
cookie requires a secure context, so reaching a `--dev` daemon from another
machine fails to sign in with no useful error.

The certificate is written next to the device store and reused, so each browser
warns once and remembers. Before accepting that warning, compare the SHA-256 the
daemon logs at startup with the one the browser shows -- with no CA involved,
that comparison is the only thing distinguishing your daemon from someone else's
certificate on the same network.

The name has to resolve: a VPN DNS entry, or a line in `/etc/hosts` on each
machine. `--tls-port` avoids needing root.

Then enrol a browser. There are no passwords and no login form:

```sh
./tmux-web enroll --name laptop
# https://tmux.example.com/enroll#Ck9tR2p…   single use, expires in 10m
```

Open that link in the browser you want to use. That browser is now enrolled
permanently — you only do this once per device. The dialog inside the app can
mint further links (with a QR code, for a phone) so you never need to come back
to the shell for the next device.

```sh
./tmux-web devices        # what is enrolled
./tmux-web revoke <id>    # cut a device off, including any terminal it has open
```

Lost every device? SSH in and run `enroll` again.

## How the auth works

A browser arriving over the network carries no identity worth trusting, so
nothing about it is used to decide access. A process connecting to a unix socket
does: the kernel reports its uid, and it cannot be forged. So the daemon listens
on a socket only its own user can open, and *being able to run `tmux-web enroll`
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
- Right-click a row — long-press on a phone — to rename, split, zoom, label or
  kill it, and to open a new window. The `+` in the sidebar header makes a new
  session, and it is in the header rather than on a row because with no tmux
  server running there are no rows to right-click.
- Scroll with the mouse wheel to enter tmux's copy mode.
- `Shift+click` or `Ctrl+click` a URL to open it. Real hyperlinks (`gh`, `delta`,
  `eza`) and bare URLs both work; a plain click still goes to tmux.
- Close the tab whenever. tmux is where the state lives, so reconnecting drops
  you back on the same pane.

## Which agent needs you

An agent pane carries its agent's mark and a dot saying working, blocked or
idle; a window or a session carries the most urgent state under it, so the
question is answerable without expanding anything. A shell gets no dot — a shell
is not idle, it is a shell. A tab you are not looking at puts the count in its
title and a dot on its favicon, for the agents that are blocked or that have
finished since this device last looked.

All of that is read off the screen, and reading the screen has two limits. It
cannot say *what* an agent is doing: the pane title looks like it should and
does not, because Claude Code and opencode both generate one from the session's
first turn and never revise it, and pi's is the working directory. And it only
runs while a browser is attached — a pane nobody is looking at is never
captured, so nothing is computed about it.

The integrations fix both. They are small files, shipped inside the binary, that
an agent runs to report what it is doing onto its own tmux pane:

```sh
./tmux-web install-integration --agent claude
./tmux-web install-integration --agent opencode
./tmux-web install-integration --agent pi
```

claude gets `.claude/tmux-web-report.sh` and four hooks merged into
`.claude/settings.json`, which is a file you own and is merged rather than
rewritten; opencode gets `.opencode/plugin/tmux-web.js`; pi gets
`.pi/extensions/tmux-web.ts`.

It writes into the current directory, or into a directory you name after the
flags. It prints every path it is going to touch and asks before writing
anything, refuses any file tmux-web did not write itself, and `--remove` takes
it back out again.

With one installed, the second line of the row says what the agent is doing
*now* — `run wc -l sample.txt`, `read sample.txt`, `Claude needs your
permission` — and it keeps saying it **with no browser connected**. That is the
case the app exists for: walk away, come back on a phone, and see which agent
finished and which one is waiting, instead of an hour of nothing because nothing
was watching.

A report goes to tmux and never to the daemon. So it costs nothing on a machine
where tmux-web is not running, and a report survives the daemon being restarted
under it. It exits 0 on every path, and Claude's hooks are registered `async` so
they cannot hold up a turn: a reporting feature that can stall an agent is worse
than no reporting at all.

`--global` installs into the agent's own configuration instead of a project, and
all three support it. Each one is a **directory drop** and no file you own is
edited:

- claude: `~/.claude/tmux-web-report.sh`, with the four hooks merged into
  `~/.claude/settings.json`.
- opencode: `$XDG_CONFIG_HOME/opencode/plugin/tmux-web.js` (`~/.config` when that is
  unset). Nothing is added to `opencode.jsonc` — an absolute path in its `plugin`
  array does work, but a dropped file uninstalls with one `rm` and leaves your
  config alone. opencode's own bootstrap (`package.json`, `node_modules/`, a
  `.gitignore`) lands beside the plugin in opencode's config directory, **not in
  any repository**.
- pi: `$PI_CODING_AGENT_DIR/extensions/tmux-web.ts` (`~/.pi/agent` when that is
  unset), which pi auto-loads with no settings entry and no trust prompt.
  `~/.pi/agent/settings.json` is not edited and `pi install` is not run:
  measured on pi 0.85.1, `pi install` and `pi remove` keep every value and the
  key order but **reformat the whole file** — indentation normalised, arrays
  exploded one element per line, the trailing newline dropped. That is a
  wholesale rewrite of a file you own.

**A global pi extension is loaded by every pi on the machine, and one pi cannot
load stops pi *starting* in every project.** So `--global --agent pi` loads the
file with a real `pi` before writing it, in a throwaway config directory, and
refuses if pi rejects it — or if it cannot run pi at all, in which case it tells
you to install into a project instead, where a bad extension breaks one project
and pi prints the `pi -ne` hint itself.

Installing both globally and in a project is allowed and says so. Neither
runtime dedupes by filename, so both copies load in one process; they agree at
run time on one of them doing the reporting, and the newer of the two wins.

Installing is a command and not a button in the web UI, and it is not going to
become one. The UI is reachable over the network from a phone, and writing
executable code into a repository is not a thing a network request should be
able to do however well it is authenticated.

### What the reports still miss

- **Four kinds of Claude notification produce no badge**: an MCP form, an MCP
  request to open a URL, the quota press-Enter banner, and a teammate's setup
  question. Nothing about them is recognisable on the screen either, and nobody
  has managed to capture one to write a rule against, so both authorities are
  blind to those four waits.
- **A nested `claude`, started from another agent's shell, can report a finish
  early.** It inherits the pane, loads the same project settings and is the root
  of its own session, so its turn end lands on the outer agent's row.
- **With a browser open, Claude's blocked badge is slower with the integration
  than without it** — about six seconds against about 1.5, because a fresh
  report suppresses the screen capture that would have found the dialog. The
  trade buys the disconnected case, where there is otherwise nothing at all.

## What it does not do yet

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
