# lemul-cc

Remote, sandboxed Claude Code workspaces that run in the **customer's own AWS
account** and bill to their Bedrock.

The design record is [`CC_REMOTE_ANALYSIS.md`](CC_REMOTE_ANALYSIS.md) — read §1
(decisions and the assumption register), §2 (architecture) and §12 (open
decisions) before changing anything structural. Every non-obvious choice in this
code has a rationale there, and the comments cite it by section.

**Status: Phase 0 complete. Phase 1 substantially done** — the split, sandbox
image, both local drivers, gateway inference, preflight, telemetry and session
lifecycle (stop/resume/delete) all work end to end. Remaining: the `ecs` driver +
Terraform, and idle detection / warm hold / admission gating.

## Components

```
cmd/ourcli          the user's local client. Raw mode, WSS, Ctrl-] to detach
cmd/controlplane    the only public listener: orchestrator API + relay
cmd/runner          one per tenant, in their VPC. CONTROL ONLY, never on the data path
cmd/supervisor      one per workspace task. Owns N PTYs, dials out on its own tunnel
cmd/keyprobe        diagnostic: what bytes does your terminal send for a key?
cmd/preflight       is this AWS account's Bedrock actually usable?
```

The runner/supervisor split is decision #6, *task-dials-out*. The runner places
tasks and nothing else, so it can crash, redeploy or auto-update while sessions
keep running. Each workspace task holds its own tunnel.

```
customer VPC                                    control plane
┌──────────────────────────────┐               ┌──────────────────┐
│ runner   ── control ─────────┼──── WSS ─────>│ orchestrator     │
│   ecs:RunTask/StopTask       │               │ relay            │
│                              │               │ registries       │
│ workspace task               │               └──────────────────┘
│   supervisor ── session data ┼──── WSS ─────────────^   ^
│     ├─ PTY (session 1)       │                          │
│     └─ PTY (session 2)       │              user ───────┘
└──────────────────────────────┘
```

Nothing listens inbound in the customer's account.

## Packages

| Path | What it is |
|---|---|
| `internal/ptysession` | The heart of the split. N PTYs, replay ring, **mode prelude**, SIGWINCH nudge, viewer enforcement. PTY lifetime is independent of connection lifetime — that is what detach means. |
| `internal/tunnel` | yamux over WSS, `[type:1][len:4][payload]` framing, and the control-message union. WebSocket frame types do not survive a yamux stream, so the tag is explicit. |
| `internal/proto` | The client↔relay contract: binary frames are PTY bytes, text frames are control. |
| `internal/agent` | The dial-out loop both the runner and the supervisor use. |
| `internal/driver` | Workspace runtime behind an interface: `local` (process), `docker` (the real sandbox image), `ecs` next. Exists from day one so our debugging environment shares the product's code path (§13). |
| `internal/registry` | Live tunnels. A **set** per tenant, never one — §2.8. |
| `internal/controlplane` | API handlers, the two tunnel endpoints, and the byte-transparent relay. |
| `internal/bedrock` | Three-layer model preflight. Layer 1 (availability) is advisory, layer 2 (a real `InvokeModel`) is authoritative, layer 3 maps errors to the actual fix. |
| `internal/gateway` | The supported inference path. One loopback proxy per session; the supervisor holds the gateway credential and injects attribution the session cannot forge. |
| `internal/store` | Workspaces and sessions. JSON-backed; only `Generation` genuinely needs the durability, because it feeds the ECS `client-token`. |

## Run it locally

```bash
go build -o bin/ ./cmd/...

bin/controlplane -addr :9000 -agent-token dev-token -session-cmd claude
bin/runner       -control-plane ws://localhost:9000 -token dev-token -supervisor ./bin/supervisor
bin/ourcli connect w1
```

```
ourcli connect <workspace>               reattach to an idle session, else start one
ourcli connect <workspace> -new          always start a new session
ourcli connect <workspace> -session <id> attach to a specific session
ourcli connect <workspace> -mode viewer  read-only; input is dropped
ourcli ls <workspace>                    what is in there, and who is on it
ourcli stop   <workspace> -session <id>  Ctrl-C/Ctrl-D; the conversation is kept
ourcli resume <workspace> -session <id>  start it again, history intact
ourcli rm     <workspace> -session <id>  end it and drop the conversation
```

`-session` takes any **unique prefix**. Session ids are UUIDs because Claude
Code's `--session-id` requires one, which is worth the length: our id *is* the
conversation id, so nothing maps between them (§12.7).

Bare `connect` reattaches only to a session **nobody is attached to**. Both
attachers write the same PTY stdin (§2.5), so joining an in-use session by
default would silently make two people co-drive one Claude Code — while opening
a second terminal should give you a second session, which is what the shared
filesystem is for.

Detach is `Ctrl-]`. The client recognises all three encodings a terminal may use
for it — the legacy `0x1d`, kitty `CSI 93;5u`, and modifyOtherKeys
`CSI 27;5;93~` — because Claude Code negotiates modes that stop emulators
sending the legacy one. `bin/keyprobe -cc` prints what your terminal actually
produces if detach ever stops working.

Pick a port nothing squats on. `9222` is Chrome DevTools and will silently win
for `localhost`, the same way OrbStack takes 4318.

The runner places the supervisor through the `local` driver, so this is the same
code path the ECS driver will take — only the placement substrate differs.

## Tests

```bash
go test ./... -race
LEMUL_E2E_CLAUDE=1 go test ./e2e -run TestClaudeCode -v   # needs claude + a login
```

`e2e/` runs the whole thing in one process: real control plane, real runner, real
local driver, and the **real supervisor binary**, built by `TestMain`. Only the
client is a harness. `fidelity_test.go` is the §4.1 suite ported from the S3
prototype — its job is to prove the split did not regress what S3 established.

## The sandbox image

```bash
image/build.sh                      # cross-compiles the supervisor, builds the image
bin/runner -driver docker -image lemul-workspace:dev -project ~/w/some-project
```

Same image, entrypoint, managed settings and supervisor binary that Fargate will
run — only the placement substrate differs. Managed settings are rendered at
container start from environment, not baked, because one image has to serve both
a Bedrock workspace and local development against a host login (`-host-login`
mounts your Claude credentials in; never use it for a Bedrock workspace).

The entrypoint also seeds Claude Code's **first-run state**. A workspace task
starts with an empty `CLAUDE_CONFIG_DIR`, so without it the first thing a user
sees is the onboarding theme picker, with the per-directory trust prompt behind
it blocking every tool call. Seeded by merge, never overwrite, so a workspace
that comes back keeps whatever the user chose.

Container naming carries the idempotency key, so the container runtime itself
enforces one task per workspace generation.

## Inference

Model traffic is brokered through a **customer-hosted gateway** (LiteLLM or
similar). Direct-to-Bedrock is a draft — see §12.5; Bedrock becomes one backend
*behind* the gateway rather than something Claude Code speaks natively.

Which backend is the customer's choice, not ours, and the local stack
demonstrates it: `litellm-spike/` now runs **OpenRouter** where it ran Bedrock,
and nothing above the gateway changed except the `-pins` names. Those names are
the coupling to watch — they are what Claude Code asks for, so a pin naming a
model the gateway does not serve kills the session at its first prompt.

```bash
bin/controlplane -gateway-url https://litellm.internal:4000 -gateway-key <key>
```

The credential never enters a session. The supervisor opens a loopback listener
per session, and each Claude Code process gets
`ANTHROPIC_BASE_URL=http://127.0.0.1:<its port>` plus a placeholder token. That
matters because **Claude Code's Bash tool inherits the environment** — anything
left there is handed to every command the agent runs, including ones the model
wrote.

**Be precise about what this buys.** The session can still *see* that base URL and
call the proxy — anything in the sandbox can. What it cannot do is take the
credential anywhere: a loopback port on an ephemeral number dies with the
session, whereas a leaked `sk-…` works from anywhere, indefinitely, for every
workspace. And every call through the proxy is tagged with the workspace and
session from the supervisor's own port mapping, so **unattributed spend is not
possible** — verified from inside a sandbox, where a call carrying a forged
credential *and* forged tags was still recorded against the correct workspace and
session.

The capability is deliberately not fenced off: the agent legitimately has
inference and runs arbitrary code, so a Bash command can always just ask Claude
Code for a completion. What bounds it is a per-workspace budget at the gateway
(§12.4), not the proxy.

## Telemetry

Claude Code's OpenTelemetry is for **audit and behaviour**, not billing — the
gateway is the cost source of record (§12.4). Nothing but OTel answers what the
agent *did*: which tools ran, which the user rejected, which a hook blocked,
whether anyone flipped into bypass mode. It is optional infrastructure, gated on
an endpoint being configured.

Content stays redacted: all four `OTEL_LOG_*` flags are set off **explicitly**,
because `OTEL_LOG_ASSISTANT_RESPONSES` silently falls back to
`OTEL_LOG_USER_PROMPTS` when unset.

## Checking an AWS account

```bash
AWS_PROFILE=… bin/preflight -region us-east-2
```

Verifies each pinned model is genuinely usable. `AUTHORIZED` is not proof — a
model can report authorized and still refuse to invoke — so only the real
`InvokeModel` decides. Failures come with the fix, and the distinctions matter:
an IAM policy gap, a missing inference-profile prefix, and an account that never
completed the First Time Use form all look similar and have nothing in common.

Opus and Haiku are required (Claude Code reaches for Haiku on nearly every turn);
Sonnet only warns.

The same check runs **inside each workspace task**, because the supervisor is the
only component holding the sandbox task role — the credential sessions actually
use. A check run anywhere else would test a different principal (§12.3). Turn it
on with `-bedrock-preflight` on the control plane; the config reaches tasks as
environment, which is what both drivers pass through.

A blocking verdict refuses session creation with the fix attached, rather than
letting Claude Code start and then die on the first prompt:

```
$ curl -X POST localhost:9000/v1/workspaces/w1/sessions
HTTP 424
bedrock is not usable in this workspace: us.anthropic.claude-opus-5 (opus) is not
invocable. the account does not have model access… Submit the Anthropic First Time
Use form… a denial has NO self-service recovery and needs an AWS support case
```

`GET /v1/workspaces/{wid}/preflight` serves the structured report for the admin
console. It blocks on *evidence* of breakage, never on the absence of one: a
report that never arrives fails open with a loud log, and throttling is reported
without blocking, since it means the check reached no verdict.

## Spikes (Phase 0, frozen)

| Path | |
|---|---|
| `s1-bedrock/` | Claude Code in a container against Bedrock — 7/7 |
| `otel-probe/` | Telemetry is content-free by default, canary-tested |
| `tui-proxy-proto/` | TUI fidelity over WSS, one hop and two. Frozen as the S3 evidence; production code lives above |
| `winch-probe/` | Does Claude Code repaint fully on SIGWINCH? Gates decision #4 — see its `RESULTS.md` |
| `litellm-spike/` | Does Claude Code work through a gateway, and do header tags partition cost? Validates decision #12 |

## Session lifecycle

Compute, process and client presence are three independent axes (§2.4). These
move the **process** axis only — stopping a session leaves the workspace task up,
because a sibling session may still be working in it.

```
POST   /v1/sessions/{sid}/stop     Ctrl-C/Ctrl-D; ?force=1 is SIGKILL
POST   /v1/sessions/{sid}/resume   starts the process with NO client attached
DELETE /v1/sessions/{sid}          ends it and drops the conversation
```

Stop keeps the record, because the conversation outlives the process. Resume is a
verb rather than a side effect of attaching, since a console has to be able to
put an agent back to work without becoming its terminal. Delete refuses a running
session without `?force=1` — it is the one unrecoverable verb.

Whether a resumed session gets `--resume` or `--session-id` is decided **by the
supervisor, from the workspace volume**, never by a control-plane flag: the two
flags fail in each other's case, and a replacement task's disk is empty while any
flag we stored would still say "started". §12.7 has the measurements.

## Authentication

Optional and off unless configured. Set up the local identity provider —
Logto + Mailpit, config fully scripted — with
[`auth-stack/`](auth-stack/README.md):

```bash
docker compose -f auth-stack/docker-compose.yml up -d
bun auth-stack/seed.ts > auth-stack/.env.generated
set -a; . auth-stack/.env.generated; set +a
```

Then the control plane validates bearer tokens on the management API
(`internal/auth`: signature via JWKS, issuer, audience, expiry — **no scope
checks**, because there is no user model to check against yet):

```bash
bin/controlplane -auth-issuer "$LEMUL_AUTH_ISSUER" -auth-audience "$LEMUL_AUTH_AUDIENCE" \
                 -auth-cli-client-id "$LEMUL_CLI_CLIENT_ID" …
ourcli login          # OAuth device flow (RFC 8628); approve in a browser
```

**The CLI is told nothing.** `ourcli login` reads `GET /v1/auth/config` from the
control plane at `-server` and learns the issuer, the audience and its own client
id from there, then caches them with the token so no later command pays a round
trip. Those are properties of the deployment, not of a laptop: a client that has
to be handed them cannot be pointed at two control planes without two sets of
environment variables, and nothing catches the mismatch — the token gets minted
by the wrong identity provider and the only symptom is a 401. Phase 3 sharpens
it, since each tenant authenticates against its own identity provider.

The endpoint is unauthenticated by necessity — it is what a client reads *before*
it has a token — and carries no secret: `ourcli` is a public OAuth client, so its
client id already travels in every device-flow request. `LEMUL_AUTH_ISSUER`,
`LEMUL_CLI_CLIENT_ID` and `LEMUL_AUTH_AUDIENCE` still override discovery, all
three or none; a partial set is refused rather than merged, because the hybrid
fails much later at token exchange with an error that points at neither half.

Two endpoints stay unauthenticated on purpose: `/v1/tunnel/*`, where the runner
and workspace tasks present their own credentials and have no user to be, and
`/v1/sessions/{sid}/attach`, which carries a single-use attach credential —
a browser cannot set an `Authorization` header on a WebSocket handshake, so
requiring one would make the console's viewer impossible.

**Authentication is not authorisation.** Anyone who can sign in reaches every
workspace; §2.5's owner-scoped attach is still outstanding. Loopback is what
bounds that today.

With no issuer configured, everything behaves exactly as before — which is what
keeps the e2e suite running without an identity provider.

## Operator console

```bash
cd console && bun install && bun run dev     # http://127.0.0.1:3000
```

TanStack Start (SSR) over the control plane's JSON API — workspace and task
state, sessions with their lifecycle buttons, and the structured preflight
report. See [`console/README.md`](console/README.md).

**It binds to loopback deliberately.** Phase 1 has no auth anywhere, so the
console is exactly as exposed as the control plane behind it; that is a control
in `vite.config.ts`, not a default.

Behind `FF_VIEW_SESSION=1` (off by default), `view` on a running session opens a
**read-only** xterm.js terminal on the existing `?mode=viewer` attach path — no
capability the CLI did not already have, with input dropped by the relay *and*
the supervisor. It never resizes the session, since the PTY has one size and
honouring the browser window would reflow the controller's Claude Code. The flag
gates the route as well as the button, so it is a control rather than decoration.
Driving still means `ourcli`.

It is served by its own process, not the Go binary. Backing endpoints:

```
GET /v1/status                     runner count and what this control plane does
GET /v1/workspaces                 records, plus whether a task is really connected
GET /v1/workspaces/{wid}
GET /v1/workspaces/{wid}/fs        directory listing (metadata only, never contents)
GET /v1/workspaces/{wid}/processes what is running, attributed to sessions
GET /v1/workspaces/{wid}/resources CPU / memory / disk / network, with history
```

The last three back the **workspace explorer**, behind `FF_WORKSPACE_EXPLORER`
(off by default). They are reads that never place a task, and there is no write
counterpart on the supervisor to call — read-only is structural rather than
enforced. The path jail lives in the supervisor (`internal/supervisor/fsjail.go`)
because that process runs as root: `/proc/self/environ` there holds the gateway
key, so containment is policy and this is the only copy of it.

## Not yet built

The `ecs` driver and Terraform · idle detection, warm hold, admission control
gating · workspace CRUD. Tracked as the §8 checklist.
