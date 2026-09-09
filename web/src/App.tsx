import { useState } from 'react'

import { Terminal } from '@/components/Terminal'
import type { TerminalStatus } from '@/components/Terminal'

/**
 * Placeholder shell: one terminal, full height. The sidebar (task 21) and the
 * footer, devices dialog and palette (task 22) replace this, at which point the
 * base session comes from the snapshot instead of the query string and the
 * status below feeds the header dot rather than a debug line.
 *
 * `?session=` picks the base tmux session to group onto; the daemon answers 404
 * on the socket if there is no such session, which surfaces here as a terminal
 * that keeps reconnecting.
 */
export default function App() {
  const session = new URLSearchParams(window.location.search).get('session') ?? 'main'
  const [status, setStatus] = useState<TerminalStatus | null>(null)

  return (
    <main className="flex h-svh flex-col">
      <header className="text-muted-foreground flex items-center gap-2 border-b px-3 py-1.5 text-xs">
        <span className="text-foreground font-medium">tmux-web</span>
        <span>{session}</span>
        <span className="ml-auto">
          {status?.phase ?? 'connecting'}
          {status?.pane ? ` · ${status.pane}` : ''}
        </span>
      </header>
      <div className="min-h-0 flex-1">
        <Terminal session={session} onStatusChange={setStatus} />
      </div>
    </main>
  )
}
