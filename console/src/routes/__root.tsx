import { HeadContent, Scripts, createRootRoute } from '@tanstack/react-router'
import { authConfigured, getSession } from '#/lib/session'

import appCss from '../styles.css?url'

export const Route = createRootRoute({
  // The root loader, so the header can name who is signed in on every page --
  // including the ones outside the _authenticated boundary.
  loader: async () => ({
    session: await getSession(),
    authOn: await authConfigured(),
  }),
  head: () => ({
    meta: [
      { charSet: 'utf-8' },
      { name: 'viewport', content: 'width=device-width, initial-scale=1' },
      { title: 'lemul console' },
    ],
    links: [{ rel: 'stylesheet', href: appCss }],
  }),
  shellComponent: RootDocument,
})

function RootDocument({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" className="dark">
      <head>
        <HeadContent />
      </head>
      <body className="bg-neutral-900 text-neutral-200 antialiased">
        <div className="mx-auto max-w-6xl p-6">
          <Header />
          {children}
        </div>
        <Scripts />
      </body>
    </html>
  )
}

function Header() {
  const { session, authOn } = Route.useLoaderData()
  return (
    <header className="mb-6 flex flex-wrap items-baseline justify-between gap-3 border-b border-neutral-800 pb-3">
      <a href="/" className="text-sm font-medium tracking-wide text-neutral-300">
        lemul <span className="text-neutral-600">console</span>
      </a>
      <div className="flex items-baseline gap-4 text-xs">
        {/* It used to say "no authorisation", on the grounds that anyone who
            could sign in could reach every workspace. That stopped being true
            and the banner did not notice: §2.5's owner-scoped attach closed in
            Phase 4, a caller with no standing in a workspace gets a 404 rather
            than a listing, and Phase 6 added access_scope on top —
            TestAnOrganizationMemberCannotReachAWorkspaceTheyAreNotIn and the
            access-scope suite are what say so. A warning that overstates is
            still a warning that is wrong, and this one is on every page.

            What IS still true is the rest of it: no CSRF answer on the attach
            WebSocket (internal/relay accepts every origin), and the explorer
            reads command lines.

            "No TLS" stopped being true on 2026-08-03, when deploy/ put the
            console behind Caddy with a real certificate — so the banner is now
            shown only when sign-in is OFF, which is the one case where it is
            unambiguously right and unambiguously serious. Leaving it on a
            deployment that HAS TLS and HAS authorisation was the same mistake
            the paragraph above describes: a warning that overstates is a
            warning nobody reads, and this one was on every page of a host that
            was deliberately exposed. What remains true is recorded in §13
            rather than shouted on every render — the relay's missing origin
            check is defence in depth there, not the boundary, because attach
            takes a short-TTL credential from the control plane rather than a
            cookie a foreign page could ride. */}
        {!authOn ? (
          <span className="text-amber-500/80">NO SIGN-IN · localhost only · do not expose</span>
        ) : null}
        {authOn && session.authenticated ? (
          <>
            <span className="text-neutral-500">{session.email ?? session.subject}</span>
            <a href="/api/auth/sign-out" className="text-neutral-400 hover:text-neutral-200">
              sign out
            </a>
          </>
        ) : null}
      </div>
    </header>
  )
}
