import { Outlet, createFileRoute, redirect, useParams } from '@tanstack/react-router'
import { OrgSwitcher } from '#/components/OrgSwitcher'
import { listOrgs } from '#/lib/control-plane'
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
 *
 * It also loads the organization list, because the header's switcher needs it
 * on every page beneath here and it is the same answer for all of them.
 */
export const Route = createFileRoute('/_authenticated')({
  beforeLoad: async () => {
    const session = await getSession()
    if (!session.authenticated) {
      // `href`, not `to`: the sign-in route is a server route rather than a
      // page in the route tree, so it has no typed path to navigate to.
      throw redirect({ href: '/api/auth/sign-in' })
    }
    // Before the loader, not after: the identity relay is also what NAMES the
    // caller's own organization, and the switcher below would otherwise show a
    // slug where a name belongs until the next navigation.
    await rememberIdentity(session)
    return { session }
  },
  loader: async () => ({ orgs: await listOrgs() }),
  component: Authenticated,
})

/**
 * The organization bar, above every authenticated page.
 *
 * It lives here rather than in the root header because the root route sits
 * outside this boundary and cannot read a loader that only runs for signed-in
 * users -- and an organization switcher on a sign-in page would be showing
 * something nobody has yet.
 */
function Authenticated() {
  const { orgs } = Route.useLoaderData()
  // strict: false because this renders above routes that have no {org} segment
  // -- the redirect at `/` among them -- and an org-less page is a real state
  // rather than an error.
  const params = useParams({ strict: false }) as { org?: string }
  return (
    <>
      <div className="mb-4 flex justify-end text-xs">
        <OrgSwitcher orgs={orgs} current={params.org} />
      </div>
      <Outlet />
    </>
  )
}

/**
 * Relays the signed-in user's ID token to the control plane, once per subject
 * per console process.
 *
 * Here rather than only in the sign-in callback so that an EXISTING session
 * gets one too -- otherwise everyone already signed in stays an opaque subject,
 * in an organization named after its own slug, until they happen to sign out
 * and back in. The guard keeps it to one call per person rather than one per
 * navigation; losing the set on restart just means one redundant, idempotent
 * write.
 *
 * The token is fetched inside the server function rather than passed in, so
 * nothing about it crosses into the client bundle.
 */
const registered = new Set<string>()

async function rememberIdentity(session: { subject?: string }) {
  const { subject } = session
  if (!subject || registered.has(subject)) return
  registered.add(subject)
  try {
    const { registerIdentity } = await import('#/lib/control-plane')
    await registerIdentity()
  } catch {
    // A name is a nicety. If the control plane is unreachable the console must
    // still render, so this never blocks the route.
    registered.delete(subject)
  }
}
