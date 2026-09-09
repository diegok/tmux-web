import { defineConfig } from '@playwright/test'

/**
 * The end-to-end suite: a real browser against the real daemon and a real tmux
 * server. Everything else in this repository is a unit or integration test, so
 * the only journeys that belong here are the ones that need all three at once.
 *
 * `workers: 1` and `fullyParallel: false` because each test starts a daemon on
 * an ephemeral port and a tmux server on its own socket; running them in
 * parallel would work, but it would multiply the tmux servers alive at any
 * moment on a developer's own machine for no gain at this suite size.
 *
 * `retries: 0` on purpose. A retry hides flake, and a flaky end-to-end test is
 * worse than none: it trains people to re-run rather than to look.
 */
export default defineConfig({
  testDir: './e2e',
  globalSetup: './e2e/global-setup.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: !!process.env.CI,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : [['list']],
  // Generous: each test builds nothing but does start a daemon, enroll a
  // browser, and wait out at least one 1.5s snapshot poll.
  timeout: 90_000,
  expect: { timeout: 15_000 },
  use: {
    // Pinned because App renders <ThemeProvider defaultTheme="system">, so the
    // class on <html> -- and every colour derived from it -- would otherwise be
    // whatever the machine running the suite prefers.
    colorScheme: 'light',
    viewport: { width: 1280, height: 800 },
    trace: 'retain-on-failure',
    video: 'off',
  },
  projects: [{ name: 'chromium', use: { browserName: 'chromium' } }],
})
