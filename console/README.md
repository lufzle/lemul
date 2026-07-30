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
src/lib/control-plane.ts                every call to the Go API, as server functions
src/routes/index.tsx                    status header + workspace table
src/routes/workspaces.$wid.tsx          sessions, lifecycle actions, preflight report
src/routes/workspaces.$wid_.sessions.$sid.tsx   read-only terminal
src/components/Viewer.tsx               xterm.js over the viewer WebSocket
src/components/ui.tsx                   panel/button/dot, and the auto-refresh hook
```

The viewer route carries the `_` suffix (`$wid_`) so it does **not** nest inside
the workspace route. Without it `workspaces.$wid.tsx` silently becomes a layout
and needs an `<Outlet />`, and the child renders as the parent page instead.

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

## The read-only viewer — off by default

Behind `FF_VIEW_SESSION`, which **ships dark**:

```bash
FF_VIEW_SESSION=1 bun run dev
```

Unset, `0`, or anything unrecognised is off — a flag gating a capability should
fail closed on a typo rather than guess the operator meant yes.

It gates the **route** as well as the button. Hiding the link alone would leave
the URL reachable and make the flag decoration; with the flag off the route
answers 404 and the terminal component is never even loaded. The flag is read
server-side for the same reason: a value that ships to the browser is one an
operator can flip in devtools.

`view` on a running session opens an xterm.js terminal on the existing
`?mode=viewer` attach path — the same one `ourcli connect -mode viewer` uses, so
the browser gets no capability the CLI did not already have.

**Read-only is enforced on the server, three times over.** The relay drops input
frames from a viewer connection and the supervisor drops them again on arrival
(§2.5), because both attachers write the same PTY stdin and a viewer that can
write is silently a co-driver. `disableStdin` in the browser is cosmetic — it
stops the cursor inviting typing, and nothing more.

**It never resizes the session.** The PTY has one size, so honouring the browser
window would reflow the *controller's* Claude Code. The terminal is built at the
session's existing geometry and the panel scrolls if that does not fit.

Two things that are true and worth knowing:

- **The WebSocket goes browser → control plane directly**, not through this
  process. A byte stream cannot usefully be tunnelled through an RPC boundary,
  and proxying it would put the console on the session data path — the exact
  position §2.7 spends a phase getting us *out* of. So the viewer needs the
  control plane reachable from the operator's browser, which on a loopback
  console it is. Only the JSON API goes through server functions.
- **Attaching a viewer nudges the session to repaint**, which the person driving
  it sees as a brief reflow. That is how a late joiner gets the current screen
  without a VT state model (decision #4); the Phase 2 model removes the need.

This is a *viewer*, not the Phase 4 web client (§11) — that one is the Agent SDK
against the same sandbox, and a different product surface.

## What it deliberately does not do

- **No driving from the browser.** Taking control stays in `ourcli`. Input from
  the browser would need the CSRF question `internal/controlplane/server.go`
  currently sets aside (`CheckOrigin` accepts every origin) to be answered first.
- **No workspace create/delete.** Workspaces are still created on demand by their
  first session; the CRUD API is not built yet.
