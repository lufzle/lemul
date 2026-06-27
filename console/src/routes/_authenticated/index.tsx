import { createFileRoute, redirect } from '@tanstack/react-router'
import { listOrgs } from '#/lib/control-plane'

/**
 * `/` is a redirect, not a page.
 *
 * There is nothing to show that is not scoped to an organization, and the
 * control plane deliberately will not pick one -- every organization-scoped
 * route names it in the path. So the choice is made here, where it is visible
 * and where the URL that results is bookmarkable and shareable.
 *
 * The user's own organization is the default. It is the one they always have
 * and cannot leave, so it is the only choice that is right for every account.
 */
export const Route = createFileRoute('/_authenticated/')({
  loader: async () => {
    const orgs = await listOrgs()
    if (orgs.length === 0) {
      // Not reachable through a normal sign-in: everyone gets their own
      // organization on their first request. If it happens, saying so beats
      // redirecting to a URL with an empty segment.
      return { empty: true as const }
    }
    const home = orgs.find((o) => o.personal) ?? orgs[0]
    throw redirect({ to: '/orgs/$org', params: { org: home.slug } })
  },
  component: () => (
    <p className="text-sm text-neutral-400">
      You are not a member of any organization.
    </p>
  ),
})
