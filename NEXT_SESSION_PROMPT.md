# Paste this into a fresh session

---

We're building "Claude Code as a Service": remote, sandboxed, multi-tenant Claude Code
workspaces that run in the **customer's own AWS account**, billed to their Bedrock.

Read `CC_REMOTE_ANALYSIS.md` first — it is the complete decision record from a long
design session. Read §1 (decisions + assumption register), §2 (architecture, domain
model, lifecycles, E2E data path, HA), §8 (Phase 1), and §12 (open decisions). Then
skim `tui-proxy-proto/README.md`, which explains the terminal-fidelity constraints and
why each design choice in the prototype is what it is.

**Phase 0 is complete — all four spikes passed. Do not re-run or re-litigate them:**

- **S1** — Claude Code in a container against Bedrock: 7/7 (`s1-bedrock/RESULTS.md`)
- **S2** — Claude Code OTel is content-free by default, canary-tested (`otel-probe/RESULTS.md`)
- **S3** — TUI fidelity over WSS, one hop *and* two hops through a yamux tunnel: no
  regression, mux costs ~70 µs (`tui-proxy-proto/`)
- **S4** — cancelled (was Fly-specific; control plane moved to ECS)

## The task: Phase 1, starting with the runner/supervisor split

`tui-proxy-proto/runner/main.go` currently forks PTYs *and* holds the tunnel — that is
the **runner-proxy** shape. Open decision #6 settled on **task-dials-out**, which splits
it into two components:

- **Runner** — one per tenant, in their VPC. Control only: `ecs:RunTask` / `StopTask`.
  Never on the data path. Holds a tunnel to the control plane for commands.
- **Supervisor** — one per *workspace task*. Owns **N PTYs (one per session)**, dials
  out on its **own** tunnel, reports resource headroom for admission control.

Please start there, then work through the §8 Phase 1 checklist. Propose a plan before
writing code; I'd rather agree on structure first.

## Constraints that are already decided — please don't reopen without new evidence

- **Proxy the real Claude Code CLI.** Not a custom client on the Agent SDK. S2 proved
  OTel gives us the audit/cost data without owning the UI. (Web client is Phase 4.)
- **Task-dials-out**, not runner-proxy (decision #6, §2.8). Rationale is runner
  auto-update safety and blast radius, not throughput.
- **One Fargate task per workspace**, N sessions as PTYs inside it. Sessions share a
  filesystem — that is the point (git worktrees, shared dev DB).
- **Three orthogonal lifecycles** (§2.4): workspace compute, session process, client
  attachment. Conflating any two breaks a real use case.
- **Idle = user-idle AND agent-idle.** Naive PTY-idle would kill overnight agent runs.
  The signal is OTel `active_time.total{type=cli}` (§2.4).
- **Design for N runner replicas, deploy 1** (§2.8). Model tunnels as a *set* per
  tenant, and make dispatch idempotent via ECS `RunTask --client-token` — both cost
  nothing now and are expensive to retrofit.
- **Endpoint negotiation from day one** (§2.7): the client asks the control plane where
  to connect. v0.1 only ever answers `relay`, but hardcoding it makes E2E/`direct`/
  `tailnet` a rewrite.
- **Driver interface from day one** (§8): `ecs` driver (the product) and `local` driver
  (Docker, for our own development). Once the data plane lives in customer accounts we
  cannot reproduce failures — our debug environment must share the product's code path.

## Gotchas already paid for — don't rediscover these

- **`pty.StartWithSize`, never `pty.Start`.** A PTY defaults to 0×0; a resize arriving
  after the fork means Claude Code already drew its first frame into a zero-size
  terminal. Size travels in the connect request.
- **Close the WebSocket when the child exits**, with an explicit `CloseNormalClosure`.
  Otherwise Ctrl-D hangs the client forever. The close *code* matters — it is how the
  client distinguishes "session over" from "connection dropped".
- **Binary WS frames for PTY bytes, text frames for control.** Terminal output is not
  valid UTF-8 at arbitrary chunk boundaries.
- **WebSocket frame types do not survive the yamux tunnel** — a stream is a raw byte
  pipe. `tunnel/frame.go` carries an explicit type tag (`[type:1][len:4][payload]`).
- **Ctrl-C must reach Claude Code, not exit the client.** Detach is `Ctrl-]`.
- **`ncurses-term` in the image**, plus `TERM` and `COLORTERM=truecolor`.
- **Bedrock: pin model IDs in managed settings.** Unpinned deployments fall back to
  Claude Code's built-in default and get billed at Opus rates.
- **`list-inference-profiles` returning `ACTIVE` is not an entitlement check.** Only a
  real `InvokeModel` proves access — hence the three-layer preflight in §8.
- **The ECS container credential provider requires *temporary* credentials.** A
  long-term IAM key yields a misleading "could not load credentials from any providers"
  even though the endpoint was fetched successfully.
- OrbStack squats on port **4318**; use a different port for a local OTLP collector.

## Environment

- Go 1.24.5, `aws` CLI 2.36.10 (arm64), Docker/OrbStack 29.4.0, boto3 1.40.76
- Working AWS profile for testing: **`aureum-dev-full-access`** (`us-east-2`), Bedrock
  `AUTHORIZED` for `us.anthropic.claude-opus-5` and haiku 4.5.
  The `dario` profile has **no** model access (FTU form denied, needs an AWS support
  case) — do not use it.
- Not a git repo yet. Worth `git init` early.
- Per `~/.claude/CLAUDE.md`: journal completed work to
  `~/w/.claude-journal/lufzle-lemul-cc.md` as `{YYMMddTHHmm}: {description}`.

## One decision I still owe you

**Open decision #4 — tmux vs. our own VT state model** for reconnect and
Join-as-Viewer. It is now on the Phase 1 critical path: E2E rules out relay-side screen
state, so the model has to live in the supervisor, which shapes how the supervisor is
written from the first commit. Raise it early with a recommendation.
