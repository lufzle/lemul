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
  /** The deployment, not an organization: one cell serves many. */
  cell_id: string
  inference: string
  gateway_url?: string
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
  owner?: string
  owner_name?: string
  /** Who may use this workspace: owner | members | org. */
  access_scope: string
  /** Section 2.4's lifecycle policy: idle stop, warm hold, admission. */
  policy: WorkspacePolicy
  /**
   * Set while the task is up with no session left in it — section 2.4's grace
   * period before it scales to zero. Its presence IS the warm-hold state:
   * there is deliberately no such workspace status, so a status of `active`
   * with this set is what "nobody is using it, and we are still paying" looks
   * like.
   */
  warm_hold_until?: string
  /**
   * Set while this workspace would be stopping and the only thing holding it
   * open is a background process inside a session — a dev server somebody left
   * running, typically.
   *
   * It changes no behaviour: that process was started deliberately, and
   * stopping the session would kill it. It is here so the cost is visible,
   * because the failure mode is nobody noticing rather than anybody deciding.
   */
  idle_pinned_since?: string
  /**
   * Why the LAST placement attempt failed, absent when it did not.
   *
   * It is the whole reason placement could move to create: a background
   * operation has no response left to fail on, so without this a workspace that
   * never started is indistinguishable from one that is merely stopped. It
   * describes ONE attempt — every status write clears it — so a message here is
   * always about the state the row is in now.
   */
  last_error?: string
  created_at?: string
}

export type WorkspacePolicy = {
  /** 0 disables the idle stop for this workspace. */
  idle_timeout_secs?: number
  warm_hold_secs?: number
  /** unlimited | max_sessions | min_free_memory_pct */
  admission_policy?: string
  max_sessions?: number
  min_free_memory_pct?: number
}

export type Session = {
  id: string
  status: string
  created_at: string
  owner?: string
  owner_name?: string
  /**
   * Whether the SIGNED-IN user started this session.
   *
   * Only their own may be driven; anything else is watchable at best (§2.5),
   * and the control plane refuses a control credential for it. Reported by the
   * server because nothing else tells the console who it is — there is no
   * endpoint that hands back your own user id on its own.
   */
  mine?: boolean
  attachers: number
  rows?: number
  cols?: number
}

export type Member = {
  user_id: string
  email?: string
  role: string
  /** The signed-in user's own row. */
  me?: boolean
}

/**
 * Everything an organization owns hangs off /v1/orgs/{org}.
 *
 * The control plane never assumes one — the organization is what the database
 * transaction is scoped to, so it has to be named before any row is read. That
 * makes the org a required input to every call below rather than ambient state,
 * which is deliberate: ambient state is what lets a UI act on the wrong
 * customer after a switcher click that half-applied.
 */
const orgPath = (org: string, suffix: string) =>
  `/v1/orgs/${encodeURIComponent(org)}${suffix}`

const str = (d: unknown, key: string): string => {
  if (typeof d !== 'object' || d === null || typeof (d as any)[key] !== 'string') {
    throw new Error(`${key} is required`)
  }
  return (d as any)[key] as string
}

const wid = (d: unknown) => ({ org: str(d, 'org'), wid: str(d, 'wid') })

const sid = (d: unknown) => ({
  org: str(d, 'org'),
  sid: str(d, 'sid'),
  force: (d as any)?.force === true,
})

const orgOnly = (d: unknown) => ({ org: str(d, 'org') })

export const getStatus = createServerFn({ method: 'GET' }).handler(async () =>
  api<Status>('/v1/status'),
)

export type Org = {
  slug: string
  name: string
  /** The CALLER's role here: owner or user. */
  role: string
  /** The organization created for this user at sign-up. */
  personal: boolean
}

/**
 * The one call that is not scoped to an organization, because it is the
 * question asked before one can be named. It backs the switcher.
 */
export const listOrgs = createServerFn({ method: 'GET' }).handler(async () => {
  const out = await api<{ orgs: Array<Org> | null }>('/v1/orgs')
  return out.orgs ?? []
})

export const listWorkspaces = createServerFn({ method: 'GET' })
  .validator(orgOnly)
  .handler(async ({ data }) => {
    const out = await api<{ workspaces: Array<Workspace> | null }>(
      orgPath(data.org, '/workspaces'),
    )
    return out.workspaces ?? []
  })

export const getWorkspace = createServerFn({ method: 'GET' })
  .validator(wid)
  .handler(async ({ data }) =>
    api<Workspace>(orgPath(data.org, `/workspaces/${encodeURIComponent(data.wid)}`)),
  )

export const listSessions = createServerFn({ method: 'GET' })
  .validator(wid)
  .handler(async ({ data }) => {
    const out = await api<{ sessions: Array<Session> | null }>(
      orgPath(data.org, `/workspaces/${encodeURIComponent(data.wid)}/sessions`),
    )
    return out.sessions ?? []
  })


export type Endpoint = {
  transport: string
  address: string
  credential: string
  /** What the credential actually authorises, which need not be what was asked. */
  mode?: string
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
 *
 * `mode=viewer` is requested HERE, not on the WebSocket URL. It travels inside
 * the signed credential, so input-dropping is enforced against something the
 * browser cannot rewrite -- a mode in the attach query string would be asserted
 * by the very party it constrains (§2.5).
 */
export const getViewerEndpoint = createServerFn({ method: 'POST' })
  .validator(sid)
  .handler(async ({ data }) =>
    api<Endpoint>(
      orgPath(data.org, `/sessions/${encodeURIComponent(data.sid)}/endpoint?mode=viewer`),
    ),
  )

/* ----------------------------------------------------------------------------
 * Workspace CRUD and membership (§2.5, §2.6). None of it existed before Phase 4:
 * a workspace appeared as a side effect of asking for a session in one.
 * ------------------------------------------------------------------------- */

const createWs = (d: unknown) => ({
  org: str(d, 'org'),
  // null asks the SERVER to generate a name. Absent and null are different in
  // the API — absent is "you forgot", null is "you pick" — so this is
  // deliberately `string | null` rather than an optional field.
  name: typeof (d as any)?.name === 'string' ? ((d as any).name as string) : null,
  // Absent defaults to `owner` on the server, which is the closed answer. Sent
  // explicitly anyway: the create form always has a radio selected, so an
  // omitted scope here would mean the form and the record disagreed.
  access_scope:
    typeof (d as any)?.access_scope === 'string' ? ((d as any).access_scope as string) : 'owner',
})

/**
 * Creating a workspace PLACES its task, and answers 202 rather than 201 — the
 * record exists and the machine does not. Callers must not read the returned
 * document as "ready": its `status` is `starting`, and the row is what says when
 * that stops being true.
 */
export const createWorkspace = createServerFn({ method: 'POST' })
  .validator(createWs)
  .handler(async ({ data }) =>
    api<Workspace>(orgPath(data.org, '/workspaces'), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: data.name, access_scope: data.access_scope }),
    }),
  )

export const setAccessScope = createServerFn({ method: 'POST' })
  .validator((d: unknown) => ({ ...wid(d), access_scope: str(d, 'access_scope') }))
  .handler(async ({ data }) =>
    api<Workspace>(orgPath(data.org, `/workspaces/${encodeURIComponent(data.wid)}`), {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ access_scope: data.access_scope }),
    }),
  )

/**
 * Organization settings (§2.5). Readable by any member — a member refused a
 * workspace has to be able to see why, and this page is where an owner changes
 * it.
 */
export type OrgSettings = {
  members_can_create_workspaces: boolean
}

export const getOrgSettings = createServerFn({ method: 'GET' })
  .validator(orgOnly)
  .handler(async ({ data }) => api<OrgSettings>(orgPath(data.org, '/settings')))

export const updateOrgSettings = createServerFn({ method: 'POST' })
  .validator((d: unknown) => ({
    org: str(d, 'org'),
    // A pointer on the wire, so absent means "leave it alone" rather than
    // "false". A plain bool cannot tell a missing key from an explicit false,
    // and a PATCH that silently turns a setting off because a client did not
    // mention it is only ever found by its consequences.
    members_can_create_workspaces:
      typeof (d as any)?.members_can_create_workspaces === 'boolean'
        ? ((d as any).members_can_create_workspaces as boolean)
        : undefined,
  }))
  .handler(async ({ data }) =>
    api<OrgSettings>(orgPath(data.org, '/settings'), {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        members_can_create_workspaces: data.members_can_create_workspaces,
      }),
    }),
  )

export const renameWorkspace = createServerFn({ method: 'POST' })
  .validator((d: unknown) => ({ ...wid(d), name: str(d, 'name') }))
  .handler(async ({ data }) =>
    api<Workspace>(orgPath(data.org, `/workspaces/${encodeURIComponent(data.wid)}`), {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: data.name }),
    }),
  )

// force is what the confirmation dialog buys: the server refuses a workspace
// holding sessions without it, because deleting one drops their conversations.
export const deleteWorkspace = createServerFn({ method: 'POST' })
  .validator((d: unknown) => ({ ...wid(d), force: (d as any)?.force === true }))
  .handler(async ({ data }) => {
    await api<undefined>(
      orgPath(
        data.org,
        `/workspaces/${encodeURIComponent(data.wid)}${data.force ? '?force=1' : ''}`,
      ),
      { method: 'DELETE' },
    )
    return { ok: true }
  })

/** Everyone in the organization — the only place a client can learn a user id. */
export const listOrgMembers = createServerFn({ method: 'GET' })
  .validator(orgOnly)
  .handler(async ({ data }) => {
    const out = await api<{ members: Array<Member> | null }>(orgPath(data.org, '/members'))
    return out.members ?? []
  })

export const listWorkspaceMembers = createServerFn({ method: 'GET' })
  .validator(wid)
  .handler(async ({ data }) => {
    const out = await api<{ members: Array<Member> | null }>(
      orgPath(data.org, `/workspaces/${encodeURIComponent(data.wid)}/members`),
    )
    return out.members ?? []
  })

export const addWorkspaceMember = createServerFn({ method: 'POST' })
  .validator((d: unknown) => ({ ...wid(d), userId: str(d, 'userId'), role: str(d, 'role') }))
  .handler(async ({ data }) =>
    api<Member>(
      orgPath(
        data.org,
        `/workspaces/${encodeURIComponent(data.wid)}/members/${encodeURIComponent(data.userId)}`,
      ),
      {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ role: data.role }),
      },
    ),
  )

export const removeWorkspaceMember = createServerFn({ method: 'POST' })
  .validator((d: unknown) => ({ ...wid(d), userId: str(d, 'userId') }))
  .handler(async ({ data }) => {
    await api<undefined>(
      orgPath(
        data.org,
        `/workspaces/${encodeURIComponent(data.wid)}/members/${encodeURIComponent(data.userId)}`,
      ),
      { method: 'DELETE' },
    )
    return { ok: true }
  })

export const createSession = createServerFn({ method: 'POST' })
  .validator(wid)
  .handler(async ({ data }) =>
    api<{ id: string }>(orgPath(data.org, `/workspaces/${encodeURIComponent(data.wid)}/sessions`), {
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
      orgPath(data.org, `/sessions/${encodeURIComponent(data.sid)}/stop${data.force ? '?force=1' : ''}`),
      { method: 'POST' },
    ),
  )

export const resumeSession = createServerFn({ method: 'POST' })
  .validator(sid)
  .handler(async ({ data }) =>
    api<SessionState>(orgPath(data.org, `/sessions/${encodeURIComponent(data.sid)}/resume`), {
      method: 'POST',
    }),
  )

// DELETE answers 204 with no body, so there is nothing to hand back but the
// fact that it worked. A thrown error is how failure travels.
export const deleteSession = createServerFn({ method: 'POST' })
  .validator(sid)
  .handler(async ({ data }) => {
    await api<undefined>(
      orgPath(data.org, `/sessions/${encodeURIComponent(data.sid)}${data.force ? '?force=1' : ''}`),
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

const widPath = (d: unknown) => ({
  org: str(d, 'org'),
  wid: str(d, 'wid'),
  path: typeof (d as any)?.path === 'string' ? ((d as any).path as string) : '/',
})

export const listDir = createServerFn({ method: 'GET' })
  .validator(widPath)
  .handler(async ({ data }) =>
    api<DirListing>(
      orgPath(
        data.org,
        `/workspaces/${encodeURIComponent(data.wid)}/fs?path=${encodeURIComponent(data.path)}`,
      ),
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
    api<ProcessList>(orgPath(data.org, `/workspaces/${encodeURIComponent(data.wid)}/processes`)),
  )

/**
 * Every group carries its own `available`. A zero where cgroups cannot be read
 * -- the local driver on darwin -- means "we could not look", not "nothing is
 * being used", and the panel has to be able to say which.
 */
export type ResourceUsage = {
  cpu: {
    available: boolean
    cores: number
    limit: number
    cpus: number
    throttled_percent: number
    history?: Array<number>
  }
  memory: {
    available: boolean
    anon_mb: number
    cache_mb: number
    used_mb: number
    limit_mb: number
    total_mb: number
    oom_kills: number
    history?: Array<number>
  }
  disk: {
    available: boolean
    used_mb: number
    total_mb: number
    path?: string
    inodes_used_percent: number
  }
  network: {
    available: boolean
    rx_bytes_per_sec: number
    tx_bytes_per_sec: number
    rx_total: number
    tx_total: number
    errors: number
    drops: number
    rx_history?: Array<number>
    tx_history?: Array<number>
  }
  sampled_at?: string
}

export const getResources = createServerFn({ method: 'GET' })
  .validator(wid)
  .handler(async ({ data }) =>
    api<ResourceUsage>(orgPath(data.org, `/workspaces/${encodeURIComponent(data.wid)}/resources`)),
  )

/**
 * Relay the ID token so the control plane learns who the signed-in user is.
 *
 * The console is the only component that holds one. The access token it
 * presents to the API is minted for an API resource and carries a subject and
 * no identity claims at all — which is OAuth working as designed, not a gap —
 * so an email can only arrive as a signed assertion from the identity provider.
 *
 * This used to send a self-asserted `label`, which was display-only precisely
 * because anyone could send anything. A verified ID token is not: the control
 * plane checks its signature, its audience, and that its subject matches the
 * access token's, which is what stops a caller writing somebody else's address
 * onto their own record.
 *
 * It also names the caller's own organization. Sign-up happens on a request
 * carrying only an access token, so until this runs the organization is named
 * after its own slug.
 */
export const registerIdentity = createServerFn({ method: 'POST' }).handler(async () => {
  const { idToken } = await import('./logto.server')
  const token = await idToken()
  if (!token) return { ok: false as const }
  const out = await api<{ email?: string; org?: string; org_name?: string }>('/v1/identity', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ id_token: token }),
  })
  return { ok: true as const, ...out }
})
