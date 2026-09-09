/**
 * The end-to-end harness: one throwaway tmux server, one real daemon, one real
 * browser, per test.
 *
 * ## Why the isolation is this elaborate
 *
 * These tests run on a developer's own machine, where a tmux server holding
 * their actual work is already running. `wterm-web serve` has no flag for
 * choosing a tmux server -- `front.Config.TmuxArgs` exists but no CLI flag
 * reaches it -- so a daemon started here would drive the *default* server: it
 * would resize their windows to the headless browser's size, create sessions
 * beside theirs, and sweep `_web-` sessions on startup.
 *
 * The fix is environmental, and needs both halves:
 *
 *   - `TMUX_TMPDIR` moves the default socket into a temp directory, so
 *     `tmux new-session` from inside the daemon cannot reach the real server.
 *   - A `tmux` shim first on `PATH` prepends `-L <random> -f /dev/null`. `-L`
 *     is belt and braces for the socket, and `-f /dev/null` is not: a server
 *     on a private socket still reads `~/.tmux.conf`, and this developer's
 *     config sets non-default options that would leak into what these tests
 *     observe.
 *
 * Both `internal/tmux.Client` and `internal/ptybridge` invoke bare `"tmux"`,
 * so the shim covers every path into tmux the daemon has.
 *
 * The device store (`XDG_STATE_HOME`) and the admin socket (`XDG_RUNTIME_DIR`,
 * plus an explicit `--socket`) are redirected the same way, so nothing here
 * touches `~/.local/state/wterm-web/devices.json`.
 *
 * ## Why per test rather than per run
 *
 * A daemon starts in well under a second, and a suite that shares one grows
 * order dependencies immediately: the revocation test would sever the sockets
 * of tests that ran later, and every test would see every other test's windows
 * in the sidebar. A fixture also guarantees teardown on failure, which a
 * `globalSetup` pair does not do as reliably when a test crashes the worker.
 */

import { test as base } from '@playwright/test'
import type { Locator, Page } from '@playwright/test'
import { execFileSync, spawn } from 'node:child_process'
import type { ChildProcess } from 'node:child_process'
import * as fs from 'node:fs'
import * as net from 'node:net'
import * as os from 'node:os'
import * as path from 'node:path'
import { fileURLToPath } from 'node:url'

export const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
export const binary = path.join(repoRoot, 'wterm-web')

/** The base tmux session every test starts with, and the window inside it. */
export const BASE_SESSION = 'e2e'
export const BASE_WINDOW = 'shell'

/** One row of `wterm-web devices`. */
export interface DeviceRow {
  id: string
  name: string
}

export interface Wterm {
  /** `http://localhost:<ephemeral>`, the origin the daemon puts in its links. */
  readonly baseURL: string
  /** Mint an enrollment link over the admin socket, as a person would. */
  enroll(name: string): string
  /** `wterm-web devices`, parsed. */
  devices(): DeviceRow[]
  /** `wterm-web revoke <id>`. */
  revoke(id: string): void
  /** Run a tmux command against this test's private server. */
  tmux(...args: string[]): string
  /**
   * Stop the daemon and start another on the same port and state. The nearest
   * thing to "the network went away and came back" that a browser can actually
   * be made to see -- see the note on `setOffline` in terminal.spec.ts.
   */
  restart(): Promise<void>
  /** Everything the daemon has written to stderr, for a failure message. */
  log(): string
}

/** A port nothing is listening on. */
async function freePort(): Promise<number> {
  return await new Promise((resolve, reject) => {
    const srv = net.createServer()
    srv.on('error', reject)
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address()
      if (addr === null || typeof addr === 'string') {
        srv.close()
        reject(new Error('no port'))
        return
      }
      const { port } = addr
      srv.close(() => resolve(port))
    })
  })
}

async function waitForHTTP(url: string, deadlineMs: number): Promise<void> {
  const until = Date.now() + deadlineMs
  for (;;) {
    try {
      // Any answer means it is listening; an unenrolled browser gets a 401,
      // which is the daemon working, not failing.
      await fetch(url, { redirect: 'manual' })
      return
    } catch {
      if (Date.now() > until) throw new Error(`daemon never answered on ${url}`)
      await new Promise((r) => setTimeout(r, 50))
    }
  }
}

class Harness implements Wterm {
  readonly baseURL: string
  readonly #dir: string
  readonly #socket: string
  readonly #tmuxSocket: string
  readonly #tmuxTmp: string
  readonly #realTmux: string
  readonly #env: NodeJS.ProcessEnv
  readonly #port: number
  #daemon: ChildProcess | null = null
  #log = ''

  private constructor(dir: string, port: number, realTmux: string) {
    this.#dir = dir
    this.#port = port
    this.#realTmux = realTmux
    this.baseURL = `http://localhost:${port}`
    this.#socket = path.join(dir, 'run', 'wterm-web.sock')
    this.#tmuxSocket = `wterm-e2e-${path.basename(dir).slice(-6)}-${process.pid}`
    this.#tmuxTmp = path.join(dir, 'tmux')
    this.#env = {
      ...process.env,
      PATH: `${path.join(dir, 'bin')}:${process.env.PATH ?? ''}`,
      TMUX_TMPDIR: this.#tmuxTmp,
      XDG_STATE_HOME: path.join(dir, 'state'),
      XDG_RUNTIME_DIR: path.join(dir, 'run'),
      // The daemon is started from inside a tmux pane when a developer runs
      // this suite by hand. $TMUX makes tmux refuse to nest, and a browser tab
      // is not a nested client.
      TMUX: undefined,
    }
  }

  static async start(): Promise<Harness> {
    // Short, because $XDG_RUNTIME_DIR/wterm-web.sock has to fit in the 108-byte
    // sockaddr_un limit and the scratchpad paths on this machine do not.
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'wterm-e2e-'))
    const realTmux = execFileSync('sh', ['-c', 'command -v tmux'], { encoding: 'utf8' }).trim()
    if (realTmux === '') throw new Error('tmux is not on PATH')

    const h = new Harness(dir, await freePort(), realTmux)
    for (const sub of ['bin', 'run', 'state', 'tmux']) fs.mkdirSync(path.join(dir, sub))
    fs.writeFileSync(
      path.join(dir, 'bin', 'tmux'),
      `#!/bin/sh\n# e2e shim: pin the daemon to a throwaway server with no user config.\nexec ${realTmux} -L ${h.#tmuxSocket} -f /dev/null "$@"\n`,
      { mode: 0o755 },
    )

    // The session the sidebar will show and the terminal will attach to. `sh`
    // rather than the developer's login shell: its prompt is one short line
    // with no git status, no async rendering and no colour, which is what makes
    // asserting on screen text stable.
    h.tmux('new-session', '-d', '-s', BASE_SESSION, '-n', BASE_WINDOW, '-x', '120', '-y', '40', 'sh')
    await h.startDaemon()
    return h
  }

  async startDaemon(): Promise<void> {
    const proc = spawn(
      binary,
      [
        'serve',
        '--host',
        'localhost',
        '--dev',
        '--port',
        String(this.#port),
        '--socket',
        this.#socket,
      ],
      { env: this.#env, stdio: ['ignore', 'pipe', 'pipe'] },
    )
    proc.stdout?.on('data', (b: Buffer) => (this.#log += b.toString()))
    proc.stderr?.on('data', (b: Buffer) => (this.#log += b.toString()))
    proc.on('exit', (code, signal) => {
      if (proc === this.#daemon) this.#log += `\n[daemon exited code=${code} signal=${signal}]\n`
    })
    this.#daemon = proc
    await waitForHTTP(this.baseURL + '/', 15_000)
  }

  async stopDaemon(): Promise<void> {
    const proc = this.#daemon
    this.#daemon = null
    if (proc === null || proc.exitCode !== null) return
    const ended = new Promise<void>((resolve) => proc.once('exit', () => resolve()))
    proc.kill('SIGTERM')
    const timer = setTimeout(() => proc.kill('SIGKILL'), 5_000)
    await ended
    clearTimeout(timer)
  }

  async restart(): Promise<void> {
    await this.stopDaemon()
    await this.startDaemon()
  }

  /**
   * One CLI call over this test's admin socket.
   *
   * stderr is captured rather than inherited: the CLI writes its commentary
   * there on purpose ("single use, expires in 10m"), and inheriting it would
   * scatter that through the test reporter's output. It is put back into the
   * exception, which is the only place it is worth reading.
   */
  cli(...args: string[]): string {
    try {
      return execFileSync(binary, [...args, '--socket', this.#socket], {
        env: this.#env,
        encoding: 'utf8',
        stdio: ['ignore', 'pipe', 'pipe'],
      }).trim()
    } catch (err) {
      const stderr = (err as { stderr?: string }).stderr ?? ''
      throw new Error(`wterm-web ${args.join(' ')} failed: ${stderr.trim() || String(err)}`)
    }
  }

  enroll(name: string): string {
    return this.cli('enroll', '--name', name)
  }

  devices(): DeviceRow[] {
    const out = this.cli('devices')
    if (out === '') return []
    return out
      .split('\n')
      .slice(1) // the ID/NAME/... header
      .map((line) => line.trim().split(/\s+/))
      .filter((cells) => cells.length >= 2)
      .map(([id, name]) => ({ id, name }))
  }

  revoke(id: string): void {
    this.cli('revoke', id)
  }

  tmux(...args: string[]): string {
    return execFileSync(this.#realTmux, ['-L', this.#tmuxSocket, '-f', '/dev/null', ...args], {
      env: { ...process.env, TMUX_TMPDIR: this.#tmuxTmp, TMUX: undefined },
      encoding: 'utf8',
    }).trimEnd()
  }

  log(): string {
    return this.#log
  }

  /**
   * Kill everything this test started. Ordered so that a failure in one step
   * cannot leak the next: the tmux server is killed by its own socket name
   * (never by process name -- there is a real tmux server on this machine),
   * and the temp directory goes last.
   */
  async dispose(): Promise<void> {
    try {
      await this.stopDaemon()
    } finally {
      try {
        this.tmux('kill-server')
      } catch {
        // Already gone, or never started.
      }
      fs.rmSync(this.#dir, { recursive: true, force: true })
    }
  }
}

export const test = base.extend<{ wterm: Wterm }>({
  wterm: async ({}, use, testInfo) => {
    const h = await Harness.start()
    try {
      await use(h)
    } finally {
      if (testInfo.status !== testInfo.expectedStatus) {
        testInfo.attach('daemon.log', { body: h.log(), contentType: 'text/plain' })
      }
      await h.dispose()
    }
  },
})

export { expect } from '@playwright/test'

/**
 * The terminal's own status pill. It is rendered only while the socket is not
 * live, so its absence is "ready" -- and unlike the phase word in the header,
 * which is `hidden sm:inline`, it says so at every viewport width.
 */
export function pill(page: Page): Locator {
  return page.locator('[role="status"][aria-live="polite"]')
}

/**
 * Put the caret back in the terminal.
 *
 * This is a workaround for a real defect, not test ceremony, and the calls to
 * it should be deleted when the defect is fixed. `<Terminal>` grabs focus once,
 * when the socket first goes live, and never again; `App.handleSelectPane`
 * calls `term.select()` but not the `focus()` that the same handle exposes. So
 * after navigating to a pane -- by either of the two ways the product offers --
 * the caret is not in the terminal and nothing the user types is sent:
 *
 *   after load                  document.activeElement = the terminal textarea
 *   after a sidebar pane click  document.activeElement = the sidebar button
 *   after a palette pane pick   document.activeElement = <body>
 *
 * In the last two the keystrokes reach tmux not at all -- verified against
 * `capture-pane`, not merely against the DOM. On a phone that also means no
 * on-screen keyboard until the user taps the terminal, and the palette is the
 * primary navigation there.
 */
export async function focusTerminal(page: Page): Promise<void> {
  await page.getByRole('textbox', { name: 'Terminal' }).click()
}

/**
 * Enroll a browser: open the link the CLI printed, wait for the redeem to land
 * on the SPA, and wait for the terminal to go live.
 *
 * Waiting for live is not cosmetic. `TerminalSession` refuses input until the
 * socket is open and the remembered pane has been re-selected, so a test that
 * typed earlier would have its keystrokes dropped by design.
 */
export async function enroll(page: Page, wterm: Wterm, name: string): Promise<void> {
  await page.goto(wterm.enroll(name))
  await page.waitForURL((url) => url.pathname === '/')
  await page.locator('.term-row').first().waitFor()
  await pill(page).waitFor({ state: 'detached' })
}
