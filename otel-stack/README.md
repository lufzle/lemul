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

**Events and metrics — confirmed working** over `http/protobuf`. A handful of
turns produces `api_request`, `assistant_response`, `user_prompt`, `tool_result`
and `tool_decision` in the `default` logs stream.

**Metrics.** `claude_code_cost_usage`,
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

### Use `http/protobuf`, not `http/json`

OpenObserve's OTLP **JSON** parser rejects part of Claude Code's payload:

```
OTLPExporterError: Bad Request 400
"Invalid json: invalid type: map, expected f64 at line 1 column 1676"
```

Some payloads got through and some didn't, so telemetry looked partially broken
rather than misconfigured. `http/protobuf` produces zero errors and is now the
default.

This was invisible until `CLAUDE_CODE_OTEL_DIAG_STDERR=1` — exporter failures are
otherwise swallowed unless you run with `--debug`. It is on by default in the
image for exactly this reason.

### Traces: Claude Code 2.1.220 emits none

Not a configuration problem on our side, and not worth more time.

`-otel-traces` renders `OTEL_TRACES_EXPORTER=otlp`,
`OTEL_TRACES_SAMPLER=always_on`, an export interval and
`CLAUDE_CODE_PROPAGATE_TRACEPARENT` into managed settings, all confirmed present
in a live workspace with zero exporter errors.

**Proved against a neutral receiver.** Pointed at the S2 probe, which accepts
anything and writes one file per signal, with traces explicitly configured plus
raised flush and shutdown timeouts, Claude Code produced:

```
0001-logs.json  0002-logs.json  0003-metrics.json     ← no traces payload, ever
```

Ruled out: the receiver, the protocol (`http/json` and `http/protobuf` both),
sampling, export interval, flush and shutdown timeouts, and trace-context
propagation.

Traces are documented as **beta** (§5) and S2 listed them as an open gap. One
plausible explanation left, which we cannot test from here: they may be gated
behind a **server-side feature flag**, which a sandbox on a gateway connection
with restricted egress would never receive — feature-flag fetching is
Anthropic-bound and is exactly what `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`
turns off.

Re-check on a Claude Code upgrade. Metrics and events already cover billing and
audit, and `--output-format stream-json` covers per-turn cost with no collector
at all.

## Two traps that cost time here

**`doc_num` lies.** The streams API reports 0 while data sits in the write-ahead
log. It led me to report the pipeline as broken **twice** when it was working.
Query the stream; never trust the stat.

**Managed settings win.** Since they cannot be overridden, `docker exec -e …`
cannot redirect telemetry for a debugging run. To point a workspace at a
different collector, restart the control plane with a different
`-otel-endpoint` — that is the only lever.
