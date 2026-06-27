import { useRouter } from '@tanstack/react-router'
import { Link, createFileRoute, useLoaderData } from '@tanstack/react-router'
import { useState } from 'react'
import {
  deleteWorkspace,
  getOrgSettings,
  getStatus,
  listWorkspaces,
  renameWorkspace,
} from '#/lib/control-plane'
import { Avatar, Button, Dot, Panel, relativeTime, useAutoRefresh } from '#/components/ui'

export const Route = createFileRoute('/_authenticated/orgs/$org/')({
  loader: async ({ params }) => ({
    org: params.org,
    status: await getStatus(),
    workspaces: await listWorkspaces({ data: { org: params.org } }),
    // Who may create one here, which is an organization setting plus the
    // caller's role in it. Read so the page can offer the verb or explain its
    // absence, rather than sending somebody to a form that will 403.
    settings: await getOrgSettings({ data: { org: params.org } }),
    // One server-side clock for every relative timestamp, so the server render
    // and the hydrated one agree. See relativeTime.
    now: Date.now(),
  }),
  component: Overview,
  errorComponent: ({ error }) => (
    <Panel title="control plane unreachable">
      <p className="text-sm text-red-300">{error.message}</p>
      <p className="mt-2 text-sm text-neutral-400">
        Start it with <code className="text-neutral-200">bin/controlplane -addr :9000</code>, or
        point the console elsewhere with <code className="text-neutral-200">LEMUL_API</code>.
      </p>
    </Panel>
  ),
})

function Overview() {
  const { org, status, workspaces, settings, now } = Route.useLoaderData()
  // The organization list comes from the layout above rather than from this
  // loader. It is the same answer for every page beneath there, and this
  // loader re-runs every four seconds -- fetching it here would be one
  // redundant round trip per refresh, per open tab.
  const { orgs } = useLoaderData({ from: '/_authenticated' })
  const router = useRouter()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  useAutoRefresh()

  const here = orgs.find((o) => o.slug === org)
  const isOwner = here?.role === 'owner'
  // The server's rule, mirrored. An owner is never gated by the setting: one
  // who could not create a workspace could not turn the setting on either.
  const mayCreate = isOwner || settings.members_can_create_workspaces

  // Every mutation goes through this: it keeps the org argument in one place,
  // surfaces the control plane's own sentence rather than a status code, and
  // reloads so the table reflects what actually happened rather than what was
  // optimistically assumed.
  const act = async (fn: () => Promise<unknown>) => {
    setBusy(true)
    setError(null)
    try {
      await fn()
      await router.invalidate()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  const onRename = (wid: string, current: string) => {
    const name = window.prompt(`rename ${current} to`, current)
    if (!name || name === current) return
    return act(() => renameWorkspace({ data: { org, wid, name } }))
  }

  const onDelete = (wid: string, sessions: number) => {
    // The count is in the question because it is the thing being destroyed:
    // deleting a workspace drops its sessions' conversations, and the
    // transcript IS a session's memory.
    const what =
      sessions > 0
        ? `Delete ${wid} and the conversations of its ${sessions} session(s)? This cannot be undone.`
        : `Delete ${wid}?`
    if (!window.confirm(what)) return
    return act(() => deleteWorkspace({ data: { org, wid, force: sessions > 0 } }))
  }

  return (
    <div className="space-y-6">
      <Panel>
        <dl className="grid grid-cols-2 gap-x-8 gap-y-3 text-sm sm:grid-cols-3">
          <Field label="organization" value={org} />
          <Field label="cell" value={status.cell_id} />
          <Field
            label="llm gateway"
            value={status.inference}
            hint={status.gateway_url || undefined}
            tone="muted"
          />
          <Field label="telemetry" value={status.telemetry ? 'on' : 'off'} />
          <Field label="image" value={status.image || '—'} />
        </dl>
              </Panel>

      <Panel
        title={`workspaces (${workspaces.length})`}
        actions={
          <span className="flex items-center gap-3">
            <Link
              to="/orgs/$org/settings"
              params={{ org }}
              className="text-xs text-neutral-400 hover:text-neutral-200 hover:underline"
            >
              settings
            </Link>
            {mayCreate ? (
              <Link
                to="/orgs/$org/new-workspace"
                params={{ org }}
                className="rounded border border-neutral-700 px-2 py-1 text-xs text-neutral-200 transition hover:bg-neutral-800"
              >
                new workspace
              </Link>
            ) : null}
          </span>
        }
      >
        {error ? <p className="mb-3 text-sm text-red-300">{error}</p> : null}
        {workspaces.length === 0 ? (
          // The empty state carries the next step rather than only the fact.
          // It is the first screen a new organization has, and "none yet" on
          // its own is a dead end on the one page that should not have one.
          <div className="py-6 text-center">
            <p className="text-sm text-neutral-300">No workspaces yet.</p>
            <p className="mx-auto mt-1 max-w-md text-sm text-neutral-500">
              A workspace is a shared machine your organization’s members work in, each at their own
              uid and in their own home, with one <code className="text-neutral-400">/shared</code>{' '}
              directory in common.
            </p>
            {mayCreate ? (
              <>
                <Link
                  to="/orgs/$org/new-workspace"
                  params={{ org }}
                  className="mt-4 inline-block rounded border border-neutral-700 px-3 py-1.5 text-xs text-neutral-200 transition hover:bg-neutral-800"
                >
                  create your first workspace
                </Link>
                <p className="mt-3 text-xs text-neutral-600">
                  or <code className="text-neutral-500">lem --org {org} workspace create</code>
                </p>
              </>
            ) : (
              <p className="mt-4 text-xs text-neutral-500">
                Only an owner of {org} may create one here. An owner can allow it under{' '}
                <Link
                  to="/orgs/$org/settings"
                  params={{ org }}
                  className="text-sky-300 hover:underline"
                >
                  settings
                </Link>
                .
              </p>
            )}
          </div>
        ) : (
          <table className="w-full text-left text-sm">
            <thead className="text-xs uppercase tracking-wide text-neutral-500">
              <tr>
                <Th>name</Th>
                <Th>state</Th>
                <Th>sessions</Th>
                <Th>access</Th>
                <Th>owner</Th>
                <Th>created</Th>
                <Th>actions</Th>
              </tr>
            </thead>
            <tbody className="divide-y divide-neutral-800">
              {workspaces.map((w) => (
                <tr key={w.id} className="hover:bg-neutral-900/60">
                  <Td>
                    <span className="inline-flex items-center gap-2">
                      <Link
                        to="/orgs/$org/workspaces/$wid"
                        params={{ org, wid: w.id }}
                        className="font-mono text-sky-300 hover:underline"
                      >
                        {w.id}
                      </Link>
                      {/* This workspace would have stopped by now, and a
                          background process in some session is the only thing
                          keeping it up. Deliberately a NOTICE and not an
                          action: the member who started that process meant to,
                          and reaping their session would kill it. What was
                          missing was never enforcement, it was somebody being
                          able to SEE that a machine is still being paid for. */}
                      {w.idle_pinned_since ? (
                        <span
                          title={
                            'Idle since ' +
                            w.idle_pinned_since +
                            ', but a background process in a session is holding it open. ' +
                            'It will not stop on its own while that process runs.'
                          }
                          className="inline-flex items-center gap-1 text-xs text-amber-400"
                        >
                          <span aria-hidden="true">⚠</span>
                          held open {relativeTime(w.idle_pinned_since, now)}
                        </span>
                      ) : null}
                    </span>
                  </Td>
                  <Td>
                    <State ws={w} />
                  </Td>
                  {/* Running and idle in one cell: they sum to the total, so
                      two columns of numbers that add up was a column too many.
                      Running is only knowable while a task holds a tunnel --
                      the supervisor is the only thing that knows a PTY exists --
                      so a disconnected workspace shows its records in grey and
                      claims no green ones. */}
                  <Td className="tabular-nums">
                    <span className="inline-flex items-center gap-3">
                      {w.connected ? (
                        <span className="inline-flex items-center gap-1.5">
                          <Dot on={w.running > 0} />
                          {w.running}
                        </span>
                      ) : null}
                      <span className="inline-flex items-center gap-1.5 text-neutral-400">
                        <Dot on={false} />
                        {w.connected ? w.sessions - w.running : w.sessions}
                      </span>
                    </span>
                  </Td>
                  <Td className="text-neutral-400">
                    <Access scope={w.access_scope} />
                  </Td>
                  <Td className="text-neutral-400">
                    {w.owner ? (
                      <span className="inline-flex items-center gap-2">
                        <Avatar name={w.owner_name ?? w.owner} size="sm" />
                        {w.owner_name ?? w.owner}
                      </span>
                    ) : (
                      // No owner model yet: authentication is on but nothing
                      // records who a workspace belongs to before this change,
                      // and older records have none. Say so rather than guess.
                      <span className="text-neutral-600">—</span>
                    )}
                  </Td>
                  <Td className="text-neutral-400" title={w.created_at}>
                    {w.created_at ? relativeTime(w.created_at, now) : '—'}
                  </Td>
                  <Td>
                    <span className="flex gap-2">
                      <Button busy={busy} onClick={() => onRename(w.id, w.id)}>
                        rename
                      </Button>
                      <Button
                        busy={busy}
                        tone="danger"
                        onClick={() => onDelete(w.id, w.sessions)}
                      >
                        delete
                      </Button>
                    </span>
                  </Td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>
    </div>
  )
}

/**
 * The compute axis of §2.4, in one cell.
 *
 * The column came back because placement moved to CREATE: a workspace now
 * spends its first 20-60 s coming up, with nobody having asked for a session,
 * and a table that showed only session counts left that state invisible. It had
 * previously been dropped as noise, and it was — while the only thing that
 * placed a task was somebody waiting for one.
 *
 * Three readings that all deserve to be distinguishable, and only one of them
 * is the record's `status` on its own:
 *
 *  - `starting` — asked for, on its way. The common state of a new row.
 *  - a FAILED placement — `last_error` is set, so the row is not merely stopped,
 *    it tried and could not. This is the state that would otherwise be silent,
 *    and the reason last_error exists at all.
 *  - `active` with no tunnel — the record and reality disagree, which means the
 *    task died and nothing noticed. The most important thing on this page and
 *    the least likely to be looked for.
 */
function State({
  ws,
}: {
  ws: { status: string; connected: boolean; last_error?: string; warm_hold_until?: string }
}) {
  if (ws.last_error) {
    return (
      <span
        title={ws.last_error}
        className="inline-flex items-center gap-1.5 text-xs text-red-300"
      >
        <span aria-hidden="true">✕</span>
        could not start
      </span>
    )
  }
  if (ws.status === 'starting') {
    return (
      <span
        title="The task has been asked for and has not dialed in yet. A cold start is 20-60 s."
        className="inline-flex items-center gap-1.5 text-xs text-sky-300"
      >
        <span className="inline-block size-2 animate-pulse rounded-full bg-sky-400" aria-hidden />
        starting
      </span>
    )
  }
  if (ws.status === 'active' && !ws.connected) {
    return (
      <span
        title="Recorded active, but no task is holding a tunnel. It died without anything noticing."
        className="inline-flex items-center gap-1.5 text-xs text-amber-400"
      >
        <span aria-hidden="true">⚠</span>
        no task
      </span>
    )
  }
  if (ws.status === 'active' && ws.warm_hold_until) {
    // Warm hold is deliberately not a status value -- every comparison against
    // status would have had to learn a fourth -- so it is derived here exactly
    // as §2.4 says it is: active, up, and nothing running in it.
    return (
      <span
        title={'Nothing is running in it. The task stops at ' + ws.warm_hold_until + '.'}
        className="inline-flex items-center gap-1.5 text-xs text-neutral-400"
      >
        <Dot on={false} />
        warm hold
      </span>
    )
  }
  if (ws.status === 'active') {
    return (
      <span className="inline-flex items-center gap-1.5 text-xs text-emerald-300">
        <Dot on />
        up
      </span>
    )
  }
  return (
    <span
      title="No task. Asking for a session starts one."
      className="inline-flex items-center gap-1.5 text-xs text-neutral-500"
    >
      <Dot on={false} />
      stopped
    </span>
  )
}

/**
 * Who may use this workspace.
 *
 * Rendered as a sentence rather than the stored token, because `owner` and
 * `org` read as categories when what a person wants to know is whether their
 * colleagues are in here too — which on a shared machine is the question the
 * table exists to answer.
 */
function Access({ scope }: { scope: string }) {
  const label =
    scope === 'org' ? 'everyone here' : scope === 'members' ? 'named members' : 'owner only'
  const tone = scope === 'org' ? 'text-neutral-300' : 'text-neutral-500'
  return (
    <span title={`access_scope = ${scope}`} className={`text-xs ${tone}`}>
      {label}
    </span>
  )
}

function Field({
  label,
  value,
  hint,
  tone = 'warn',
}: {
  label: string
  value: React.ReactNode
  hint?: string
  tone?: 'warn' | 'muted'
}) {
  return (
    <div>
      <dt className="text-xs uppercase tracking-wide text-neutral-500">{label}</dt>
      <dd className="mt-0.5 font-mono text-neutral-200">{value}</dd>
      {hint ? (
        <p className={`mt-0.5 text-xs ${tone === 'warn' ? 'text-amber-400' : 'text-neutral-500'}`}>
          {hint}
        </p>
      ) : null}
    </div>
  )
}

function Th({ children, className = '' }: { children: React.ReactNode; className?: string }) {
  return <th className={`px-2 py-2 font-medium ${className}`}>{children}</th>
}

function Td({
  children,
  className = '',
  title,
}: {
  children: React.ReactNode
  className?: string
  /** Carries the exact timestamp behind a relative one, on hover. */
  title?: string
}) {
  return (
    <td title={title} className={`px-2 py-2 ${className}`}>
      {children}
    </td>
  )
}
