/**
 * Input that never reached tmux, and what becomes of it.
 *
 * WHY THIS EXISTS. From the daemon's own log, on a connection that died while
 * the owner was typing into an agent's prompt:
 *
 *   frames_in=289 bytes_in=2219 since_last_read=3.223s slowest_write=0s
 *   cause="failed to ping: failed to wait for pong: context deadline exceeded"
 *
 * 7.7 bytes an inbound frame -- keystrokes -- and a server that was never once
 * blocked writing. The link had gone silent in the browser-to-daemon
 * direction, where a browser hands `send()` to a socket that is still OPEN, so
 * the app believed every keystroke delivered. They were in the socket's own
 * buffer and died with it.
 *
 * Only a browser can produce that: the unit tests can prove the arithmetic of
 * the journal and the three safety conditions, and none of them can prove that
 * a real stalled socket, a real reconnect and a real tmux add up to the text
 * appearing in the pane. This stalls the socket for real -- the proxy below
 * stops forwarding in one direction, which is what a dead uplink looks like --
 * and then asserts on what tmux itself holds.
 */
import { test, expect, enroll, BASE_SESSION } from './harness'

const pane = `${BASE_SESSION}:0`

test('what a stalled socket swallowed is typed again after the reconnect', async ({
  page,
  tmuxWeb,
}) => {
  // One direction of one socket, under the test's control. `stalled` is read
  // per message, so it can be flipped in the middle of a live connection.
  let stalled = false
  const sockets: Array<{ close: () => void }> = []
  await page.routeWebSocket(/\/ws/, (ws) => {
    const server = ws.connectToServer()
    // 1001, not 1006: a reserved code cannot be sent on the wire, and the app
    // reconnects on anything but a 1000.
    sockets.push({ close: () => ws.close({ code: 1001, reason: 'the link died' }) })
    // Browser -> daemon: dropped while stalled. Not buffered and replayed,
    // because a stalled uplink does not replay either.
    ws.onMessage((message) => {
      if (!stalled) server.send(message)
    })
    // Daemon -> browser keeps flowing, which is what makes the stall
    // invisible to the app: the socket stays OPEN and keeps receiving.
    server.onMessage((message) => ws.send(message))
  })

  await enroll(page, tmuxWeb, 'stalled')
  await expect(page.locator('.term-row').first()).toBeVisible()

  // A keystroke gets through before the stall, so the socket is known good.
  await page.keyboard.type('# before\n')
  await expect.poll(() => tmuxWeb.tmux('capture-pane', '-p', '-t', pane)).toContain('# before')

  // The link goes quiet. Nothing in the browser can tell.
  stalled = true
  await page.keyboard.type('# RESCUED')
  await page.waitForTimeout(1500)

  // The premise of the whole test: tmux never saw it.
  expect(
    tmuxWeb.tmux('capture-pane', '-p', '-t', pane),
    'the stall did not actually stop the input, so this test proves nothing',
  ).not.toContain('# RESCUED')

  // And now the connection dies, the way it died in the log: the socket the
  // browser still believed in is gone, and with it everything it was holding.
  stalled = false
  sockets.at(-1)?.close()

  // What has to happen: the new socket confirms the same pane, and the text
  // the user typed into the void is typed again, by itself.
  await expect
    .poll(() => tmuxWeb.tmux('capture-pane', '-p', '-t', pane), {
      message: 'the input swallowed by the stalled socket never reached tmux',
      timeout: 30_000,
    })
    .toContain('# RESCUED')

  // Exactly once. A replay that also ran on the next reconnect would double
  // whatever the user typed.
  await page.waitForTimeout(2000)
  const screen = tmuxWeb.tmux('capture-pane', '-p', '-t', pane)
  expect(screen.split('# RESCUED').length - 1, `the text was typed more than once:\n${screen}`).toBe(
    1,
  )
})
