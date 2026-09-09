/**
 * Every browser that can drive this box, and the two operations that change
 * that list: minting an enrollment link and revoking one.
 *
 * ## Why a QR code
 *
 * The realistic use of "add a device" is enrolling a phone from the laptop that
 * is already enrolled, and the token in the link is 43 base64 characters that
 * nobody is going to retype off a screen. So the link is rendered twice -- as
 * text to copy and as a QR to point a camera at -- and the QR is deliberately
 * black on white in both themes, because a scanner needs contrast, not taste.
 *
 * ## The link is a credential, and it is treated as one
 *
 * `POST /api/devices` returns a bearer token in a URL fragment that anyone can
 * redeem, once, for `EnrollTTL`. Three rules follow, and they are the reason
 * this file has a clock in it:
 *
 *   - It is shown with its remaining life counted down out loud, so the user
 *     knows whether the phone in their hand still has time to scan it.
 *   - When that runs out the link and the QR are **replaced**, not merely
 *     annotated: a dead credential on screen invites someone to scan it and
 *     wonder why nothing happened, and a live one left on screen at the end of
 *     the day is exactly what the ten-minute TTL exists to prevent.
 *   - Closing the dialog drops it from state, so it is neither in the DOM nor
 *     waiting to reappear the next time the dialog is opened.
 *
 * Nothing here can un-mint a token -- the daemon holds it until it expires or
 * is redeemed -- so this is about not leaving it lying around, not about
 * revoking it.
 *
 * ## Revocation
 *
 * `DELETE /api/devices/{id}` removes the device *and* severs its live sockets,
 * so revoking the device you are sitting in front of ends your own session.
 * That case is not handled here: it is the same operation as Sign out, and it
 * is routed to `onSignOut` so that exactly one piece of code decides what the
 * UI does when the ground disappears. See UserMenu.tsx.
 */

import { Check, Copy, Loader2, Plus, Trash2, TriangleAlert } from 'lucide-react'
import { toDataURL } from 'qrcode'
import { useCallback, useEffect, useState } from 'react'
import type { FormEvent } from 'react'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import type { FetchLike } from '@/lib/useSnapshot'

/** The devices collection. Listing is a GET, minting a POST, revoking a DELETE. */
export const DEVICES_URL = '/api/devices'

/**
 * How long a minted link stays redeemable, mirroring `auth.EnrollTTL`.
 *
 * A copy of a Go constant, so the test pins it against the Go source rather
 * than against itself. Being wrong here is not a security hole -- the daemon
 * enforces the real one -- but a countdown that disagrees with the server tells
 * the user their link is fine when it is already dead.
 */
export const ENROLL_TTL_MS = 10 * 60 * 1000

/** One enrolled device, as `deviceJSON` marshals it. */
export interface DeviceInfo {
  id: string
  name: string
  /** The browser's User-Agent when it enrolled. Free-form and untrusted. */
  user_agent: string
  created_at: string
  /** RFC3339. Go's zero time means "has never made a request". */
  last_seen: string
  /** The device making the request. Present only on that one. */
  current: boolean
}

/** A freshly minted enrollment link, with the moment it was minted. */
export interface MintedLink {
  name: string
  url: string
  /** `Date.now()` at mint. The countdown is relative to this. */
  mintedAt: number
}

/** A request the daemon refused, carrying whatever it said about why. */
export class DeviceApiError extends Error {
  readonly status: number
  constructor(message: string, status = 0) {
    super(message)
    this.name = 'DeviceApiError'
    this.status = status
  }
}

const browserFetch: FetchLike = (url, init) => globalThis.fetch(url, init)

/**
 * Normalise the device list.
 *
 * The field names are the Go struct's json tags -- `user_agent`, `last_seen` --
 * and they are kept verbatim rather than camel-cased on the way in, so that the
 * contract test can compare these keys to the tags in the Go source and catch a
 * rename instead of quietly rendering `undefined`.
 *
 * A row that is not an object, or has no id, is dropped rather than rendered:
 * the id is what the revoke button sends, and a row whose button cannot work is
 * worse than a row that is not there.
 */
export function parseDevices(body: unknown): DeviceInfo[] {
  if (typeof body !== 'object' || body === null) {
    throw new DeviceApiError('the devices response was not an object')
  }
  const raw = (body as { devices?: unknown }).devices
  if (raw === undefined || raw === null) return []
  if (!Array.isArray(raw)) throw new DeviceApiError('the devices response carried no device list')
  const out: DeviceInfo[] = []
  for (const item of raw) {
    if (typeof item !== 'object' || item === null) continue
    const d = item as Partial<Record<keyof DeviceInfo, unknown>>
    if (typeof d.id !== 'string' || d.id === '') continue
    out.push({
      id: d.id,
      name: typeof d.name === 'string' && d.name !== '' ? d.name : d.id,
      user_agent: typeof d.user_agent === 'string' ? d.user_agent : '',
      created_at: typeof d.created_at === 'string' ? d.created_at : '',
      last_seen: typeof d.last_seen === 'string' ? d.last_seen : '',
      current: d.current === true,
    })
  }
  return out
}

/** Turn a non-2xx into an error carrying the daemon's own sentence. */
async function refuse(res: Response, fallback: string): Promise<DeviceApiError> {
  let message = `${fallback} (${res.status})`
  try {
    const body = (await res.json()) as { error?: unknown }
    if (typeof body?.error === 'string' && body.error !== '') message = body.error
  } catch {
    // `Protect` answers 401 in plain text; the status is all there is to say.
  }
  return new DeviceApiError(message, res.status)
}

/** `GET /api/devices`. */
export async function fetchDevices(
  signal?: AbortSignal,
  fetchImpl: FetchLike = browserFetch,
): Promise<DeviceInfo[]> {
  const res = await fetchImpl(DEVICES_URL, {
    signal,
    headers: { Accept: 'application/json' },
    credentials: 'same-origin',
  })
  if (!res.ok) throw await refuse(res, 'could not list devices')
  return parseDevices(await res.json())
}

/**
 * `POST /api/devices`: mint an enrollment link.
 *
 * No CSRF token: the daemon checks `Origin` against its own exact host, which
 * the browser sets on this request and a page on another host cannot forge.
 */
export async function mintLink(
  name: string,
  fetchImpl: FetchLike = browserFetch,
  now: () => number = Date.now,
): Promise<MintedLink> {
  const res = await fetchImpl(DEVICES_URL, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    credentials: 'same-origin',
    body: JSON.stringify({ name }),
  })
  if (!res.ok) throw await refuse(res, 'could not mint an enrollment link')
  const body = (await res.json()) as { name?: unknown; url?: unknown }
  if (typeof body?.url !== 'string' || body.url === '') {
    throw new DeviceApiError('the daemon minted a link with no url in it')
  }
  return {
    name: typeof body.name === 'string' && body.name !== '' ? body.name : name,
    url: body.url,
    mintedAt: now(),
  }
}

/**
 * `DELETE /api/devices/{id}`: revoke.
 *
 * A 401 is success, not a failure: it is what the daemon answers when the
 * device doing the revoking was the device revoked and the response raced the
 * cookie being cleared. Reporting it as an error would tell the user their own
 * sign-out failed when it is the thing that just worked.
 */
export async function revokeDevice(
  id: string,
  fetchImpl: FetchLike = browserFetch,
  signal?: AbortSignal,
): Promise<void> {
  const res = await fetchImpl(`${DEVICES_URL}/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    headers: { Accept: 'application/json' },
    credentials: 'same-origin',
    signal,
  })
  if (!res.ok && res.status !== 401) throw await refuse(res, 'could not revoke that device')
}

/** The list with one device gone; what a successful revoke leaves behind. */
export function withoutDevice(devices: readonly DeviceInfo[], id: string): DeviceInfo[] {
  return devices.filter((d) => d.id !== id)
}

/**
 * Revoke, then hand back the list without that device.
 *
 * The two halves are one function because their *order* is the rule worth
 * keeping: the row goes only after the daemon says the device is gone. A list
 * pruned optimistically would tell the owner a lost laptop had been cut off
 * while it still held a shell, which is the one lie this dialog must not tell.
 */
export async function revokeAndPrune(
  id: string,
  devices: readonly DeviceInfo[],
  fetchImpl?: FetchLike,
): Promise<DeviceInfo[]> {
  await revokeDevice(id, fetchImpl)
  return withoutDevice(devices, id)
}

/**
 * Whether a timestamp is Go's zero time.
 *
 * `LastSeen` is zero for a device that enrolled and has not made a request
 * since, and "1 Jan 0001" in a list of laptops is a bug report waiting to
 * happen.
 */
export function isZeroTime(iso: string): boolean {
  const t = Date.parse(iso)
  return !iso || Number.isNaN(t) || new Date(t).getUTCFullYear() < 1971
}

/** "3m ago", for the one column that says whether a device is still in use. */
export function formatLastSeen(iso: string, now: number = Date.now()): string {
  if (isZeroTime(iso)) return 'never'
  const diff = now - Date.parse(iso)
  if (diff < 0) return 'just now'
  const minutes = Math.floor(diff / 60_000)
  if (minutes < 1) return 'just now'
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  if (days < 30) return `${days}d ago`
  return new Date(Date.parse(iso)).toLocaleDateString()
}

/** How much of a minted link's ten minutes is left. */
export interface LinkLife {
  remainingMs: number
  expired: boolean
  /** "9:58", or "expired". */
  label: string
}

export function linkLife(mintedAt: number, now: number, ttlMs = ENROLL_TTL_MS): LinkLife {
  const remainingMs = Math.max(0, mintedAt + ttlMs - now)
  if (remainingMs === 0) return { remainingMs, expired: true, label: 'expired' }
  const total = Math.ceil(remainingMs / 1000)
  const mm = Math.floor(total / 60)
  const ss = String(total % 60).padStart(2, '0')
  return { remainingMs, expired: false, label: `${mm}:${ss}` }
}

/** Options the QR is drawn with. Fixed colours: a scanner needs contrast. */
const QR_OPTIONS = {
  margin: 1,
  width: 168,
  errorCorrectionLevel: 'M' as const,
  color: { dark: '#000000', light: '#ffffff' },
}

/**
 * The enrollment link as a data-URI PNG.
 *
 * The encoder is injectable for exactly one reason: the thing worth testing is
 * that what gets encoded is the link and nothing else -- not a name, not a
 * truncation, not the base URL without the fragment -- and no test can read
 * that back out of a PNG.
 */
export async function qrDataUrl(
  url: string,
  encode: (text: string, options: typeof QR_OPTIONS) => Promise<string> = toDataURL,
): Promise<string> {
  return encode(url, QR_OPTIONS)
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}

export interface DevicesDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /**
   * Revoke *this* device. Owned by the user menu, because signing out is the
   * same operation and the interesting half is what happens afterwards.
   * Resolves to the failure to show, or null when the page is on its way out.
   */
  onSignOut: () => Promise<string | null>
  /** Injectable for tests and stories; defaults to the browser's fetch. */
  fetchImpl?: FetchLike
}

export function DevicesDialog({ open, onOpenChange, onSignOut, fetchImpl }: DevicesDialogProps) {
  // The list and the moment it was read, together: "last seen 3m ago" is
  // relative to when the list was fetched, and keeping the two in one state
  // means the clock cannot drift from the rows it describes.
  const [list, setList] = useState<{ devices: DeviceInfo[]; at: number } | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [confirming, setConfirming] = useState<string | null>(null)
  const [minted, setMinted] = useState<MintedLink | null>(null)
  const [qr, setQr] = useState<string | null>(null)
  const [now, setNow] = useState(() => Date.now())

  const load = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const devices = await fetchDevices(signal, fetchImpl)
        setList({ devices, at: Date.now() })
        setError(null)
      } catch (err) {
        if (signal?.aborted) return
        setError(message(err))
      }
    },
    [fetchImpl],
  )

  // Open: read the list fresh, because it changes on another device's schedule.
  // Closed: forget the minted link, which is a live credential, along with
  // everything else that was on screen.
  useEffect(() => {
    if (!open) {
      setMinted(null)
      setQr(null)
      setName('')
      setError(null)
      setConfirming(null)
      return
    }
    const controller = new AbortController()
    void load(controller.signal)
    return () => controller.abort()
  }, [open, load])

  // The countdown, and with it the moment the link is taken off the screen.
  useEffect(() => {
    if (!minted) return
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [minted])

  // The QR is drawn from the link, in an effect because encoding is async.
  useEffect(() => {
    if (!minted) return
    let live = true
    qrDataUrl(minted.url).then(
      (data) => {
        if (live) setQr(data)
      },
      () => {
        // The URL beside it is still copyable, so a failed encode is a missing
        // convenience rather than a broken dialog.
        if (live) setQr(null)
      },
    )
    return () => {
      live = false
    }
  }, [minted])

  // Clamped to the mint, because `now` is only advanced by the interval above:
  // between mounting this dialog and minting a link, hours can pass, and an
  // unclamped subtraction would render "13:07 left" for a ten-minute token
  // until the first tick corrected it.
  const life = minted ? linkLife(minted.mintedAt, Math.max(now, minted.mintedAt)) : null

  /** Drop the link and the image of it together; both are the credential. */
  function clearMinted() {
    setMinted(null)
    setQr(null)
  }

  async function onMint(event: FormEvent) {
    event.preventDefault()
    const wanted = name.trim()
    if (!wanted || busy) return
    setBusy(true)
    setError(null)
    try {
      setMinted(await mintLink(wanted, fetchImpl))
      setQr(null)
      setName('')
    } catch (err) {
      setError(message(err))
    } finally {
      setBusy(false)
    }
  }

  async function onRevoke(device: DeviceInfo) {
    if (device.current) {
      // This dialog is modal, so the answer has to come back into it rather
      // than appear in the footer underneath the overlay.
      setBusy(true)
      try {
        const failure = await onSignOut()
        if (failure) setError(failure)
      } finally {
        setBusy(false)
      }
      return
    }
    setBusy(true)
    setError(null)
    try {
      // The row goes now rather than on a re-fetch: the device is gone, its
      // sockets are closed, and leaving it on screen would invite a second
      // revoke that answers 404.
      const devices = await revokeAndPrune(device.id, list?.devices ?? [], fetchImpl)
      setList((current) => (current ? { ...current, devices } : current))
      setConfirming(null)
    } catch (err) {
      setError(message(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Devices</DialogTitle>
          <DialogDescription>
            Every browser enrolled on this host. Revoking one closes its terminal immediately.
          </DialogDescription>
        </DialogHeader>

        {error && (
          <p
            role="alert"
            className="text-destructive flex items-start gap-1.5 text-xs"
          >
            <TriangleAlert className="mt-px size-3.5 shrink-0" aria-hidden />
            <span className="break-words">{error}</span>
          </p>
        )}

        <ul className="divide-border max-h-64 divide-y overflow-y-auto text-sm">
          {list === null && !error && (
            <li className="text-muted-foreground py-3 text-xs">Loading devices…</li>
          )}
          {list?.devices.length === 0 && (
            <li className="text-muted-foreground py-3 text-xs">
              No devices are enrolled — which cannot be true of the browser reading this, so the
              daemon and this page disagree about something.
            </li>
          )}
          {list?.devices.map((device) => (
            <DeviceRow
              key={device.id}
              device={device}
              now={list.at}
              confirming={confirming === device.id}
              busy={busy}
              onAsk={() => setConfirming(device.id)}
              onCancel={() => setConfirming(null)}
              onRevoke={() => void onRevoke(device)}
            />
          ))}
        </ul>

        {minted && life && !life.expired ? (
          <MintedPanel minted={minted} life={life} qr={qr} onDone={clearMinted} />
        ) : minted ? (
          <div className="rounded-lg border border-dashed p-3 text-xs" role="status">
            <p className="font-medium">That link has expired.</p>
            <p className="text-muted-foreground mt-1">
              Enrollment links last ten minutes. Mint another for {minted.name}.
            </p>
            <Button size="xs" variant="outline" className="mt-2" onClick={clearMinted}>
              Dismiss
            </Button>
          </div>
        ) : (
          <form onSubmit={onMint} className="flex items-end gap-2">
            <label className="min-w-0 flex-1 text-xs">
              <span className="text-muted-foreground">Add a device</span>
              <Input
                className="mt-1"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="phone"
              />
            </label>
            <Button type="submit" size="sm" disabled={busy || name.trim() === ''}>
              {busy ? <Loader2 className="animate-spin" aria-hidden /> : <Plus aria-hidden />}
              Create link
            </Button>
          </form>
        )}

        <DialogFooter>
          <Button variant="outline" size="sm" onClick={() => onOpenChange(false)}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

/**
 * One device.
 *
 * Revoking is two clicks, and the second one says what it will do. Cutting a
 * device off is not undoable -- the browser has to be enrolled again from the
 * host or from another device -- and on the row marked "this device" the button
 * ends the session that is reading it, which is not something a mis-tap on a
 * phone should be able to do.
 */
export function DeviceRow({
  device,
  now,
  confirming,
  busy,
  onAsk,
  onCancel,
  onRevoke,
}: {
  device: DeviceInfo
  /** When the list was read; "last seen" is relative to that. */
  now: number
  confirming: boolean
  busy: boolean
  onAsk: () => void
  onCancel: () => void
  onRevoke: () => void
}) {
  return (
    <li className="flex items-center gap-2 py-2">
      <div className="min-w-0 flex-1">
        <p className="flex items-center gap-1.5">
          <span className="truncate font-medium">{device.name}</span>
          {device.current && (
            <Badge variant="secondary" className="shrink-0">
              this device
            </Badge>
          )}
        </p>
        <p className="text-muted-foreground truncate text-xs" title={device.user_agent}>
          last seen {formatLastSeen(device.last_seen, now)}
          {device.user_agent ? ` · ${device.user_agent}` : ''}
        </p>
      </div>
      {confirming ? (
        <div className="flex shrink-0 items-center gap-1">
          <Button size="xs" variant="destructive" disabled={busy} onClick={onRevoke}>
            {device.current ? 'Sign out' : 'Revoke'}
          </Button>
          <Button size="xs" variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
        </div>
      ) : (
        <Button
          size="xs"
          variant="ghost"
          className="shrink-0"
          disabled={busy}
          onClick={onAsk}
          title={
            device.current
              ? 'Revoke this device: it signs this browser out'
              : `Revoke ${device.name}`
          }
        >
          <Trash2 aria-hidden />
          Revoke
        </Button>
      )}
    </li>
  )
}

/**
 * The link itself: QR, URL, and how long it has left.
 *
 * Split out so the countdown and the credential live in one small component
 * that is unmounted -- not merely hidden -- the moment the link is done with.
 */
export function MintedPanel({
  minted,
  life,
  qr,
  onDone,
}: {
  minted: MintedLink
  life: LinkLife
  qr: string | null
  onDone: () => void
}) {
  const [copied, setCopied] = useState(false)

  async function copy() {
    try {
      await navigator.clipboard.writeText(minted.url)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      // Clipboard access can be refused outright; the input beside the button
      // is selectable, so nothing is lost but the shortcut.
      setCopied(false)
    }
  }

  return (
    <div className="rounded-lg border p-3" role="status">
      <div className="flex gap-3">
        {qr ? (
          // White regardless of theme: a scanner needs the contrast, and a dark
          // mode QR that half the phones refuse is not a style choice.
          <img
            src={qr}
            alt={`Enrollment QR code for ${minted.name}`}
            className="size-[168px] shrink-0 rounded bg-white"
            width={168}
            height={168}
          />
        ) : (
          <div className="bg-muted size-[168px] shrink-0 animate-pulse rounded" aria-hidden />
        )}
        <div className="flex min-w-0 flex-1 flex-col gap-2">
          <p className="text-sm font-medium">Enroll {minted.name}</p>
          <p className="text-muted-foreground text-xs">
            Scan it, or open the link on that device. It works once and expires in{' '}
            <span className="font-mono tabular-nums">{life.label}</span>. Anyone who sees it can
            enroll until then.
          </p>
          <Input readOnly value={minted.url} className="font-mono text-xs" aria-label="Enrollment link" />
          <div className="flex gap-2">
            <Button size="xs" variant="outline" onClick={() => void copy()}>
              {copied ? <Check aria-hidden /> : <Copy aria-hidden />}
              {copied ? 'Copied' : 'Copy link'}
            </Button>
            <Button size="xs" variant="ghost" onClick={onDone}>
              Done
            </Button>
          </div>
        </div>
      </div>
    </div>
  )
}

export default DevicesDialog
