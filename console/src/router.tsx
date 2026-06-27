import { createRouter as createTanStackRouter } from '@tanstack/react-router'
import { routeTree } from './routeTree.gen'

export function getRouter() {
  const router = createTanStackRouter({
    routeTree,
    scrollRestoration: true,
    defaultPreload: 'intent',
    defaultPreloadStaleTime: 0,
    // A notFound() thrown from a loader resolves to the nearest ancestor that
    // can render one, which for a top-level route is the root -- so without this
    // a deleted session's URL falls through to the router's own bare "Not
    // Found". Configured here rather than only per-route so a mistyped URL lands
    // somewhere with a way back.
    defaultNotFoundComponent: () => (
      <div className="rounded-lg border border-neutral-800 bg-neutral-950 p-4">
        <p className="text-sm text-neutral-300">Nothing here.</p>
        <a href="/" className="mt-2 inline-block text-sm text-sky-300 hover:underline">
          ← workspaces
        </a>
      </div>
    ),
  })

  return router
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof getRouter>
  }
}
