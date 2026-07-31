import { createServerFn } from '@tanstack/react-start'

/**
 * The console's only door to the control plane.
 *
 * Every call is a server function, deliberately. Route loaders in TanStack Start
 * are isomorphic -- they run in the browser too -- so a loader that fetched the
 * control plane directly would put its address (and, once Phase 3 adds one, its
 * credential) in the client bundle and require the API to be reachable from the
 * operator's browser. Going through the server keeps the control plane reachable
 * only from the console process, which is the same posture as binding to
 * loopback.
 *
 * Read at call time, never at module scope: a non-VITE_ env var read at the top
 * level of a shared module can be pulled into the client bundle.
 */
function apiBase(): string {
  return process.env.LEMUL_API ?? 'http://127.0.0.1:9000'
}

/**
 * Every call to the control plane, carrying the caller's access token.
 *
 * The token is what makes authorisation structural here rather than a check
 * someone has to remember. Server functions are RPC endpoints reachable by a
 * direct POST regardless of which route renders the UI, so a route guard
 * protects nothing — but a server function cannot reach the control plane
 * without a token, and the token only exists if the session cookie does. The Go
 * side then validates it independently (internal/auth), so there are two
 * layers and neither trusts the other.
 *
 * The import is deferred, not top-level: logto.server.ts reaches into
 * `@tanstack/react-start/server`, and a module-scope import of it from a file
 * that routes import fails the client build outright.
 */
async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const { accessToken } = await import('./logto.server')
  const token = await accessToken()
  const res = await fetch(apiBase() + path, {
    ...init,
    headers: {
      ...init?.headers,
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
  })
  const body = await res.text()
  if (!res.ok) {
    // The control plane's errors are the useful part -- a 424 carries the
    // Bedrock advice, a 409 says what to do instead. Surfacing the status alone
    // would throw away the whole point of those messages.
    throw new Error(`${res.status} ${res.statusText}: ${body.trim()}`)
  }
  return body ? (JSON.parse(body) as T) : (undefined as T)
}

export type Status = {
  tenant_id: string
  runners: number
  inference: string
  telemetry: boolean
  bedrock_preflight: boolean
  image?: string
  pins?: string
  now: string
}

export type Workspace = {
  id: string
  tenant_id: string
  status: string
  generation: number
  task_ref?: string
  connected: boolean
  sessions: number
  running: number
}

export type Session = {
  id: string
  status: string
  created_at: string
  attachers: number
  rows?: number
  cols?: number
}

export type PreflightModel = {
  role: string
  model_id: string
  required: boolean
  authorization: string
  invocable: boolean
  error_code?: string
  error?: string
  advice?: string
}

export type Preflight = {
  workspace_id: string
  available: boolean
  report?: {
    region?: string
    checked_at?: string
    skipped?: boolean
    blocking: boolean
    models?: Array<PreflightModel>
  }
}

const wid = (d: unknown) => {
  if (typeof d !== 'object' || d === null || typeof (d as any).wid !== 'string') {
    throw new Error('wid is required')
  }
  return d as { wid: string }
}

const sid = (d: unknown) => {
  if (typeof d !== 'object' || d === null || typeof (d as any).sid !== 'string') {
    throw new Error('sid is required')
  }
  return d as { sid: string; force?: boolean }
}

export const getStatus = createServerFn({ method: 'GET' }).handler(async () =>
  api<Status>('/v1/status'),
)

export const listWorkspaces = createServerFn({ method: 'GET' }).handler(async () => {
  const out = await api<{ workspaces: Array<Workspace> | null }>('/v1/workspaces')
  return out.workspaces ?? []
})

export const listSessions = createServerFn({ method: 'GET' })
  .validator(wid)
  .handler(async ({ data }) => {
    const out = await api<{ sessions: Array<Session> | null }>(
      `/v1/workspaces/${encodeURIComponent(data.wid)}/sessions`,
    )
    return out.sessions ?? []
  })

/**
 * Preflight is fetched tolerantly. A workspace whose task has never dialled in
 * has no report at all, and that is a normal state rather than an error -- the
 * console must be able to render a workspace it cannot yet say anything about.
 */
export const getPreflight = createServerFn({ method: 'GET' })
  .validator(wid)
  .handler(async ({ data }) => {
    try {
      return await api<Preflight>(`/v1/workspaces/${encodeURIComponent(data.wid)}/preflight`)
    } catch {
      return null
    }
  })

export type Endpoint = {
  transport: string
  address: string
  credential: string
  peer_pubkey?: string
}

/**
 * Mints an attach endpoint for the read-only viewer.
 *
 * The credential is single-use with a two-minute TTL, so this is fetched per
 * connection rather than cached -- and it is a POST-style side effect despite
 * reading like a getter, which is why it is not folded into a route loader that
 * an auto-refresh would re-run.
 *
 * Note what this does NOT do: the WebSocket itself goes browser -> control
 * plane, not through this process. A byte stream cannot usefully be tunnelled
 * through an RPC boundary, and proxying it would put the console on the session
 * data path -- the exact position §2.7 spends a phase getting us out of. So the
 * viewer needs the control plane reachable from the operator's browser, which on
 * a loopback console it is.
 */
export const getViewerEndpoint = createServerFn({ method: 'POST' })
  .validator(sid)
  .handler(async ({ data }) =>
    api<Endpoint>(`/v1/sessions/${encodeURIComponent(data.sid)}/endpoint`),
  )

export const createSession = createServerFn({ method: 'POST' })
  .validator(wid)
  .handler(async ({ data }) =>
    api<{ id: string }>(`/v1/workspaces/${encodeURIComponent(data.wid)}/sessions`, {
      method: 'POST',
    }),
  )

export type SessionState = {
  id: string
  workspace_id?: string
  status: string
}

export const stopSession = createServerFn({ method: 'POST' })
  .validator(sid)
  .handler(async ({ data }) =>
    api<SessionState>(
      `/v1/sessions/${encodeURIComponent(data.sid)}/stop${data.force ? '?force=1' : ''}`,
      { method: 'POST' },
    ),
  )

export const resumeSession = createServerFn({ method: 'POST' })
  .validator(sid)
  .handler(async ({ data }) =>
    api<SessionState>(`/v1/sessions/${encodeURIComponent(data.sid)}/resume`, { method: 'POST' }),
  )

// DELETE answers 204 with no body, so there is nothing to hand back but the
// fact that it worked. A thrown error is how failure travels.
export const deleteSession = createServerFn({ method: 'POST' })
  .validator(sid)
  .handler(async ({ data }) => {
    await api<undefined>(
      `/v1/sessions/${encodeURIComponent(data.sid)}${data.force ? '?force=1' : ''}`,
      { method: 'DELETE' },
    )
    return { ok: true }
  })

/* ----------------------------------------------------------------------------
 * Workspace explorer (§13's data-plane observability gap).
 *
 * All reads. There is no write counterpart on the supervisor to call, which is
 * a stronger guarantee than a client that declines to offer one.
 *
 * Note what is absent: file CONTENTS. Listings carry names and sizes; bodies
 * would put customer source code through our relay in cleartext, which is the
 * claim §2.7 exists to make good on. That is an E2E decision, not a feature
 * toggle.
 * ------------------------------------------------------------------------- */

export type DirEntry = {
  name: string
  is_dir: boolean
  size: number
  mode: string
  mod_time: string
  is_symlink?: boolean
}

export type DirListing = {
  path: string
  entries: Array<DirEntry>
  total: number
  truncated: boolean
}

const widPath = (d: unknown) => {
  if (typeof d !== 'object' || d === null || typeof (d as any).wid !== 'string') {
    throw new Error('wid is required')
  }
  const v = d as { wid: string; path?: string }
  return { wid: v.wid, path: typeof v.path === 'string' ? v.path : '/' }
}

export const listDir = createServerFn({ method: 'GET' })
  .validator(widPath)
  .handler(async ({ data }) =>
    api<DirListing>(
      `/v1/workspaces/${encodeURIComponent(data.wid)}/fs?path=${encodeURIComponent(data.path)}`,
    ),
  )

export type ProcessInfo = {
  pid: number
  ppid: number
  name: string
  state: string
  rss_kb: number
  cpu_secs: number
  started_at?: string
  cmdline?: string
  session_id?: string
}

export type ProcessList = { processes: Array<ProcessInfo>; available: boolean }

export const listProcesses = createServerFn({ method: 'GET' })
  .validator(wid)
  .handler(async ({ data }) =>
    api<ProcessList>(`/v1/workspaces/${encodeURIComponent(data.wid)}/processes`),
  )

/**
 * Every group carries its own `available`. A zero where cgroups cannot be read
 * -- the local driver on darwin -- means "we could not look", not "nothing is
 * being used", and the panel has to be able to say which.
 */
export type ResourceUsage = {
  cpu: { available: boolean; cores: number; limit: number; history?: Array<number> }
  memory: { available: boolean; used_mb: number; limit_mb: number; history?: Array<number> }
  disk: { available: boolean; used_mb: number; total_mb: number; path?: string }
  network: {
    available: boolean
    rx_bytes_per_sec: number
    tx_bytes_per_sec: number
    rx_total: number
    tx_total: number
    history?: Array<number>
  }
  sampled_at?: string
}

export const getResources = createServerFn({ method: 'GET' })
  .validator(wid)
  .handler(async ({ data }) =>
    api<ResourceUsage>(`/v1/workspaces/${encodeURIComponent(data.wid)}/resources`),
  )
