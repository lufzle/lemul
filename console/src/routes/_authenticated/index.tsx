import { Link, createFileRoute } from '@tanstack/react-router'
import { getStatus, listWorkspaces } from '#/lib/control-plane'
import { Dot, Panel, useAutoRefresh } from '#/components/ui'

export const Route = createFileRoute('/_authenticated/')({
  loader: async () => ({
    status: await getStatus(),
    workspaces: await listWorkspaces(),
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
  const { status, workspaces } = Route.useLoaderData()
  useAutoRefresh()

  return (
    <div className="space-y-6">
      <Panel title="control plane">
        <dl className="grid grid-cols-2 gap-x-8 gap-y-3 text-sm sm:grid-cols-3">
          <Field label="tenant" value={status.tenant_id} />
          {/* A runner is what places workspace tasks. None connected means
              nothing can start, which is the first thing an operator asks. */}
          <Field
            label="runners"
            value={
              <span className="inline-flex items-center gap-2">
                <Dot on={status.runners > 0} />
                {status.runners}
              </span>
            }
            hint={status.runners === 0 ? 'none: workspaces cannot be placed' : undefined}
          />
          <Field label="inference" value={status.inference} />
          <Field label="telemetry" value={status.telemetry ? 'on' : 'off'} />
          <Field label="bedrock preflight" value={status.bedrock_preflight ? 'on' : 'off'} />
          <Field label="image" value={status.image || '—'} />
        </dl>
        {status.pins ? (
          <p className="mt-4 font-mono text-xs text-neutral-500">pins: {status.pins}</p>
        ) : null}
      </Panel>

      <Panel title={`workspaces (${workspaces.length})`}>
        {workspaces.length === 0 ? (
          <p className="text-sm text-neutral-400">
            None yet. A workspace is created on demand by its first session —{' '}
            <code className="text-neutral-200">ourcli connect &lt;name&gt;</code>.
          </p>
        ) : (
          <table className="w-full text-left text-sm">
            <thead className="text-xs uppercase tracking-wide text-neutral-500">
              <tr>
                <Th>workspace</Th>
                <Th>status</Th>
                <Th>task</Th>
                <Th className="text-right">gen</Th>
                <Th className="text-right">sessions</Th>
                <Th className="text-right">running</Th>
              </tr>
            </thead>
            <tbody className="divide-y divide-neutral-800">
              {workspaces.map((w) => (
                <tr key={w.id} className="hover:bg-neutral-900/60">
                  <Td>
                    <Link
                      to="/workspaces/$wid"
                      params={{ wid: w.id }}
                      className="font-mono text-sky-300 hover:underline"
                    >
                      {w.id}
                    </Link>
                  </Td>
                  <Td className="text-neutral-300">{w.status}</Td>
                  {/* The record and the tunnel can disagree, and that gap is the
                      interesting one: a workspace recorded active whose task
                      died reads active + no task. */}
                  <Td>
                    <span className="inline-flex items-center gap-2">
                      <Dot on={w.connected} />
                      <span className="text-neutral-400">
                        {w.connected ? 'connected' : 'no task'}
                      </span>
                    </span>
                  </Td>
                  <Td className="text-right tabular-nums text-neutral-400">{w.generation}</Td>
                  <Td className="text-right tabular-nums">{w.sessions}</Td>
                  <Td className="text-right tabular-nums">{w.connected ? w.running : '—'}</Td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>
    </div>
  )
}

function Field({
  label,
  value,
  hint,
}: {
  label: string
  value: React.ReactNode
  hint?: string
}) {
  return (
    <div>
      <dt className="text-xs uppercase tracking-wide text-neutral-500">{label}</dt>
      <dd className="mt-0.5 font-mono text-neutral-200">{value}</dd>
      {hint ? <p className="mt-0.5 text-xs text-amber-400">{hint}</p> : null}
    </div>
  )
}

function Th({ children, className = '' }: { children: React.ReactNode; className?: string }) {
  return <th className={`px-2 py-2 font-medium ${className}`}>{children}</th>
}

function Td({ children, className = '' }: { children: React.ReactNode; className?: string }) {
  return <td className={`px-2 py-2 ${className}`}>{children}</td>
}
