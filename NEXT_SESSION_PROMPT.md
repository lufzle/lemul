# Paste this into a fresh session

---

We're building "Claude Code as a Service": remote, sandboxed, multi-tenant Claude
Code workspaces that run in the **customer's own AWS account**.

Read `CC_REMOTE_ANALYSIS.md` — the decision record. Read §1 (decisions +
assumption register), §2 (architecture, domain model, lifecycles), §8 (Phase 1
checklist), and **§12 (open decisions) in full, especially §12.4–§12.6**, which
are the newest and least intuitive. Then `README.md` for the code layout.

**Phase 0 spikes are done, and Phase 1 is substantially built. Do not re-litigate
settled decisions** — they are marked as such in §12, each with the evidence that
settled it.

## What exists and works

`cmd/{ourcli,controlplane,runner,supervisor,preflight,keyprobe}` + `internal/*`,
with `e2e/` running the real binaries. `go test ./... -race` passes.

- **Runner/supervisor split** (decision #6). Runner is control-only; each
  workspace task holds its own tunnel. PTY lifetime is independent of connection
  lifetime — that is what makes detach real.
- **Sandbox image** (`image/`), Claude Code pinned at 2.1.220, managed settings
  **rendered at container start** from env rather than baked.
- **Drivers**: `local` (process) and `docker` (the real image). `ecs` is the gap.
- **Gateway inference** (decision #12) — the supported path. The supervisor holds
  the gateway credential and brokers each session through **its own loopback
  port**, so no credential enters a session and attribution cannot be forged.
- **Bedrock preflight**, three layers, gating session creation.
- **Telemetry** to OpenObserve; **spend** to LiteLLM.

## Run it

```bash
docker compose -f litellm-spike/docker-compose.yml up -d     # gateway   :4000
docker compose -f otel-stack/docker-compose.yml up -d        # telemetry :5080
go build -o bin/ ./cmd/...

AUTH=$(printf 'admin@lemul.local:Lemul-dev1!' | base64)
bin/controlplane -addr :9000 -agent-token dev-token -session-cmd claude \
  -gateway-url http://host.docker.internal:4000 -gateway-key sk-lemul-spike \
  -pins 'opus=claude-opus-5,sonnet=claude-sonnet-4-6,haiku=claude-haiku-4-5' \
  -otel-endpoint http://host.docker.internal:5080/api/default \
  -otel-headers "Authorization=Basic $AUTH" &

bin/runner -control-plane ws://localhost:9000 -token dev-token \
  -driver docker -image lemul-workspace:dev -project ~/w/some-project &

bin/ourcli connect myproj        # also: ls / -new / -session <id> / -mode viewer
```

`litellm-spike/.env` is gitignored and must be regenerated:
`aws configure export-credentials --profile aureum-dev-full-access --format env-no-export > litellm-spike/.env`
then append `AWS_REGION_NAME=us-east-2`.

UIs: LiteLLM `localhost:4000/ui` (`admin` / `sk-lemul-spike`), OpenObserve
`localhost:5080` (`admin@lemul.local` / `Lemul-dev1!`).

## AWS accounts — read before touching Bedrock

| Profile | Account | State |
|---|---|---|
| `aureum-dev-full-access` | 901598252261 | **The only one that can invoke.** LiteLLM points here. |
| `lufzle-full-access` | 112324749796 | Blocked. SSO, **1-hour token**, expect frequent re-login. |
| `dario` | 446133822735 | Unusable — FTU form submitted with a two-word use case, no self-service recovery. |

112324749796 has an accepted use-case form and active Marketplace subscriptions,
but **a zero daily token quota** (`Adjustable: false`) and `403 not offered` for
Opus 4.8 / Opus 5 / Sonnet 5. A support case is open covering both. Everything on
our side is done; nothing further to try. **Ask whether AWS replied** — if so,
`AWS_PROFILE=lufzle-full-access bin/preflight -region us-east-2` verifies in
seconds.

## Next, in the order I'd take it

1. **`ecs` driver + Terraform** — the last structural piece and the Phase 1 exit
   criterion. Everything else runs on `local`/`docker`. Note §12.6: the
   credential-scrub hardening needs `bubblewrap` **plus namespace privileges
   Fargate probably denies** — worth confirming early, since it bears on whether
   direct-to-Bedrock can ever be revived.
2. **Session lifecycle** — `POST /v1/sessions/{sid}/stop` does not exist; killing
   a session today means `pkill` inside the container. The supervisor already
   implements `stop_session` over the tunnel, so this is only an HTTP handler.
   Resume can use Claude Code's own `CLAUDE_CODE_RESUME_INTERRUPTED_TURN` /
   `RESUME_PROMPT` (§12.6).
3. **Idle detection + warm hold + admission gating** — one coherent pass. Idle is
   now **four local conditions with no OTel dependency** (§2.4), and the fourth —
   *no tool executing* — is what stops a one-hour build being reaped. Mind that
   MCP servers are long-lived children and need a start-time baseline.
4. **Gateway preflight** — gateway workspaces currently report `skipped`, so
   nothing checks the gateway is reachable at task start. Needs the `Provider`
   interface §12.4 asks for.

## Sharp edges that cost real time

- **`doc_num` lies.** OpenObserve reports 0 while data is in the write-ahead log.
  It made me report the pipeline broken **twice**. Query the stream instead.
- **Use `http/protobuf`.** OpenObserve's OTLP JSON parser rejects part of Claude
  Code's payload, and the failure is silent without `CLAUDE_CODE_OTEL_DIAG_STDERR`
  (now on by default in the image).
- **Managed settings cannot be overridden.** `docker exec -e OTEL_…` will not
  redirect a workspace's telemetry; restart the control plane instead.
- **Headless `-p` under-reports telemetry.** Judge completeness from a real
  interactive session, not a `-p` run.
- **Traces do not exist.** Claude Code 2.1.220 emits zero spans; proved against a
  neutral receiver, and 2.1.220 is the latest on npm. Do not chase it.
- **Ports**: OrbStack squats **4318**, Chrome DevTools squats **9222**.
- **Do not conclude from one ambiguous observation.** Two things I reported this
  session were wrong and had to be retracted — verify before asserting.

## Standing constraints

- **Attach must become owner-scoped** when users exist (§2.5). Both
  `/v1/sessions/{sid}/endpoint` and `ourcli`'s reattach currently accept any
  session in a workspace, which is safe only because there is no auth.
- Per `~/.claude/CLAUDE.md`: journal completed work to
  `~/w/.claude-journal/lufzle-lemul-cc.md` as `{YYMMddTHHmm}: {description}`, and
  commit as you go. Git is local only — no remote, nothing pushed.
