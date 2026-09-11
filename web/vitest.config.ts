import { defineConfig, mergeConfig } from 'vitest/config'

import viteConfig from './vite.config.ts'

// Layered on top of the app's vite config so that tests see the same `@/`
// alias and plugins the app does, rather than a second, drifting resolution.
export default mergeConfig(
  viteConfig,
  defineConfig({
    test: {
      // Node, not jsdom: the transport's only DOM dependency is the WebSocket
      // constructor, which its tests replace anyway. A component test added
      // later can opt in per file with `// @vitest-environment jsdom`.
      environment: 'node',
      // The integrations' shared queue lives in internal/integrations/,
      // because that tree is what the installer //go:embed's, and its test
      // lives beside it. Reaching outside this root keeps one copy of the
      // module rather than two that drift.
      include: ['src/**/*.test.ts', 'src/**/*.test.tsx', '../internal/integrations/*.test.ts'],
    },
  }),
)
