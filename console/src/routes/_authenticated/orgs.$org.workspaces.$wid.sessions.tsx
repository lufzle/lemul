import { useState } from 'react'
import { Link, createFileRoute, useRouter } from '@tanstack/react-router'

import {
  createSession,
  deleteSession,
  listSessions,
  resumeSession,
  stopSession,
} from '#/lib/control-plane'
import { getFlags } from '#/lib/flags'
import { Avatar, Button, Dot, Panel, relativeTime, useAutoRefresh } from '#/components/ui'

export const Route = createFileRoute('/_authenticated/orgs/$org/workspaces/$wid/sessions')({
  loader: async ({ params }) => ({
    org: params.org,
    wid: params.wid,
    sessions: await listSessions({ data: { org: params.org, wid: params.wid } }),
    flags: await getFlags(),
    // One server-side clock for every relative timestamp on the page, so the
    // server render and the hydrated one agree. See relativeTime.
    now: Date.now(),
  }),
  component: Sessions,
})

function Sessions() {
  const { org, wid, sessions, flags, now } = Route.useLoaderData()
  const router = useRouter()
  useAutoRefresh()

  // One in-flight action at a time, keyed by session, so a row's buttons
  // disable while its own request is out rather than the whole table freezing.
  const [busy, setBusy] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  async function act(key: string, fn: () => Promise<unknown>) {
    setBusy(key)
    setError(null)
    try {
      await fn()
      await router.invalidate()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(null)
    }
  }

  return (
    <div className="space-y-4">
      {error ? (
        <div className="rounded border border-red-900 bg-red-950/40 px-4 py-3 text-sm text-red-200">
          {/* The control plane's own message, not a generic one: a 424 carries
              the Bedrock fix and a 409 says what to do instead. */}
          {error}
        </div>
      ) : null}

      <Panel
        actions={
          <Button busy={busy === 'new'} onClick={() => act('new', () => createSession({ data: { org, wid } }))}>
            new session
          </Button>
        }
      >
        {sessions.length === 0 ? (
          <p className="text-sm text-neutral-400">
            No sessions. Creating one starts the workspace task if it is stopped.
          </p>
        ) : (
          <table className="w-full text-left text-sm">
            <thead className="text-xs uppercase tracking-wide text-neutral-500">
              <tr>
                <th className="px-2 py-2 font-medium">session</th>
                <th className="px-2 py-2 font-medium">status</th>
                <th className="px-2 py-2 font-medium">owner</th>
                <th className="px-2 py-2 font-medium">created</th>
                <th className="px-2 py-2 text-right font-medium">actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-neutral-800">
              {sessions.map((s) => {
                const isRunning = s.status === 'running'
                return (
                  <tr key={s.id} className="hover:bg-neutral-900/60">
                    <td className="px-2 py-2">
                      {/* Full id, because it is also Claude Code's conversation
                          id -- it is what you grep the transcript by. */}
                      <span className="font-mono text-xs text-neutral-300">{s.id}</span>
                    </td>
                    <td className="px-2 py-2">
                      <span className="inline-flex items-center gap-2">
                        <Dot on={isRunning} />
                        <span className="text-neutral-400">{s.status}</span>
                        {isRunning && s.attachers === 0 ? (
                          <span className="text-xs text-neutral-600" title="alive, with no client attached">
                            detached
                          </span>
                        ) : null}
                      </span>
                    </td>
                    <td className="px-2 py-2 text-neutral-400">
                      {s.mine ? (
                        // Whose it is decides what can be done to it, so say it
                        // plainly rather than making the reader compare a uuid
                        // against one they were never told (§2.5).
                        <span className="text-neutral-300">you</span>
                      ) : s.owner ? (
                        <span className="inline-flex items-center gap-2">
                          <Avatar name={s.owner_name ?? s.owner} size="sm" />
                          {s.owner_name ?? s.owner}
                        </span>
                      ) : (
                        // Recorded from the authenticated subject at creation,
                        // so sessions made before that existed have none.
                        <span className="text-neutral-600">—</span>
                      )}
                    </td>
                    <td className="px-2 py-2 text-neutral-400" title={s.created_at}>
                      {relativeTime(s.created_at, now)}
                    </td>
                    <td className="px-2 py-2">
                      <div className="flex justify-end gap-1.5">
                        {/* Behind FF_VIEW_SESSION, and only while a process
                            exists: there is no terminal to watch on a stopped
                            session, and the ring dies with the PTY.

                            The viewer is read-only whoever opens it, so this is
                            offered for somebody else's session too — and it is
                            the ONLY thing offered for one, since §2.5 gives a
                            control credential to the session's own user alone.
                            Watching is visible to them: it bumps the attacher
                            count in the column to the left. */}
                        {flags.viewSession && isRunning ? (
                          <Link
                            to="/orgs/$org/workspaces/$wid/sessions/$sid"
                            params={{ org, wid, sid: s.id }}
                            title={s.mine ? 'read-only view' : "read-only; this is another user's session"}
                            className="rounded border border-neutral-700 px-2 py-1 text-xs text-neutral-200 transition hover:bg-neutral-800"
                          >
                            view
                          </Link>
                        ) : null}
                        {isRunning ? (
                          <Button
                            busy={busy === s.id}
                            title="Ctrl-C/Ctrl-D semantics; the conversation is kept"
                            onClick={() => act(s.id, () => stopSession({ data: { org, sid: s.id } }))}
                          >
                            stop
                          </Button>
                        ) : (
                          <Button
                            busy={busy === s.id}
                            title="start the process again, history intact"
                            onClick={() => act(s.id, () => resumeSession({ data: { org, sid: s.id } }))}
                          >
                            resume
                          </Button>
                        )}
                        <Button
                          tone="danger"
                          busy={busy === s.id}
                          title="ends it and drops the conversation; not recoverable"
                          onClick={() => {
                            // The one unrecoverable verb, so it asks. The API
                            // refuses a running session without force anyway;
                            // this makes the intent explicit before we send it.
                            const msg = isRunning
                              ? `Session ${s.id} is RUNNING.\n\nDelete it and drop its conversation? This cannot be undone.`
                              : `Delete session ${s.id} and drop its conversation? This cannot be undone.`
                            if (!confirm(msg)) return
                            void act(s.id, () =>
                              deleteSession({ data: { org, sid: s.id, force: isRunning } }),
                            )
                          }}
                        >
                          delete
                        </Button>
                      </div>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
        <p className="mt-4 text-xs text-neutral-500">
          Attach from a terminal:{' '}
          <code className="text-neutral-300">lem --org {org} connect {wid} -session &lt;id&gt;</code> — the
          console moves the process, not your terminal.
        </p>
      </Panel>
    </div>
  )
}
