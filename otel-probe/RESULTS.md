# S2 Results — Claude Code OpenTelemetry at content-off

**Date:** 2026-07-29 · **Claude Code:** 2.1.220 · **Transport:** OTLP `http/json`

**Verdict: PASS.** With all content flags off, Claude Code's telemetry is a
sufficient audit and cost-attribution trail. The "proxy the CLI rather than own
the client" decision in `CC_REMOTE_ANALYSIS.md` §1 holds — Phase 4 stays where
it is.

## Method

`otel-probe` (a minimal OTLP/HTTP-JSON receiver) captured a real headless
Claude Code session that read a file and ran a bash command. Two canary strings
were planted — one in the prompt, one in the bash command output — to test
redaction empirically rather than trusting the docs.

```bash
otel-probe -addr :14318 -out ./capture

CLAUDE_CODE_ENABLE_TELEMETRY=1 \
OTEL_METRICS_EXPORTER=otlp OTEL_LOGS_EXPORTER=otlp \
OTEL_EXPORTER_OTLP_PROTOCOL=http/json \
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:14318 \
OTEL_METRIC_EXPORT_INTERVAL=3000 OTEL_LOGS_EXPORT_INTERVAL=1000 \
OTEL_RESOURCE_ATTRIBUTES=tenant.id=acme-corp,workspace.id=ws_001 \
claude -p 'Read sample.txt, then run: echo SECRET_CANARY_BASH_67890. …' \
  --allowedTools Read Bash
```

No content flags were set: `OTEL_LOG_USER_PROMPTS`, `OTEL_LOG_ASSISTANT_RESPONSES`,
`OTEL_LOG_TOOL_DETAILS`, `OTEL_LOG_RAW_API_BODIES` all left at their defaults.

## 1. Privacy — clean

Grepped all 7 raw payloads for every canary and every piece of session content:

| Probe | Result |
|---|---|
| `SECRET_CANARY_BASH_67890` (bash command + its output) | **not present** |
| `SECRET_CANARY_PROMPT_12345` (in the prompt) | **not present** |
| `sample.txt` (the filename the agent read) | **not present** |
| `hello` (the file's contents) | **not present** |
| `tool_parameters`, `tool_input` | absent (0 occurrences) |
| `api_request_body`, `api_response_body` | absent (0 occurrences) |

`prompt` and `response` attributes are present but carry the literal string
`<REDACTED>` (3 occurrences). Only `prompt_length` / `response_length` are real.

> The 4 hits for `command` are `hook_type: "command"` and `command_path_count`
> — plugin metadata, not shell commands.

**This is a strong security-review position:** filenames, commands, file
contents, and prompt text never leave the sandbox. Not "we don't store it" —
it is never emitted.

## 2. Per-tenant attribution — works exactly as needed

`OTEL_RESOURCE_ATTRIBUTES` landed on **7/7 payloads**, and on every individual
metric data point and event attribute set:

```
tenant.id       acme-corp
workspace.id    ws_001
service.name    claude-code
service.version 2.1.220
```

Per-workspace billing therefore needs no custom instrumentation — inject the
attributes per sandbox at task launch and the accounting falls out.

## 3. Cost data — sufficient for billing

`api_request` events carry, per request:

| Field | Observed |
|---|---|
| `cost_usd_micros` | 16589, 80251 |
| `cost_usd` | 0.016589, 0.080251 |
| `input_tokens` / `output_tokens` | 2 / 162, 57 |
| `cache_read_tokens` | 16432, 27308 |
| `cache_creation_tokens` | 10876, 240 |
| `duration_ms` | 2579, 3717 |
| `model` | `claude-opus-5` |
| `effort` | `high` |
| `speed` | `normal` |
| `query_source` | `sdk` |

Plus metrics `claude_code.cost.usage` and `claude_code.token.usage` (the latter
split by `type`: `input` / `output` / `cacheRead` / `cacheCreation`).

**Note:** metric-level `model` was `us.anthropic.claude-opus-5[1m]` — the
Bedrock inference-profile ID with the 1M-context suffix — while the event-level
`model` was the bare `claude-opus-5`. Normalize when aggregating across both.

## 4. Audit trail — sufficient

`tool_decision` and `tool_result` pair up per tool call via `tool_use_id`:

```
tool_decision  tool_name=Bash|Read  decision=accept  source=config
               tool_source=builtin  tool_use_id=toolu_bdrk_019D3…
tool_result    tool_name=Bash|Read  success=true  duration_ms=2|720
               tool_input_size_bytes=46|78  tool_result_size_bytes=10|24
               tool_use_id=toolu_bdrk_019D3…
```

So you can prove **which tools ran, whether they were approved and by what
mechanism, whether they succeeded, and how much data moved** — without seeing
any of the content. `source=config` here reflects `--allowedTools`; an
interactive rejection would show `user_reject`.

`prompt.id` correlated all 11 events from the single turn, as documented.

## 5. Event types observed (10)

`api_request` · `assistant_response` · `user_prompt` · `tool_decision` ·
`tool_result` · `mcp_server_connection` · `plugin_loaded` · `hook_registered` ·
`hook_execution_start` · `hook_execution_complete`

Metrics (4): `claude_code.session.count` · `claude_code.cost.usage` ·
`claude_code.token.usage` · `claude_code.active_time.total`

## Findings that change the plan

### F1 · Event names have no `claude_code.` prefix

Events arrive as `api_request`, `tool_result`, `user_prompt` — **not**
`claude_code.api_request`. Metrics *do* carry the prefix
(`claude_code.cost.usage`). Anything filtering on the documented prefixed event
names will silently match nothing.

### F2 · Hook and plugin telemetry is emitted, and it is useful

Not in the documented list, but present: `hook_registered`,
`hook_execution_start/complete` (with `hook_name`, `num_blocking`, `safe_mode`,
`managed_only`) and `plugin_loaded` (with `plugin.name`, `plugin.scope`,
`marketplace.name`, `plugin_id_hash`).

**This is a security-review asset we did not know we had:** it lets us prove
which hooks and plugins ran in a customer's sandbox. Worth surfacing in the
dashboard — an unexpected plugin load is exactly the kind of thing a platform
team wants alerted on.

### F3 · `user.id` and `terminal.type` are emitted by default

`user.id` (a hash) and `terminal.type` (`iTerm.app`) appear on every event and
metric. Both are documented as always-on. For a per-tenant dashboard this is
fine and arguably desirable, but it should be in the privacy disclosure, and
`OTEL_METRICS_INCLUDE_ACCOUNT_UUID=false` is worth setting if a customer objects
to account-level identifiers.

### F4 · `query_source=sdk` in headless mode

Headless (`-p`) runs report `query_source: sdk`, not `main`. Any per-source cost
breakdown must treat `sdk` as a first-class value alongside `main`, `subagent`,
and `auxiliary`.

### F5 · Ports — check before assuming 4318 is free

The first run silently 404'd because **OrbStack** was already listening on 4318.
Use a non-default port for the collector, or check
`lsof -nP -iTCP:4318 -sTCP:LISTEN` first.

## Not covered by this spike

- **Managed-settings enforcement.** Payload contents are validated; the v2.1.217+
  behavior where managed settings *strip* developer-set `OTEL_EXPORTER_OTLP_*`
  vars is not. Test in the container image build (Phase 1), since that is where
  the managed settings file lives — and remember exporter *selectors* follow
  normal precedence, so they must be set in managed settings too or a user can
  silence a signal with `OTEL_LOGS_EXPORTER=none`.
- **Interactive-session events.** `permission_mode_changed` (bypass-mode
  detection) and `tool_decision` with `source=user_reject` need an interactive
  run; headless with `--allowedTools` can't produce them.
- **`api_error` / `api_refusal`.** Not triggered by a successful session.
- **Traces (beta).** `CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1` not exercised.
- **Volume.** One short session; not a load test of the ingest path.
