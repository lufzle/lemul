import { Link, Outlet, createFileRoute } from '@tanstack/react-router'

import { getFlags } from '#/lib/flags'
import { Panel } from '#/components/ui'

/**
 * The workspace shell: name, tabs, and whichever tab is showing.
 *
 * This route became a LAYOUT the moment it gained children, which is why its
 * former body now lives in workspaces.$wid.index.tsx. Without that split the
 * child renders in place of the parent page and the tabs vanish with it -- the
 * same trap the session viewer avoids from the other direction, by opting out
 * of nesting with a trailing underscore.
 */
export const Route = createFileRoute('/_authenticated/orgs/$org/workspaces/$wid')({
  loader: async ({ params }) => ({ org: params.org, wid: params.wid, flags: await getFlags() }),
  component: WorkspaceShell,
  errorComponent: ({ error }) => (
    <Panel title="workspace unavailable">
      <p className="text-sm text-red-300">{error.message}</p>
    </Panel>
  ),
})

function WorkspaceShell() {
  const { org, wid, flags } = Route.useLoaderData()

  return (
    <div className="space-y-4">
      <div>
        <Link to="/orgs/$org" params={{ org }} className="text-sm text-neutral-500 hover:text-neutral-300">
          ← workspaces
        </Link>
        <h1 className="mt-1 font-mono text-lg text-neutral-100">{wid}</h1>
      </div>

      <nav className="flex gap-1 border-b border-neutral-800">
        {/* Overview matches exactly. Its path is a prefix of every other tab's,
            so without `exact` the router marks it active on all of them and two
            tabs light up at once. */}
        <Tab to="/orgs/$org/workspaces/$wid" org={org} wid={wid} exact>
          overview
        </Tab>
        <Tab to="/orgs/$org/workspaces/$wid/sessions" org={org} wid={wid}>
          sessions
        </Tab>
        <Tab to="/orgs/$org/workspaces/$wid/members" org={org} wid={wid}>
          members
        </Tab>
        {/* Filesystem and processes stay behind the flag: §2.5's owner-scoped
            attach is outstanding, and those two read file paths and command
            lines across every workspace. Resources and sessions do not. */}
        {flags.explorer ? (
          <>
            <Tab to="/orgs/$org/workspaces/$wid/filesystem" org={org} wid={wid}>
              filesystem
            </Tab>
            <Tab to="/orgs/$org/workspaces/$wid/processes" org={org} wid={wid}>
              processes
            </Tab>
          </>
        ) : null}
      </nav>

      <Outlet />
    </div>
  )
}

function Tab({
  to,
  org,
  wid,
  exact = false,
  children,
}: {
  to:
    | '/orgs/$org/workspaces/$wid'
    | '/orgs/$org/workspaces/$wid/sessions'
    | '/orgs/$org/workspaces/$wid/members'
    | '/orgs/$org/workspaces/$wid/filesystem'
    | '/orgs/$org/workspaces/$wid/processes'
  org: string
  wid: string
  exact?: boolean
  children: React.ReactNode
}) {
  return (
    <Link
      to={to}
      params={{ org, wid }}
      activeOptions={{ exact }}
      className="-mb-px border-b-2 px-3 py-2 text-sm transition"
      activeProps={{ className: 'border-neutral-300 text-neutral-100' }}
      inactiveProps={{
        className: 'border-transparent text-neutral-500 hover:text-neutral-300',
      }}
    >
      {children}
    </Link>
  )
}
