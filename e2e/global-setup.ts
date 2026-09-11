/**
 * Build the thing under test, once per run.
 *
 * `make build` and not `go build`: the binary embeds `internal/front/dist`, so
 * a `go build` alone would test whatever frontend happened to be lying there
 * from an earlier session. `make build` also restores
 * `internal/front/dist/.gitkeep`, which `pnpm build` deletes and which is what
 * lets `//go:embed all:dist` compile on a fresh clone -- running the suite must
 * not leave the repository in a state where `go build` fails.
 *
 * Set TMUX_WEB_E2E_SKIP_BUILD=1 to iterate on the tests without rebuilding.
 */

import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'

import { binary, repoRoot } from './harness'

export default function globalSetup(): void {
  if (process.env.TMUX_WEB_E2E_SKIP_BUILD === '1') {
    if (!fs.existsSync(binary)) {
      throw new Error(`TMUX_WEB_E2E_SKIP_BUILD=1 but ${binary} does not exist`)
    }
    return
  }
  execFileSync('make', ['build'], { cwd: repoRoot, stdio: 'inherit' })
}
