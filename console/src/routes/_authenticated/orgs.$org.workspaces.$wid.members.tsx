import { createFileRoute, useRouter } from '@tanstack/react-router'
import { useState } from 'react'

import {
  addWorkspaceMember,
  getWorkspace,
  listOrgMembers,
  listWorkspaceMembers,
  removeWorkspaceMember,
  setAccessScope,
} from '#/lib/control-plane'
import { Avatar, Button, Panel } from '#/components/ui'

/**
 * Who can reach this workspace.
 *
 * Membership is what §2.5 replaced `access_scope` with, and it is the thing
 * that decides visibility now: belonging to an organization is not belonging to
 * every workspace in it. A plain member sees what they own or were added to,
 * and this page is where "were added to" comes from.
 *
 * It is a console surface only — there are deliberately no `lem` verbs for it.
 * Adding somebody to a workspace is an administrative act done occasionally
 * from a screen that can show who is already there, not something to type.
 */
export const Route = createFileRoute('/_authenticated/orgs/$org/workspaces/$wid/members')({
  loader: async ({ params }) => ({
    org: params.org,
    wid: params.wid,
    // The access scope, which is the other half of the answer this tab gives.
    // Without it the page shows a member list that may decide nothing: at
    // `owner` nobody on it can reach the workspace, and at `org` everybody can
    // whether they are on it or not.
    workspace: await getWorkspace({ data: { org: params.org, wid: params.wid } }),
    members: await listWorkspaceMembers({ data: { org: params.org, wid: params.wid } }),
    // The organization's roster is what turns "add a member" into a choice
    // rather than a uuid to paste: the API takes a user id and this is the only
    // endpoint that hands one out.
    candidates: await listOrgMembers({ data: { org: params.org } }),
  }),
  component: Members,
  errorComponent: ({ error }) => (
    <Panel title="members unavailable">
      <p className="text-sm text-red-300">{error.message}</p>
    </Panel>
  ),
})

/**
 * The three scopes, in the words somebody picking one would use. Shared in
 * meaning with the create form and deliberately not shared in code: the wording
 * differs because the tenses do -- "add them after creating it" is advice on a
 * form and noise on a page that already has the list.
 *
 * `team` is absent because it still names an entity that does not exist.
 */
const accessScopes = [
  { value: 'owner', title: 'Owner only', detail: 'You, plus owners of this organization.' },
  { value: 'members', title: 'Specific people', detail: 'The members listed below, and nobody else.' },
  {
    value: 'org',
    title: 'Everyone in this organization',
    detail: 'Anybody here, at their own uid and in their own home.',
  },
]

function Members() {
  const { org, wid, workspace, members, candidates } = Route.useLoaderData()
  const router = useRouter()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

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

  const inWorkspace = new Set(members.map((m) => m.user_id))
  const addable = candidates.filter((c) => !inWorkspace.has(c.user_id))
  const label = (m: { email?: string; user_id: string }) => m.email ?? m.user_id

  return (
    <div className="space-y-4">
      {error ? (
        <Panel>
          <p className="text-sm text-red-300">{error}</p>
        </Panel>
      ) : null}

      {/* Above the list, because it decides whether the list means anything.
          Changing it under a live task is safe and deliberately unguarded,
          unlike renaming: the scope exists only in this row -- no registry key,
          no argv, no derived credential -- and refusing to narrow it while
          somebody is working would mean an owner cannot revoke access at the
          one moment they most want to. Sessions already running are not
          evicted; narrowing stops the next endpoint negotiation rather than
          killing a conversation mid-sentence. */}
      <Panel title="who can use this workspace">
        <div className="space-y-2">
          {accessScopes.map((s) => (
            <label
              key={s.value}
              className={`flex cursor-pointer gap-3 rounded border p-3 transition ${
                workspace.access_scope === s.value
                  ? 'border-sky-700 bg-sky-950/30'
                  : 'border-neutral-800 hover:bg-neutral-900/60'
              }`}
            >
              <input
                type="radio"
                name="access_scope"
                value={s.value}
                className="mt-0.5"
                checked={workspace.access_scope === s.value}
                disabled={busy}
                onChange={() =>
                  act(() => setAccessScope({ data: { org, wid, access_scope: s.value } }))
                }
              />
              <span>
                <span className="block text-sm text-neutral-200">{s.title}</span>
                <span className="block text-xs text-neutral-500">{s.detail}</span>
              </span>
            </label>
          ))}
        </div>
      </Panel>

      <Panel title={`members (${members.length})`}>
        {workspace.access_scope !== 'members' ? (
          <p className="mb-3 rounded border border-neutral-800 bg-neutral-900/60 px-3 py-2 text-sm text-neutral-400">
            {workspace.access_scope === 'org'
              ? 'This workspace is open to everyone in the organization, so these rows do not decide who may use it — only who administers it. The owner role here is what grants that.'
              : 'This workspace is owner-only, so these rows grant nothing at the moment. Switch it to “specific people” above for them to take effect.'}
          </p>
        ) : null}
        <table className="w-full text-left text-sm">
          <thead className="text-xs uppercase tracking-wide text-neutral-500">
            <tr>
              <th className="px-2 py-2 font-medium">who</th>
              <th className="px-2 py-2 font-medium">role</th>
              <th className="px-2 py-2 font-medium">actions</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-neutral-800">
            {members.map((m) => (
              <tr key={m.user_id} className="hover:bg-neutral-900/60">
                <td className="px-2 py-2">
                  <span className="inline-flex items-center gap-2">
                    <Avatar name={label(m)} size="sm" />
                    {label(m)}
                    {m.me ? <span className="text-xs text-neutral-500">you</span> : null}
                  </span>
                </td>
                <td className="px-2 py-2 text-neutral-400">{m.role}</td>
                <td className="px-2 py-2">
                  <span className="flex gap-2">
                    <Button
                      busy={busy}
                      onClick={() =>
                        act(() =>
                          addWorkspaceMember({
                            data: {
                              org,
                              wid,
                              userId: m.user_id,
                              role: m.role === 'owner' ? 'user' : 'owner',
                            },
                          }),
                        )
                      }
                    >
                      make {m.role === 'owner' ? 'user' : 'owner'}
                    </Button>
                    <Button
                      busy={busy}
                      tone="danger"
                      onClick={() => {
                        if (!window.confirm(`Remove ${label(m)} from ${wid}?`)) return
                        return act(() =>
                          removeWorkspaceMember({ data: { org, wid, userId: m.user_id } }),
                        )
                      }}
                    >
                      remove
                    </Button>
                  </span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </Panel>

      <Panel title="add somebody">
        {addable.length === 0 ? (
          <p className="text-sm text-neutral-400">
            Everyone in this organization is already a member. Invite more people to the
            organization first — a workspace can only hold its own organization's users.
          </p>
        ) : (
          <ul className="space-y-2 text-sm">
            {addable.map((c) => (
              <li key={c.user_id} className="flex items-center justify-between gap-4">
                <span className="inline-flex items-center gap-2">
                  <Avatar name={label(c)} size="sm" />
                  {label(c)}
                </span>
                <Button
                  busy={busy}
                  onClick={() =>
                    act(() =>
                      addWorkspaceMember({ data: { org, wid, userId: c.user_id, role: 'user' } }),
                    )
                  }
                >
                  add
                </Button>
              </li>
            ))}
          </ul>
        )}
      </Panel>
    </div>
  )
}
