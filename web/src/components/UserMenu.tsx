/**
 * The sidebar footer: which box this tab is driving, which device it is
 * driving it from, and the three things that are done to that answer.
 *
 * ## Why an identity line at all
 *
 * There is exactly one user here -- the uid the daemon runs as -- so the line
 * is not "who am I", which is never in doubt. It is *which machine am I
 * driving*, and that only starts to matter once there is a second one, which is
 * the point at which two tabs with identical sidebars are a genuinely dangerous
 * way to run `rm`. The second line is this browser's enrolled device name and
 * the socket's state, so a tab that is not typing anywhere says so at the same
 * glance.
 *
 * ## Sign out revokes
 *
 * Not "clears the cookie". A cookie-only logout leaves a device in the store
 * that is fully privileged, indefinitely valid, and no longer identifiable by
 * anyone: the browser that could have named it has forgotten it. So Sign out is
 * `DELETE /api/devices/{me}` -- the same operation as revoking any other row --
 * and the cookie is cleared by the daemon on the way back.
 *
 * That request severs this device's live sockets as a side effect, which is why
 * `performSignOut` treats a 401 and a 404 as success (the credential is gone,
 * which is the whole ask) and refuses to reload on anything else. Reloading
 * after a failed revoke would show the friendly "not enrolled" page while the
 * device was still, in fact, enrolled -- the exact false confirmation the
 * design is trying to avoid.
 */

import { ChevronsUpDown, LogOut, Monitor, Moon, Smartphone, Sun, TriangleAlert } from 'lucide-react'
import { useTheme } from 'next-themes'
import { useCallback, useEffect, useState } from 'react'

import { DevicesDialog, DeviceApiError, revokeDevice } from '@/components/DevicesDialog'
import type { TerminalPhase } from '@/components/Terminal'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuSub,
  DropdownMenuSubContent,
  DropdownMenuSubTrigger,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { SidebarMenu, SidebarMenuButton, SidebarMenuItem } from '@/components/ui/sidebar'
import type { FetchLike } from '@/lib/useSnapshot'

/** Where the footer's two lines come from. */
export const IDENTITY_URL = '/api/user'

/**
 * How long a sign-out waits for its own revocation to be confirmed.
 *
 * Revoking yourself closes your sockets, so the pessimistic case is a response
 * that never lands. Waiting forever would leave a menu item spinning; giving up
 * silently would claim a sign-out that may not have happened. The timeout ends
 * in a message, not a reload.
 */
export const SIGN_OUT_TIMEOUT_MS = 6000

/** `GET /api/user`. */
export interface Identity {
  user: string
  host: string
  /** This browser's enrolled device name. */
  device: string
  deviceId: string
}

const browserFetch: FetchLike = (url, init) => globalThis.fetch(url, init)

export function parseIdentity(body: unknown): Identity {
  if (typeof body !== 'object' || body === null) {
    throw new Error('the identity response was not an object')
  }
  const raw = body as Partial<Record<keyof Identity, unknown>>
  const str = (v: unknown) => (typeof v === 'string' ? v : '')
  const id = str(raw.deviceId)
  if (id === '') throw new Error('the identity response named no device')
  return { user: str(raw.user), host: str(raw.host), device: str(raw.device), deviceId: id }
}

export async function fetchIdentity(
  signal?: AbortSignal,
  fetchImpl: FetchLike = browserFetch,
): Promise<Identity> {
  const res = await fetchImpl(IDENTITY_URL, {
    signal,
    headers: { Accept: 'application/json' },
    credentials: 'same-origin',
  })
  if (!res.ok) throw new Error(`could not read the identity (${res.status})`)
  return parseIdentity(await res.json())
}

/** `dev@devbox`, or a placeholder while the request is in flight. */
export function identityLabel(identity: Identity | null): string {
  if (!identity) return '…'
  if (identity.user && identity.host) return `${identity.user}@${identity.host}`
  return identity.user || identity.host || 'this host'
}

/**
 * The socket's state in a word a person reads rather than a phase name.
 *
 * `closed` is deliberately "no session": it is what the terminal reports when
 * there is nothing to attach to at all, which is not the same as a drop.
 */
export function connectionLabel(phase: TerminalPhase | null): string {
  switch (phase) {
    case 'ready':
      return 'connected'
    case 'connecting':
      return 'connecting'
    case 'reconnecting':
      return 'reconnecting'
    case 'ended':
      return 'session ended'
    case 'closed':
      return 'no session'
    default:
      return 'connecting'
  }
}

/** What a sign-out attempt concluded. */
export interface SignOutOutcome {
  /** The credential is gone. The page is being reloaded. */
  ok: boolean
  /** Why it is not gone, for the user to read. */
  message?: string
}

export interface SignOutOptions {
  fetchImpl?: FetchLike
  /** Defaults to a full reload, which lands on the daemon's 401 page. */
  reload?: () => void
  timeoutMs?: number
  /** Injectable timer, so the timeout can be tested without waiting for it. */
  setTimer?: (fn: () => void, ms: number) => ReturnType<typeof setTimeout>
  clearTimer?: (handle: ReturnType<typeof setTimeout>) => void
}

/**
 * Revoke this device and leave.
 *
 * The reload is what turns a revoked cookie into something a user can see: the
 * daemon answers `/` for an unenrolled browser with a page explaining that the
 * fix is `tmux-web enroll` on the host. Staying on a dead SPA would show a
 * terminal reconnecting forever instead.
 */
export async function performSignOut(
  deviceId: string,
  {
    fetchImpl,
    reload = () => globalThis.location.reload(),
    timeoutMs = SIGN_OUT_TIMEOUT_MS,
    setTimer = setTimeout,
    clearTimer = clearTimeout,
  }: SignOutOptions = {},
): Promise<SignOutOutcome> {
  const controller = new AbortController()
  const timer = setTimer(() => controller.abort(), timeoutMs)
  try {
    await revokeDevice(deviceId, fetchImpl, controller.signal)
  } catch (err) {
    // 404 is a device that is already gone, which is the outcome asked for.
    // Everything else -- including the abort above -- leaves the device
    // enrolled as far as anyone here can tell, and must say so.
    if (!(err instanceof DeviceApiError && err.status === 404)) {
      return {
        ok: false,
        message: controller.signal.aborted
          ? 'The daemon did not confirm the sign-out. This device may still be enrolled — check the devices list, or revoke it from the host.'
          : `Sign out failed: ${err instanceof Error ? err.message : String(err)}`,
      }
    }
  } finally {
    clearTimer(timer)
  }
  reload()
  return { ok: true }
}

export interface UserMenuProps {
  /** The socket's phase, for the second line. */
  connection: TerminalPhase | null
  /** Injectable for tests; defaults to the browser's fetch. */
  fetchImpl?: FetchLike
}

export function UserMenu({ connection, fetchImpl }: UserMenuProps) {
  const [identity, setIdentity] = useState<Identity | null>(null)
  const [devicesOpen, setDevicesOpen] = useState(false)
  const [signingOut, setSigningOut] = useState(false)
  const [notice, setNotice] = useState<string | null>(null)

  useEffect(() => {
    const controller = new AbortController()
    fetchIdentity(controller.signal, fetchImpl).then(
      (id) => setIdentity(id),
      (err: unknown) => {
        if (controller.signal.aborted) return
        // Not fatal: the footer degrades to the device it does not know the
        // name of, and everything else in the app still works.
        setNotice(`Could not read who you are: ${err instanceof Error ? err.message : err}`)
      },
    )
    return () => controller.abort()
  }, [fetchImpl])

  /**
   * Returns the failure to show, or null when the page is on its way out. The
   * devices dialog is modal, so a sign-out started from the row marked "this
   * device" has to be able to report back into the dialog: a notice painted in
   * the footer behind the overlay is a notice nobody reads.
   */
  const signOut = useCallback(async (): Promise<string | null> => {
    if (signingOut) return null
    setSigningOut(true)
    setNotice(null)
    try {
      // The id is re-read when the footer never got one, so that a failed
      // identity request does not become a browser that cannot sign out.
      const id = identity?.deviceId ?? (await fetchIdentity(undefined, fetchImpl)).deviceId
      const outcome = await performSignOut(id, { fetchImpl })
      if (outcome.ok) return null
      const failure = outcome.message ?? 'Sign out failed.'
      setNotice(failure)
      return failure
    } catch (err) {
      const failure = `Sign out failed: ${err instanceof Error ? err.message : String(err)}`
      setNotice(failure)
      return failure
    } finally {
      setSigningOut(false)
    }
  }, [identity, signingOut, fetchImpl])

  const who = identityLabel(identity)
  const state = connectionLabel(connection)
  const device = identity?.device ?? 'this device'

  return (
    <SidebarMenu>
      {notice && (
        <SidebarMenuItem>
          <p
            role="alert"
            className="text-sidebar-foreground/80 flex items-start gap-1.5 px-2 py-1 text-xs group-data-[collapsible=icon]:hidden"
          >
            <TriangleAlert className="mt-px size-3.5 shrink-0" aria-hidden />
            <span className="break-words">{notice}</span>
          </p>
        </SidebarMenuItem>
      )}
      <SidebarMenuItem>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <SidebarMenuButton
              size="lg"
              // The collapsed rail is 3rem wide: the two lines are hidden by
              // the group selectors below and the avatar is all that is left,
              // so the tooltip is the only thing that still says which box.
              tooltip={`${who} · ${device} · ${state}`}
              className="data-open:bg-sidebar-accent data-open:text-sidebar-accent-foreground"
            >
              <IdentityLines identity={identity} connection={connection} />
              <ChevronsUpDown className="ml-auto shrink-0 group-data-[collapsible=icon]:hidden" />
            </SidebarMenuButton>
          </DropdownMenuTrigger>
          <DropdownMenuContent
            // Upward: the trigger is the last thing in the sidebar, and on
            // mobile it is the last thing in a Sheet that is pinned to the
            // bottom of the viewport.
            side="top"
            align="start"
            sideOffset={8}
            className="w-56"
          >
            <DropdownMenuLabel className="text-muted-foreground text-xs font-normal">
              {who}
            </DropdownMenuLabel>
            <DropdownMenuSeparator />
            <DropdownMenuItem onSelect={() => setDevicesOpen(true)}>
              <Smartphone aria-hidden />
              Devices…
            </DropdownMenuItem>
            <AppearanceMenu />
            <DropdownMenuSeparator />
            <DropdownMenuItem variant="destructive" disabled={signingOut} onSelect={() => void signOut()}>
              <LogOut aria-hidden />
              {signingOut ? 'Signing out…' : 'Sign out'}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </SidebarMenuItem>

      <DevicesDialog
        open={devicesOpen}
        onOpenChange={setDevicesOpen}
        onSignOut={signOut}
        fetchImpl={fetchImpl}
      />
    </SidebarMenu>
  )
}

/**
 * The two lines, and the avatar that is all of them that survives the collapsed
 * rail.
 *
 * Separated from the menu so that what the footer *says* can be rendered and
 * asserted on its own: the container's answer arrives from an effect, and an
 * effect is the one thing a static render will not run.
 */
export function IdentityLines({
  identity,
  connection,
}: {
  identity: Identity | null
  connection: TerminalPhase | null
}) {
  return (
    <>
      <span
        className="bg-sidebar-primary text-sidebar-primary-foreground flex size-8 shrink-0 items-center justify-center rounded-lg text-xs font-medium"
        aria-hidden
      >
        {(identity?.user || '?').slice(0, 1).toUpperCase()}
      </span>
      <span className="grid min-w-0 flex-1 text-left leading-tight group-data-[collapsible=icon]:hidden">
        <span className="truncate text-sm font-medium">{identityLabel(identity)}</span>
        <span className="text-sidebar-foreground/70 truncate text-xs">
          {identity?.device ?? 'this device'} · {connectionLabel(connection)}
        </span>
      </span>
    </>
  )
}

/**
 * Appearance, which is a theme and nothing else.
 *
 * The design lists "theme and font size". Font size is not here: the terminal's
 * cell size belongs to wterm's own configuration, and a control that changed
 * the app's text while leaving the terminal alone would be a setting that does
 * not do what its name says. Three radio items in the menu the user already
 * opened is also the whole feature -- a separate Appearance *panel* would be a
 * dialog with one control in it.
 */
function AppearanceMenu() {
  const { theme, setTheme } = useTheme()
  return (
    <DropdownMenuSub>
      <DropdownMenuSubTrigger>
        <Sun aria-hidden />
        Appearance
      </DropdownMenuSubTrigger>
      <DropdownMenuSubContent>
        <DropdownMenuRadioGroup value={theme ?? 'system'} onValueChange={setTheme}>
          <DropdownMenuRadioItem value="system">
            <Monitor aria-hidden />
            System
          </DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="light">
            <Sun aria-hidden />
            Light
          </DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="dark">
            <Moon aria-hidden />
            Dark
          </DropdownMenuRadioItem>
        </DropdownMenuRadioGroup>
      </DropdownMenuSubContent>
    </DropdownMenuSub>
  )
}

export default UserMenu
