import { useEffect, useState } from 'react'
import { useRouter } from '@tanstack/react-router'

/**
 * Re-runs the current route's loaders on an interval.
 *
 * The console shows live state -- which tasks are connected, which PTYs exist --
 * and all of it is derived by the control plane on demand, so the honest way to
 * keep it fresh is to ask again. Router invalidation rather than client-side
 * polling of the API keeps every fetch inside a server function (see
 * lib/control-plane.ts) and keeps one code path for first render and refresh.
 *
 * Paused while the tab is hidden: an operator leaving the console open in a
 * background tab should not have it asking the supervisor to enumerate PTYs
 * every few seconds forever.
 */
export function useAutoRefresh(ms = 4000) {
  const router = useRouter()
  const [paused, setPaused] = useState(false)

  useEffect(() => {
    const onVisibility = () => setPaused(document.hidden)
    onVisibility()
    document.addEventListener('visibilitychange', onVisibility)
    return () => document.removeEventListener('visibilitychange', onVisibility)
  }, [])

  useEffect(() => {
    if (paused) return
    const id = setInterval(() => void router.invalidate(), ms)
    return () => clearInterval(id)
  }, [router, ms, paused])
}

export function Panel({
  title,
  actions,
  children,
}: {
  title: string
  actions?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <section className="rounded-lg border border-neutral-800 bg-neutral-950">
      <header className="flex items-center justify-between gap-4 border-b border-neutral-800 px-4 py-2.5">
        <h2 className="text-sm font-medium tracking-wide text-neutral-300">{title}</h2>
        {actions}
      </header>
      <div className="overflow-x-auto p-4">{children}</div>
    </section>
  )
}

export function Dot({ on }: { on: boolean }) {
  return (
    <span
      aria-hidden
      className={`inline-block size-2 rounded-full ${on ? 'bg-emerald-400' : 'bg-neutral-600'}`}
    />
  )
}

export function Button({
  children,
  onClick,
  busy,
  tone = 'normal',
  title,
}: {
  children: React.ReactNode
  onClick: () => void
  busy?: boolean
  tone?: 'normal' | 'danger'
  title?: string
}) {
  return (
    <button
      type="button"
      title={title}
      disabled={busy}
      onClick={onClick}
      className={`rounded border px-2 py-1 text-xs transition disabled:opacity-40 ${
        tone === 'danger'
          ? 'border-red-900 text-red-300 hover:bg-red-950'
          : 'border-neutral-700 text-neutral-200 hover:bg-neutral-800'
      }`}
    >
      {busy ? '…' : children}
    </button>
  )
}

/**
 * Table cells, hoisted here once the explorer added two more tables. They were
 * defined locally in the overview route and duplicated inline in the workspace
 * route.
 */
export function Th({ children }: { children: React.ReactNode }) {
  return <th className="px-2 py-2 font-medium">{children}</th>
}

export function Td({
  children,
  className = '',
}: {
  children: React.ReactNode
  className?: string
}) {
  return <td className={`px-2 py-2 align-top ${className}`}>{children}</td>
}

/**
 * A sparkline, hand-rolled rather than pulled from a charting library.
 *
 * Three of these do not justify a dependency, and the alternative is a package
 * whose own styling would have to be fought into a console that has no design
 * tokens at all. An SVG polyline is the whole implementation.
 *
 * Scaling is to the series maximum, with a floor, so a flat-zero series renders
 * as a flat line at the bottom rather than dividing by zero or drawing noise
 * amplified from nothing.
 */
export function Sparkline({
  data,
  width = 160,
  height = 32,
  className = 'text-sky-400',
}: {
  data: Array<number>
  width?: number
  height?: number
  className?: string
}) {
  if (data.length < 2) {
    return <div className="text-xs text-neutral-600">collecting…</div>
  }
  const max = Math.max(...data, Number.EPSILON)
  const step = width / (data.length - 1)
  const points = data
    .map((v, i) => `${(i * step).toFixed(1)},${(height - (v / max) * height).toFixed(1)}`)
    .join(' ')

  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      className={className}
      preserveAspectRatio="none"
      aria-hidden
    >
      <polyline points={points} fill="none" stroke="currentColor" strokeWidth="1.5" />
    </svg>
  )
}

/** Bytes as a human figure. Sizes in a file listing are unreadable otherwise. */
export function bytes(n: number): string {
  // Rounded, because these are also fed rates: an unrounded 131.86341060475917
  // B/s rendered exactly like that in the network panel the first time it saw
  // real traffic.
  if (n < 1024) return `${Math.round(n)} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let v = n / 1024
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${units[i]}`
}
