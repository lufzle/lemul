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
  title?: string
  actions?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <section className="rounded-lg border border-neutral-800 bg-neutral-950">
      {title || actions ? (
        <header className="flex items-center justify-between gap-4 border-b border-neutral-800 px-4 py-2.5">
          {title ? (
            <h2 className="text-sm font-medium tracking-wide text-neutral-300">{title}</h2>
          ) : (
            <span />
          )}
          {actions}
        </header>
      ) : null}
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
  className = 'text-[#b3a55c]',
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
export function bytes(n: number, digits = 1): string {
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
  return `${digits === 0 ? Math.round(v) : trim1(v)} ${units[i]}`
}

/** One decimal, dropped when it is zero: 1.5 stays, 1.0 becomes 1. */
export function trim1(v: number): string {
  return v.toFixed(1).replace(/\.0$/, '')
}

/**
 * A timestamp as "5 minutes ago", relative to a `now` the CALLER supplies.
 *
 * The anchor is a parameter rather than Date.now() for two reasons, and the
 * first one bites immediately. These pages render on the server and then
 * hydrate in the browser: computing the elapsed time separately in each place
 * gives two different answers a second or so apart, which React reports as a
 * hydration mismatch and which can visibly flip "just now" to "1 minute ago" on
 * load. Passing one value through the loader makes both renders agree.
 *
 * The second is clock skew. `now` from the loader and the timestamp from the
 * control plane are both server-side, so the subtraction never involves the
 * viewer's clock -- a laptop several minutes out cannot make a session look
 * like it started in the future.
 *
 * Freshness comes from useAutoRefresh re-running the loader, which is where the
 * clock effectively ticks.
 */
export function relativeTime(iso: string, nowMs: number): string {
  const then = Date.parse(iso)
  if (Number.isNaN(then)) return iso

  const secs = Math.round((nowMs - then) / 1000)
  if (secs < 0) return 'just now' // clock skew between our own components
  if (secs < 45) return 'just now'

  const units: Array<[number, string]> = [
    [60, 'second'],
    [60, 'minute'],
    [24, 'hour'],
    [7, 'day'],
    [4.35, 'week'],
    [12, 'month'],
    [Number.POSITIVE_INFINITY, 'year'],
  ]
  let value = secs
  for (let i = 0; i < units.length; i++) {
    const [size, name] = units[i]
    if (value < size || i === units.length - 1) {
      const n = Math.round(value)
      return `${n} ${name}${n === 1 ? '' : 's'} ago`
    }
    value /= size
  }
  return iso
}

/**
 * A capacity bar: how much of the ceiling is in use.
 *
 * It exists because the percentages beside it do not all share a ceiling. CPU
 * usage is processor-time-over-wall-time, so it runs past 100% on a multi-core
 * task; this bar is the same figure against the vCPU count, which is what makes
 * "400%" legible as "four of ten cores" at a glance.
 *
 * The width is an inline style, not a class. Tailwind scans source files as
 * plain text, so an interpolated `w-[${pct}%]` is never generated -- it fails
 * silently and the bar simply never fills.
 */
export function Bar({ percent }: { percent: number }) {
  const pct = Math.max(0, Math.min(100, percent))
  // Static class names per band. Tailwind never sees an interpolated one.
  const fill = pct > 75 ? 'bg-red-500' : pct > 50 ? 'bg-yellow-500' : 'bg-emerald-500'
  return (
    <div className="h-1.5 w-full overflow-hidden rounded-full bg-neutral-800" role="presentation">
      <div className={`h-full rounded-full ${fill}`} style={{ width: `${pct}%` }} />
    </div>
  )
}

/**
 * Two series on one set of axes, sharing a scale so the lines are comparable.
 *
 * Receive and transmit are drawn together because the interesting shape is
 * usually one direction moving while the other does not -- an agent pulling a
 * package, or pushing a large diff. A single combined series hides exactly that.
 */
export function DualLine({
  a,
  b,
  aClass = 'text-cyan-400',
  bClass = 'text-fuchsia-400',
  width = 160,
  height = 32,
}: {
  a: Array<number>
  b: Array<number>
  aClass?: string
  bClass?: string
  width?: number
  height?: number
}) {
  const n = Math.max(a.length, b.length)
  if (n < 2) return <div className="text-xs text-neutral-600">collecting…</div>

  // One maximum across both, or the quieter direction would be scaled up to
  // look as busy as the louder one.
  const max = Math.max(...a, ...b, Number.EPSILON)
  const step = width / (n - 1)
  const path = (d: Array<number>) =>
    d.map((v, i) => `${(i * step).toFixed(1)},${(height - (v / max) * height).toFixed(1)}`).join(' ')

  return (
    <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none" aria-hidden>
      <polyline points={path(a)} fill="none" stroke="currentColor" strokeWidth="1.5" className={aClass} />
      <polyline points={path(b)} fill="none" stroke="currentColor" strokeWidth="1.5" className={bClass} />
    </svg>
  )
}

/**
 * A stacked bar per sample: the coloured part is what is in use, the grey above
 * it is the headroom left.
 *
 * Every bar is the same full height because the total is the same, so the grey
 * makes the ceiling visible at every point in time -- a line chart of the same
 * data tells you the shape but not how close to the top it is.
 */
export function StackedBars({
  data,
  total,
  width = 160,
  height = 32,
}: {
  data: Array<number>
  total: number
  width?: number
  height?: number
}) {
  if (data.length < 2 || total <= 0) {
    return <div className="text-xs text-neutral-600">collecting…</div>
  }
  const slot = width / data.length
  const barW = Math.max(1, slot - 1)

  return (
    <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none" aria-hidden>
      {data.map((v, i) => {
        const used = Math.max(0, Math.min(height, (v / total) * height))
        return (
          <g key={i}>
            <rect x={i * slot} y={0} width={barW} height={height - used} className="fill-neutral-800" />
            <rect x={i * slot} y={height - used} width={barW} height={used} className="fill-emerald-500" />
          </g>
        )
      })}
    </svg>
  )
}

/**
 * A round badge carrying a person's initials, standing in for a picture.
 *
 * People only. A workspace is not a person, and giving one an avatar makes the
 * table read as though it were.
 *
 * Deterministic colour from the name, so the same workspace or person keeps the
 * same badge between renders and between machines -- a random or index-based
 * colour would reshuffle on every sort and stop being recognisable, which is
 * the only thing a badge like this is for.
 *
 * The palette is a literal array because Tailwind scans source as text: a class
 * assembled from a variable is never generated.
 */
const badgeTones = [
  'bg-sky-500/15 text-sky-300',
  'bg-emerald-500/15 text-emerald-300',
  'bg-amber-500/15 text-amber-300',
  'bg-fuchsia-500/15 text-fuchsia-300',
  'bg-cyan-500/15 text-cyan-300',
  'bg-violet-500/15 text-violet-300',
]

export function Avatar({ name, size = 'md' }: { name: string; size?: 'sm' | 'md' }) {
  const tone = badgeTones[hash(name) % badgeTones.length]
  const box = size === 'sm' ? 'size-5 text-[10px]' : 'size-6 text-[11px]'
  return (
    <span
      aria-hidden
      title={name}
      className={`inline-flex ${box} shrink-0 items-center justify-center rounded-full font-medium ${tone}`}
    >
      {initials(name)}
    </span>
  )
}

/**
 * Up to two initials. Splits on the separators these names actually use --
 * spaces, hyphens, underscores, dots -- and falls back to the first two
 * characters of a single unbroken word, since "DE" reads better than "D" for
 * `demo`. An email is reduced to its local part first, because everyone's
 * initial would otherwise be the domain's.
 */
function initials(name: string): string {
  const base = (name.split('@')[0] || name).trim()
  if (!base) return '?'
  const parts = base.split(/[\s\-_.]+/).filter(Boolean)
  if (parts.length >= 2) return (parts[0][0] + parts[1][0]).toUpperCase()
  return base.slice(0, 2).toUpperCase()
}

/** FNV-1a, for a stable colour per name. */
function hash(s: string): number {
  let h = 2166136261
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i)
    h = Math.imul(h, 16777619)
  }
  return Math.abs(h)
}
