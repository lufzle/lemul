import { Link, createFileRoute, notFound, useRouter } from '@tanstack/react-router'
import { useState } from 'react'

import {
  type DirListing,
  type ProcessList,
  type ResourceUsage,
  getResources,
  listDir,
  listProcesses,
} from '#/lib/control-plane'
import { getFlags } from '#/lib/flags'
import { Panel, Sparkline, Td, Th, bytes, useAutoRefresh } from '#/components/ui'

/**
 * The workspace explorer: what is on the disk, what is running, and what it is
 * consuming.
 *
 * This exists for §13's largest standing risk -- once workspaces run in customer
 * accounts we cannot reproduce a failure or attach to a wedged sandbox. Until
 * now the only way to see inside a task was to attach to a session and type,
 * which means co-driving somebody's live Claude Code.
 *
 * Read-only throughout, and structurally so: the supervisor has no write verb to
 * call. File CONTENTS are deliberately absent -- listings answer the operational
 * questions (did the clone land, what is eating the disk) while file bodies
 * through the relay would contradict §2.7's whole claim.
 */
export const Route = createFileRoute('/_authenticated/workspaces/$wid_/explorer')({
  loader: async ({ params }) => {
    // Before any fetch. Gating the link alone would leave the URL reachable,
    // which makes the flag decoration rather than a control.
    const flags = await getFlags()
    if (!flags.explorer) throw notFound()

    // Independent panels, so one failing must not blank the other two: a
    // workspace with no procfs (the local driver on darwin) still has files and
    // a disk worth showing.
    const [files, processes, resources] = await Promise.all([
      listDir({ data: { wid: params.wid, path: '/' } }).catch((e: Error) => e.message),
      listProcesses({ data: { wid: params.wid } }).catch((e: Error) => e.message),
      getResources({ data: { wid: params.wid } }).catch((e: Error) => e.message),
    ])
    return { files, processes, resources }
  },
  notFoundComponent: () => (
    <main className="mx-auto max-w-6xl p-6">
      <p className="text-sm text-neutral-400">
        The workspace explorer is off. Start the console with{' '}
        <code className="text-neutral-200">FF_WORKSPACE_EXPLORER=1</code> to enable it.
      </p>
    </main>
  ),
  component: Explorer,
})

function Explorer() {
  const { wid } = Route.useParams()
  const { files, processes, resources } = Route.useLoaderData()
  useAutoRefresh(4000)

  return (
    <main className="space-y-4">
      <div className="flex items-baseline justify-between gap-4">
        <h1 className="font-mono text-lg">{wid}</h1>
        <Link to="/workspaces/$wid" params={{ wid }} className="text-sm text-sky-300 hover:underline">
          ← back to workspace
        </Link>
      </div>

      <Resources data={resources} />
      <Files wid={wid} data={files} />
      <Processes data={processes} />
    </main>
  )
}

/* -------------------------------------------------------------------------- */

function Files({ wid, data }: { wid: string; data: DirListing | string }) {
  const router = useRouter()
  const [path, setPath] = useState('/')
  const [listing, setListing] = useState<DirListing | string>(data)
  const [busy, setBusy] = useState(false)

  // The loader supplies the root; navigating deeper is a client-side fetch so a
  // directory change does not re-run the process and resource panels too.
  async function go(next: string) {
    setBusy(true)
    try {
      setListing(await listDir({ data: { wid, path: next } }))
      setPath(next)
    } catch (e) {
      setListing(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
      void router.invalidate()
    }
  }

  if (typeof listing === 'string') {
    return (
      <Panel title="files">
        <Unavailable message={listing} />
      </Panel>
    )
  }

  const parent = path === '/' ? null : path.slice(0, path.lastIndexOf('/')) || '/'

  return (
    <Panel
      title="files"
      actions={
        <span className="font-mono text-xs text-neutral-500">
          {listing.path}
          {busy ? ' …' : ''}
        </span>
      }
    >
      <table className="w-full text-left text-sm">
        <thead className="text-xs text-neutral-500">
          <tr>
            <Th>name</Th>
            <Th>size</Th>
            <Th>mode</Th>
            <Th>modified</Th>
          </tr>
        </thead>
        <tbody className="font-mono">
          {parent !== null ? (
            <tr className="border-t border-neutral-800">
              <Td>
                <button onClick={() => void go(parent)} className="text-sky-300 hover:underline">
                  ../
                </button>
              </Td>
              <Td>—</Td>
              <Td>—</Td>
              <Td>—</Td>
            </tr>
          ) : null}
          {listing.entries.map((e) => (
            <tr key={e.name} className="border-t border-neutral-800">
              <Td>
                {e.is_dir ? (
                  <button
                    onClick={() => void go(`${path === '/' ? '' : path}/${e.name}`)}
                    className="text-sky-300 hover:underline"
                  >
                    {e.name}/
                  </button>
                ) : (
                  <span className={e.is_symlink ? 'text-neutral-400 italic' : ''}>{e.name}</span>
                )}
              </Td>
              {/* A directory's st_size is filesystem bookkeeping, not the size
                  of what it holds, so showing it would be a number that means
                  nothing to the person reading it. */}
              <Td>{e.is_dir ? '—' : bytes(e.size)}</Td>
              <Td className="text-neutral-500">{e.mode}</Td>
              <Td className="text-neutral-500">{e.mod_time.replace('T', ' ').replace('Z', '')}</Td>
            </tr>
          ))}
        </tbody>
      </table>
      {listing.entries.length === 0 ? <Empty>empty directory</Empty> : null}
      {listing.truncated ? (
        <p className="pt-3 text-xs text-amber-400">
          showing {listing.entries.length} of {listing.total} entries
        </p>
      ) : null}
    </Panel>
  )
}

/* -------------------------------------------------------------------------- */

function Processes({ data }: { data: ProcessList | string }) {
  if (typeof data === 'string') {
    return (
      <Panel title="processes">
        <Unavailable message={data} />
      </Panel>
    )
  }
  if (!data.available) {
    return (
      <Panel title="processes">
        <Unavailable message="no procfs in this workspace — expected on the local driver" />
      </Panel>
    )
  }

  return (
    <Panel title="processes" actions={<span className="text-xs text-neutral-500">{data.processes.length}</span>}>
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

/* -------------------------------------------------------------------------- */

function Resources({ data }: { data: ResourceUsage | string }) {
  if (typeof data === 'string') {
    return (
      <Panel title="resources">
        <Unavailable message={data} />
      </Panel>
    )
  }

  return (
    <Panel
      title="resources"
      actions={
        data.sampled_at ? (
          <span className="text-xs text-neutral-500">
            sampled {data.sampled_at.replace('T', ' ').replace('Z', '')}
          </span>
        ) : null
      }
    >
      <div className="grid grid-cols-1 gap-6 sm:grid-cols-2 lg:grid-cols-4">
        <Metric
          label="cpu"
          available={data.cpu.available}
          value={`${data.cpu.cores.toFixed(2)} cores`}
          sub={data.cpu.limit > 0 ? `of ${data.cpu.limit.toFixed(1)}` : 'no quota'}
          history={data.cpu.history}
        />
        {/* A container started without --memory has no quota, and "of 0 MB" is
            not a smaller number than the usage -- it is a missing one. Say so,
            the way CPU does. */}
        <Metric
          label="memory"
          available={data.memory.available}
          value={bytes(data.memory.used_mb * 1024 * 1024)}
          sub={data.memory.limit_mb > 0 ? `of ${bytes(data.memory.limit_mb * 1024 * 1024)}` : 'no quota'}
          history={data.memory.history}
        />
        <Metric
          label="disk"
          available={data.disk.available}
          value={bytes(data.disk.used_mb * 1024 * 1024)}
          sub={`of ${bytes(data.disk.total_mb * 1024 * 1024)}`}
        />
        <Metric
          label="network"
          available={data.network.available}
          value={`${bytes(data.network.rx_bytes_per_sec)}/s in`}
          sub={`${bytes(data.network.tx_bytes_per_sec)}/s out`}
          history={data.network.history}
        />
      </div>
    </Panel>
  )
}

/**
 * `available: false` is rendered as "unknown", never as a zero. The local driver
 * on darwin has no cgroups, and a confident 0% there would be the console
 * stating something false rather than admitting it could not look.
 */
function Metric({
  label,
  available,
  value,
  sub,
  history,
}: {
  label: string
  available: boolean
  value: string
  sub?: string
  history?: Array<number>
}) {
  return (
    <div className="space-y-1">
      <div className="text-xs tracking-wide text-neutral-500 uppercase">{label}</div>
      {available ? (
        <>
          <div className="font-mono text-lg text-neutral-100">{value}</div>
          {sub ? <div className="font-mono text-xs text-neutral-500">{sub}</div> : null}
          {history && history.length > 1 ? <Sparkline data={history} /> : null}
        </>
      ) : (
        <div className="font-mono text-lg text-neutral-600">unknown</div>
      )}
    </div>
  )
}

function Unavailable({ message }: { message: string }) {
  return <p className="text-sm text-neutral-500">{message}</p>
}

function Empty({ children }: { children: React.ReactNode }) {
  return <p className="text-sm text-neutral-600">{children}</p>
}
