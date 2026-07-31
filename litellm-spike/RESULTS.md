# LiteLLM gateway spike — does Claude Code work through a gateway?

**Verdict: yes, and cost partitioning works.** Open decision #12 (§12.4) is
validated on its two load-bearing assumptions.
Measured 2026-07-30, Claude Code **2.1.220**, `ghcr.io/berriai/litellm:main-stable`.

> **The backend moved on 2026-07-31.** Everything below was measured against
> Bedrock; `config.yaml` now points at **OpenRouter** (`anthropic/claude-opus-4.8`,
> `claude-sonnet-4.6`, `claude-haiku-4.5`). The findings are about the gateway
> hop and still hold, but read the Bedrock model IDs here as history rather than
> as current configuration.
>
> **Use OpenRouter's native Anthropic wire, not the `openrouter/` provider.**
> OpenRouter serves `POST /api/v1/messages` in Anthropic format, so LiteLLM's
> `anthropic/` provider can call it directly. The `openrouter/` provider instead
> converts Anthropic → OpenAI → Anthropic for a request that started and ended
> in Anthropic format. Measured difference in the response: the native path
> returns `cache_creation_input_tokens`, `cache_read_input_tokens`, the
> `cache_creation.ephemeral_5m/1h` split, `thinking_tokens` and `service_tier`;
> the translated path returns none of them. Attribution tags survive on both.
>
> `api_base` takes **no `/v1`** — LiteLLM appends `/v1/messages` itself, and
> `.../api/v1` yields `.../api/v1/v1/messages`, which OpenRouter answers with the
> HTML of its marketing site.
>
> **Prompt caching works on the native wire, reads included** — measured with a
> 4401-token cached system prefix: first call `cache_creation_input_tokens=4401`,
> second `cache_read_input_tokens=4401`. This matters because Claude Code's
> per-call floor is ~35–41 k tokens, so losing the cache would be expensive
> rather than cosmetic.
>
> **Cost needs explicit pricing, and the reason is not the obvious one.**
> LiteLLM *does* read a cost from OpenRouter — but only on the `openrouter/`
> provider path (`llms/openrouter/chat/transformation.py`), and what it reads is
> `usage.cost`. **This account is BYOK**, so OpenRouter charges nothing and
> `usage.cost` is `0` *by definition*; the real figure arrives as
> `usage.cost_details.upstream_inference_cost`, which LiteLLM 1.94.0 has no
> reference to anywhere. Both paths therefore recorded `0.0` — the `openrouter/`
> one faithfully, the `anthropic/` one for want of a price map.
>
> Fixed with `model_info` rates per model, **including the cache tiers**, which
> are load-bearing rather than garnish: one measured Claude Code turn was 34,455
> cache-write tokens out of 34,650. Verified against
> `upstream_inference_cost` on all three models and both cache tiers — exact
> agreement to eight significant figures, including a 12× difference between a
> cache-write call and a cache-read call of the same prompt.
>
> These rates are a **model** of the cost, not the invoice, so reconcile them
> against `upstream_inference_cost` periodically. That is §12.4's
> two-independent-signals argument doing real work.
>
> **Two log fields that lie, and one of them cost me a wrong conclusion.** The
> `x-litellm-response-cost` **response header** prices only the non-cached
> tokens — it read `2.8e-05` where the true cost was `0.00553`, a 200× understatement
> — because it is computed before cache usage is known. The `/spend/logs` entry
> is correct. Read cost from the log, never the header. Separately, that same log
> leaves `prompt_tokens_details.cache_write_tokens` **unset** on the
> anthropic-provider path though the cost calculator plainly sees those tokens,
> so do not infer cache behaviour from it either; the response body is the
> authority.

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

## The attribution model, tested

LiteLLM exposes three identity fields, and they are populated by three *different*
mechanisms — which matters, because only one of them is unforgeable by the session.

| LiteLLM field | Populate with | How | Forgeable by the session? |
|---|---|---|---|
| **Team ID** | our **workspace id** | the **virtual key's** team binding | **No** — the proxy chooses the key |
| **End User** | our **user id** | `x-litellm-end-user-id` header | Only if the session can set the header |
| **Session ID** | *Claude Code's* session id | CC's own `metadata` block | n/a — CC's, not ours |
| Tags | our session id, anything else | `x-litellm-tags` header | Only if the session can set the header |

Verified with a key bound to a team:

```
team_id    = 394b877d-8109-43bf-88f9-542783ed4f41   (from the key)
end_user   = dario                                  (from the header)
session_id = 6d99ce78-…                             (not ours)
```

**Our `end_user` header beats Claude Code's `metadata.user_id`.** Tested with both
present in the same request; the header won. Without that, the field would be
unusable — Claude Code fills it with its own JSON blob
(`{"device_id":…,"account_uuid":…,"session_id":…}`).

**Session ID is Claude Code's, not ours.** Which is arguably useful — it
correlates a spend row to a specific CC conversation — but our session id has to
live in a tag.

**Team-via-key is the only unforgeable binding**, because the session never sees
which key the supervisor's proxy uses. It also unlocks LiteLLM's per-team budgets
and rate limits, so a workspace could carry a spend cap enforced at the gateway.

### The provisioning tension this creates

Binding workspace → team means creating a team and a key per workspace, which
requires **admin access to the customer's gateway**. That cuts against the
posture that we hold no credentials inside their environment, so it should not be
the default:

- **Default — customer issues one key.** Attribution via `end_user` and tags
  only. No admin access, no provisioning step. Loses per-workspace budget
  *enforcement*; keeps per-workspace *reporting*.
- **Opt-in — we provision teams and keys.** Requires a gateway admin credential,
  and buys enforceable per-workspace budgets and revocation scoped to one
  workspace.

Worth deciding per customer rather than globally, and it belongs with the
per-tenant configuration work in Phase 3.

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
