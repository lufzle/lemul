# Claude Code as a Service — Architecture Analysis

**Status:** pre-implementation design
**Last updated:** 2026-07-29

Remote, sandboxed Claude Code workspaces, multi-tenant, running in the customer's own AWS account. Two access modes:

1. **Local CLI** — user runs a thin client on their machine, operates a remote Claude Code (feels like SSH, is actually WSS).
2. **Web client** — browser-based Claude Code.

`Tenant ─1:n─ Workspace ─1:n─ Session ─1:1─ Claude Code process`, where a **workspace** owns one Fargate task (its sandbox) and each **session** is one PTY inside it. Full model in §2.3.

### Repo layout

| Path | What it is |
|---|---|
| `CC_REMOTE_ANALYSIS.md` | This document. The decision record — read §1, §2, §12 before changing architecture. |
| `cmd/`, `internal/`, `e2e/` | **Phase 1 product code.** The runner/supervisor split, relay, driver interface, local CLI. See `README.md`. |
| `winch-probe/` | **Decision #4 gate** (✅). Measures whether Claude Code repaints fully on SIGWINCH. `RESULTS.md` has the verdict and the mode-prelude finding it turned up. |
| `tui-proxy-proto/` | **Spike S3** (✅). Go: PTY-over-WSS, one-hop and two-hop (yamux tunnel). **Frozen as the S3 evidence** — its `proto/` and `tunnel/` were lifted into `internal/`, and its merged runner-proxy shape was replaced by the §2.8 split. `README.md` has results and the reasoning behind each design choice. |
| `otel-probe/` | **Spike S2** (✅). Minimal OTLP/HTTP-JSON receiver used to prove telemetry is content-free. `RESULTS.md` has the captured findings. |
| `s1-bedrock/` | **Spike S1** (✅). Dockerfile + `run-s1.sh` (7/7) for Claude Code against Bedrock in a container, plus the image's managed-settings file. `RESULTS.md` covers the Bedrock feature gap and model-access traps. |
| `~/w/.claude-journal/lufzle-lemul-cc.md` | Chronological work log. |

**Status: Phase 0 complete (all spikes pass). Phase 1 in progress — increment 1
(runner/supervisor split, relay, driver interface, endpoint negotiation) is done
and under test.**

---

## 1. Decisions

| Layer | Decision | Rationale |
|---|---|---|
| Model access | **Amazon Bedrock**, customer's AWS account, ECS task role | No cross-account role, no credential-injection egress proxy. Bedrock spend draws down their existing EDP commitment. |
| Compute | **Sandboxes in customer VPC**; runner dials out to control plane | Their code and credentials never leave their account. No ingress into their VPC. No IAM grant to us. |
| Control plane | **AWS ECS/Fargate** — orchestrator, session relay, OTel ingest, dashboard + RDS Postgres | One vendor in the subprocessor list; PrivateLink to the control plane is *possible* (impossible on Fly); same-region as the customer VPC; one IAM model to operate. See §1.1. |
| Client | **Proxy the real Claude Code CLI** over WSS | Weeks not months; free feature velocity; pitch is "keep using Claude Code, but governed". |
| Observability | **Claude Code OpenTelemetry**, admin-enforced via managed settings | Gives structured audit/cost data without owning the client. |
| Pricing | **Seats / workspaces.** Customer pays their own Bedrock + compute | Avoids competing with the $200 Max plan subsidy; margin is infra margin, not token resale. |

### 1.1 Rejected alternatives

| Rejected | Why |
|---|---|
| **ECS/Fargate as primary substrate (vendor-hosted)** | Superseded once compute moved into customer VPC. Fargate is still the sandbox runtime *inside* their account. |
| **EKS** | Poor fit for stateful interactive dev environments (Gitpod publicly migrated off K8s for this workload in 2023). Pod ≠ isolation boundary — would still need gVisor/Kata on top. |
| **Cloudflare Containers** | Isolation model is fine (per-instance VM), but disk caps at **20 GB and is ephemeral** — *"the next time it is started, it will have a fresh disk"*. FUSE-to-R2 workaround reintroduces network-FS latency. No placement control. |
| **Cloudflare Workers + Durable Objects front door** | A net-added hop, and a DO would shadow state that belongs next to the PTY. WS Hibernation solves a problem we don't have (if a terminal is attached, the task is running and billed anyway). |
| **EFS for workspace storage** | NFS small-file latency destroys `npm install` / `git status`. Users would blame Claude Code. |
| **Anthropic OAuth for programmatic API key provisioning** | Does not exist. No third-party OAuth program; `POST /v1/organizations/api_keys` has no create endpoint — *"new API keys can only be created through the Claude Console for security reasons."* |
| **Brokering user subscription (Pro/Max) OAuth tokens** | Prohibited and server-side enforced since early 2026. Restricted to Claude Code and claude.ai. |
| **Reselling our own Bedrock capacity** | Explicitly out of scope — we do not want to be in the token-billing business. |
| **Claude Platform on AWS** | Full feature parity + PrivateLink + Marketplace billing, but: no HIPAA/FedRAMP/IL4-5, no Fast mode, Claude Code integration unverified (different SigV4 service name `aws-external-anthropic`; no `CLAUDE_CODE_USE_*` flag found). Deferred — revisit if the WebSearch gap or feature lag becomes a real customer blocker. |
| **Full BYOC (we provision their fleet)** | Requires permission to create IAM roles in their account — reviewers find that *more* alarming than a scoped role. Also kills our ability to debug. |
| **Fly.io for the control plane** | Chosen while sandboxes ran there; every reason evaporated once compute moved into the customer VPC (persistent volumes, sub-second machine resume, per-workspace scale-to-zero all became irrelevant — the control plane is a boring always-on service). Kept only `fly-replay` instance routing, which is ~100 lines on ECS (§2.2) and not needed at all for a single-task v0.1. Against that: a second vendor to disclose in every security questionnaire, and **no PrivateLink** — the same gap that ruled Fly out for the Bedrock path. |

### 1.2 Credential model history (why we are where we are)

- **Option A — user pastes their own Anthropic API key.** Blocked as a *primary* motion: individuals and small teams compare metered API against the $200/mo Max plan (20× Pro quota) and metered loses. Structural, not fixable by packaging. Retained as a self-serve/BYO path.
- **Option B — we own the Anthropic org and resell tokens.** Rejected. Puts us in token billing with margin exposure to model price changes.
- **Option C — customer's own Bedrock.** Selected. No Max-plan equivalent exists at enterprise scale, and Bedrock spend is pre-committed EDP budget rather than new spend.

**Open question deferred to Anthropic partnerships:** may a SaaS provision an environment in which the *user themselves* runs `claude login` with their Pro/Max subscription, with the token staying on their volume and never brokered by us? Competitors operate here; no authoritative Anthropic statement found. Blocks the self-serve tier only. Do not build on an inferred reading.

### 1.3 Assumption register

**Decision (2026-07-29): proceed without a design partner.** Three questions that only a real customer can answer are therefore *assumed*. They are recorded here with the cost of being wrong, so that when a customer contradicts one we know immediately whether it is a config change or a rewrite.

| # | Assumption | Confidence | If wrong | Correction cost |
|---|---|---|---|---|
| **A1** | *"Our runner in your account, outbound-only, no IAM grant to us"* **clears security review** | Moderate–good (see evidence below) | **(a)** They forbid vendor egress from *workspace subnets* → decision #6 flips to runner-proxy. **(b)** They forbid *any* vendor dial-out → fully self-hosted control plane. | **(a) 1–2 weeks** — supervisor becomes a passive PTY manager, runner becomes a data proxy, and safe runner auto-update is lost (§2.8). **(b) months** — the control plane must become deployable in their account. |
| **A2** | Their VPN does **not** route to the workspace VPC → build **relay-only** | Deliberately pessimistic | They *do* have a route → `direct` transport becomes available | **Small, and purely additive.** Slots into the endpoint-negotiation contract (§2.7). No rework. |
| **A3** | *"We route only ciphertext; we hold no key that can decrypt your session"* **is sufficient** | Moderate | They require the data path to physically avoid us → Tailscale / `tsnet` | **~2–3 weeks, additive.** `tailnet` is already an endpoint-negotiation transport. No rework. |

**A1 is the only expensive one.** A2 and A3 are chosen to fail in the additive direction — that is why relay-only + E2E is the right v0.1 even though it is the *least* privacy-preserving transport on paper.

**Supporting evidence for A1** (why it is an assumption rather than a leap): outbound-only agents in customer infrastructure are a well-established enterprise pattern — GitHub self-hosted runners, Tailscale, Cloudflare Tunnel, and **Anthropic's own self-hosted Managed Agents sandboxes** (`config: {type: "self_hosted"}`, environment-scoped key, outbound-polling worker) all work this way and are widely deployed. We are not asking for anything novel.

#### Interaction worth noting: A2 + A3 together promote E2E

With no `direct` escape hatch (A2), the relay is on the data path for **every** customer. So E2E is the *only* answer to "does our source code pass through your servers." That makes it the **first item in Phase 2**, not a late one — the first serious customer conversation will raise it, and "not yet" is a much weaker answer than "we cannot read it."

#### Revisit triggers

- **First security questionnaire** → validates or breaks A1 and A3 directly. Read it before writing more code.
- **First customer whose devs are already on a VPN into the workspace VPC** → A2 flips; implement `direct`.
- Any of these landing → update this table and the affected decision in §12.

---

## 2. Architecture

```
customer AWS account                                  control plane (our AWS account)
┌────────────────────────────────────────────┐        ┌──────────────────────┐
│ runner (ECS service, always-on, ~256 MB)   │        │ orchestrator API     │
│   role: ecs:RunTask/StopTask on ONE taskdef│──WSS──>│ session relay        │
│   NO bedrock permissions                   │ (mux,  │ OTel ingest          │
│                                            │ outbnd)│ Postgres · dashboard  │
│ sandbox task (Fargate, one per workspace)  │        │ (ALB + Fargate + RDS) │
│   role: bedrock:Invoke* ONLY               │        └──────────────────────┘
│   NO ecs permissions                       │                   ▲
│   ├─ claude  (CLAUDE_CODE_USE_BEDROCK=1)   │                   │ WSS
│   ├─ supervisor: N PTYs (one per session)  │        user CLI ───┘
│   └─ OTel ──> collector ───────────────────────────────────────┘
│                                            │
│ VPC endpoint: com.amazonaws.<region>.bedrock-runtime
│ S3 bucket: workspace snapshots             │
└────────────────────────────────────────────┘
```

### 2.1 The two-role split (load-bearing security property)

| Principal | Can | Cannot |
|---|---|---|
| Runner task role | `ecs:RunTask`, `ecs:StopTask` on **one** task definition | Call Bedrock |
| Sandbox task role | `bedrock:InvokeModel`, `InvokeModelWithResponseStream`, `ListInferenceProfiles`, `GetInferenceProfile` | Start/stop tasks; touch anything else |

Neither escalates into the other. **Consequence:** the sandbox reaching its task-metadata endpoint (`169.254.170.2`) is now *intended* — the task role is precisely the credential it should hold. (This differs from the vendor-hosted design, where metadata access had to be blocked.)

### 2.2 Connectivity — reverse tunnel, not a job queue

**Two independent tunnels** — the runner carries control only, each workspace task carries its own data (decision #6, §2.8):

1. Runner boots in the customer VPC, dials **out** (WSS) to the control plane, authenticates with its tenant token, holds the connection open. This is registration.
2. User runs `ourcli connect <workspace>` locally.
3. Control plane authenticates the user, resolves the workspace, picks any live runner tunnel for that tenant.
4. Sends a control frame down it: `{start_workspace, workspace_id, generation, tunnel_credential}`.
5. Runner calls `ecs:RunTask` with `--client-token` derived from `workspace_id + generation` (idempotent — §2.8).
6. **The workspace task** dials out on its own WSS tunnel and registers as workspace `W`; the relay binds the credential to the task identity on first connect.
7. Control plane answers the client's endpoint query (§2.7) and relays bytes: `user WSS ⟷ workspace tunnel ⟷ supervisor ⟷ PTY`. Under E2E it routes ciphertext it cannot decrypt.

The runner is never on the data path, so its failure or redeploy leaves running sessions untouched.

No ingress. No inbound firewall rule. Invisible to the user.

Precedent to cite in security review: this is the GitHub self-hosted-runner / `cloudflared` shape, and **Anthropic's own self-hosted Managed Agents sandboxes work the same way** (`config: {type: "self_hosted"}`, environment-scoped key, outbound-polling worker).

**Routing.** A user's connection must land on the process holding that tenant's tunnel — stateful routing an ALB cannot spray (its stickiness is client-based, not tenant-based). Three stages:

1. **v0.1 — single task.** No routing problem exists; every tunnel and every user connection hits the same process. Terminal traffic is a few KB/s per session, so one task carries a lot.
2. **Tenant → task IP in Postgres.** The tunnel-holder registers its task IP on connect; a user landing on task A looks up that the tunnel is on task B and proxies over the VPC (sub-millisecond). ~100 lines. This is what `fly-replay` would have done for free.
3. **Shard by tenant**, a service per shard addressed via Cloud Map. Only if (2) is outgrown.

**Placement:** put the relay in the same **AWS region** as the customer's VPC, not near the user. The relay→tunnel leg should be short; the user pays one long hop instead of two. Same-region also opens PrivateLink / VPC peering for customers who refuse public egress.

**ALB notes:** raise the idle timeout (default 60 s kills long-lived tunnels). yamux keepalive at 15 s keeps connections non-idle, but set it anyway. Replacing a task drops its tunnels; the runner's 2 s reconnect turns that into a blip rather than lost sessions — verify explicitly once reconnect handling lands.

### 2.3 Domain model

```
Tenant ──1:n── Workspace ──1:n── Session ──1:1── Claude Code process
   │                │                 │
   │        one Fargate task     one PTY inside that task
   │        (0..1 at a time)
   └──1:n── User
```

| Entity | Durability | Notes |
|---|---|---|
| **Tenant** | permanent | Enterprise customer, or a managed fleet later. Carries `placement` ∈ `byo_runner` \| `managed_runner` — **do not assume a customer AWS account in the schema**, or the managed-fleet case becomes a migration. |
| **User** | permanent | Belongs to a tenant. Holds permissions (§2.5). |
| **Workspace** | durable | `owner` (user), `access_scope` ∈ `owner` \| `team` \| `org`, repo, snapshot, task tier, admission policy. Has **0..1 current Fargate task** — the task ARN is a mutable field, never the workspace's identity. |
| **Session** | durable record, ephemeral process | User-specific. 1:1 with a Claude Code process. The **conversation** lives on disk in the workspace, so a session's history outlives its process. |

**Why the task belongs to the workspace, not the session.** Multiple sessions per workspace exist to share a filesystem — git worktrees, shared assets, a shared dev database. That requires one task. It also buys a good property: the *second* session in a running workspace starts in ~1 s (fork a PTY) instead of paying a 20–60 s Fargate cold start, and one active workspace costs one task regardless of session count.

**Consequence — the supervisor is a multi-PTY manager.** The Phase 0 prototype merges runner and supervisor with one PTY per connection. Production splits them:

- **Runner** — one per tenant. Control only: `ecs:RunTask` / `StopTask`.
- **Supervisor** — one per workspace task. Owns N PTYs, one per session. Also reports resource headroom for admission control (§2.4).

### 2.4 Three orthogonal lifecycles

The thing to keep straight: **compute, process, and client presence are independent axes.** Conflating any two produces the wrong behavior for a real use case.

| Axis | States | Operation | Analogy |
|---|---|---|---|
| **Workspace compute** | `stopped` → `starting` → `active` → `warm_hold` → `stopped` | stop / resume workspace | Booting the machine |
| **Session process** | `stopped` → `running` → `stopped` | stop / resume session | Quitting `claude`, then `claude --resume <id>` |
| **Client presence** | `attached` / `detached` | attach / detach | Closing the laptop lid |

The combinations that must all work:

| Workspace | Session | Client | Real case |
|---|---|---|---|
| `active` | `running` | `detached` | **Overnight agent run.** Must not be reaped. |
| `active` | `stopped` | — | You quit CC; files intact, a sibling session still running |
| `stopped` | `stopped` | — | Fully cold. Resuming the session implies starting the workspace first. |
| `stopped` | `running` | — | **Impossible** — the process lives in the task. |

**Stopping a session is Ctrl-C / Ctrl-D semantics** — it interrupts the agent exactly as local Claude Code does. That is expected behavior, not data loss. Resume is `claude --resume <id>`; because the conversation is on the workspace volume, a resumed session picks up its history even though the process is new. **Surface that in the UX** — "resume" feeling lossless is a much better experience than "your session is gone."

**Warm hold.** When the last session in a workspace stops, keep the task alive for a grace period (~5 min, configurable) before scaling to zero. Cheap insurance against paying a cold start for someone who quit and immediately reconnected.

**Auto-stop cascade.** Session idle-stop → last session stops → warm hold → workspace stops → snapshot to S3.

**Idle must mean user-idle AND agent-idle.** A naive PTY-idle check sees zero keystrokes during an unattended multi-hour run and would kill the exact workload remote sandboxes exist for. The signal is already validated in S2 (`otel-probe/RESULTS.md`):

> stop when: no client attached **AND** no `claude_code.active_time.total{type=cli}` activity **AND** no recent `api_request` / `tool_result` events

(`type=user` is keyboard; **`type=cli` is tool execution and model responses** — that is the "agent is working" signal.) Per-session configurable, and the user can disable it outright for a specific session.

#### Admission control (resource saturation)

Fargate task size is **fixed at launch** — it cannot grow. So the control plane admits or refuses new sessions, per a workspace policy:

| Policy | Behavior |
|---|---|
| `max_sessions: N` | Simple count. Predictable, ignores actual load. |
| `min_free_memory_mb: N` | Admit only while the supervisor reports that much headroom. Adapts to real usage. |
| `unlimited` | User's call; warn at create time. |

The supervisor is the honest source for headroom (cgroup usage vs. the task's limit from the ECS task-metadata endpoint) and reports it up the tunnel.

**Admission is necessary but not sufficient**, because Claude Code's footprint grows with context — a session that admits cleanly can balloon later. Realistically 2 vCPU / 8 GiB holds ~2–4 sessions plus a dev server.

> ⚠️ **OOM blast radius:** an OOM kills the *task*, killing **every session in that workspace** — including a sibling's overnight run. Never let this be discovered by OOM: refuse at session-create with a clear error. Mitigation for Phase 2 is a per-session cgroup memory cap inside the task, so a runaway session is killed instead of its neighbours.

#### Deliberately deferred: vertical migration

Growing a saturated workspace by launching a larger task, migrating sessions, signalling the CC instances to stop, syncing the delta, reconnecting clients, and stopping the old task. **Explicitly not v0.1–v0.3.**

Worth noting only because two Phase 1 decisions should not preclude it: session IDs must be stable across tasks, and the client needs a reconnect protocol. The reconnect protocol is the same one network-drop recovery needs, so building that properly keeps the door open for free.

### 2.5 Permissions and access scope

Keep the two orthogonal: **permission gates the verb, scope gates the object.**

- **Permissions** (admin-granted, per user): `CreateWorkspace`, `DeleteWorkspace`, `CreateSession`, `JoinAsViewer`, `Delegate`, `ViewAllWorkspaces`, `ManageTenant`
- **Scope** (per workspace): `owner` \| `team` \| `org`

> An action on workspace `W` is allowed iff the user holds the permission **and** `W`'s scope includes them (or they hold an admin override).

`team` implies a team entity not yet in the model — either add it now or restrict v1 scopes to `owner` \| `org`.

**Join-as-Viewer has two hard requirements.** It is the feature that forces open decision #4 (tmux vs. own VT state model): showing a late joiner the *current screen* needs server-side screen state, which a raw replay buffer cannot provide — you would replay from the top or paint garbage. And **the relay must drop input frames from viewer connections**, or "viewer" silently becomes co-driver, since both attachers write the same PTY stdin.

**Delegate** transfers control of a session that runs with the workspace's task role (tenant Bedrock credentials) and full filesystem access. Log it, and time-box it.

### 2.6 API surface

Workspace and session creation are **async** — Fargate cold start is 20–60 s, so return `202` and let the client poll or stream. Creating a session in a `stopped` workspace implicitly starts the workspace.

```
POST   /v1/workspaces                    202 {id, status:"starting"}
GET    /v1/workspaces                    ?owner= &repo= &status=
GET    /v1/workspaces/{wid}
PATCH  /v1/workspaces/{wid}              name, access_scope, admission policy, idle policy
POST   /v1/workspaces/{wid}/stop         ?force= (stops live sessions)
POST   /v1/workspaces/{wid}/resume
DELETE /v1/workspaces/{wid}              also drops the snapshot

POST   /v1/workspaces/{wid}/sessions     202 {id}; starts the workspace if stopped
GET    /v1/workspaces/{wid}/sessions
GET    /v1/sessions/{sid}
POST   /v1/sessions/{sid}/stop           Ctrl-C/Ctrl-D semantics
POST   /v1/sessions/{sid}/resume         → claude --resume <id>
DELETE /v1/sessions/{sid}                end and drop the conversation
WS     /v1/sessions/{sid}/attach         ?mode=control|viewer

GET    /v1/workspaces/{wid}/events       SSE — status transitions
GET    /v1/tenants/{tid}/usage           derived from OTel
```

MCP is a thin wrapper over this later — not a parallel implementation.

#### Client wrapper UX

Workspace name is **required** (or from a `WORKSPACE` env var). If it does not exist, prompt; the user must explicitly confirm. Creating requires `CreateWorkspace`; attaching to an existing one requires `CreateSession`.

```
ourcli connect <workspace-name>      # prompts if not found
ourcli connect <name> --create       # non-interactive
```

> **The prompt needs a non-interactive path.** It breaks three callers otherwise: CI, scripts, and **our own MCP server**. When stdin is not a TTY, do not prompt — fail with a message naming `--create`. Otherwise `ourcli connect` in a pipeline hangs forever.

### 2.7 Session data path — end-to-end encryption

**The problem.** With a relay in the middle we TLS-terminate both legs, so session bytes — conversation content, file contents the agent echoes to the terminal, keystrokes — are **cleartext inside our process**. We do not log them (the relay is byte-transparent by design, because TUI fidelity required it), but that is a *policy* claim. The goal is an *architectural* one: **we cannot read session content**, not merely that we choose not to.

That distinction is the whole value. "Could, but doesn't" and "cannot" are different answers in a security review.

#### The four options

| Option | Content reaches us | Effort | Customer requirement |
|---|---|---|---|
| **E2E through the relay** | ciphertext only | ~1 week | none |
| **Direct connection** | no | low | devs on a network that routes to their VPC |
| **Tailscale / WireGuard mesh** | no | low–medium | they run a tailnet |
| Build our own NAT traversal (STUN/hole-punch/TURN) | no (usually) | very high | none |
| Relay in cleartext (v0.1 as built) | **yes** | done | none |

**Plan: E2E as the default (Phase 2), direct as an option, Tailscale for customers who demand it. Do not build NAT traversal** — that is what Tailscale *is*, as a company.

#### E2E design, and the key-distribution trap

Noise handshake between client and supervisor; the relay routes ciphertext. The trap: **if we distribute the supervisor's public key, we can substitute our own and MITM.** So the trust anchor must be customer-rooted:

```
onboarding   customer's Terraform generates a signing keypair;
             public half is pinned into the CLI config — obtained from THEM, not us
task start   supervisor generates an ephemeral keypair, signed by that key
connect      client verifies the signature, then Noise-handshakes with the supervisor
             → relay routes bytes it cannot decrypt
```

Without the customer-rooted anchor this is security theatre — we would be certifying the key we could also forge.

#### Endpoint negotiation — the decision to make in v0.1

Even while the only answer is "the relay," **the client must ask the control plane where to connect** rather than hardcoding an endpoint:

```
GET /v1/sessions/{sid}/endpoint
  → { transport: "relay" | "direct" | "tailnet",
      address:     "...",
      credential:  "...",
      peer_pubkey: "..." }
```

Bake this in and the data path becomes per-tenant config. Hardcode the relay and every option above becomes a rewrite.

#### What still reaches us regardless

Be precise in the pitch; overclaiming will get caught.

| | Reaches us |
|---|---|
| Conversation content, file contents, keystrokes | **No** (with E2E or direct) |
| OTel telemetry | Yes — but S2 proved it is content-free by default (§5.4) |
| Metadata: session existence, timing, duration, byte counts | Yes — unavoidable, and needed for billing |

The defensible claim is **"we see metadata, never content."** Same shape as Anthropic's own vault design for credentials.

#### Two consequences

**E2E rules out a relay-side VT state model** — we cannot parse ciphertext into a screen model. That pushes it into the supervisor, in the customer's VPC, which is where it belonged anyway (open decision #4). Join-as-Viewer then fans out inside *their* infrastructure.

**Multi-attach under E2E needs the session key shared with each attacher** — a group key, or encrypt-to-N. A real wrinkle to design for, not a blocker.

### 2.8 Failure domains and HA

Decision #6 (task-dials-out) already removed the runner from the data path, which is what makes the rest of this tractable.

| Component | Failure impact | Mitigation |
|---|---|---|
| **Runner** | Cannot create/stop workspaces. **Existing sessions and in-flight agent work unaffected.** | Stateless → N replicas (below). `desiredCount: 1` already self-heals in ~30–60 s. |
| **Workspace task** | That workspace's sessions only | Inherent — the task *is* the workspace. Snapshot on stop limits data loss. |
| **Relay / orchestrator** | Relay-routed sessions drop — but see below | Multi-AZ Fargate; §2.2 sharding |
| **Postgres** | Control plane unavailable | RDS Multi-AZ |

**Relay loss is a reconnect, not a lost session.** The workspace task and its PTYs keep running in the customer's VPC, so a reconnecting client lands back in a live session. That makes control-plane HA a *quality* problem rather than a data-loss one — **provided the reconnect path is solid.** Note this is the third consumer of the same mechanism, alongside network-drop recovery and (later) vertical migration. Build it properly once.

#### Runner HA

The runner is a **stateless command executor** — nothing is pinned to a replica, which is what makes N replicas easy:

```
ECS service, desiredCount: 2, spread across AZs
  ├── runner A ──WSS──> relay   (registers)
  └── runner B ──WSS──> relay   (registers)

control plane holds a SET of live tunnels per tenant, dispatches to any one
tunnel drops → yamux keepalive (15 s) → control plane routes to the survivor
```

**The tunnel is the health check.** No liveness probes, no service discovery — failover is "pick a different tunnel from the set."

Two problems, neither needing distributed consensus:

**1 · Duplicate `RunTask` is the dangerous failure.** A dispatch that times out and is retried against another runner yields two Fargate tasks for one workspace — two filesystems, split-brain on the snapshot.

Fix: derive an idempotency key from `workspace_id + generation` and pass it as ECS `RunTask --client-token` (verified available; 64 chars max). Duplicate dispatch then becomes harmless.

> **Do this even with one runner.** It protects every retry-after-timeout, so it is correctness work that happens to enable HA — not HA work.

**2 · Reconciliation needs a single actor.** Someone must periodically list tasks by tag and compare against desired state in Postgres (orphans, tasks that died silently). With N runners you do not want N reconcilers racing.

The tempting answer is leader election among runners (DynamoDB lease or similar). **Don't.** The control plane already sees every tunnel and already picks one for dispatch — let it assign the role:

```
control plane → runner A: "you are the reconciler"
   A's tunnel drops    → control plane promotes B
```

No consensus protocol in the customer's account. The coordinator already exists.

#### Recommendation: design for N, deploy 1

Two choices cost nothing now and are expensive to retrofit:

1. **Model tunnels as a set per tenant** — `map[tenant][]tunnel`, not `map[tenant]tunnel`. Pure data-structure choice.
2. **Idempotent dispatch via `client-token`** — needed regardless.

Then `desiredCount: 1 → 2` is a Terraform variable, not a project. Cost is irrelevant (2× a 256 MB task); the only reason to wait is untested code paths, which argues for running 2 in our own test tenant early.

Gives a crisp security-review answer worth having: *"our agent in your account is stateless and control-only. If it dies, your running sessions are untouched, and you can run two replicas across AZs."*

---

## 3. Bedrock feature gap

Both Bedrock surfaces (legacy `InvokeModel` and Mantle `bedrock-mantle.{region}.api.aws`) list the same unsupported set. **These are API-level features — Claude Code's client-side equivalents are unaffected.**

| Unsupported on Bedrock | Real impact on Claude Code |
|---|---|
| Server-side code execution | **None** — sandbox has bash |
| Agent Skills (`container.skills`) | **None** — CC loads skills from `.claude/skills/` locally |
| MCP connector (`mcp_servers` API param) | **None** — CC runs its own MCP client (`.mcp.json`) |
| Files API, Message Batches, Models API | None for CC |
| Server-side `fallbacks` | Minor — client-side fallback pattern exists |
| **WebSearch** | **Real — the only genuine loss.** CC docs: *"The WebSearch tool is not available on Amazon Bedrock."* |

**Mitigation:** search MCP server (Brave / Tavily / Exa) baked into the image's `.mcp.json`. A customer-hosted gateway fronting the real Anthropic API would plausibly restore WebSearch outright (§12.4) — unverified, and not a reason to choose a gateway on its own.
**To verify (S1):** WebFetch is client-side in CC and the docs only call out WebSearch — confirm empirically.

### 3.1 Model pinning is mandatory

Unpinned, `opus`/`sonnet` resolve to Claude Code's built-in Bedrock default, which can lag the newest release or be unavailable in the customer's account — CC then silently falls back. The docs also warn an unpinned deployment gets **billed at Opus rates**. Make pins a per-tenant config value with a sane default:

```bash
ANTHROPIC_DEFAULT_OPUS_MODEL='us.anthropic.claude-opus-5'
ANTHROPIC_DEFAULT_SONNET_MODEL='us.anthropic.claude-sonnet-4-6'
ANTHROPIC_DEFAULT_HAIKU_MODEL='us.anthropic.claude-haiku-4-5-20251001-v1:0'
```

All three verified `AUTHORIZED` and invocable on `aureum-dev-full-access`
(us-east-2) by `cmd/preflight` on 2026-07-29. They are the defaults in
`internal/bedrock.DefaultPins`. **Pin the cross-region inference profile ID**
(`us.` prefix): the bare foundation model ID returns *"Invocation of model ID …
with on-demand throughput isn't supported"* even on an account that is fully
authorized for it.

### 3.2 Mantle vs the Invoke API — use both flags, plan on Invoke

Claude Code supports the Messages-API Bedrock endpoint ("Mantle") natively:
`CLAUDE_CODE_USE_MANTLE=1`, model IDs `anthropic.claude-sonnet-5` (no version
suffix), `ANTHROPIC_BEDROCK_MANTLE_BASE_URL` to point at a gateway. **Verified
working**, including **dual mode**: with `CLAUDE_CODE_USE_BEDROCK=1` *and*
`CLAUDE_CODE_USE_MANTLE=1`, CC routes by model-ID shape.

Mantle is exempt from the Anthropic First Time Use form, which looks like less
onboarding friction. **It is not** — Mantle is allowlist-gated per account and
requires an AWS account-team request, whereas the FTU form is self-service and
instant. Tested on two accounts: one authenticates to Mantle but has zero models
granted (404 on every ID), the other 403s outright. Feature parity is identical
to the Invoke API either way (WebSearch still absent).

**So: plan on the Invoke API, set both flags anyway** so a Mantle-entitled
customer works with no config change. Details in
[`s1-bedrock/RESULTS.md`](s1-bedrock/RESULTS.md) § Addendum.

### 3.3 Network path

**Same AWS account ≠ private network.** Without a VPC endpoint, a task calling `bedrock-runtime.<region>.amazonaws.com` routes out through the IGW/NAT, across the public internet, and back into AWS. TLS + SigV4 so it's cryptographically fine, but it shows as internet egress in flow logs and fails a "no public egress from this subnet" control.

Use an interface VPC endpoint (`com.amazonaws.<region>.bedrock-runtime`). Endpoint policies can additionally pin allowed model ARNs at the network layer. Target shape: sandboxes in private subnets, VPC endpoints for Bedrock/ECR/S3/Logs, plus an allowlisted egress proxy for package managers (npm/pypi/git — true zero-egress isn't achievable since CC needs them).

---

## 4. Terminal fidelity

The transport is not the fidelity variable. A PTY over WSS carries the same byte stream as a PTY over SSH. What matters:

### 4.1 The six requirements

1. **Allocate a real PTY** (`forkpty`, not pipes). With pipes `isatty()` is false and CC degrades to non-interactive — no TUI at all. Binary failure mode.
2. **Raw mode on the local client** (`cfmakeraw` — disable ICANON/ECHO/ISIG). Otherwise Ctrl-C kills our CLI instead of reaching the remote process. Restore termios on exit *and* on panic.
3. **Out-of-band resize channel** — `{type:"resize",rows,cols}` → `ioctl(TIOCSWINSZ)` → `SIGWINCH`. Without it everything renders at 80×24.
4. **Binary WS frames, not text.** Terminal output isn't guaranteed valid UTF-8 at chunk boundaries; text frames force validation and corrupt multi-byte sequences straddling frames. Intermittent and infuriating to debug.
5. **`TCP_NODELAY` on every hop.** One miss and Nagle batches keystrokes into ~40 ms delays — reads as "the product feels laggy".
6. **`TERM` / terminfo agreement.** Ship `ncurses-term` or normalize to `xterm-256color`; set `COLORTERM=truecolor`.

The relay must be **8-bit clean and byte-transparent** — no line processing, no sanitization, no logging layer that touches the bytes.

### 4.2 Claude Code specific sharp edges

- **Bracketed paste (mode 2004)** — highest value. Break it and a multi-line paste submits on the first newline.
- **Enhanced keyboard protocol** (kitty keyboard / `modifyOtherKeys`) — this is what makes Shift+Enter a newline rather than submit. Negotiation escapes must pass through untouched **both** directions.
- **Image paste** — a local-terminal-to-local-process affordance; does not survive a remote hop. Needs a product answer (upload endpoint, CLI subcommand, or paste hook that ships the file and rewrites as a path).
- **OSC 52 clipboard** — works if we don't filter OSC sequences.

### 4.3 Web client

xterm.js is a genuinely capable VT emulator (powers VS Code's terminal). Required addons: **fit** (feeds resize channel), **webgl** (canvas/DOM chokes on large output), **unicode11** (width calc for emoji/CJK — mismatched grapheme width shears box-drawing layouts, and CC draws boxes). Optional: web-links, image/sixel.

### 4.4 Latency

WSS costs essentially nothing over SSH; the latency is geography. 20–40 ms RTT feels native, 150 ms+ feels like syrup. Mitigate by region placement. **Do not** attempt Mosh-style predictive local echo — it degrades badly inside full-screen redraws, which is exactly this workload.

### 4.5 Test matrix (run against both clients)

Automated portion: `tui-proxy-proto/fidelity_test.go` (runs against both 1-hop and 2-hop transports). Interactive portion: `tui-proxy-proto/MANUAL_MATRIX.md`. Item 7 has a dedicated harness — `tui-proxy-proto/render-test.sh`, run locally first for a reference, then inside the proxied session at 80 and 200 columns.

1. Paste a 30-line code block — stays one message?
2. Shift+Enter — newline or submit?
3. Resize mid-render — clean reflow or garbage?
4. Ctrl-C during a long tool call — interrupts the tool, doesn't kill the session?
5. `cat` a 100k-line file — client keeps up?
6. Kill network 60 s — session survives and repaints correctly?
7. Box-drawing + emoji at 80 and 200 cols — any shearing?

---

## 5. Observability — Claude Code OpenTelemetry

This is what makes "don't own the client" viable. CC emits metrics, events/logs, and (beta) traces over standard OTLP.

### 5.1 Topology

```
sandbox (CC) ──OTLP──> collector sidecar in customer VPC ──> control plane ingest
                              └──> customer's own SIEM / Datadog (free fan-out)
```

### 5.2 Admin enforcement

Config goes in the **managed settings file**, baked into the image. Env vars there have highest precedence and cannot be overridden by the user. As of **v2.1.217**, setting an `OTEL_EXPORTER_OTLP_*` var in managed settings makes CC *strip conflicting developer-set vars at startup*:

- `..._ENDPOINT` → removes every developer-set per-signal endpoint
- `..._PROTOCOL` → removes every developer-set per-signal protocol
- Credentials (`..._HEADERS`, `..._CLIENT_KEY`, `..._CLIENT_CERTIFICATE`) → removes per-signal versions **plus every developer-set endpoint var**, so credentials can't leak to an unapproved collector

**Gap to close:** exporter *selectors* (`OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER`) follow normal per-key precedence — a user could set `none` and silence a signal. **Set the selectors in managed settings too.**

Lock-down list: `CLAUDE_CODE_ENABLE_TELEMETRY=1`, both exporter selectors, `OTEL_EXPORTER_OTLP_PROTOCOL`, `_ENDPOINT`, `_HEADERS`.

### 5.3 What we get with content logging OFF

> **Validated 2026-07-29 (S2) — see [`otel-probe/RESULTS.md`](otel-probe/RESULTS.md).**
> Real session captured against a local OTLP receiver on CC 2.1.220. Canary
> strings planted in the prompt and in a bash command were **absent from all
> payloads**, as were the read filename and file contents. `tenant.id` landed on
> 7/7 payloads. Cost, token, tool-decision, and tool-result data are all
> sufficient. Three corrections to the docs below.
>
> - **Event names carry no `claude_code.` prefix** on the wire (`api_request`,
>   not `claude_code.api_request`). Metrics *do*. Filtering on the prefixed
>   event names matches nothing.
> - **Hook and plugin telemetry is emitted** and undocumented above:
>   `hook_registered`, `hook_execution_start`/`_complete`, `plugin_loaded`.
>   Useful — lets us prove which hooks/plugins ran in a customer sandbox.
> - **`query_source` is `sdk`** in headless mode, not `main`.
> - Metric-level `model` is the Bedrock profile ID
>   (`us.anthropic.claude-opus-5[1m]`); event-level is bare (`claude-opus-5`).
>   Normalize when aggregating.

Events: `api_request` (`cost_usd_micros`, input/output/cache tokens, `model`, `query_source`, `effort`, `duration_ms`) · `tool_result` (`tool_name`, `success`, `duration_ms`, `error_type`) · `tool_decision` (`decision`, `source` — including `user_reject`, `hook`, `config`) · `permission_mode_changed` (detects flips into bypass mode) · `api_refusal` (`category`) · `mcp_server_connection` · `auth`

Metrics: `cost.usage` · `token.usage` · `lines_of_code.count` · `commit.count` · `pull_request.count` · `active_time.total` · `code_edit_tool.decision`

**Per-tenant attribution is free** — inject `OTEL_RESOURCE_ATTRIBUTES="tenant.id=…,workspace.id=…"` per sandbox; every metric and event carries it. That is per-workspace billing with no custom instrumentation.

**`prompt.id`** (UUID v4) correlates every event from one prompt — filter on it to reconstruct a turn. Deliberately excluded from metrics (cardinality), so turn reconstruction happens in the event pipeline.

### 5.4 Privacy posture

Prompt and response content is **redacted by default** — only `prompt_length` / `response_length`. Keep all four content flags off (`OTEL_LOG_USER_PROMPTS`, `OTEL_LOG_ASSISTANT_RESPONSES`, `OTEL_LOG_TOOL_DETAILS`, `OTEL_LOG_RAW_API_BODIES`). "We can prove we never collect your prompts" beats any dashboard feature in a security review.

If a customer later requests content: it must be **their** per-tenant opt-in, never our default. Note `OTEL_LOG_ASSISTANT_RESPONSES` **falls back to `OTEL_LOG_USER_PROMPTS` when unset** — set it `=0` explicitly to keep responses redacted while prompts are logged.

### 5.5 Operational notes

- OTel vars are **not** passed to subprocesses (Bash, hooks, MCP servers).
- `otelHeadersHelper` script hook (http protocols only, refreshes every ~29 min) for rotating the ingest token — likely needed, since a long-lived token in a customer-visible image is poor hygiene.
- Metrics export interval defaults to 60 000 ms; logs 5 000 ms.
- No default protocol — `OTEL_EXPORTER_OTLP_PROTOCOL` must be set explicitly.

---

## 6. Trust & compliance

**SOC 2 is not on the v0.1 critical path.** The gating axis is *who signs*, not company size:

| Buyer | Gate |
|---|---|
| Individual dev | Nothing — does it work, what does it cost |
| Seed–Series A | A few pointed CTO questions; security page + architecture diagram closes it |
| Series B+ | Often a first security hire, or **inherited requirements** (below) |
| Enterprise | SOC 2 Type II, DPA, pen test summary, subprocessor list, questionnaire |

**Downstream inheritance is the mechanism that surprises people:** a 40-person customer with SOC 2 has a vendor-management policy obligating them to assess us. They ask for our report because their auditor makes them. Expect this earlier than headcount suggests. Regulated verticals ignore size entirely.

**Our trust bar is higher than typical SaaS** — we run LLM-generated code against customer source. But the runner architecture answers most of it structurally: their code never leaves their VPC, we hold no AWS credentials, no ingress.

### 6.1 Trust artifacts (≈1 week, doubles as sales collateral)

- [ ] Public security page — architecture + Fly's SOC 2 / ISO posture as substrate
- [ ] Explicit "what we can and cannot access" statement
- [ ] Encryption in transit / at rest, stated plainly
- [ ] **The actual IAM policy JSON**, both roles, scoped
- [ ] Terraform module (+ CloudFormation variant)
- [ ] Data retention + delete-on-request
- [ ] Subprocessor list (Fly, AWS; note: no Anthropic subprocessor relationship in the Bedrock model)

### 6.2 SOC 2 timing

Turn on the compliance tooling (Vanta/Drata) early — mostly automated evidence collection, forces reasonable hygiene, and the Type II observation window (~3 months minimum) is the one thing that can't be compressed. Pull the report when a deal needs it. Rough: **$10–25k first year, 4–6 months cold start to Type II** — get quotes, don't plan on these.

---

## 7. Phase 0 — spikes ✅ **COMPLETE** (2026-07-29)

Two of these can invalidate design assumptions. Run them first.

### S1 · Claude Code in a container against Bedrock — ✅ **PASS 7/7** (2026-07-29)
Full results: [`s1-bedrock/RESULTS.md`](s1-bedrock/RESULTS.md). Run under
OrbStack, not ECS — **ECS was not needed.** It adds only the VPC-endpoint network
path and microVM isolation, neither of which affects whether CC functions; the
one ECS-specific mechanism (task-role credentials) is reachable over plain HTTP
and was exercised with a local shim.

Confirmed: headless `-p` works · managed-settings model pins resolve (verified
via OTel: `us.anthropic.claude-opus-5`) · **WebFetch works** (client-side) ·
**MCP works** (CC runs its own client) · OTel escapes the container · task-role
credential provider works with no static keys in the image.

**The gap really is one tool.** Everything else the docs list as unsupported on
Bedrock is an *API-level* feature with a client-side equivalent in Claude Code.
Only WebSearch is a genuine loss.

**Gotcha for Phase 1:** the container credential provider requires *temporary*
credentials. A long-term IAM user key produces `Could not load credentials from
any providers` **even though the endpoint was fetched successfully** — badly
misleading. Fargate always supplies temp credentials so production is fine, but
check credential *type* before suspecting networking or IAM.

### S2 · OTel payloads at content-off — ✅ **PASS** (2026-07-29)
Full results: [`otel-probe/RESULTS.md`](otel-probe/RESULTS.md). Content is
genuinely redacted (canary-tested), per-tenant attribution works, cost and
tool-audit data are sufficient. **The "don't own the client" decision holds.**
Remaining gaps: managed-settings *enforcement* (test in the Phase 1 image
build), interactive-only events (`permission_mode_changed`, `source=user_reject`),
`api_error`/`api_refusal`, traces beta.

### S3 · TUI fidelity — ✅ **PASS, both steps** (2026-07-29)
Prototype: [`tui-proxy-proto/`](tui-proxy-proto/). **Step 1 (one hop) passes** —
11/11 automated tests under `-race`, and the manual matrix confirms bracketed
paste, **Shift+Enter**, mid-render resize, and **plan mode (Shift+Tab)** all
work. Those were the two flagged as likeliest to fail, so the enhanced keyboard
protocol survives the hop.

Two bugs found and fixed: the supervisor never closed the WebSocket on child
exit (Ctrl-D hung the client), and the PTY started at 0×0 because the initial
size arrived as a post-connect control message instead of at fork.

**Step 2 (relay + yamux tunnel) ✅ PASSES** — no fidelity regression. Every
automated check runs against both transports as subtests; all 2hop twins pass
byte-for-byte. The mux adds **~70 µs** keystroke RTT (three orders of magnitude
below real network latency) and no measurable throughput cost.

One design consequence worth carrying into Phase 1: **WebSocket frame types do
not survive the tunnel.** The client↔relay hop uses binary-vs-text frames to
separate PTY bytes from control messages; a yamux stream is a raw byte pipe, so
the relay↔runner leg needs explicit framing (`[type:1][len:4][payload]`). A
control frame misread as data would inject JSON into the user's terminal.

**S3 is complete. The CLI-proxy architecture is validated.**

### S4 · `fly-replay` for WebSocket upgrades — ~~½ day~~ **CANCELLED**
Moot: the control plane moved to ECS (§1, §1.1). v0.1 runs a single task, so
tenant→process routing does not arise; when it does, it is the Postgres lookup in
§2.2 stage 2. **No Fly account needed. Phase 0 is complete.**

---

## 8. Phase 1 — vertical slice

One workspace, one tenant, IDs hardcoded in config. No auth, no multi-tenancy, no billing, no web UI.

- [x] **Bedrock preflight — three layers, in the supervisor and as a standalone binary** (`internal/bedrock`, `cmd/preflight`, §12.3). **Not in the runner:** §2.1 denies the runner any Bedrock permission, and a runner-side check would test the wrong principal anyway. A blocking verdict refuses session creation and attach with the advice attached; the structured report is served at `GET /v1/workspaces/{wid}/preflight` for the admin console. Three layers, because each catches a different failure:
  1. `bedrock:GetFoundationModelAvailability` per configured model — assert **`authorizationStatus == AUTHORIZED`**, not merely that the model is listed. `list-inference-profiles` returning `ACTIVE` is **not** an entitlement check (a test account listed 25 Anthropic profiles `ACTIVE` and could invoke none).
  2. A real minimal `InvokeModel`, since only that proves end-to-end.
  3. Map failures to actionable messages — `NOT_AUTHORIZED` and `Operation not allowed` mean different things and have different fixes.
- [x] **Onboarding runbook for Bedrock model access** — [`docs/onboarding-bedrock.md`](docs/onboarding-bedrock.md). Ordered so the multi-day failure is cleared first. Documents **five distinct failure modes**, all hit on real accounts, plus two findings that no error message reveals: a zero daily-token quota is usually downstream of a **declined payment card** breaking the Marketplace subscription, and **model agreements are a separate per-model step** from the account-wide form. Also records the AWS status fields that do not mean what they say (`ACTIVE`, `AUTHORIZED`, `agreementAvailability`) — the reason preflight layer 2 is a real invocation.
- [x] Sandbox image — [`image/`](image/). CC pinned at 2.1.220, supervisor and preflight baked in, `ncurses-term` + git/ripgrep/jq, `.mcp.json` template. **No tmux.** Managed settings are **rendered at container start from environment, not baked**: the same image has to serve a Bedrock workspace and local development against a host login, and those need different settings — baking them would mean two images, which is the drift the driver interface exists to prevent.
- [x] Supervisor — **multi-PTY manager** (one per session, §2.3), resize channel, resource-headroom reporter (`internal/ptysession`, `internal/supervisor`). Idle reporter still outstanding — it needs the OTel signal below.
- [x] **Detach/reattach without tmux or a VT model** (decision #4): (a) the child now outlives client disconnect — that *is* detach; (b) bounded replay ring per session (~256 KB); (c) SIGWINCH nudge on reattach; **(d) mode prelude** — replay the terminal-mode negotiation, which the repaint does *not* restore. The repaint assumption is **measured, not assumed** (§12.2, `winch-probe/RESULTS.md`): full-viewport repaint confirmed, and (d) was the finding that fell out of it. The ring turned out to be polish rather than correctness.
- [ ] Session lifecycle — stop (Ctrl-C/Ctrl-D semantics) / resume via `claude --resume <id>`; conversation persists on the workspace volume (§2.4)
- [ ] Admission control at session-create — `max_sessions` and `min_free_memory_mb` policies (§2.4). **Refuse with a clear error; never let OOM be the discovery mechanism.** *(The supervisor already reports headroom up the tunnel; the control plane logs it but does not yet gate on it.)*
- [ ] Idle detection using the S2-validated signal: no client attached AND no `active_time.total{type=cli}` AND no recent `api_request`/`tool_result` (§2.4). Per-session opt-out.
- [ ] Warm hold (~5 min) before scaling a workspace task to zero (§2.4)
- [x] Runner — outbound tunnel, stream multiplexing, control-only (`internal/runner`). `RunTask`/`StopTask` proper arrive with the `ecs` driver; the `local` driver exercises the same interface.
- [x] Relay — authenticate → resolve workspace → byte pump (`internal/controlplane`). Tunnels are a **set per tenant** as required, with round-robin dispatch so a second replica is exercised rather than idle.
- [x] **Idempotent workspace dispatch** — the generation counter, the derived key (`driver.Spec.IdempotencyKey`, 64-char capped) and per-workspace placement locking are all in. Wiring the key to ECS `--client-token` lands with the `ecs` driver; the `local` driver already honours it. Generation is persisted, since a control-plane restart that reset it would silently void the protection.
- [x] **Endpoint negotiation** — `GET /v1/sessions/{sid}/endpoint` returns `{transport, address, credential, peer_pubkey}`; the client refuses any transport it does not speak rather than assuming. Credentials are single-use.
- [x] Local CLI — raw mode, WSS, resize, termios restore on exit/panic/SIGTERM, `Ctrl-]` detach, reattach by session id (`cmd/ourcli`). `--create` and the not-found prompt wait for the workspace CRUD API; the non-TTY guard is already in, so the pipeline-hang failure mode cannot appear.
- [ ] Terraform module — runner service, both IAM roles, task definition, VPC endpoint, S3 bucket, and **runner token delivery** (Terraform variable → Secrets Manager → task env; document rotation)
- [x] **Workspace-runtime driver interface, from day one** — `internal/driver` with the `local` (process) and `docker` (real sandbox image) drivers landed. The `ecs` driver is the remaining implementation. This is the mitigation for the biggest standing risk in §13: once the data plane lives in customer accounts we lose the ability to reproduce failures, so our debugging environment must share a code path with the product — the e2e suite runs the real supervisor binary through the real driver for exactly that reason.

**Exit criterion:** `ourcli connect <workspace>` from a laptop yields a working Claude Code in a fresh Fargate sandbox in a test AWS account; a second session in the same workspace starts in ~1 s; `Ctrl-]` detaches and reattaching lands back in the live session.

> **Where increment 1 leaves it:** everything above holds today against the
> `local` driver, covered by `e2e/` — including real Claude Code detach/reattach
> (`TestClaudeCodeDetachReattach`) and the second-session-is-faster property.
> What remains for the exit criterion proper is the substrate: the sandbox image,
> the `ecs` driver and Terraform.

> **Not** "survives a 60-second network drop" — that needs the hardened reconnect/replay path, which is Phase 2 item 2. Phase 1's bar is detach/reattach while the connection is healthy. (The prototype supervisor currently *kills* the child on disconnect; keeping it alive is Phase 1, replaying missed output is Phase 2.)

## 9. Phase 2 — productize the runner path

Workspace lifecycle (create/suspend/resume/destroy) · snapshot to S3 on suspend, restore on resume · idle timeout · OTel ingest with per-tenant attribution (`tenant.id` + `workspace.id` + `user.id`) · runner auto-update · structured logging · trust artifacts from §6.1.

**Ordered.** E2E is first: under assumptions A2+A3 (§1.3) the relay is on the data path for every customer, so it is the only answer to "does our source code pass through your servers" — and the first serious customer conversation will ask.

1. [ ] **End-to-end encryption of the session data path** (§2.7) — Noise between client and supervisor; relay routes ciphertext. Requires the **customer-rooted trust anchor**: signing keypair generated by their Terraform, public half pinned into the CLI from them. Without that anchor it is security theatre, and a reviewer will say so.
2. [ ] **VT state model in the supervisor** (decision #4) — parse the output stream into a screen model, emit a reconstruction on attach. Unlocks proper Join-as-Viewer (better than tmux read-only: we own the fan-out and enforce input-dropping at the relay) and lossless reconnect. Serves three consumers with one mechanism: network-drop recovery, relay redeploy, and (later) vertical migration (§2.4, §2.8). Free dividend: screen snapshots → live dashboard thumbnails.
   - **The hard part is the modes, not the grid** — alternate screen, bracketed paste (2004), keyboard-protocol mode must all be replayed, or a reattached client gets working display with broken input. Test for that specifically.
   - Use a maintained Go library (`charmbracelet/x/vt`, `hinshun/vt10x` — evaluate both), don't hand-roll a parser.
3. [ ] **Per-session cgroup memory cap inside the task** — a runaway session gets OOM-killed instead of taking down every sibling session in the workspace (§2.4).
4. [ ] Runner auto-update (safe now that the runner is off the data path — §2.8) and `desiredCount: 2` in our own test tenant to exercise the multi-tunnel path.

**Deferred, reachable without rework** (both slot into endpoint negotiation, §2.7): `direct` transport if A2 flips · Tailscale/`tsnet` if A3 breaks.

## 10. Phase 3 — multi-tenant

Auth · tenant/user/workspace/session model (§2.3) · permissions × access scope (§2.5) · per-tenant runner tokens with rotation · onboarding flow · usage dashboard from OTel · seat billing · the management API (§2.6).
**Blocked on a design partner** — see §12.

## 11. Phase 4 — web client and collaboration

Claude Agent SDK against the **same** sandbox, workspace, and volume. A second entry point, not a second product. The Agent SDK *is* Claude Code packaged as a library (built-in Read/Write/Edit/Bash/Glob/Grep, agent loop, context management, hooks, subagents, MCP) — so this is a UI choice, not an agent rewrite.

Trade-off vs. proxying: structured events, real diffs, approval modals, mobile — at the cost of feature lag and losing the user's existing `.claude/` config, custom commands, and plan mode.

---

## 12. Open decisions

| # | Decision | Recommendation | Notes |
|---|---|---|---|
| 1 | ~~Design partner for Option C~~ **DECIDED: proceed without one** | Build on the recorded assumptions in §1.3 | A1/A2/A3 are assumed rather than validated. A2 and A3 fail additively; **A1 is the expensive one** (1–2 weeks if partly wrong, months if fully wrong). Still worth acquiring a partner opportunistically — the first real security questionnaire validates or breaks A1 and A3 in one document. |
| 2 | Runner: orchestrator or host? | **Orchestrator** — `ecs:RunTask` on one task definition | Tiny legible IAM ask; capacity is ECS's problem. Host mode = zero AWS perms but bin-packing and the isolation boundary (gVisor/Kata) become ours. |
| 3 | Workspace persistence | **Fargate ephemeral disk + git clone + S3 snapshot** for uncommitted state | ~5–15 s resume, no AZ pinning. EBS-attach-to-Fargate is better long-term (real block perf, no snapshot step) but adds cold-start latency and AZ pinning. EFS is out (§1.1). |
| 4 | ~~Reconnect: tmux or own VT state model?~~ **DECIDED and VERIFIED: neither tmux nor VT model in Phase 1** | **Phase 1:** keep the child alive on disconnect + **mode prelude** + bounded replay ring + SIGWINCH-nudge repaint on reattach. **Phase 2:** VT state model in the supervisor. **Never tmux.** | tmux is a second emulator that normalizes `TERM` to `tmux-256color`, needs explicit config for truecolor and OSC 52, and has partial `extended-keys` support: it risks regressing exactly the Shift+Enter and plan-mode behaviour the manual matrix validated, and becomes a permanent third suspect for every rendering oddity. The VT model belongs in the supervisor anyway (E2E rules out relay-side state, §2.7) and pays a dividend — screen snapshots give live dashboard thumbnails. **The gating assumption is now measured, not assumed** — see [`winch-probe/RESULTS.md`](winch-probe/RESULTS.md) and §12.2. |
| 5 | Who pays for the search MCP provider? | **Us** initially | Cheap, invisible, one less onboarding step. |
| 6 | ~~Session data path: runner-proxy or task-dials-out?~~ **DECIDED: task-dials-out** | Each workspace task holds its own outbound tunnel | Keeps the runner off the data path — no throughput bottleneck, and runner failure/redeploy leaves running sessions untouched (§2.8). Runner passes a workspace-scoped, short-TTL tunnel credential at `RunTask` time; relay binds it to the task identity on first connect. **Caveat:** requires workspace tasks to reach our endpoint. They already need egress for npm/pypi/git, so it is an extra allowlist entry — but a customer forbidding *all* vendor egress from workspace subnets forces runner-proxy. Ask early (§12.1). |
| 7 | Session auto-stop default: on or off? | **On**, with per-session opt-out | Protects against forgotten sessions burning the customer's Bedrock quota. The §2.4 idle signal makes it safe for unattended runs. |
| 8 | `team` access scope in v1? | **Defer** — ship `owner` \| `org` | `team` implies a team entity not yet in the model (§2.5). Add when a customer asks. |
| 9 | Default admission policy | `min_free_memory_mb` | Adapts to real usage rather than guessing a session count. Needs the supervisor's headroom reporter (§2.4). |
| 10 | Default session data path | **DECIDED: `relay` + E2E** (§2.7, assumption A2/A3) | Relay-only in v0.1 — assume no customer VPN route. `direct` and `tailnet` deferred but reachable without rework via endpoint negotiation. **Because there is no `direct` escape hatch, E2E is the first Phase 2 item, not a late one** (§1.3). |
| 11 | Runner replica count in v0.1 | **Design for N, deploy 1** (§2.8) | `desiredCount: 1` self-heals in ~30–60 s and the runner is control-only, so the exposure is "cannot create a workspace" for under a minute. Run 2 in our own test tenant so the multi-tunnel path is exercised. |
| 12 | Inference provider: Bedrock only, or also a customer-hosted gateway? | **Support a customer-hosted gateway; adopt the provider shape now, build later** — mechanism **validated** (`litellm-spike/`) | Claude Code already supports it natively (`ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_CUSTOM_HEADERS`, `CLAUDE_CODE_USE_VERTEX` — all verified present in 2.1.220). **Customer-hosted only**: hosting it ourselves is §1.2 Option B, already rejected. Full analysis in §12.4. |

### 12.3 ~~Where does the Bedrock preflight run?~~ **DECIDED: supervisor, plus a standalone binary for onboarding** (2026-07-29)

§8 put the Bedrock preflight in **the runner's startup health check**. §2.1 says
the runner task role holds `ecs:RunTask`/`StopTask` and **cannot call Bedrock**.
Both cannot be true, and the two-role split is the load-bearing security
property, so §8 is the side that gave.

The argument that settled it is not about security, though: **a preflight run by
the runner tests the runner's credentials, not the sandbox's.** Those are
different principals with different policies, so a runner-side check could come
back green while every session still fails. Granting the runner Bedrock would
have bought a check that does not test the thing it claims to — worse than no
check, because it is a false all-clear.

That leaves the deploy-time signal genuinely worth having but needing a different
home, and `cmd/preflight` already is one: the customer runs it **before anything
is deployed**, with their own credentials, catching "this account never did the
FTU form" before a VPC exists.

| When | Who | Whose credentials | Catches |
|---|---|---|---|
| Onboarding, pre-deploy | Customer runs `preflight` by hand | Their own admin creds | Account-level model access — the long-lead failure |
| Every workspace task start | **Supervisor**, reporting up its event stream | **Sandbox task role** | Everything, with the credential sessions actually use |
| — | **Runner: nothing** | — | §2.1 intact |

Rejected: the runner launching a one-shot preflight task on the sandbox task
definition. It uses the right role and restores deploy-time feedback, but the
onboarding run covers the same ground for free.

**Behaviour, as built.** A blocking verdict refuses session creation *and*
attach with the advice attached, rather than letting Claude Code start and then
die on the user's first prompt. Three rules turned out to matter:

- **Block on evidence, never on its absence.** A report that never arrives — an
  older supervisor, a bug in our own event path — fails open with a loud log.
  Only a report that says a required model is unusable fails closed.
- **Skipped ≠ absent.** A workspace not using Bedrock reports `skipped`, so
  "not applicable" cannot be confused with "not yet".
- **Transient ≠ misconfigured.** Throttling means the check reached no verdict;
  refusing sessions over it would be a self-inflicted outage.

The verdict is dropped when its task's tunnel goes, so a replacement task is
judged on its own check — access is often granted after a failure, and a stale
"no" would keep a fixed account looking broken.

Verified end to end against two real accounts (2026-07-29): the denied account
returns `424` with the FTU instruction and the structured report at
`GET /v1/workspaces/{wid}/preflight`; the authorized account admits sessions with
all three pins `AUTHORIZED` and invocable.

### 12.2 Decision #4, measured (2026-07-29)

The gate on decision #4 was whether Claude Code repaints its *whole* frame on
SIGWINCH or only patches part of it. Measured rather than eyeballed, because an
already-attached terminal holds the correct screen and so cannot distinguish the
two: [`winch-probe/`](winch-probe/) captures **only** the bytes a `cols-1 → cols`
nudge produces and replays them into a *fresh* VT emulator — exactly what a
reattaching client with an empty ring would see.

**Result: full repaint, in all five scenarios tested** (fresh screen, panel
within the viewport, content scrolled off the top, viewport overflow, and a
tool-output transcript). The reconstructed screen matched the ground truth
line-for-line every time. The repaint homes the cursor and emits `ESC[2K` — erase
entire line — twice per row before rewriting it, so it is correct even onto a
*dirty* screen. There is no alternate screen and no erase-display; Claude Code is
an inline TUI and does not need one.

Two consequences for the §8 checklist:

1. **The replay ring is not load-bearing.** The nudge alone reconstructs the
   viewport; the ring only restores scrollback above it. Item (b) is polish, not
   correctness.

2. **The repaint restores the grid but not the modes — a new required item.**
   Claude Code emits `ESC[?2004h` (bracketed paste), `ESC[>1u` (kitty keyboard)
   and `ESC[>4;2m` (modifyOtherKeys) exactly once at startup and never again, not
   even on SIGWINCH. A client reattaching into a fresh terminal would get a
   perfect-looking screen with **broken Shift+Enter and broken paste** — working
   display, broken input, precisely the failure §9 item 2 predicts for the Phase 2
   VT model. The fix is a **mode prelude**: scan the stream for the DECSET/DECRST,
   `CSI > … u` and `CSI > 4 ; … m` shapes, keep the latest of each, replay it
   ahead of the ring on attach. An incremental CSI scanner, not a screen model.
   Mode **2026** (synchronized output) is deliberately excluded — replaying a
   stale "begin" freezes the client's display.

A third finding came out of implementing it: **the nudge must be skipped on the
attach that creates the session.** The child has drawn nothing yet, so there is
nothing to repaint, and nudging races its first frame — the child reads the
shrunken width and renders one column narrow. Same class of bug as starting a PTY
at 0×0, and just as easy to misattribute to the TUI. Caught by
`e2e/fidelity_test.go:TestInitialSizeAppliedBeforeChildStarts`.

### 12.4 Open: inference through a customer-hosted gateway (2026-07-30)

Raised while an account's Bedrock entitlement was stuck, but it stands on its own
merits — the entitlement mess is a bad reason to reorder a roadmap, and a good
reason to notice a gap.

> **Validated 2026-07-30 — see [`litellm-spike/RESULTS.md`](litellm-spike/RESULTS.md).**
> Claude Code runs through LiteLLM end to end (`claude → LiteLLM → Bedrock`), and
> **header-injected tags partition spend per workspace, user and session** — the
> two assumptions this decision rests on. Three things the spike added: a gateway
> deployment needs Postgres or spend logging silently does nothing; Claude Code's
> per-call floor is ~41 k tokens because the system prompt and tool definitions
> dominate; and LiteLLM appends its own tags, so aggregation must filter rather
> than trust. Still unvalidated: the loopback proxy binding itself, streaming, and
> whether a gateway restores WebSearch.
>
> **Attribution mapping, tested:** LiteLLM's **Team ID** takes our *workspace id*
> via the virtual key's team binding — the only **unforgeable** binding, since the
> session never sees which key the proxy uses, and it unlocks per-workspace budget
> enforcement. **End User** takes our *user id* via header, and that header
> **beats** Claude Code's own `metadata.user_id` (CC otherwise fills the field
> with a device/account blob). **Session ID** is Claude Code's own, so our session
> id goes in a tag. Binding workspace→team needs admin access to the customer's
> gateway, so it must be opt-in rather than default — see the provisioning
> tension in the spike results.

**The mechanism already exists.** Claude Code 2.1.220 supports pointing at any
Anthropic-Messages-compatible endpoint, verified by inspecting the shipped binary:
`ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_KEY`,
`ANTHROPIC_CUSTOM_HEADERS`, plus `CLAUDE_CODE_USE_VERTEX` and
`ANTHROPIC_VERTEX_BASE_URL`. Our `image/entrypoint.sh` already renders managed
settings from environment, so this is a switch in that renderer rather than new
architecture.

**Customer-hosted only.** A gateway the customer runs (LiteLLM or similar, in
their VPC) preserves every property the Bedrock decision was made for: their
data, their credentials, their spend, their egress policy — and it is arguably
*better* on a "no public egress from workspace subnets" control, since the
gateway is in-VPC rather than across an interface endpoint. A gateway **we** host
is §1.2 Option B (own the org, resell tokens), rejected for margin exposure, and
it would also put us on the content path that §2.7 exists to keep us off. That
rejection stands.

**The commercial argument is stronger than the technical one.** Many enterprises
already mandate an LLM gateway — it is where they do cost attribution, guardrails
and PII filtering. For those buyers "you must use Bedrock" is a hard blocker and
"point us at your existing gateway" is an easy yes. Two smaller upsides: Vertex
comes nearly free, and a gateway fronting the real Anthropic API would plausibly
restore **WebSearch**, the one genuine feature loss on Bedrock (§3) — worth
verifying rather than assuming.

#### The credential problem, and the design that solves it

Bedrock mode takes credentials from the task role through the container
credential provider: temporary, rotated, never a static secret. Gateway mode puts
a bearer token in the environment — and the session's own bash can read it. Claude
Code runs LLM-generated code, so that is a materially worse exposure and must not
arrive quietly.

**The supervisor holds the credential and runs a loopback proxy.** Sessions get
`ANTHROPIC_BASE_URL=http://127.0.0.1:<port>`; the supervisor adds the auth header
and forwards to the real gateway. The token never enters the session environment.
The supervisor is inside the customer's account, so this does not touch the "we
cannot read your content" claim.

Two design constraints on that proxy:

- **Headers only; never touch the request body.** Rewriting JSON to inject
  metadata would break SSE streaming and put us inside message content — the same
  reason the relay is byte-transparent. LiteLLM accepts metadata via headers.
- **A loopback port per session**, with the supervisor mapping port → session.
  The obvious alternative — per-session `ANTHROPIC_CUSTOM_HEADERS` — lives in the
  session's own environment and can be rewritten by the session, which is fine
  for curiosity and not fine if costs become chargebacks. A port mapping is
  bookkeeping the agent cannot influence. Sessions can reach each other's ports,
  but they already share a filesystem and a task: same trust boundary.

#### What the proxy buys beyond credential hygiene

**Two independent cost signals.** Claude Code's self-reported `cost_usd_micros`
via OTel (§5.3), and the gateway's own accounting keyed on injected workspace and
user metadata. One is what the agent thinks it spent; the other is what the
customer is actually billed, in their own FinOps tooling. Being able to reconcile
those is a materially stronger billing position than either alone.

**A second idle signal, free.** The proxy sees every model call, which is an
immediate and independent "agent is working" indicator. §2.4's idle detection
currently rests entirely on OTel `active_time.total{type=cli}`; this backstops it
with no new instrumentation.

#### The asymmetry to remember

This is **gateway-mode only**. In Bedrock mode Claude Code signs with SigV4
against the real endpoint host, so a loopback proxy invalidates the signature
unless we re-sign — which means holding AWS credentials in the proxy and
reimplementing signing. `ANTHROPIC_BEDROCK_BASE_URL` exists in the binary, so it
may be possible, but do not assume symmetry. Bedrock keeps OTel-only attribution,
which is what §5.3 already promises.

#### What to do now

**Not build it.** Adopt the shape, for the same reason the driver interface exists
— cheap now, expensive to retrofit:

1. Put the preflight behind a `Provider` interface. It is Bedrock-specific today
   (`GetFoundationModelAvailability` + `InvokeModel`); a gateway preflight is a
   one-token `POST /v1/messages`. Same three layers, different mechanics.
2. Rename the Bedrock-specific configuration to a provider concept
   (`LEMUL_PROVIDER=bedrock|gateway`, pins becoming provider-scoped) before more
   code accretes around `LEMUL_BEDROCK`.

**This must not displace the `ecs` driver.** Phase 1's exit criterion is a real
Fargate sandbox; provider flexibility is Phase 3 territory (per-tenant
configuration).

### 12.1 Questions for the first customer conversation

These were the design-partner questions; with §1.3's assumptions in place they are now **validation** questions rather than blockers. The first three map directly to A1/A2/A3 — ask them before writing Phase 3 code.

- Do you require **private connectivity** to Bedrock? (Yes → VPC endpoint is mandatory, not optional.)
- Does **"our runner in your account, outbound-only, no IAM grant to us"** clear your security review? (If yes, full BYOC is wasted work. If you require *nothing* dials out to a vendor control plane, we're headed to fully self-hosted and the ops-velocity cost becomes a real budget line.)
- Any **outbound policy** blocking persistent WSS to non-allowlisted vendors? (Usually solved by allowlisting our hostname. If workspace *subnets* specifically may not reach a vendor endpoint, that forces runner-proxy — see decision #6.)
- **Does your VPN / Zero Trust network route to the VPC your workspaces run in?** (Yes → `direct` transport is available: session bytes never touch our infrastructure at all, and it takes load off our relay. §2.7)
- Would **"we route only ciphertext; we hold no key that can decrypt your session"** satisfy your data-handling review, or do you need the data path to physically avoid us? (Distinguishes E2E-through-relay from `direct`/`tailnet`.)
- Are you in a **regulated vertical**? (HIPAA/FedRAMP → Bedrock is required and the WebSearch gap is permanent. Platform on AWS would fix features but excludes those certifications — can't have both.)

---

## 13. Known risks

| Risk | Mitigation |
|---|---|
| **Loss of observability into the data plane** — can't reproduce failures, can't attach to a wedged sandbox; every customer is a slightly different environment (SCPs, permission boundaries, VPC layouts, quotas) | Build the vendor-hosted driver too and use it for internal dev + demos, so our debugging environment shares a code path with the product. Abstract the workspace runtime behind a driver interface from day one. |
| TUI fidelity degrades through the mux | S3 gates this before product code |
| Claude Code version skew across customer runners | Runner auto-update (Phase 2); pin CC version in the image and roll forward centrally |
| WebSearch gap becomes a customer blocker | Search MCP server; escalate to Platform on AWS reconsideration |
| Long-lived OTel ingest token in a customer-visible image | `otelHeadersHelper` rotation (§5.5) |
| **OOM in a workspace task kills every session in it** — including a sibling's unattended overnight run | Admission control at session-create (§2.4); per-session cgroup cap in Phase 2; conservative default task tier |
| **Duplicate `RunTask` → two tasks for one workspace** (split-brain filesystem and snapshot) | Idempotency key as ECS `--client-token`, from day one, even with a single runner (§2.8) |
| Session content transits our control plane in cleartext until E2E ships | Honest disclosure in v0.1; E2E in Phase 2 (§2.7); `direct` transport for customers who cannot accept transit |
| E2E without a customer-rooted trust anchor is security theatre — we could substitute the key we also distribute | Signing key generated by *their* Terraform, pinned into the CLI from them (§2.7) |
| FTU-form denial has no self-service recovery — customer is stuck behind an AWS support case and our onboarding looks broken | Onboarding runbook with example `useCases` wording + `authorizationStatus` preflight (§8) |
| Building enterprise plumbing on spec | Decision #1 — get a named design partner |
