import { Link } from '@tanstack/react-router'
import type { Org } from '#/lib/control-plane'

/**
 * Which organization the console is acting in, and how to change it.
 *
 * A plain `<details>` rather than a headless menu library: it is a list of
 * links, it works before hydration, and it closes on Escape and on click-away
 * without any of that being written here.
 *
 * The current organization is read from the URL rather than held in state.
 * That is the whole reason the org is a path segment -- a switcher backed by
 * client state can disagree with the page it is sitting on, and the failure
 * mode of that disagreement is acting on the wrong customer's workspace. Here
 * it cannot: switching IS navigating.
 */
export function OrgSwitcher({ orgs, current }: { orgs: Array<Org>; current?: string }) {
  if (orgs.length === 0) return null
  const active = orgs.find((o) => o.slug === current)

  // Nothing to switch to. Still named, because "which organization am I looking
  // at" is worth answering even when there is only one answer.
  if (orgs.length === 1) {
    return (
      <span className="text-neutral-400" title={orgs[0].slug}>
        {orgs[0].name}
      </span>
    )
  }

  return (
    <details className="relative">
      <summary className="cursor-pointer list-none text-neutral-400 hover:text-neutral-200">
        {active?.name ?? 'choose an organization'} <span className="text-neutral-600">▾</span>
      </summary>
      <ul className="absolute right-0 z-10 mt-1 min-w-56 rounded border border-neutral-800 bg-neutral-900 py-1 shadow-lg">
        {orgs.map((o) => (
          <li key={o.slug}>
            <Link
              to="/orgs/$org"
              params={{ org: o.slug }}
              className="block px-3 py-1.5 hover:bg-neutral-800"
              activeProps={{ className: 'block px-3 py-1.5 bg-neutral-800 text-neutral-100' }}
            >
              <span className="block">{o.name}</span>
              <span className="block text-neutral-600">
                {o.slug} · {o.role}
                {o.personal ? ' · your own' : ''}
              </span>
            </Link>
          </li>
        ))}
      </ul>
    </details>
  )
}
