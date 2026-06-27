import { createFileRoute } from '@tanstack/react-router'

import { getResources } from '#/lib/control-plane'
import { useAutoRefresh } from '#/components/ui'
import { Resources } from '#/components/Resources'

/**
 * Overview: what this workspace is consuming right now.
 *
 * The index route of the workspace shell, so /workspaces/{id} lands here.
 */
export const Route = createFileRoute('/_authenticated/orgs/$org/workspaces/$wid/')({
  loader: async ({ params }) => ({
    // Tolerated rather than thrown: a workspace with no running task answers
    // 409, which is a state and not a failure, and the tab should say so rather
    // than render an error page.
    resources: await getResources({ data: { org: params.org, wid: params.wid } }).catch((e: Error) => e.message),
  }),
  component: Overview,
})

function Overview() {
  const { resources } = Route.useLoaderData()
  useAutoRefresh()
  return <Resources data={resources} />
}
