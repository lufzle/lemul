import { HeadContent, Scripts, createRootRoute } from '@tanstack/react-router'

import appCss from '../styles.css?url'

export const Route = createRootRoute({
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
          <header className="mb-6 flex items-baseline justify-between border-b border-neutral-800 pb-3">
            <a href="/" className="text-sm font-medium tracking-wide text-neutral-300">
              lemul <span className="text-neutral-600">console</span>
            </a>
            {/* Not a disclaimer for its own sake. There is no auth anywhere in
                Phase 1 -- §2.5 records that attach accepts any session in a
                workspace -- so this console is only safe because it is bound to
                loopback. Saying so where an operator will see it is cheaper than
                someone discovering the assumption by breaking it. */}
            <span className="text-xs text-amber-500/80">
              no auth · localhost only · do not expose
            </span>
          </header>
          {children}
        </div>
        <Scripts />
      </body>
    </html>
  )
}
