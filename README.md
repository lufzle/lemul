# lemul-cc

Remote, sandboxed Claude Code workspaces that run in the **customer's own AWS
account** and bill to their Bedrock.

The design record is [`CC_REMOTE_ANALYSIS.md`](CC_REMOTE_ANALYSIS.md) — read §1
(decisions and the assumption register), §2 (architecture) and §12 (open
decisions) before changing anything structural. Every non-obvious choice in this
code has a rationale there, and the comments cite it by section.

**Status: Phase 0 complete. Phase 1 increment 1 (the runner/supervisor split) complete.**

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
| `internal/driver` | Workspace runtime behind an interface: `local` today, `ecs` next. Exists from day one so our debugging environment shares the product's code path (§13). |
| `internal/registry` | Live tunnels. A **set** per tenant, never one — §2.8. |
| `internal/controlplane` | API handlers, the two tunnel endpoints, and the byte-transparent relay. |
| `internal/bedrock` | Three-layer model preflight. Layer 1 (availability) is advisory, layer 2 (a real `InvokeModel`) is authoritative, layer 3 maps errors to the actual fix. |
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
```

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

## Spikes (Phase 0, frozen)

| Path | |
|---|---|
| `s1-bedrock/` | Claude Code in a container against Bedrock — 7/7 |
| `otel-probe/` | Telemetry is content-free by default, canary-tested |
| `tui-proxy-proto/` | TUI fidelity over WSS, one hop and two. Frozen as the S3 evidence; production code lives above |
| `winch-probe/` | Does Claude Code repaint fully on SIGWINCH? Gates decision #4 — see its `RESULTS.md` |

## Not yet built

Sandbox image and Bedrock preflight · the `ecs` driver and Terraform · session
stop/resume, idle detection, warm hold, admission control · workspace CRUD.
Tracked as the §8 checklist.
