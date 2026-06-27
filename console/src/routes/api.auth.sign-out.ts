import { createFileRoute } from '@tanstack/react-router'

export const Route = createFileRoute('/api/auth/sign-out')({
  server: {
    handlers: {
      GET: async ({ request }) => {
        const { signOutRedirect } = await import('#/lib/logto.server')
        const origin = process.env.CONSOLE_ORIGIN ?? new URL(request.url).origin
        // Ends the Logto session too, not just ours. Clearing only our cookie
        // would leave the next sign-in silently instant, which looks like the
        // sign-out did nothing.
        const url = await signOutRedirect(`${origin}/`)
        return new Response(null, { status: 302, headers: { Location: url } })
      },
    },
  },
})
