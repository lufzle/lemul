# otel-stack — local telemetry viewer

Single-container [OpenObserve](https://openobserve.ai) for browsing Claude Code's
OpenTelemetry locally. OTLP ingest and a UI in one process, which is why it is
here rather than a Collector + Prometheus + Loki + Grafana stack.

```bash
docker compose up -d
```

UI: **http://localhost:5080** — `admin@lemul.local` / `Lemul-dev1!`

Port 5080 is deliberate. OrbStack squats **4318**, the conventional OTLP port,
and the resulting failure looks like the collector is down rather than like a
port conflict.

## Wiring it to workspaces

The control plane renders these into managed settings, so a session cannot
override or silence them (§5.2):

```bash
AUTH=$(printf 'admin@lemul.local:Lemul-dev1!' | base64)

bin/controlplane … \
  -otel-endpoint http://host.docker.internal:5080/api/default \
  -otel-headers "Authorization=Basic $AUTH" \
  -otel-protocol http/json
```

`host.docker.internal`, not `localhost`: the endpoint is consumed *inside* the
workspace container.

Leave `-otel-endpoint` empty and no telemetry is emitted at all. Since decision
#12 the gateway is the cost source of record, so telemetry is optional
infrastructure for audit rather than a dependency (§5).

## What arrives

**Metrics — confirmed working.** `claude_code_cost_usage`,
`claude_code_token_usage`, `claude_code_active_time_total`,
`claude_code_session_count`, each carrying `tenant_id`, `workspace_id`, `model`
and Claude Code's own `session_id`.

Per-workspace attribution comes from `OTEL_RESOURCE_ATTRIBUTES`, injected by the
image entrypoint. Content stays redacted: all four `OTEL_LOG_*` flags are set off
explicitly, because `OTEL_LOG_ASSISTANT_RESPONSES` silently falls back to
`OTEL_LOG_USER_PROMPTS` when unset (§5.4).

> **Note on `doc_num`.** The streams API reports 0 while data sits in the
> write-ahead log. Query the stream rather than trusting the stat — it cost me a
> wrong conclusion once already.

## Open: events are not arriving

**Metrics land; logs/events do not.** No `api_request`, `tool_result`,
`tool_decision`, `permission_mode_changed` — which is exactly the audit half that
justifies keeping telemetry at all after decision #12.

Ruled out so far:

- OpenObserve ingests logs fine — a hand-made OTLP/JSON `POST /v1/logs` returns
  200 and creates the stream.
- Not a flush-timing problem. Logs export every 5 s and metrics every 60 s
  (§5.5), so if a short-lived process were losing batches, metrics would be the
  casualty, not logs.
- `OTEL_LOGS_EXPORTER=otlp` is present in the rendered managed settings.

Still to try: `http/protobuf` instead of `http/json` for the logs signal; a
per-signal `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`; and whether the events S2 captured
needed a longer interactive session than a headless `-p` run.

S2 did capture these events against its own receiver
(`otel-probe/RESULTS.md`), so they exist — the gap is in this delivery path.
