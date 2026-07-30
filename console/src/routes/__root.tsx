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
        {/* The warning is still earned even with sign-in working. Authentication
            is not authorisation: there are no scopes, no per-user scoping, and
            §2.5's owner-scoped attach is still outstanding, so anyone who can
            sign in can reach every workspace. Loopback is what bounds that. */}
        <span className="text-amber-500/80">
          {authOn
            ? 'no authorisation · localhost only · do not expose'
            : 'NO AUTH · localhost only · do not expose'}
        </span>
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
