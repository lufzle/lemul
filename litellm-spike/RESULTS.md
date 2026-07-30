# LiteLLM gateway spike — does Claude Code work through a gateway?

**Verdict: yes, and cost partitioning works.** Open decision #12 (§12.4) is
validated on its two load-bearing assumptions.
Measured 2026-07-30, Claude Code **2.1.220**, `ghcr.io/berriai/litellm:main-stable`.

## What was being tested

§12.4 proposes supporting a **customer-hosted** inference gateway alongside
Bedrock, with the supervisor running a loopback proxy that injects workspace and
user metadata so the customer can partition costs in their own gateway. Two
things had to be true and neither had been demonstrated:

1. Claude Code actually works pointed at a gateway rather than a first-party API.
2. Metadata injected as **headers** is enough for per-workspace / per-session cost
   attribution — because rewriting the request body is off the table (it breaks
   SSE streaming and puts us inside message content).

## Topology

```
claude (ANTHROPIC_BASE_URL) ──> LiteLLM :4000 ──> Bedrock (aureum-dev-full-access)
```

The backend is deliberately the one AWS account with working entitlement. The
spike is about the **gateway hop**; putting a broken backend behind it would make
a failure prove nothing.

## Results

**1 · Claude Code runs through the gateway.**

```
$ ANTHROPIC_BASE_URL=http://localhost:4000 ANTHROPIC_AUTH_TOKEN=sk-… \
  claude -p "Reply with exactly: CC_VIA_GATEWAY_OK"

⚠ claude.ai connectors are disabled because ANTHROPIC_API_KEY or another auth
  source is set and takes precedence over your claude.ai login
CC_VIA_GATEWAY_OK
```

The warning is useful confirmation in itself: the env override beat an existing
claude.ai login, which is the precedence a workspace task depends on. LiteLLM's
logs show the matching `POST /v1/messages`, so the traffic really did take the
gateway path rather than leaking out to the first-party API.

**2 · LiteLLM serves the Anthropic message format**, not just OpenAI's. `POST
/v1/messages` returns a proper Anthropic envelope — `type: message`,
`content: [{type: text}]`, `stop_reason`, and an Anthropic-shaped `usage` block
including cache-token fields.

**3 · Header-injected tags partition spend.** Sending
`x-litellm-tags: workspace:…,user:…,session:…` produced:

```
1 call  $0.01075305  41,383 tokens   workspace:myproj,user:dario,session:s-live
1 call  $0.00009680       24 tokens   workspace:myproj,user:dario,session:s-abc123
```

That is exactly the attribution decision #12 wants, achieved without touching the
request body.

## Findings worth carrying forward

**Spend tracking requires Postgres.** Without `DATABASE_URL`, `/spend/logs`
silently returns nothing and the admin UI has no store to authenticate against —
requests still succeed, so the gap is invisible until you go looking for the
data. A production gateway deployment is two services, not one.

**Claude Code's per-call floor is ~41 k tokens.** A trivial `-p` prompt cost
41,383 tokens and about a cent, because the system prompt and tool definitions
dominate. Any cost modelling that assumes prompt size drives spend will be badly
wrong for short interactions. Prompt caching is what makes this bearable in a
real session, and the `usage` block does report cache fields.

**LiteLLM appends its own tags.** `User-Agent: curl/8.7.1` and similar arrive
alongside ours, so anything aggregating by tag has to filter rather than assume
every tag is one it set.

**Model names are ours to choose.** `model_name` in the LiteLLM config is what
Claude Code sends, and it must match the `ANTHROPIC_DEFAULT_*_MODEL` pins exactly.
Gateway aliases are arbitrary strings, which is why §12.4 says pins have to become
provider-scoped rather than staying Bedrock model IDs.

## What this does NOT validate

- **The loopback proxy itself.** Tags were injected here by `curl` and by
  `ANTHROPIC_CUSTOM_HEADERS`, both of which live in the session's own
  environment and are therefore forgeable by the session. The unforgeable design
  — supervisor holds the credential, one loopback port per session — is
  unbuilt. This spike proves the *mechanism*, not the *binding*.
- **Streaming.** Only non-streaming calls were exercised. Claude Code streams in
  interactive use, and SSE through a gateway is exactly where a proxy that
  touches the body would break.
- **Interactive TUI over the gateway.** Only headless `-p`.
- **WebSearch.** §3 records it as the one genuine Bedrock feature loss, and
  §12.4 speculates a gateway fronting the real Anthropic API would restore it.
  Untested — the backend here is Bedrock, so it could not have worked anyway.

## Run it

```bash
aws configure export-credentials --profile aureum-dev-full-access \
  --format env-no-export > .env          # gitignored; never commit
echo "AWS_REGION_NAME=us-east-2" >> .env

docker compose up -d
curl -s localhost:4000/health/liveliness
```

Admin UI at **http://localhost:4000/ui** — username `admin`, password is the
master key (`sk-lemul-spike`, set in `config.yaml` and `docker-compose.yml`).

Then point Claude Code at it:

```bash
env -u CLAUDE_CODE_USE_BEDROCK -u ANTHROPIC_API_KEY \
  ANTHROPIC_BASE_URL=http://localhost:4000 \
  ANTHROPIC_AUTH_TOKEN=sk-lemul-spike \
  ANTHROPIC_DEFAULT_OPUS_MODEL=claude-opus-5 \
  ANTHROPIC_DEFAULT_HAIKU_MODEL=claude-haiku-4-5 \
  claude -p "Reply with exactly: CC_VIA_GATEWAY_OK"
```

The master key here is a development placeholder and is committed deliberately;
the AWS credentials it fronts are not.
