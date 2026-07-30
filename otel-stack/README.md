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

## Events do arrive — with one caveat

Events land in the `default` **logs** stream, carrying `event_name`,
`prompt_id`, `tenant_id`, `workspace_id`, `session_id` and per-event fields.

**Two things verified live here, both of which the analysis had listed as
unconfirmed:**

*Content redaction (§5.4).* A prompt of `"Run: echo hello. Then say DONE"`
arrived as:

```
prompt        = '<REDACTED>'
prompt_length = 54
```

*Managed-settings enforcement (§5.2).* Passing `-e OTEL_EXPORTER_OTLP_ENDPOINT=…`
to `docker exec` had **no effect** — telemetry still went where managed settings
said. That is the mechanism the whole "admin-enforced telemetry" claim rests on,
and it was previously untested.

### The caveat: headless runs emit almost nothing

A headless `claude -p` run yields **`user_prompt` and nothing else** — no
`api_request`, no `tool_result`, no `tool_decision`.

**This is not an OpenObserve problem.** Pointing Claude Code at the neutral S2
receiver (`otel-probe`) produced the same single `user_prompt` record, so the
events are never sent rather than being rejected downstream. Dropping the log
export interval to 2 s changed nothing.

The likely mechanism is ordering against process exit: `user_prompt` fires at the
*start* of a turn and flushes while the turn runs, whereas `api_request` and
`tool_result` fire at the *end* and are still queued when a short-lived process
exits.

**S2 captured the full event set** (`otel-probe/RESULTS.md`) — using an
*interactive* `claude`, not `claude -p`. Our product runs long-lived interactive
PTYs, so this is very likely a headless-testing artefact rather than a production
gap. But it does mean:

> **Never judge telemetry completeness from a `-p` run.** Attach a real session
> with `ourcli connect`, work in it for a few minutes, and query then.

### Traces: enabled, but nothing is emitted

`-otel-traces` renders `OTEL_TRACES_EXPORTER=otlp`,
`OTEL_TRACES_SAMPLER=always_on` and an export interval into managed settings —
confirmed present in a live workspace. **Claude Code 2.1.220 still produces zero
spans.**

Ruled out: OpenObserve rejecting them (a hand-made OTLP/JSON `POST /v1/traces`
returns 200 and creates the stream), sampling (`always_on`), and the export
interval.

Traces are documented as **beta** (§5), and S2 also listed them as an open gap.
The most likely explanations are that they need an opt-in beyond the standard
OTel variable, or that the instrumentation is not wired in this build. Worth
re-checking on a Claude Code upgrade; not worth chasing further now, since
metrics and events already cover billing and audit.

## Two traps that cost time here

**`doc_num` lies.** The streams API reports 0 while data sits in the write-ahead
log. It led me to report the pipeline as broken **twice** when it was working.
Query the stream; never trust the stat.

**Managed settings win.** Since they cannot be overridden, `docker exec -e …`
cannot redirect telemetry for a debugging run. To point a workspace at a
different collector, restart the control plane with a different
`-otel-endpoint` — that is the only lever.
