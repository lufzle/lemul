import { Link, createFileRoute, useLoaderData, useRouter } from '@tanstack/react-router'
import { useState } from 'react'

import { getOrgSettings, listOrgMembers, updateOrgSettings } from '#/lib/control-plane'
import { Avatar, Panel } from '#/components/ui'

/**
 * Organization settings, and the roster they govern.
 *
 * GATED ON THE ORG-OWNER ROLE, and gated in two places for two different
 * reasons. Here it decides what to render, which is UX: showing a member a
 * toggle that will 403 is a worse answer than a sentence. The real enforcement
 * is the control plane's, which refuses the PATCH from anyone but an owner —
 * a route guard in the console protects nothing, because the server functions
 * behind it are RPC endpoints reachable by direct POST whatever the router is
 * showing.
 *
 * The settings themselves live in the CELL database rather than the directory:
 * the directory exists only to route a principal to a cell, and product policy
 * in the global tier would be policy every cell has to agree about.
 */
export const Route = createFileRoute('/_authenticated/orgs/$org/settings')({
  loader: async ({ params }) => ({
    org: params.org,
    settings: await getOrgSettings({ data: { org: params.org } }),
    members: await listOrgMembers({ data: { org: params.org } }),
  }),
  component: Settings,
  errorComponent: ({ error }) => (
    <Panel title="settings unavailable">
      <p className="text-sm text-red-300">{error.message}</p>
    </Panel>
  ),
})

function Settings() {
  const { org, settings, members } = Route.useLoaderData()
  // Where a client learns its own role: an access token carries a subject and
  // no claims about what it may do, so the organization list is the answer --
  // and the layout above already holds it.
  const { orgs } = useLoaderData({ from: '/_authenticated' })
  const router = useRouter()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const here = orgs.find((o) => o.slug === org)
  const isOwner = here?.role === 'owner'

  const toggle = async (value: boolean) => {
    setBusy(true)
    setError(null)
    try {
      await updateOrgSettings({ data: { org, members_can_create_workspaces: value } })
      await router.invalidate()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="space-y-4">
      <Panel
        title={`${here?.name ?? org} — settings`}
        actions={
          <Link
            to="/orgs/$org"
            params={{ org }}
            className="text-xs text-neutral-400 hover:text-neutral-200 hover:underline"
          >
            back to workspaces
          </Link>
        }
      >
        {error ? <p className="mb-3 text-sm text-red-300">{error}</p> : null}
        {!isOwner ? (
          <p className="mb-4 rounded border border-neutral-800 bg-neutral-900/60 px-3 py-2 text-sm text-neutral-400">
            You are a member of this organization, so these are read-only. An owner can change them.
          </p>
        ) : null}

        <label className="flex items-start gap-3">
          <input
            type="checkbox"
            className="mt-1"
            checked={settings.members_can_create_workspaces}
            disabled={busy || !isOwner}
            onChange={(e) => void toggle(e.target.checked)}
          />
          <span>
            <span className="block text-sm text-neutral-200">Members can create workspaces</span>
            <span className="mt-0.5 block text-sm text-neutral-500">
              Off by default, and the default is the point: a workspace starts a task somebody pays
              for the moment it is created, so “everyone may start one” is a cost decision that
              belongs to this organization rather than a choice we make for it. Owners are never
              gated by this.
            </span>
          </span>
        </label>
      </Panel>

      <Panel title={`people (${members.length})`}>
        <p className="mb-3 text-sm text-neutral-500">
          Everyone in this organization. Who can reach a particular workspace is a separate
          question — that is its access scope, and its members tab.
        </p>
        <table className="w-full text-left text-sm">
          <thead className="text-xs uppercase tracking-wide text-neutral-500">
            <tr>
              <th className="px-2 py-2 font-medium">who</th>
              <th className="px-2 py-2 font-medium">role</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-neutral-800">
            {members.map((m) => {
              const label = m.email ?? m.user_id
              return (
                <tr key={m.user_id} className="hover:bg-neutral-900/60">
                  <td className="px-2 py-2">
                    <span className="inline-flex items-center gap-2">
                      <Avatar name={label} size="sm" />
                      {label}
                      {m.me ? <span className="text-xs text-neutral-500">you</span> : null}
                    </span>
                  </td>
                  <td className="px-2 py-2 text-neutral-400">{m.role}</td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </Panel>
    </div>
  )
}
