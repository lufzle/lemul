import { useEffect, useRef, useState } from 'react'
import { getViewerEndpoint } from '#/lib/control-plane'

type State = 'connecting' | 'live' | 'ended' | 'dropped' | 'error'

/**
 * Read-only terminal for one session.
 *
 * Read-only is enforced in three places, and this component is the weakest of
 * them. The relay drops input frames from a viewer connection and the supervisor
 * drops them again on arrival (§2.5), because both attachers write the same PTY
 * stdin and a viewer that can write is silently a co-driver. Turning off stdin
 * here is only so the cursor does not invite typing -- never rely on it.
 *
 * Everything is loaded and constructed inside an effect: xterm touches `document`
 * at import time, so a top-level import would break the SSR render of the page
 * that contains it.
 */
export function Viewer({
  org,
  sessionId,
  rows,
  cols,
}: {
  /** Endpoint negotiation is organization-scoped, like every other read. */
  org: string
  sessionId: string
  rows: number
  cols: number
}) {
  const host = useRef<HTMLDivElement | null>(null)
  const [state, setState] = useState<State>('connecting')
  const [detail, setDetail] = useState<string | null>(null)

  useEffect(() => {
    let disposed = false
    let ws: WebSocket | null = null
    let term: { write: (d: Uint8Array) => void; dispose: () => void } | null = null

    void (async () => {
      const [{ Terminal }] = await Promise.all([
        import('@xterm/xterm'),
        import('@xterm/xterm/css/xterm.css'),
      ])
      if (disposed || !host.current) return

      // Sized to the PTY, not to the window. A viewer must not resize the
      // session -- the PTY has one size and changing it reflows the
      // CONTROLLER's Claude Code -- so the terminal is built at whatever
      // geometry the session already has and the page scrolls if that does not
      // fit. The server enforces this too: the relay ignores resize control
      // messages from a viewer connection.
      const t = new Terminal({
        rows,
        cols,
        disableStdin: true,
        cursorBlink: false,
        convertEol: false,
        fontFamily:
          'ui-monospace, SFMono-Regular, Menlo, Monaco, "Cascadia Mono", monospace',
        fontSize: 13,
        theme: { background: '#0a0a0a', foreground: '#e5e5e5' },
      })
      t.open(host.current)
      term = t as unknown as typeof term

      let ep: Awaited<ReturnType<typeof getViewerEndpoint>>
      try {
        ep = await getViewerEndpoint({ data: { org, sid: sessionId } })
      } catch (e) {
        if (!disposed) {
          setState('error')
          setDetail(e instanceof Error ? e.message : String(e))
        }
        return
      }
      if (disposed) return
      if (ep.transport !== 'relay') {
        setState('error')
        setDetail(`unsupported transport ${ep.transport}`)
        return
      }

      if (ep.mode && ep.mode !== 'viewer') {
        setState('error')
        setDetail(`refusing to attach: the credential authorises ${ep.mode}`)
        return
      }

      const url = new URL(ep.address)
      url.searchParams.set('credential', ep.credential)
      // No `mode` here: it is inside the signed credential, so the relay
      // enforces what was authorised rather than what this page claims to be.
      // Reported for completeness; the relay drops a viewer's size, and the
      // supervisor no longer lets a viewer's attach resize the PTY either.
      url.searchParams.set('rows', String(rows))
      url.searchParams.set('cols', String(cols))

      const sock = new WebSocket(url.toString())
      ws = sock
      // Binary frames are PTY bytes and must stay bytes. Terminal output is not
      // valid UTF-8 at arbitrary chunk boundaries -- an escape sequence or a
      // multi-byte rune straddles frames -- so decoding to a string here would
      // corrupt exactly the sequences that make the TUI render.
      sock.binaryType = 'arraybuffer'

      sock.onopen = () => !disposed && setState('live')
      sock.onmessage = (ev) => {
        if (typeof ev.data === 'string') return // control frame; nothing to draw
        t.write(new Uint8Array(ev.data as ArrayBuffer))
      }
      sock.onclose = (ev) => {
        if (disposed) return
        // The close CODE is the contract: a normal close means the child exited
        // and there is nothing to come back to, anything else means the
        // connection dropped while the session kept running (§2.8).
        setState(ev.code === 1000 || ev.code === 1001 ? 'ended' : 'dropped')
      }
      sock.onerror = () => !disposed && setState('dropped')
    })()

    return () => {
      disposed = true
      ws?.close()
      term?.dispose()
    }
  }, [sessionId, rows, cols])

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-3 text-xs">
        <Badge state={state} />
        <span className="text-neutral-500">
          read-only · {cols}×{rows} · input is dropped by the relay
        </span>
      </div>
      {detail ? <p className="text-xs text-red-300">{detail}</p> : null}
      <div
        ref={host}
        className="overflow-auto rounded border border-neutral-800 bg-[#0a0a0a] p-2"
      />
      {state === 'dropped' ? (
        <p className="text-xs text-amber-400">
          Connection dropped. The session is unaffected — reload to reconnect.
        </p>
      ) : null}
      {state === 'ended' ? (
        <p className="text-xs text-neutral-400">
          Session ended. Its conversation is still on the workspace volume, so it can be
          resumed.
        </p>
      ) : null}
    </div>
  )
}

function Badge({ state }: { state: State }) {
  const tone =
    state === 'live'
      ? 'border-emerald-800 text-emerald-300'
      : state === 'connecting'
        ? 'border-neutral-700 text-neutral-400'
        : state === 'ended'
          ? 'border-neutral-700 text-neutral-400'
          : 'border-amber-800 text-amber-300'
  return <span className={`rounded border px-1.5 py-0.5 ${tone}`}>{state}</span>
}
