import { useState } from 'react'
import { Link, createFileRoute, useRouter } from '@tanstack/react-router'
import {
  createSession,
  deleteSession,
  getPreflight,
  listSessions,
  resumeSession,
  stopSession,
} from '#/lib/control-plane'
import { Button, Dot, Panel, useAutoRefresh } from '#/components/ui'

export const Route = createFileRoute('/workspaces/$wid')({
  loader: async ({ params }) => ({
    wid: params.wid,
    sessions: await listSessions({ data: { wid: params.wid } }),
    preflight: await getPreflight({ data: { wid: params.wid } }),
  }),
  component: WorkspaceDetail,
  errorComponent: ({ error }) => (
    <Panel title="workspace unavailable">
      <p className="text-sm text-red-300">{error.message}</p>
    </Panel>
  ),
})

function WorkspaceDetail() {
  const { wid, sessions, preflight } = Route.useLoaderData()
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
    <div className="space-y-6">
      <div className="flex items-baseline gap-3">
        <Link to="/" className="text-sm text-neutral-500 hover:text-neutral-300">
          ← workspaces
        </Link>
        <h1 className="font-mono text-lg text-neutral-100">{wid}</h1>
      </div>

      {error ? (
        <div className="rounded border border-red-900 bg-red-950/40 px-4 py-3 text-sm text-red-200">
          {/* The control plane's own message, not a generic one: a 424 carries
              the Bedrock fix and a 409 says what to do instead. */}
          {error}
        </div>
      ) : null}

      <Preflight report={preflight} />

      <Panel
        title="sessions"
        actions={
          <Button busy={busy === 'new'} onClick={() => act('new', () => createSession({ data: { wid } }))}>
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
                <th className="px-2 py-2 text-right font-medium">clients</th>
                <th className="px-2 py-2 text-right font-medium">size</th>
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
                      </span>
                    </td>
                    <td className="px-2 py-2 text-right tabular-nums">{s.attachers}</td>
                    <td className="px-2 py-2 text-right tabular-nums text-neutral-400">
                      {s.cols ? `${s.cols}×${s.rows}` : '—'}
                    </td>
                    <td className="px-2 py-2 text-neutral-400">{s.created_at}</td>
                    <td className="px-2 py-2">
                      <div className="flex justify-end gap-1.5">
                        {/* Only offered while a process exists: there is no
                            terminal to watch on a stopped session, and the ring
                            dies with the PTY. */}
                        {isRunning ? (
                          <Link
                            to="/workspaces/$wid/sessions/$sid"
                            params={{ wid, sid: s.id }}
                            className="rounded border border-neutral-700 px-2 py-1 text-xs text-neutral-200 transition hover:bg-neutral-800"
                          >
                            view
                          </Link>
                        ) : null}
                        {isRunning ? (
                          <Button
                            busy={busy === s.id}
                            title="Ctrl-C/Ctrl-D semantics; the conversation is kept"
                            onClick={() => act(s.id, () => stopSession({ data: { sid: s.id } }))}
                          >
                            stop
                          </Button>
                        ) : (
                          <Button
                            busy={busy === s.id}
                            title="start the process again, history intact"
                            onClick={() => act(s.id, () => resumeSession({ data: { sid: s.id } }))}
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
                              deleteSession({ data: { sid: s.id, force: isRunning } }),
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
          <code className="text-neutral-300">ourcli connect {wid} -session &lt;id&gt;</code> — the
          console moves the process, not your terminal.
        </p>
      </Panel>
    </div>
  )
}

/**
 * The preflight panel exists because "Bedrock is broken" is the unhelpful
 * message the whole three-layer check was built to replace: an operator needs to
 * see which model failed and what to do about it.
 *
 * Three states that must not be conflated, per §12.3: no report at all means the
 * task has not reported yet, `skipped` means the workspace is not using Bedrock,
 * and a blocking report is the only one that is actually a problem.
 */
function Preflight({ report }: { report: Awaited<ReturnType<typeof getPreflight>> }) {
  if (!report || !report.report) {
    return (
      <Panel title="bedrock preflight">
        <p className="text-sm text-neutral-400">
          No report yet — the workspace task has not checked in. Absence is not a failure.
        </p>
      </Panel>
    )
  }
  const r = report.report
  if (r.skipped) {
    return (
      <Panel title="bedrock preflight">
        <p className="text-sm text-neutral-400">Skipped — this workspace is not using Bedrock.</p>
      </Panel>
    )
  }
  return (
    <Panel title={`bedrock preflight — ${r.blocking ? 'BLOCKING' : 'ok'}${r.region ? ` (${r.region})` : ''}`}>
      <table className="w-full text-left text-sm">
        <thead className="text-xs uppercase tracking-wide text-neutral-500">
          <tr>
            <th className="px-2 py-2 font-medium">role</th>
            <th className="px-2 py-2 font-medium">model</th>
            <th className="px-2 py-2 font-medium">required</th>
            <th className="px-2 py-2 font-medium">invocable</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-neutral-800">
          {(r.models ?? []).map((m) => (
            <tr key={m.role + m.model_id} className="align-top">
              <td className="px-2 py-2 text-neutral-300">{m.role}</td>
              <td className="px-2 py-2 font-mono text-xs text-neutral-400">{m.model_id}</td>
              <td className="px-2 py-2 text-neutral-400">{m.required ? 'yes' : 'no'}</td>
              <td className="px-2 py-2">
                <span className="inline-flex items-center gap-2">
                  <Dot on={m.invocable} />
                  <span className="text-neutral-400">{m.invocable ? 'yes' : 'no'}</span>
                </span>
                {/* Only a real InvokeModel decides, so the advice attached to a
                    failure is the actionable part -- an IAM gap, a missing
                    inference-profile prefix and an unfinished FTU form all look
                    alike and have nothing in common. */}
                {!m.invocable && m.advice ? (
                  <p className="mt-1 max-w-prose text-xs text-amber-300">{m.advice}</p>
                ) : null}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </Panel>
  )
}
