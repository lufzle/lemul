import { createFileRoute, redirect } from '@tanstack/react-router'
import { getSession } from '#/lib/session'

/**
 * The sign-in boundary. Pathless, so every route beneath it keeps its URL.
 *
 * This is route UX, not the security boundary. The actual protection is that a
 * server function cannot obtain an access token without a session, and the Go
 * control plane validates that token independently — see lib/control-plane.ts.
 * A `beforeLoad` guard would be worth nothing on its own, because server
 * functions are RPC endpoints reachable by direct POST whatever the router is
 * showing.
 *
 * Its real job is to stop an unauthenticated visitor seeing an empty console
 * full of failed requests, and to send them somewhere useful instead.
 */
export const Route = createFileRoute('/_authenticated')({
  beforeLoad: async () => {
    const session = await getSession()
    if (!session.authenticated) {
      // `href`, not `to`: the sign-in route is a server route rather than a
      // page in the route tree, so it has no typed path to navigate to.
      throw redirect({ href: '/api/auth/sign-in' })
    }
    return { session }
  },
})
