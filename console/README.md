# lemul console

The operator console: what the control plane thinks is going on, and the session
lifecycle verbs. TanStack Start (SSR) on Bun.

```bash
bun install
bun run dev            # http://127.0.0.1:3000
LEMUL_API=http://127.0.0.1:9000 bun run dev   # if the control plane is elsewhere
```

It needs a running control plane; it holds no state of its own.

## No auth — this binds to loopback on purpose

Phase 1 has no authentication anywhere, and `CC_REMOTE_ANALYSIS.md` §2.5 records
that `GET /v1/sessions/{sid}/endpoint` will mint an attach credential for **any**
session id to anyone who can reach it. This console is a thin skin over that API,
so exposing it is exactly as bad as exposing the control plane.

`vite.config.ts` therefore pins `server.host` and `preview.host` to `127.0.0.1`.
That is a deliberate control, not a default — do not widen it before Phase 3
lands the user model.

## Shape

```
src/lib/control-plane.ts         every call to the Go API, as server functions
src/routes/index.tsx             status header + workspace table
src/routes/workspaces.$wid.tsx   sessions, lifecycle actions, preflight report
src/components/ui.tsx            panel/button/dot, and the auto-refresh hook
```

**Everything goes through a server function.** TanStack Start route loaders are
isomorphic — they run in the browser too — so a loader fetching the control plane
directly would put its address in the client bundle and require the API to be
reachable from the operator's browser. Routing through the server keeps the
control plane reachable only from this process, which is what makes the loopback
binding meaningful. (Verified against a production build: the client bundle
contains no `process.env` and no control-plane URL.)

Freshness is `router.invalidate()` on an interval rather than client-side
polling, so first render and refresh use one code path. It pauses while the tab
is hidden — a backgrounded console should not keep asking the supervisor to
enumerate PTYs forever.

## What it shows that is worth knowing

The workspace table reports the stored record **and** whether a task is actually
holding a tunnel, because those are different facts. A workspace recorded
`active` with no task is a task that died, and that is the state worth seeing —
collapsing the two would hide it.

The preflight panel keeps §12.3's three states apart: no report means the task
has not checked in (absence is not failure), `skipped` means the workspace is not
using Bedrock, and only a blocking report is a problem. A failing model shows the
advice attached to it, since an IAM gap, a missing inference-profile prefix and
an unfinished First Time Use form all look alike and have nothing in common.

## What it deliberately does not do

- **No terminal.** Attaching stays in `ourcli`; the console moves the *process*,
  not your terminal. A browser terminal is Phase 4 (§11) and reopens the
  `CheckOrigin`/CSRF question `internal/controlplane/server.go` sets aside.
- **No workspace create/delete.** Workspaces are still created on demand by their
  first session; the CRUD API is not built yet.
