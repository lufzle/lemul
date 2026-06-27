import { createFileRoute, useLoaderData, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'

import { createWorkspace, getOrgSettings } from '#/lib/control-plane'
import { Panel } from '#/components/ui'

/**
 * Creating a workspace.
 *
 * It replaced a `window.prompt` on the overview, and the reason is not polish.
 * A workspace now has an ACCESS SCOPE, and it is the one field where the safe
 * answer and the useful answer differ — a prompt that asks only for a name
 * silently creates every workspace at `owner`, which on a product whose whole
 * premise is a shared machine is the wrong default to make invisible (§12.10).
 *
 * At `/orgs/$org/new-workspace` rather than `/orgs/$org/workspaces/new`, and
 * deliberately: `workspaces/$wid` is a real route, `new` is a legal workspace
 * name, and a static segment wins the match — so the tidier URL would make a
 * workspace called `new` permanently unreachable in the console.
 *
 * THERE IS NO INSTANCE SIZE CONTROL, and its absence is a decision. The task
 * spec carries CPU and memory and the runner forwards them, but the control
 * plane never populates either, the `ecs` driver ignores both, and `docker`
 * honours only memory — so every option a dropdown could offer would be a
 * promise nothing keeps. A control that does nothing is worse than a missing
 * one: it is discovered by a customer whose 16 GiB workspace OOMs like a 2 GiB
 * one.
 */
export const Route = createFileRoute('/_authenticated/orgs/$org/new-workspace')({
  loader: async ({ params }) => ({
    org: params.org,
    settings: await getOrgSettings({ data: { org: params.org } }),
  }),
  component: NewWorkspace,
  errorComponent: ({ error }) => (
    <Panel title="cannot create a workspace">
      <p className="text-sm text-red-300">{error.message}</p>
    </Panel>
  ),
})

/**
 * The three scopes, in the words a person picking one would use.
 *
 * `team` is deliberately not here, because it still names an entity that does
 * not exist. The consequences are spelled out rather than implied: the whole
 * point of the choice is that it decides who shares the machine, and somebody
 * choosing "everyone" ought to know that is what they chose.
 */
const scopes = [
  {
    value: 'owner',
    title: 'Just me',
    detail: 'Only you, plus owners of this organization.',
  },
  {
    value: 'members',
    title: 'Specific people',
    detail: 'Only who you add on the workspace’s members tab. Add them after creating it.',
  },
  {
    value: 'org',
    title: 'Everyone in this organization',
    detail: 'Anybody here can start a session in it, at their own uid and in their own home.',
  },
]

function NewWorkspace() {
  const { org, settings } = Route.useLoaderData()
  // The caller's ROLE here, which is the other half of who may create one.
  // There is no endpoint that answers "what am I in this organization"
  // directly -- an access token carries a subject and no claims about what it
  // may do -- so the organization list the layout already holds is the answer.
  const { orgs } = useLoaderData({ from: '/_authenticated' })
  const navigate = useNavigate()
  const [name, setName] = useState('')
  const [scope, setScope] = useState('owner')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const role = orgs.find((o) => o.slug === org)?.role
  // The same rule the server enforces, mirrored so the answer is a sentence
  // rather than a 403 after filling a form in. An owner is never gated: an
  // owner who could not create a workspace could not turn the setting on
  // either.
  const mayCreate = role === 'owner' || settings.members_can_create_workspaces

  const submit = async () => {
    setBusy(true)
    setError(null)
    try {
      // An empty box is the explicit null the API takes, which is "you pick".
      // Generating a name HERE would put the word lists in two places, and
      // worse, would skip the server's collision retry — a suggested name is
      // submitted as a chosen one, and a chosen one that collides is a 409.
      const ws = await createWorkspace({
        data: { org, name: name.trim() === '' ? null : name.trim(), access_scope: scope },
      })
      // Back to the overview rather than into the workspace: the machine is
      // still starting, and the row is where that is visible — it arrives
      // saying `starting`, which is the confirmation and the caveat at once.
      void ws
      await navigate({ to: '/orgs/$org', params: { org } })
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setBusy(false)
    }
  }

  if (!mayCreate) {
    return (
      <Panel title="new workspace">
        <p className="text-sm text-neutral-300">
          Only an owner of <span className="font-mono text-neutral-200">{org}</span> may create
          workspaces here.
        </p>
        <p className="mt-2 text-sm text-neutral-400">
          A workspace is a task somebody pays for from the moment it is created, so who may start
          one is the organization’s decision. An owner can allow it under the organization’s
          settings.
        </p>
      </Panel>
    )
  }

  return (
    <form
      className="space-y-4"
      onSubmit={(e) => {
        e.preventDefault()
        void submit()
      }}
    >
      <Panel title="new workspace">
        {error ? <p className="mb-3 text-sm text-red-300">{error}</p> : null}

        <label className="block">
          <span className="text-xs uppercase tracking-wide text-neutral-500">name</span>
          <input
            autoFocus
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="leave blank and one is generated, like drifting-schooner"
            className="mt-1 w-full rounded border border-neutral-700 bg-neutral-900 px-2 py-1.5 font-mono text-sm text-neutral-100 placeholder:font-sans placeholder:text-neutral-600 focus:border-neutral-500 focus:outline-none"
          />
          <span className="mt-1 block text-xs text-neutral-500">
            Lowercase letters, digits, <code>-</code> and <code>_</code>. The name travels in URLs,
            in the task’s arguments and in the tunnel registry, so it is restricted to what all
            three carry without escaping.
          </span>
        </label>

        <fieldset className="mt-5">
          <legend className="text-xs uppercase tracking-wide text-neutral-500">who can use it</legend>
          <div className="mt-2 space-y-2">
            {scopes.map((s) => (
              <label
                key={s.value}
                className={`flex cursor-pointer gap-3 rounded border p-3 transition ${
                  scope === s.value
                    ? 'border-sky-700 bg-sky-950/30'
                    : 'border-neutral-800 hover:bg-neutral-900/60'
                }`}
              >
                <input
                  type="radio"
                  name="access_scope"
                  value={s.value}
                  checked={scope === s.value}
                  onChange={() => setScope(s.value)}
                  className="mt-0.5"
                />
                <span>
                  <span className="block text-sm text-neutral-200">{s.title}</span>
                  <span className="block text-xs text-neutral-500">{s.detail}</span>
                </span>
              </label>
            ))}
          </div>
        </fieldset>

        <p className="mt-5 text-xs text-neutral-500">
          A workspace is a shared machine with private homes: every member’s sessions run at their
          own uid, with their own home and their own Claude Code state, and one{' '}
          <code className="text-neutral-400">/shared</code> directory in common. Creating it starts
          the machine, so it begins billing now — it stops itself once nobody is using it.
        </p>
      </Panel>

      <div className="flex gap-2">
        <button
          type="submit"
          disabled={busy}
          className="rounded border border-neutral-700 px-3 py-1.5 text-xs text-neutral-200 transition hover:bg-neutral-800 disabled:opacity-40"
        >
          {busy ? 'creating…' : 'create workspace'}
        </button>
        <button
          type="button"
          disabled={busy}
          onClick={() => void navigate({ to: '/orgs/$org', params: { org } })}
          className="rounded border border-neutral-800 px-3 py-1.5 text-xs text-neutral-400 transition hover:bg-neutral-900 disabled:opacity-40"
        >
          cancel
        </button>
      </div>
    </form>
  )
}
