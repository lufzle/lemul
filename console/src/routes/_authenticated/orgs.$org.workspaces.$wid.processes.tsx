import { createFileRoute, notFound } from '@tanstack/react-router'

import { type ProcessList, listProcesses } from '#/lib/control-plane'
import { getFlags } from '#/lib/flags'
import { Panel, Td, Th, bytes, useAutoRefresh } from '#/components/ui'

/**
 * What is running inside the task, attributed to the session that spawned it.
 *
 * Environments are never read -- /proc/<pid>/environ holds the gateway
 * credential §12.4 keeps out of reach -- and command lines are redacted for
 * known credential flags by the supervisor, because the exclusion only means
 * something if it covers both doors.
 */
export const Route = createFileRoute('/_authenticated/orgs/$org/workspaces/$wid/processes')({
  loader: async ({ params }) => {
    const flags = await getFlags()
    if (!flags.explorer) throw notFound()
    return {
      processes: await listProcesses({ data: { org: params.org, wid: params.wid } }).catch(
        (e: Error) => e.message,
      ),
    }
  },
  notFoundComponent: () => (
    <p className="text-sm text-neutral-400">
      The workspace explorer is off. Start the console with{' '}
      <code className="text-neutral-200">FF_WORKSPACE_EXPLORER=1</code> to enable it.
    </p>
  ),
  component: ProcessesTab,
})

function ProcessesTab() {
  const { processes } = Route.useLoaderData()
  useAutoRefresh()
  return <Processes data={processes} />
}

function Processes({ data }: { data: ProcessList | string }) {
  if (typeof data === 'string') {
    return (
      <Panel>
        <Unavailable message={data} />
      </Panel>
    )
  }
  if (!data.available) {
    return (
      <Panel>
        <Unavailable message="no procfs in this workspace — expected on the local driver" />
      </Panel>
    )
  }

  return (
    <Panel actions={<span className="text-xs text-neutral-500">{data.processes.length}</span>}>
      <table className="w-full text-left text-sm">
        <thead className="text-xs text-neutral-500">
          <tr>
            <Th>pid</Th>
            <Th>name</Th>
            <Th>state</Th>
            <Th>rss</Th>
            <Th>cpu</Th>
            <Th>session</Th>
            <Th>command</Th>
          </tr>
        </thead>
        <tbody className="font-mono">
          {data.processes.map((p) => (
            <tr key={p.pid} className="border-t border-neutral-800">
              <Td>{p.pid}</Td>
              <Td>{p.name}</Td>
              <Td className={p.state === 'R' ? 'text-emerald-400' : 'text-neutral-500'}>{p.state}</Td>
              <Td>{bytes(p.rss_kb * 1024)}</Td>
              <Td>{p.cpu_secs.toFixed(1)}s</Td>
              <Td className="text-neutral-500">
                {p.session_id ? p.session_id.slice(0, 8) : '—'}
              </Td>
              <Td>
                <span className="block max-w-md truncate text-neutral-400" title={p.cmdline}>
                  {p.cmdline || '—'}
                </span>
              </Td>
            </tr>
          ))}
        </tbody>
      </table>
    </Panel>
  )
}

function Unavailable({ message }: { message: string }) {
  return <p className="text-sm text-neutral-500">{message}</p>
}
