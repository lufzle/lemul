import { Link, createFileRoute, notFound } from '@tanstack/react-router'
import { listSessions } from '#/lib/control-plane'
import { Panel } from '#/components/ui'
import { Viewer } from '#/components/Viewer'

export const Route = createFileRoute('/workspaces/$wid_/sessions/$sid')({
  loader: async ({ params }) => {
    const sessions = await listSessions({ data: { wid: params.wid } })
    const session = sessions.find((s) => s.id === params.sid)
    if (!session) throw notFound()
    return { wid: params.wid, session }
  },
  component: SessionViewer,
  notFoundComponent: () => (
    <Panel title="no such session">
      <p className="text-sm text-neutral-400">
        That session is not in this workspace. It may have been deleted.
      </p>
    </Panel>
  ),
  errorComponent: ({ error }) => (
    <Panel title="session unavailable">
      <p className="text-sm text-red-300">{error.message}</p>
    </Panel>
  ),
})

function SessionViewer() {
  const { wid, session } = Route.useLoaderData()
  const isRunning = session.status === 'running'

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-baseline gap-3">
        <Link
          to="/workspaces/$wid"
          params={{ wid }}
          className="text-sm text-neutral-500 hover:text-neutral-300"
        >
          ← {wid}
        </Link>
        <h1 className="font-mono text-sm text-neutral-100">{session.id}</h1>
      </div>

      <Panel title="viewer">
        {isRunning ? (
          // Keyed on the id so navigating between sessions tears the socket
          // down and builds a new one rather than writing a second session's
          // bytes into the first one's screen.
          <Viewer
            key={session.id}
            sessionId={session.id}
            rows={session.rows || 24}
            cols={session.cols || 80}
          />
        ) : (
          <p className="text-sm text-neutral-400">
            This session is stopped, so there is no terminal to watch. Resume it from the
            workspace page — its conversation is on the workspace volume, so it picks up
            where it left off.
          </p>
        )}
      </Panel>

      {isRunning ? (
        <p className="text-xs text-neutral-500">
          {/* Worth stating: opening a viewer is not free of side effects, and an
              operator seeing the controller's screen flicker should know why. */}
          Attaching a viewer nudges the session to repaint, which the person driving it
          sees as a brief reflow. To drive it yourself instead:{' '}
          <code className="text-neutral-300">
            ourcli connect {wid} -session {session.id}
          </code>
        </p>
      ) : null}
    </div>
  )
}
