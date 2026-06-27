import { createFileRoute } from '@tanstack/react-router'

/**
 * Where Logto returns the user after they enter their emailed code.
 *
 * `handleSignInCallback` is what validates the `state` parameter against the
 * value stored in the sign-in cookie, so this is the step that makes the flow
 * CSRF-resistant. The full callback URL must be passed through untouched --
 * trimming the query string here would discard both the code and the state and
 * leave a flow that appears to work while checking nothing.
 */
export const Route = createFileRoute('/api/auth/callback')({
  server: {
    handlers: {
      GET: async ({ request }) => {
        const { completeSignIn } = await import('#/lib/logto.server')
        const origin = process.env.CONSOLE_ORIGIN ?? new URL(request.url).origin
        try {
          const incoming = new URL(request.url)
          // Rebuild against the configured origin: behind any proxy the
          // request URL's host is not necessarily the one the redirect URI was
          // registered with, and Logto compares them.
          const callback = origin + incoming.pathname + incoming.search
          await completeSignIn(callback)
        } catch (error) {
          const message = error instanceof Error ? error.message : String(error)
          return new Response(`sign-in failed: ${message}`, {
            status: 400,
            headers: { 'Content-Type': 'text/plain' },
          })
        }
        return new Response(null, { status: 302, headers: { Location: '/' } })
      },
    },
  },
})
