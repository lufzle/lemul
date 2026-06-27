import { createFileRoute, notFound, useRouter } from '@tanstack/react-router'
import { useState } from 'react'

import { type DirListing, listDir } from '#/lib/control-plane'
import { getFlags } from '#/lib/flags'
import { Panel, Td, Th, bytes, useAutoRefresh } from '#/components/ui'

/**
 * Listings only -- names, sizes, modes, mtimes. File CONTENTS deliberately do
 * not travel this path: bodies through the relay would contradict §2.7's claim
 * that we cannot read session content, and being structured and addressable
 * they would be a better target than the PTY stream ever was.
 */
export const Route = createFileRoute('/_authenticated/orgs/$org/workspaces/$wid/filesystem')({
  loader: async ({ params }) => {
    // Before any fetch. Gating the tab alone would leave the URL reachable,
    // which makes the flag decoration rather than a control.
    const flags = await getFlags()
    if (!flags.explorer) throw notFound()
    return {
      org: params.org,
      wid: params.wid,
      files: await listDir({ data: { org: params.org, wid: params.wid, path: '/' } }).catch(
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
  component: Filesystem,
})

function Filesystem() {
  const { org, wid, files } = Route.useLoaderData()
  useAutoRefresh()
  return <Files org={org} wid={wid} data={files} />
}

function Files({ org, wid, data }: { org: string; wid: string; data: DirListing | string }) {
  const router = useRouter()
  const [path, setPath] = useState('/')
  const [listing, setListing] = useState<DirListing | string>(data)
  const [busy, setBusy] = useState(false)

  // The loader supplies the root; navigating deeper is a client-side fetch so a
  // directory change does not re-run the process and resource panels too.
  async function go(next: string) {
    setBusy(true)
    try {
      setListing(await listDir({ data: { org, wid, path: next } }))
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
      <Panel>
        <Unavailable message={listing} />
      </Panel>
    )
  }

  const parent = path === '/' ? null : path.slice(0, path.lastIndexOf('/')) || '/'

  return (
    <Panel
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

function Unavailable({ message }: { message: string }) {
  return <p className="text-sm text-neutral-500">{message}</p>
}

function Empty({ children }: { children: React.ReactNode }) {
  return <p className="text-sm text-neutral-600">{children}</p>
}
