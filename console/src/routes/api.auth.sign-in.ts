import { createFileRoute } from '@tanstack/react-router'

/**
 * Starts the sign-in flow.
 *
 * A server route rather than a server function because the browser has to be
 * *navigated* here — an RPC cannot hand the user to the identity provider, and
 * the callback has to arrive as a plain GET that Logto can redirect to.
 */
export const Route = createFileRoute('/api/auth/sign-in')({
  server: {
    handlers: {
      GET: async ({ request }) => {
        const { signInRedirect } = await import('#/lib/logto.server')
        const origin = process.env.CONSOLE_ORIGIN ?? new URL(request.url).origin
        const url = await signInRedirect(`${origin}/api/auth/callback`)
        return new Response(null, { status: 302, headers: { Location: url } })
      },
    },
  },
})
