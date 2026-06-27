# lemul console

The operator console: what the control plane thinks is going on, and the session
lifecycle verbs. TanStack Start (SSR) on Bun.

```bash
bun install
bun run dev            # http://127.0.0.1:3000
LEMUL_API=http://127.0.0.1:9000 bun run dev   # if the control plane is elsewhere
```

It needs a running control plane; it holds no state of its own.

## Sign-in

Passwordless email through Logto — see [`../auth-stack`](../auth-stack/README.md).
Start the stack, seed it, and run the console with the generated variables:

```bash
set -a; . ../auth-stack/.env.generated; set +a
bun run dev
```

`LOGTO_*` is not optional. Without it the console has no token to present, and
the control plane — which now refuses to start without an issuer of its own —
answers every call 401.

**Where the security boundary actually is.** Not the `_authenticated` route
guard — server functions are RPC endpoints reachable by direct POST no matter
what the router is showing, so a `beforeLoad` redirect protects the page and
nothing else. What protects the data is that `lib/control-plane.ts` cannot call
the control plane without an access token, and the token only exists if the
session cookie does. The Go side then validates that token independently. Two
layers, neither trusting the other, and no per-function check anyone can forget
to attach.

## Organizations

Every page lives under `/orgs/$org`, and `/` redirects into the signed-in user's
own organization. The header carries a switcher when there is more than one.

It reads the current organization from the URL rather than from client state,
which is the reason the organization is a path segment at all: a switcher backed
by state can disagree with the page under it, and that disagreement acts on the
wrong customer's workspace. Switching *is* navigating.

## Authorisation, and where it is actually bound

Authorisation is real at both levels now. A caller who is not a member of the
`{org}` in the URL gets a 404 and the database refuses independently through
row-level security; inside one organization, a caller with no membership of a
workspace gets 404 on every route under it, the explorer tabs included. A
session's keyboard belongs to whoever started it (§2.5): `view` opens a
**read-only** terminal, and the control plane refuses a control credential for
somebody else's session however it is asked for.

`vite.config.ts` still pins `server.host` and `preview.host` to `127.0.0.1`,
and **that governs the dev and preview servers only**. Neither runs in a
deployment: [`deploy/`](../deploy) serves the BUILT SSR bundle, which binds
`0.0.0.0` inside its container and sits behind Caddy with a real certificate.
So loopback is no longer the boundary anywhere it matters, and has not been
since 2026-08-03 — the boundary is the authorisation described above plus the
security group.

Its comment claiming the console has "no authentication of any kind" is older
still and simply wrong; the header banner that repeated it is gone for the same
reason. A warning that overstates is one nobody reads.

## Shape

```
src/lib/control-plane.ts                every call to the Go API, as server functions
src/routes/_authenticated.tsx           sign-in boundary, org list, org switcher
src/routes/_authenticated/index.tsx     redirect into the caller's own org
src/routes/_authenticated/orgs.$org.index.tsx           status header + workspace table
src/routes/_authenticated/orgs.$org.new-workspace.tsx   the create form: name + access scope
src/routes/_authenticated/orgs.$org.settings.tsx        organization settings + roster
src/routes/_authenticated/orgs.$org.workspaces.$wid.tsx workspace shell and its tabs
src/routes/_authenticated/orgs.$org.workspaces.$wid.members.tsx  who can reach it
src/routes/_authenticated/orgs.$org.workspaces.$wid_.sessions.$sid.tsx  read-only terminal
src/components/Viewer.tsx               xterm.js over the viewer WebSocket
src/components/ui.tsx                   panel/button/dot, and the auto-refresh hook
```

The viewer route carries the `_` suffix (`$wid_`) so it does **not** nest inside
the workspace route. Without it `workspaces.$wid.tsx` silently becomes a layout
and needs an `<Outlet />`, and the child renders as the parent page instead.

Creating a workspace is at `/orgs/$org/new-workspace`, not the tidier
`/orgs/$org/workspaces/new`. `workspaces/$wid` is a real route, a static segment
wins the match, and `new` is a legal workspace name — so the tidier URL would
make a workspace called `new` permanently unreachable here.

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

**The state column came back**, having once been dropped as noise. It was noise
while the only thing that placed a task was somebody waiting for one; placement
now happens at **create**, so a workspace spends its first 20–60 s coming up with
nobody having asked for a session, and a table showing only session counts left
that invisible. It distinguishes four readings the record's `status` cannot make
on its own: `starting`, **could not start** (`last_error` is set — the row did
not merely stop, it tried and failed, and this is the state that would otherwise
be silent), **no task** (recorded `active` with no tunnel), and **warm hold**,
derived rather than stored because a fourth status value is a value every
comparison would have had to learn.

The freshness that makes provisioning legible is the existing 4 s
`router.invalidate()`, not the `/events` stream. The stream exists for `lem`,
which has no loader to re-run and was staring at a blank terminal; the console
already had one refresh path and a second one for a single column would be two
ways of learning the same two fields.

The row also carries a **held open** warning, from `idle_pinned_since`. It means
this workspace would have stopped by now and the only thing keeping it up is a
background process inside a session — a dev server somebody left running,
typically. It is a notice and not an action: that process was started on
purpose, and stopping the session would kill it (§2.4). What was missing was
never enforcement, it was anyone being able to see that a machine is still being
paid for, which is why the caption says how long rather than merely that it is
happening.

The preflight panel keeps §12.3's three states apart: no report means the task
has not checked in (absence is not failure), `skipped` means the workspace is not
using Bedrock, and only a blocking report is a problem. A failing model shows the
advice attached to it, since an IAM gap, a missing inference-profile prefix and
an unfinished First Time Use form all look alike and have nothing in common.

## Membership is a console-only surface

Adding somebody to a workspace, changing their role and removing them live on
the **members** tab and nowhere else — there are deliberately no `lem` verbs for
it. It is an administrative act done occasionally from a screen that can show
who is already there, not something to type.

**This tab is what `access_scope = members` reads.** A workspace is a shared
machine (§2.3), and `access_scope` says who may use one: `owner` — which is what
a "personal" workspace now is — `members`, meaning the rows on this tab, or
`org`, meaning anybody in the organization. The overview's ACCESS column renders it as a
sentence — "owner only", "named members", "everyone here" — rather than the
stored token, because what a person wants to know on a shared machine is whether
their colleagues are in here too, and `org` reads as a category rather than an
answer. The create form asks the same question up front, which is the reason it
replaced a `window.prompt` for the name: a prompt that asks only for a name
creates every workspace at `owner`, and on a product whose premise is a shared
machine that is the wrong default to make invisible.

The picker offers the organization's own members and nothing else, because the
control plane refuses anyone outside it. That check is load-bearing rather than
tidy: `workspace_member`'s `WITH CHECK` is scoped to the tenant and nothing
else, so without it any uuid at all could be written in and would then satisfy
every policy that reads it.

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
`?mode=viewer` attach path — the same one `lem --session <id> --viewer` uses, so
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

This is a *viewer*, not the web client of §11 — that one is the Agent SDK
against the same sandbox, and a different product surface.

## What it deliberately does not do

- **No driving from the browser.** Taking control stays in `lem`. Input from
  the browser would need the CSRF question `internal/relay/relay.go`
  currently sets aside (`CheckOrigin` accepts every origin) to be answered first.
- **No renaming a workspace with a live task.** The server refuses it, and the
  reason is worth knowing rather than working around: the tunnel registry, the
  supervisor's argv, the preflight store and the derived workspace credential
  are all keyed on the **name**, so a rename under a running task orphans its
  tunnel and 401s its next dial.
- **No instance-size control on the create form**, and its absence is a
  decision rather than an omission. The task spec carries CPU and memory and the
  runner forwards them, but the control plane never populates either, the `ecs`
  driver ignores both and `docker` honours only memory — so every option a
  dropdown could offer would be a promise nothing keeps. The failure mode of
  shipping it anyway is a customer whose 16 GiB workspace OOMs like a 2 GiB one.
  Same reasoning as horizontal scaling, which is not offered because one
  workspace is one filesystem (§12.10).

## The workspace explorer — off by default

Behind `FF_WORKSPACE_EXPLORER`, which also **ships dark**:

```bash
FF_WORKSPACE_EXPLORER=1 bun run dev
```

The workspace page is a tabbed shell — **overview**, **sessions**, and behind
this flag **filesystem** and **processes**. It exists for §13's largest standing
risk, losing observability into the data plane: until now the only way to see
inside a task was to attach to a session and type, which means co-driving
somebody's live Claude Code.

Only the last two are gated. Resource counters are not sensitive the way file
paths and command lines are, so overview and sessions are always available.

Gates the routes as well as the tabs, for the same reason the viewer does: a
hidden tab whose URL still works is decoration.

**`workspaces.$wid.tsx` is a layout route**, so its body lives in
`workspaces.$wid.index.tsx`. A child route turns the parent into a layout, and
without that split the child renders *in place of* the parent page, tabs and
all. The overview tab also needs `activeOptions={{ exact: true }}`: its path is
a prefix of every other tab's, so without it two tabs light up at once.

**Why it ships dark was authorisation, and that half is now closed.** It was
written when any authenticated operator reached every workspace: attaching is at
least visible — both attachers drive the same PTY — while this reads file paths
and command lines with nothing to notice. Phase 4 landed both halves (§2.5):
owner-scoped attach, and a 404 on every `{wid}` route for a caller with no
membership, this trio included. What keeps the flag off by default is now the
**shape** of the capability rather than a missing check: a silent read across a
workspace several members share is worth turning on deliberately. Loopback plus
the flag stays the bound.

**No file contents, deliberately.** Listings carry names, sizes, modes and
mtimes. File bodies would put customer source code in cleartext through our
relay, which is exactly the claim §2.7 exists to make good on — and structured
and addressable, they would be a better target than the PTY stream ever was.
Adding them is an E2E decision, not a feature toggle.

Two things the panels are careful about:

- **`available: false` is rendered as "unknown", never as a zero.** There are no
  cgroups on darwin, where the `local` driver runs, and a confident `0%` there
  would be the console stating something false rather than admitting it could
  not look. A container with no `--memory` shows usage and `no quota` — a
  missing limit is not a limit of zero.
- **Command lines are redacted for known credential flags.** The supervisor's
  own argv carries `-token`, the workspace tunnel credential, and it rendered in
  full the first time this panel met a real task. Environments are never read at
  all (`/proc/<pid>/environ` holds the gateway key), and argv needed the same
  treatment to make that exclusion mean anything.
