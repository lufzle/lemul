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
